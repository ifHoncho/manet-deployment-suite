package meshd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"meridian/trellis/internal/config"
	"meridian/trellis/internal/hostapd"
	"meridian/trellis/internal/nl80211"
	"meridian/trellis/internal/nodestate"
	"meridian/trellis/internal/proc"
	"meridian/trellis/internal/radio"
	"meridian/trellis/internal/reconcile"
	"meridian/trellis/internal/status"
	"meridian/trellis/internal/wpa"
)

// hostapd control-interface events.
const (
	evAPEnabled         = "AP-ENABLED"
	evAPDisabled        = "AP-DISABLED"
	evAPStaConnected    = "AP-STA-CONNECTED"
	evAPStaDisconnected = "AP-STA-DISCONNECTED"
)

// DefaultAPTXPowerDBm caps a dedicated AP radio (architecture §10): phones
// are close, and fifty nearby nodes should not shout over each other.
const DefaultAPTXPowerDBm = 15

// apReconciler runs hostapd on ap0 (architecture §10).
type apReconciler struct {
	d        *Daemon
	sup      *proc.Supervisor
	supSince time.Time
	ctrl     *wpa.Ctrl
	events   *wpa.EventConn
	evStop   chan struct{}
	applied  *int32 // transmit power limit applied, mBm
	path     string
	idGen    uint64 // identity generation hostapd was started under
}

func newAPReconciler(d *Daemon) *apReconciler { return &apReconciler{d: d} }

func (a *apReconciler) Name() string { return "ap" }

func (a *apReconciler) confPath() string {
	return filepath.Join(a.d.opts.RunDir, "hostapd-"+APIfname+".conf")
}

func (a *apReconciler) ctrlDir() string { return filepath.Join(a.d.opts.RunDir, "hostapd") }

// apSSID is the configured SSID, or the callsign.
func apSSID(n *config.Node) string {
	if n.AP.SSID != "" {
		return n.AP.SSID
	}
	return n.Callsign
}

// apPassphrase is the configured passphrase, or the one generated for this
// node on first start.
func apPassphrase(n *config.Node, id *nodestate.Identity) string {
	if n.AP.Passphrase != "" {
		return n.AP.Passphrase
	}
	if id != nil {
		return id.APPassphrase
	}
	return ""
}

