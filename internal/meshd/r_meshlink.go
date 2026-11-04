package meshd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"meridian/trellis/internal/config"
	"meridian/trellis/internal/nl80211"
	"meridian/trellis/internal/proc"
	"meridian/trellis/internal/reconcile"
	"meridian/trellis/internal/status"
	"meridian/trellis/internal/wpa"
)

// meshLinkReconciler runs wpa_supplicant on one mesh link's interface for
// 802.11s + SAE and applies the transmit power cap (architecture §5). Link
// 0 is mesh0 (profile radio); link 1 is mesh1, the optional second link
// (profile radio2, decision D-09). One instance runs per link.
type meshLinkReconciler struct {
	d        *Daemon
	link     int
	sup      *proc.Supervisor
	supSince time.Time
	ctrl     *wpa.Ctrl
	events   *wpa.EventConn
	evStop   chan struct{}
	applied  *int32 // mBm limit applied, nil = automatic
	wpaPath  string
}

func newMeshLinkReconciler(d *Daemon, link int) *meshLinkReconciler {
	return &meshLinkReconciler{d: d, link: link}
}

func (m *meshLinkReconciler) Name() string {
	if m.link == 0 {
		return "meshlink"
	}
	return "meshlink2"
}

// ifname is the link's interface name.
func (m *meshLinkReconciler) ifname() string {
	if m.link == 0 {
		return MeshIfname
	}
	return Mesh2Ifname
}

// radioPlan is the link's band, channel and width from the profile, and
// whether the profile has this link at all.
func (m *meshLinkReconciler) radioPlan(p *config.Profile) (config.RadioPlan, bool) {
	if m.link == 0 {
		return p.Radio, true
	}
	if p.Radio2 == nil {
		return config.RadioPlan{}, false
	}
	return *p.Radio2, true
}

// linkFacts reads the link's interface and its radio's transmit power cap.
func (m *meshLinkReconciler) linkFacts(f *Facts) (ifname string, ifindex int, radioCap *int) {
	if m.link == 0 {
		if f.Plan != nil && f.Plan.Mesh != nil {
			radioCap = f.Plan.Mesh.TXPowerCapDBm
		}
		return f.MeshIfname, f.MeshIfindex, radioCap
	}
	if f.Plan != nil && f.Plan.Mesh2 != nil {
		radioCap = f.Plan.Mesh2.TXPowerCapDBm
	}
	return f.Link2.Ifname, f.Link2.Ifindex, radioCap
}

// setFacts records the link's supplicant state, frequency and power.
func (m *meshLinkReconciler) setFacts(sup status.Supplicant, freq int, txDBm float64) {
	m.d.facts.Update(func(f *Facts) {
		if m.link == 0 {
			f.Supplicant, f.MeshFreqMHz, f.TXPowerDBm = sup, freq, txDBm
			return
		}
		f.Link2.Supplicant, f.Link2.FreqMHz, f.Link2.TXPowerDBm = sup, freq, txDBm
	})
}

func (m *meshLinkReconciler) confPath() string {
	return filepath.Join(m.d.opts.RunDir, "wpa-"+m.ifname()+".conf")
}

func (m *meshLinkReconciler) ctrlPath() string {
	return filepath.Join(m.d.opts.RunDir, "wpa", m.ifname())
}