func (a *apReconciler) Reconcile(ctx context.Context) (reconcile.Result, error) {
	d := a.d
	var res reconcile.Result
	warn := func(level, code, format string, args ...any) {
		res.Warnings = append(res.Warnings, reconcile.Warning{Level: level, Code: code, Message: fmt.Sprintf(format, args...)})
	}
	var apIfname string
	var apIfindex int
	var shared, bridgePresent, meshJoined bool
	var plan *radio.Plan
	var meshFreq int
	var sharesLink2 bool
	d.facts.Read(func(f *Facts) {
		apIfname, apIfindex, shared, bridgePresent = f.APIfname, f.APIfindex, f.APShared, f.BridgePresent
		plan, meshFreq, meshJoined = f.Plan, f.MeshFreqMHz, f.Supplicant.Joined
		sharesLink2 = f.APSharesLink2
		if sharesLink2 {
			// The AP shares the second link's radio: follow that link.
			meshFreq, meshJoined = f.Link2.FreqMHz, f.Link2.Supplicant.Joined
		}
	})
	cfg, profile := d.cfg, d.profile
	// The shared mesh link's channel, when the AP shares a mesh radio.
	sharedRadio := profile.Radio
	if sharesLink2 && profile.Radio2 != nil {
		sharedRadio = *profile.Radio2
	}
	base := status.AP{Enabled: cfg.APEnabled(), Clients: []status.Client{}}

	if apIfname == "" || !bridgePresent || plan == nil || plan.AP == nil {
		if a.sup != nil {
			a.stop()
			res.Changed = true
		}
		d.facts.Update(func(f *Facts) { f.AP = base })
		return res, nil
	}
	rd := plan.AP
	base.Interface, base.Radio, base.SharesMeshRadio = apIfname, rd.Fingerprint, shared

	if a.path == "" {
		p, err := proc.LookPath("hostapd")
		if err != nil {
			warn("error", "ap-no-hostapd", "hostapd is not installed, so the client AP cannot start: apt install hostapd")
			res.RequeueAfter = 30 * time.Second
			d.facts.Update(func(f *Facts) { f.AP = base })
			return res, nil
		}
		a.path = p
	}

	// Band and channel.
	var choice hostapd.Choice
	if shared {
		freq, err := config.ChannelFrequencyMHz(sharedRadio.Band, sharedRadio.Channel)
		if err != nil {
			return res, err
		}
		choice = hostapd.Choice{Band: sharedRadio.Band, Channel: sharedRadio.Channel, FreqMHz: freq}
	} else {
		// The preferred band first, then the other one: some radios
		// (Intel, for one) refuse to start an AP on 5 GHz at all.
		bands := []string{plan.APBand, otherBand(plan.APBand)}
		var errs []string
		for _, band := range bands {
			if !rd.HasBand(band) {
				continue
			}
			var bi *nl80211.BandInfo
			if nb, ok := nl80211.ParseBand(band); ok && rd.Wiphy != nil {
				bi = rd.Wiphy.Bands[nb]
			}
			pinned := 0
			if cfg.AP.Band == band {
				pinned = cfg.AP.Channel
			}
			c, err := hostapd.Choose(band, bi, pinned, meshFreq)
			if err == nil {
				choice = c
				break
			}
			errs = append(errs, err.Error())
		}
		if choice.Channel == 0 {
			if a.sup != nil {
				a.stop()
				res.Changed = true
			}
			warn("error", "ap-channel", "%s cannot start an AP: %s", rd.Describe(), strings.Join(errs, "; "))
			d.facts.Update(func(f *Facts) { f.AP = base })
			return res, nil
		}
		if choice.Band != plan.APBand {
			warn("info", "ap-band-fallback", "the client AP runs on %s because %s has no usable %s channel for an AP (driver or regulatory limit)", choice.Band, rd.Describe(), plan.APBand)
		}
	}
	base.Band, base.Channel, base.FreqMHz = choice.Band, choice.Channel, choice.FreqMHz

	ssid, pass := apSSID(cfg), apPassphrase(cfg, d.identity())
	if len(pass) < 8 {
		warn("error", "ap-passphrase", "no usable AP passphrase (set ap.passphrase in node.yaml or let meshd generate one)")
		d.facts.Update(func(f *Facts) { f.AP = base })
		return res, nil
	}
	base.SSID = ssid
	country := cfg.AP.Country
	if country == "" {
		country = profile.Regulatory.Country
	}
	ht, vht := false, false
	if nb, ok := nl80211.ParseBand(choice.Band); ok && rd.Wiphy != nil {
		if bi := rd.Wiphy.Bands[nb]; bi != nil {
			ht, vht = bi.HasHT, bi.HasVHT
		}
	}
	conf := hostapd.Render(hostapd.Config{
		Interface: apIfname, Bridge: BridgeIfname, CtrlDir: a.ctrlDir(), SSID: ssid, Passphrase: pass, Country: country,
		Band: choice.Band, Channel: choice.Channel, HT: ht, VHT: vht, Isolate: cfg.AP.Isolate,
	})
	// On a shared radio the mesh comes first: hostapd joins the channel the
	// mesh already holds, instead of racing wpa_supplicant's join.
	if shared && !meshJoined && a.sup == nil {
		base.Hostapd.State = "waiting for the mesh link"
		res.RequeueAfter = 2 * time.Second
		d.facts.Update(func(f *Facts) { f.AP = base })
		return res, nil
	}
	restart := false
	if cur, err := os.ReadFile(a.confPath()); err != nil || string(cur) != conf {
		if err := os.WriteFile(a.confPath(), []byte(conf), 0o600); err != nil {
			return res, fmt.Errorf("write %s: %w", a.confPath(), err)
		}
		d.log.Info("wrote hostapd config", "path", a.confPath(), "ssid", ssid, "band", choice.Band, "channel", choice.Channel, "shares_mesh_radio", shared)
		restart = a.sup != nil
		res.Changed = true
	}
	reason := "configuration changed"
	// After a live re-address, associated clients hold leases from the old
	// pool. Restarting hostapd drops them; on reconnecting they ask for
	// their lease again, which the local dnsmasq NAKs (architecture §7.3).
	if gen := d.identityGen(); gen != a.idGen {
		a.idGen = gen
		if a.sup != nil && !restart {
			restart, reason = true, "node re-addressed; clients must re-lease"
		}
	}
	argv := []string{a.path, a.confPath()}
	if a.sup != nil && (restart || !a.sup.SameArgv(argv)) {
		d.log.Info("restarting hostapd", "reason", reason)
		a.stop()
		res.Changed = true
	}
	if a.sup == nil {
		if err := os.MkdirAll(a.ctrlDir(), 0o755); err != nil {
			return res, err
		}
		a.sup = proc.New(d.log, "hostapd", argv...)
		if err := a.sup.Start(d.ctx); err != nil {
			a.sup = nil
			return res, fmt.Errorf("start hostapd: %w", err)
		}
		a.supSince = time.Now()
		a.applied = nil
		res.Changed = true
	}

	h := status.Helper{Running: a.sup.Alive(), PID: a.sup.PID(), Restarts: a.sup.Restarts(), State: "starting"}
	base.Hostapd = h
	if h.Restarts > 3 {
		warn("warning", "ap-restarting", "hostapd has exited %d times; its messages are in the journal: journalctl -u trellis-meshd | grep proc=hostapd", h.Restarts)
	}
	if a.ctrl == nil {
		c, err := wpa.DialCtrl(filepath.Join(a.ctrlDir(), apIfname))
		if err != nil {
			res.RequeueAfter = time.Second
			d.facts.Update(func(f *Facts) { f.AP = base })
			return res, nil
		}
		a.ctrl = c
	}
	rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	st, err := a.ctrl.GetStatus(rctx)
	cancel()
	if err != nil {
		a.ctrl.Close()
		a.ctrl = nil
		a.stopEvents()
		res.RequeueAfter = time.Second
		d.facts.Update(func(f *Facts) { f.AP = base })
		return res, nil
	}
	h.State = st["state"]
	base.Hostapd = h
	if f, _ := strconv.Atoi(st["freq"]); f > 0 {
		base.FreqMHz = f
	}
	if a.events == nil {
		if ec, err := wpa.DialEvents(filepath.Join(a.ctrlDir(), apIfname)); err == nil {
			a.events = ec
			a.evStop = make(chan struct{})
			go a.eventLoop(ec, a.evStop)
		}
	}
	enabled := h.State == "ENABLED"

	// Transmit power cap, only on a radio of the AP's own.
	if !shared {
		dbm := DefaultAPTXPowerDBm
		if cfg.AP.TXPowerDBm != nil {
			dbm = *cfg.AP.TXPowerDBm
		}
		want := int32(dbm) * 100
		if enabled && (a.applied == nil || *a.applied != want) {
			if err := d.nl.SetTXPower(apIfindex, nl80211.TXPowerLimited, want); err != nil {
				warn("warning", "ap-tx-power", "cannot cap the AP transmit power: %v", err)
			} else {
				d.log.Info("AP transmit power capped", "interface", apIfname, "dbm", dbm)
				a.applied = &want
				res.Changed = true
			}
		}
	}
	if in, err := d.nl.InterfaceByIndex(apIfindex); err == nil && in.HasTXPower {
		base.TXPowerDBm = float64(in.TXPowerMBm) / 100
	}

	if !enabled {
		res.RequeueAfter = 2 * time.Second
		if time.Since(a.supSince) > 20*time.Second {
			warn("warning", "ap-not-up", "the client AP on %s is not up after %s (hostapd state %s)", apIfname, time.Since(a.supSince).Round(time.Second), h.State)
		}
	}
	d.facts.Update(func(f *Facts) { f.AP = base })
	return res, nil
}