func (m *meshLinkReconciler) Reconcile(ctx context.Context) (reconcile.Result, error) {
	d := m.d
	var res reconcile.Result
	warn := func(level, code, format string, args ...any) {
		res.Warnings = append(res.Warnings, reconcile.Warning{Level: level, Code: code, Message: fmt.Sprintf(format, args...)})
	}
	var meshIfname string
	var meshIfindex int
	var radioCap *int
	d.facts.Read(func(f *Facts) { meshIfname, meshIfindex, radioCap = m.linkFacts(f) })
	rp, hasLink := m.radioPlan(d.profile)

	if meshIfname == "" || !hasLink {
		if m.sup != nil {
			m.stop()
			res.Changed = true
		}
		m.setFacts(status.Supplicant{}, 0, 0)
		return res, nil
	}

	if m.wpaPath == "" {
		p, err := proc.LookPath("wpa_supplicant")
		if err != nil {
			return res, err
		}
		m.wpaPath = p
	}
	profile, cfg := d.profile, d.cfg
	freq, err := config.ChannelFrequencyMHz(rp.Band, rp.Channel)
	if err != nil {
		return res, err
	}
	// Beacon interval 100 TU, the same as the client AP's: drivers that
	// require all beaconing interfaces on a radio to agree ("STA/AP BI must
	// match" in iw phy) refuse to start the AP next to a mesh beaconing at
	// wpa_supplicant's mesh default of 1000 TU.
	conf := wpa.Render(wpa.MeshConfig{
		CtrlDir: filepath.Join(d.opts.RunDir, "wpa"), MeshID: profile.MeshID, Passphrase: profile.SAEPassphrase,
		FreqMHz: freq, WidthMHz: rp.Width, BeaconIntTU: 100,
	})
	restart := false
	if cur, err := os.ReadFile(m.confPath()); err != nil || string(cur) != conf {
		if err := os.WriteFile(m.confPath(), []byte(conf), 0o600); err != nil {
			return res, fmt.Errorf("write %s: %w", m.confPath(), err)
		}
		d.log.Info("wrote wpa_supplicant config", "path", m.confPath(), "mesh_id", profile.MeshID, "freq", freq, "width", rp.Width)
		restart = m.sup != nil
		res.Changed = true
	}
	argv := []string{m.wpaPath, "-i", meshIfname, "-D", "nl80211", "-c", m.confPath()}
	if m.sup != nil && (restart || !m.sup.SameArgv(argv)) {
		d.log.Info("restarting wpa_supplicant", "interface", meshIfname, "reason", "configuration changed")
		m.stop()
		res.Changed = true
	}
	if m.sup == nil {
		m.sup = proc.New(d.log, "wpa_supplicant", argv...)
		if err := m.sup.Start(d.ctx); err != nil {
			m.sup = nil
			return res, fmt.Errorf("start wpa_supplicant: %w", err)
		}
		m.supSince = time.Now()
		m.applied = nil
		res.Changed = true
	}

	sup := status.Supplicant{Running: m.sup.Alive(), PID: m.sup.PID(), Restarts: m.sup.Restarts(), State: "starting"}
	if m.ctrl == nil {
		c, err := wpa.DialCtrl(m.ctrlPath())
		if err != nil {
			res.RequeueAfter = time.Second
			m.setFacts(sup, 0, 0)
			return res, nil
		}
		m.ctrl = c
	}
	rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	st, err := m.ctrl.GetStatus(rctx)
	cancel()
	if err != nil {
		m.ctrl.Close()
		m.ctrl = nil
		m.stopEvents()
		res.RequeueAfter = time.Second
		m.setFacts(sup, 0, 0)
		return res, nil
	}
	sup.State, sup.Joined = st.State(), st.Joined()
	if m.events == nil {
		if ec, err := wpa.DialEvents(m.ctrlPath()); err == nil {
			m.events = ec
			m.evStop = make(chan struct{})
			go m.eventLoop(ec, m.evStop)
		}
	}

	// Transmit power cap.
	var want *int32
	if cfg.TXPowerDBm != nil {
		v := int32(*cfg.TXPowerDBm) * 100
		want = &v
	}
	if radioCap != nil {
		v := int32(*radioCap) * 100
		if want == nil || v < *want {
			want = &v
		}
	}
	if sup.Joined && !sameLimit(want, m.applied) {
		var err error
		if want == nil {
			err = d.nl.SetTXPower(meshIfindex, nl80211.TXPowerAutomatic, 0)
		} else {
			err = d.nl.SetTXPower(meshIfindex, nl80211.TXPowerLimited, *want)
		}
		if err != nil {
			warn("warning", "tx-power", "cannot apply transmit power setting: %v", err)
		} else {
			if want == nil {
				d.log.Info("transmit power set to automatic", "interface", meshIfname)
			} else {
				d.log.Info("transmit power capped", "interface", meshIfname, "dbm", *want/100)
			}
			m.applied = want
			res.Changed = true
		}
	}
	txDBm := 0.0
	if in, err := d.nl.InterfaceByIndex(meshIfindex); err == nil && in.HasTXPower {
		txDBm = float64(in.TXPowerMBm) / 100
	}

	if sup.Restarts > 3 {
		warn("warning", "supplicant-restarting", "wpa_supplicant has restarted %d times; see the journal", sup.Restarts)
	}
	if !sup.Joined {
		res.RequeueAfter = 2 * time.Second
		if time.Since(m.supSince) > 20*time.Second {
			warn("warning", "mesh-not-joined", "mesh interface %s is not joined after %s (wpa_state=%s)", meshIfname, time.Since(m.supSince).Round(time.Second), sup.State)
		}
	}
	m.setFacts(sup, st.Freq(), txDBm)
	return res, nil
}