func otherBand(b string) string {
	if b == config.Band5G {
		return config.Band2G
	}
	return config.Band5G
}

func (a *apReconciler) eventLoop(ec *wpa.EventConn, stop chan struct{}) {
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
		case evAPStaConnected:
			a.d.log.Info("client joined the AP", "mac", ev.Arg())
			a.d.Kick("ap client joined")
		case evAPStaDisconnected:
			a.d.log.Info("client left the AP", "mac", ev.Arg())
			a.d.Kick("ap client left")
		case evAPEnabled:
			a.d.log.Info("client AP is up")
			a.d.Kick("ap enabled")
		case evAPDisabled:
			// Expected while meshd stops: helpers end with its context.
			if a.d.stopping() {
				a.d.log.Info("client AP went down (meshd stopping)")
			} else {
				a.d.log.Warn("client AP went down")
			}
			a.d.Kick("ap disabled")
		}
	}
}

func (a *apReconciler) stopEvents() {
	if a.events != nil {
		close(a.evStop)
		a.events.Close()
		a.events = nil
	}
}

func (a *apReconciler) stop() {
	a.stopEvents()
	if a.ctrl != nil {
		a.ctrl.Close()
		a.ctrl = nil
	}
	if a.sup != nil {
		_ = a.sup.Stop(5 * time.Second)
		a.sup = nil
	}
	a.applied = nil
}

func (a *apReconciler) Teardown(ctx context.Context) error {
	a.stop()
	_ = os.Remove(a.confPath())
	return nil
}