func sameLimit(a, b *int32) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func (m *meshLinkReconciler) eventLoop(ec *wpa.EventConn, stop chan struct{}) {
	for {
		select {
		case <-stop:
			return
		default:
		}
		ev, err := ec.Next(2 * time.Second)
		if err != nil {
			if wpa.IsTimeout(err) {
				continue
			}
			return
		}
		switch ev.Name {
		case wpa.EvMeshPeerConnected:
			m.d.log.Info("mesh peer connected", "interface", m.ifname(), "peer", ev.Arg())
			m.d.Kick("peer connected")
		case wpa.EvMeshPeerDisconnected:
			m.d.log.Info("mesh peer disconnected", "interface", m.ifname(), "peer", ev.Arg())
			m.d.Kick("peer disconnected")
		case wpa.EvMeshGroupStarted:
			m.d.log.Info("mesh group started", "interface", m.ifname(), "mesh_id", ev.KV["ssid"])
			m.d.Kick("mesh started")
		case wpa.EvMeshGroupRemoved:
			if m.d.stopping() {
				m.d.log.Info("mesh group removed (meshd stopping)", "interface", m.ifname())
			} else {
				m.d.log.Warn("mesh group removed", "interface", m.ifname())
			}
			m.d.Kick("mesh removed")
		case wpa.EvMeshSAEAuthFailure:
			m.d.log.Warn("SAE authentication failed: peer has a different passphrase", "interface", m.ifname(), "peer", ev.KV["addr"])
			m.d.facts.AddSAEFailure(ev.KV["addr"], "failure", "SAE authentication failed (wrong passphrase on one side)")
		case wpa.EvMeshSAEAuthBlocked:
			m.d.log.Warn("SAE authentication blocked after repeated failures", "interface", m.ifname(), "peer", ev.KV["addr"], "seconds", ev.KV["duration"])
			m.d.facts.AddSAEFailure(ev.KV["addr"], "blocked", "SAE blocked for "+ev.KV["duration"]+" s after repeated failures")
		}
	}
}

func (m *meshLinkReconciler) stopEvents() {
	if m.events != nil {
		close(m.evStop)
		m.events.Close()
		m.events = nil
	}
}

func (m *meshLinkReconciler) stop() {
	m.stopEvents()
	if m.ctrl != nil {
		m.ctrl.Close()
		m.ctrl = nil
	}
	if m.sup != nil {
		_ = m.sup.Stop(5 * time.Second)
		m.sup = nil
	}
	m.applied = nil
}

func (m *meshLinkReconciler) Teardown(ctx context.Context) error {
	m.stop()
	_ = os.Remove(m.confPath())
	return nil
}
