// Package meshd is the privileged daemon: the sole owner of network state on
// a node. It loads configuration, runs the reconcile loop and serves status
// on a unix socket (architecture §12.2, §12.3).
package meshd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/vishvananda/netlink"

	"meridian/trellis/internal/batadv"
	"meridian/trellis/internal/config"
	"meridian/trellis/internal/gossip"
	"meridian/trellis/internal/hostapd"
	"meridian/trellis/internal/hwdb"
	"meridian/trellis/internal/nft"
	"meridian/trellis/internal/nl80211"
	"meridian/trellis/internal/nodestate"
	"meridian/trellis/internal/radio"
	"meridian/trellis/internal/reconcile"
	"meridian/trellis/internal/rtnl"
	"meridian/trellis/internal/status"
)

// Interface names Trellis owns.
const (
	MeshIfname   = "mesh0"
	Mesh2Ifname  = "mesh1" // the second mesh link (profile radio2)
	APIfname     = "ap0"
	BatIfname    = "bat0"
	BridgeIfname = "br-mesh"
)

// Options configure the daemon.
type Options struct {
	Version     string
	NodePath    string
	ProfilePath string // overrides the node config's profile path when set
	StateDir    string
	RunDir      string
	Socket      string
	SysRoot     string
	Log         *slog.Logger
	// Interval is the slow reconcile timer.
	Interval time.Duration
}

// Daemon is one meshd instance.
type Daemon struct {
	opts    Options
	log     *slog.Logger
	started time.Time

	cfg      *config.Node
	profile  *config.Profile
	identMu  sync.RWMutex
	ident    *nodestate.Identity // replaced on change, never modified in place
	identGen uint64              // bumped when the node ID or MAC changes while running
	radioMem *nodestate.Radios
	hw       *hwdb.DB

	pendingMu sync.Mutex
	pending   *pendingConfig

	nl    *nl80211.Client
	bat   *batadv.Client
	facts *Facts
	loop  *reconcile.Loop
	ctx   context.Context // lifetime of Run; parent of helper processes

	radios   *radioReconciler
	meshlink *meshLinkReconciler
	mesh2    *meshLinkReconciler // second mesh link (mesh1)
	batman   *batmanReconciler
	bridge   *bridgeReconciler
	ports    *portsReconciler
	filters  *filtersReconciler
	anycast  *anycastReconciler
	ap       *apReconciler
	dhcp     *dhcpReconciler
	gateway  *gatewayReconciler
	gossip   *gossipReconciler
	peers    *gossip.Table
}

// New loads configuration and prepares the daemon. It does not touch the
// kernel until Run.
func New(opts Options) (*Daemon, error) {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.NodePath == "" {
		opts.NodePath = config.DefaultNodePath
	}
	if opts.StateDir == "" {
		opts.StateDir = config.DefaultStateDir
	}
	if opts.RunDir == "" {
		opts.RunDir = config.DefaultRunDir
	}
	if opts.Socket == "" {
		opts.Socket = filepath.Join(opts.RunDir, "meshd.sock")
	}
	if opts.SysRoot == "" {
		opts.SysRoot = "/sys"
	}
	if opts.Interval == 0 {
		opts.Interval = 15 * time.Second
	}
	d := &Daemon{opts: opts, log: opts.Log, facts: &Facts{Roles: map[string]radio.Role{}}}
	d.facts.Hostname, _ = os.Hostname()
	if err := d.loadConfig(); err != nil {
		return nil, err
	}
	hw, err := hwdb.Load()
	if err != nil {
		return nil, err
	}
	d.hw = hw
	return d, nil
}

// readConfig reads and validates both files without applying them.
func (d *Daemon) readConfig() (*config.Node, *config.Profile, error) {
	node, err := config.LoadNode(d.opts.NodePath)
	if err != nil {
		return nil, nil, fmt.Errorf("node config %s: %w", d.opts.NodePath, err)
	}
	profilePath := node.ProfilePath
	if d.opts.ProfilePath != "" {
		profilePath = d.opts.ProfilePath
	}
	profile, err := config.LoadProfile(profilePath)
	if err != nil {
		return nil, nil, fmt.Errorf("mesh profile %s: %w", profilePath, err)
	}
	if profile.Radio.Band == config.BandS1G {
		return nil, nil, fmt.Errorf("mesh profile %s: radio.band s1g (HaLow) is not supported by this build (roadmap track H)", profilePath)
	}
	if profile.Radio2 != nil && profile.Radio2.Band == config.BandS1G {
		return nil, nil, fmt.Errorf("mesh profile %s: radio2.band s1g (HaLow) is not supported by this build (roadmap track H)", profilePath)
	}
	return node, profile, nil
}

// loadConfig reads, validates and applies both files (startup only).
func (d *Daemon) loadConfig() error {
	node, profile, err := d.readConfig()
	if err != nil {
		return err
	}
	d.cfg, d.profile = node, profile
	return nil
}

// Reload re-reads configuration. An invalid file is rejected and the running
// configuration is kept (architecture §12.6). A valid one is applied at the
// start of the next reconcile pass, never in the middle of one.
func (d *Daemon) Reload() error {
	node, profile, err := d.readConfig()
	if err != nil {
		return err
	}
	d.pendingMu.Lock()
	d.pending = &pendingConfig{node: node, profile: profile}
	d.pendingMu.Unlock()
	d.log.Info("configuration accepted; applying on the next pass", "node", d.opts.NodePath)
	if d.loop != nil {
		d.loop.Kick("reload")
	}
	return nil
}

type pendingConfig struct {
	node    *config.Node
	profile *config.Profile
}

// applyPending swaps in a reloaded configuration. Called by the first
// reconciler of each pass.
func (d *Daemon) applyPending() bool {
	d.pendingMu.Lock()
	p := d.pending
	d.pending = nil
	d.pendingMu.Unlock()
	if p == nil {
		return false
	}
	old, oldNode := d.profile, d.cfg
	d.cfg, d.profile = p.node, p.profile
	if oldNode != nil && oldNode.NodeID != p.node.NodeID && d.identity() != nil {
		d.reapplyNodeID()
	}
	if old != nil && (old.Radio != p.profile.Radio || old.MeshID != p.profile.MeshID || old.SAEPassphrase != p.profile.SAEPassphrase) {
		d.log.Info("mesh profile changed",
			"band", old.Radio.Band+" -> "+p.profile.Radio.Band,
			"channel", fmt.Sprintf("%d -> %d", old.Radio.Channel, p.profile.Radio.Channel),
			"width", fmt.Sprintf("%d -> %d", old.Radio.Width, p.profile.Radio.Width),
			"mesh_id_changed", old.MeshID != p.profile.MeshID,
			"passphrase_changed", old.SAEPassphrase != p.profile.SAEPassphrase)
	}
	d.log.Info("configuration applied")
	return true
}

// Config returns the active configuration with secrets removed.
func (d *Daemon) Config() (config.Node, config.Profile) {
	return d.cfg.Redacted(), d.profile.Redacted()
}

// stopping reports whether meshd is shutting down: its context, which is
// also the helpers' lifetime, has ended.
func (d *Daemon) stopping() bool { return d.ctx != nil && d.ctx.Err() != nil }

// Kick requests a reconcile pass.
func (d *Daemon) Kick(reason string) {
	if d.loop != nil {
		d.loop.Kick(reason)
	}
}

// Run brings the node up, serves until ctx is cancelled or a termination
// signal arrives, then tears everything down.
func (d *Daemon) Run(ctx context.Context, serve func(ctx context.Context, d *Daemon) error) error {
	d.started = time.Now()
	for _, dir := range []string{d.opts.StateDir, d.opts.RunDir, filepath.Join(d.opts.RunDir, "wpa")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	// One meshd per node. The lock is taken before anything is touched: a
	// second instance must exit without changing (or tearing down) the
	// interfaces the running one owns.
	unlock, err := lockInstance(filepath.Join(d.opts.RunDir, "meshd.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	ident, changed, err := nodestate.EnsureIdentity(d.opts.StateDir, d.cfg.NodeID)
	if err != nil {
		return fmt.Errorf("identity: %w", err)
	}
	if changed {
		d.log.Info("identity written", "mac", ident.MAC, "node_id", ident.NodeID, "source", ident.NodeIDSource)
	}
	d.identMu.Lock()
	d.ident = ident
	d.identMu.Unlock()
	if d.radioMem, err = nodestate.LoadRadios(d.opts.StateDir); err != nil {
		return err
	}

	if nl, err := nl80211.Dial(); err != nil {
		d.log.Warn("nl80211 unavailable; only wired mesh ports can be used", "err", err)
	} else {
		d.nl = nl
		nl.OnReset(func(err error) {
			d.log.Warn("nl80211 request failed; replaced the netlink socket", "err", err)
		})
		defer nl.Close()
	}

	d.radios = newRadioReconciler(d)
	d.meshlink = newMeshLinkReconciler(d, 0)
	d.mesh2 = newMeshLinkReconciler(d, 1)
	d.batman = newBatmanReconciler(d)
	d.bridge = newBridgeReconciler(d)
	d.ports = newPortsReconciler(d)
	d.filters = newFiltersReconciler(d)
	d.anycast = newAnycastReconciler(d)
	d.ap = newAPReconciler(d)
	d.dhcp = newDHCPReconciler(d)
	d.gateway = newGatewayReconciler(d)
	d.gossip = newGossipReconciler(d)
	d.peers = gossip.NewTable(ident.MAC)
	d.peers.SetBoot(d.gossip.boot)
	d.loop = reconcile.NewLoop(d.log.With("component", "reconcile"), newHostReconciler(d), d.radios, d.meshlink, d.mesh2, d.batman, d.ports, d.bridge,
		d.filters, d.anycast, d.gateway, d.gossip, d.ap, d.dhcp)
	d.loop.Interval = d.opts.Interval

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	d.ctx = ctx
	go d.watchEvents(ctx)
	go d.handleSignals(ctx, cancel)

	serveErr := make(chan error, 1)
	if serve != nil {
		go func() {
			err := serve(ctx, d)
			if err != nil && !errors.Is(err, context.Canceled) {
				d.log.Error("api server stopped; shutting down", "err", err)
				serveErr <- err
				cancel()
			}
		}()
	}
	d.log.Info("meshd starting", "version", d.opts.Version, "callsign", d.cfg.Callsign, "node_id", ident.NodeID, "mesh", d.profile.MeshID)
	err = d.loop.Run(ctx)
	select {
	case serr := <-serveErr:
		err = errors.Join(err, serr)
	default:
	}
	if d.bat != nil {
		_ = d.bat.Close()
	}
	d.log.Info("meshd stopped")
	return err
}

func (d *Daemon) handleSignals(ctx context.Context, cancel context.CancelFunc) {
	ch := make(chan os.Signal, 4)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(ch)
	for {
		select {
		case <-ctx.Done():
			return
		case sig := <-ch:
			switch sig {
			case syscall.SIGHUP:
				if err := d.Reload(); err != nil {
					d.log.Error("reload rejected; keeping the running configuration", "err", err)
				}
			default:
				d.log.Info("signal received, shutting down", "signal", sig.String())
				cancel()
				return
			}
		}
	}
}

// watchEvents turns kernel events into reconcile kicks.
func (d *Daemon) watchEvents(ctx context.Context) {
	done := make(chan struct{})
	defer close(done)
	linkEvents := make(chan rtnl.LinkEvent, 64)
	if err := rtnl.Subscribe(done, linkEvents); err != nil {
		d.log.Warn("cannot subscribe to link events; relying on the timer", "err", err)
	}
	var wifiEvents chan []nl80211.Event
	if d.nl != nil {
		if ec, err := nl80211.DialEvents(); err == nil {
			wifiEvents = make(chan []nl80211.Event, 16)
			go func() {
				defer ec.Close()
				for {
					evs, err := ec.Next()
					if err != nil {
						return
					}
					select {
					case wifiEvents <- evs:
					case <-ctx.Done():
						return
					}
				}
			}()
			go func() { <-ctx.Done(); ec.Close() }()
		} else {
			d.log.Warn("cannot subscribe to nl80211 events", "err", err)
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-linkEvents:
			// Our own changes also arrive here; the loop coalesces them.
			if ev.Deleted {
				d.loop.Kick("link deleted: " + ev.Name)
			} else {
				d.loop.Kick("link changed: " + ev.Name)
			}
		case evs := <-wifiEvents:
			for _, ev := range evs {
				switch ev.Command {
				case nl80211.NL80211_CMD_NEW_WIPHY, nl80211.NL80211_CMD_DEL_WIPHY,
					nl80211.NL80211_CMD_NEW_INTERFACE, nl80211.NL80211_CMD_DEL_INTERFACE,
					nl80211.NL80211_CMD_REG_CHANGE, nl80211.NL80211_CMD_WIPHY_REG_CHANGE:
					d.loop.Kick(fmt.Sprintf("nl80211 cmd %d", ev.Command))
				}
			}
		}
	}
}

// Status assembles a snapshot: shared facts plus live tables.
func (d *Daemon) Status(ctx context.Context) (*status.Status, error) {
	st := &status.Status{
		Version:   d.opts.Version,
		Hostname:  d.facts.Hostname,
		Time:      time.Now(),
		StartedAt: d.started,
		Warnings:  []status.Warning{},
	}
	node, profile := d.cfg, d.profile
	freq, _ := config.ChannelFrequencyMHz(profile.Radio.Band, profile.Radio.Channel)
	st.Profile = status.Profile{
		Name: profile.Name, MeshID: profile.MeshID, Band: profile.Radio.Band, Channel: profile.Radio.Channel,
		FreqMHz: freq, WidthMHz: profile.Radio.Width, Prefix: profile.Prefix,
		PinnedBatman: profile.Batman.PinnedVersion, MinBatman: profile.Batman.MinVersion, OrigIntervalMS: profile.Batman.OrigIntervalMS,
	}
	if id := d.identity(); id != nil {
		st.Node = status.Node{Callsign: node.Callsign, NodeID: id.NodeID, NodeIDSource: id.NodeIDSource, MAC: id.MAC}
	}

	var meshIfindex, batIfindex, brIfindex, apIfindex int

	var mesh2Ifindex int
	var roles map[string]radio.Role
	var radios []*radio.Radio
	var plan *radio.Plan
	d.facts.Read(func(f *Facts) {
		st.Node.Address = f.BridgeAddress
		st.Regulatory = f.Regulatory
		st.Regulatory.RequestedMode, st.Regulatory.Country = profile.Regulatory.Mode, profile.Regulatory.Country
		if st.Regulatory.ActiveMode == "" {
			st.Regulatory.ActiveMode = "not yet applied"
		}
		radios, plan, roles = f.Radios, f.Plan, f.Roles
		st.Plan.GraceActive = f.GraceActive
		st.MeshLink = status.MeshLink{
			Interface: f.MeshIfname, Transport: orDefault(f.Transport, "not yet determined"), Supplicant: f.Supplicant,
			FreqMHz: f.MeshFreqMHz, MTU: f.MeshMTU, TXPowerDBm: f.TXPowerDBm,
			SAEFailures: f.SAEFailures, WiredPorts: node.MeshPorts(), Peers: []status.Peer{},
		}
		st.Batman = status.Batman{
			Interface: BatIfname, Present: f.BatPresent, ModuleVersion: f.BatModule, ModulePath: f.BatModulePath,
			ModuleInferred: f.BatInferred, PinnedMismatch: f.BatMismatch, Algo: f.BatAlgo, MTU: f.BatMTU,
			MTUOK: f.BatMTU >= 1280, MAC: f.BatMAC, HardIfs: []status.HardIf{}, Neighbors: []status.Neighbor{},
			Originators: []status.Originator{}, Gateways: []status.Gateway{}, Settings: map[string]any{},
		}
		if profile.Radio2 != nil {
			l2 := f.Link2
			st.MeshLink2 = &status.MeshLink{Interface: l2.Ifname, Transport: "wifi", Supplicant: l2.Supplicant, FreqMHz: l2.FreqMHz,
				MTU: l2.MTU, TXPowerDBm: l2.TXPowerDBm, Peers: []status.Peer{}}
			if l2.Ifname == "" {
				st.MeshLink2.Transport = "none"
			}
			mesh2Ifindex = l2.Ifindex
		}
		st.Bridge = status.Bridge{Name: BridgeIfname, Present: f.BridgePresent, Address: f.BridgeAddress, MTU: f.BridgeMTU, Ports: []string{}}
		meshIfindex, batIfindex, brIfindex, apIfindex = f.MeshIfindex, f.BatIfindex, f.BridgeIfindex, f.APIfindex
		st.AP = f.AP
		st.ClientNet = status.ClientNet{Anycast: f.Anycast, Filters: f.Filters, DHCP: f.DHCP}
		st.Ports = append([]status.Port{}, f.Ports...)
		st.Gateway = f.Gateway
	})
	if st.Gateway.Candidates == nil {
		st.Gateway.Candidates = []status.GatewayCandidate{}
	}
	if st.Gateway.DNS == nil {
		st.Gateway.DNS = []string{}
	}
	st.Nodes = d.meshNodes()
	if d.gossip != nil {
		gs := d.gossip.Stats()
		st.Gossip = status.Gossip{Sent: gs.Sent, Received: gs.Received, BytesSent: gs.BytesSent, BytesReceived: gs.BytesReceived,
			LastRecordBytes: gs.LastRecordBytes, AuthFailed: gs.AuthFailed, Ignored: gs.Ignored, LastError: gs.LastError}
	}
	st.AP.Clients = []status.Client{}
	st.ClientNet.Leases = []status.Lease{}
	if d.dhcp != nil {
		if ls := d.dhcp.Leases(); ls != nil {
			st.ClientNet.Leases = ls
		}
	}

	// Radios and plan.
	st.Radios = []status.Radio{}
	for _, r := range radios {
		sr := status.Radio{
			Fingerprint: r.Fingerprint, FingerprintSource: r.FingerprintSource, Phy: r.Phy, Label: r.Label,
			Driver: r.Device.Driver, Bus: r.Device.Bus, Port: r.Device.Port, DeviceID: r.Device.DeviceID(),
			USBSpeedMbps: r.Device.USBSpeedMbps, PermAddr: r.PermAddrString(), Bands: r.Caps.Bands, Streams: r.Caps.Streams,
			MeshPoint: r.Caps.MeshPoint, AP: r.Caps.AP, MeshAndAP: r.Caps.MeshAndAP, Power: map[string]status.PowerFact{},
			Role: string(radio.RoleUnused), Recommended: string(radio.RoleUnused), Interfaces: []string{}, Ignored: r.Ignored,
			Broken: r.Broken, HardBlocked: r.HardBlocked, InUse: r.InUse, TXPowerCapDBm: r.TXPowerCapDBm,
		}
		if r.HWDB != nil {
			sr.Chipset = r.HWDB.Chipset
			sr.Quirks = r.HWDB.Quirks
		}
		for b, p := range r.Power {
			sr.Power[b] = status.PowerFact{DBm: p.DBm, Source: p.Source}
		}
		if role, ok := roles[r.Fingerprint]; ok {
			sr.Role = string(role)
		}
		if plan != nil {
			if rec, ok := plan.Recommended[r.Fingerprint]; ok {
				sr.Recommended = string(rec)
			}
		}
		for _, in := range r.Interfaces {
			if in.Ifindex == 0 && in.Name == "" {
				continue // P2P-device: no network interface, nothing to show
			}
			sr.Interfaces = append(sr.Interfaces, in.Name+" ("+in.TypeName()+")")
		}
		st.Radios = append(st.Radios, sr)
	}
	st.Plan.Notices = []status.Warning{}
	if plan != nil {
		st.Plan.MeshRadio, st.Plan.APRadio, st.Plan.APBand, st.Plan.Differs = plan.MeshFingerprint(), plan.APFingerprint(), plan.APBand, plan.Differs
		for _, n := range plan.Notices {
			w := status.Warning{Level: n.Level, Code: n.Code, Message: n.Message, Source: "radios"}
			st.Plan.Notices = append(st.Plan.Notices, w)
			if n.Level != "info" {
				st.Warnings = append(st.Warnings, w)
			}
		}
	}

	// Live tables.
	meshPeers := func(ifindex int) []status.Peer {
		out := []status.Peer{}
		if d.nl == nil || ifindex == 0 {
			return out
		}
		if stations, err := d.nl.Stations(ifindex); err == nil {
			for _, s := range stations {
				out = append(out, status.Peer{
					MAC: s.MAC.String(), SignalDBm: int(s.SignalDBm), TXMbps: float64(s.TXBitrateKbps) / 1000, RXMbps: float64(s.RXBitrateKbps) / 1000,
					InactiveMS: s.InactiveMS, ConnectedSeconds: s.ConnectedSeconds, PlinkState: nl80211.PlinkStateName(s.PlinkState), Established: s.Established(),
				})
			}
		}
		return out
	}
	st.MeshLink.Peers = meshPeers(meshIfindex)
	if st.MeshLink2 != nil {
		st.MeshLink2.Peers = meshPeers(mesh2Ifindex)
	}
	if d.nl != nil && apIfindex != 0 {
		leaseByMAC := map[string]status.Lease{}
		for _, l := range st.ClientNet.Leases {
			leaseByMAC[strings.ToLower(l.MAC)] = l
		}
		if stations, err := d.nl.Stations(apIfindex); err == nil {
			for _, s := range stations {
				c := status.Client{
					MAC: s.MAC.String(), SignalDBm: int(s.SignalDBm), TXMbps: float64(s.TXBitrateKbps) / 1000, RXMbps: float64(s.RXBitrateKbps) / 1000,
					InactiveMS: s.InactiveMS, ConnectedSeconds: s.ConnectedSeconds,
				}
				if l, ok := leaseByMAC[strings.ToLower(c.MAC)]; ok {
					c.IP, c.Hostname = l.IP, l.Hostname
				}
				st.AP.Clients = append(st.AP.Clients, c)
			}
		}
	}
	if d.filters != nil && st.ClientNet.Filters.Applied && d.filters.nftPath != "" {
		if drops, rules, err := nft.Counters(ctx, d.filters.nftPath); err == nil {
			st.ClientNet.Filters.Drops, st.ClientNet.Filters.Rules = drops, rules
		}
	}
	if d.bat != nil && batIfindex != 0 {
		d.collectBatman(st, batIfindex)
	}
	if brIfindex != 0 {
		if links, err := netlink.LinkList(); err == nil {
			for _, l := range links {
				if l.Attrs().MasterIndex == brIfindex {
					st.Bridge.Ports = append(st.Bridge.Ports, l.Attrs().Name)
				}
			}
		}
	}

	// Own resource use: a steadily growing count means a leak.
	st.Process = status.Process{PID: os.Getpid(), Goroutines: runtime.NumGoroutine()}
	if fds, err := os.ReadDir("/proc/self/fd"); err == nil {
		st.Process.OpenFiles = len(fds)
	}
	if st.Process.OpenFiles > 256 || st.Process.Goroutines > 500 {
		st.Warnings = append(st.Warnings, status.Warning{Level: "warning", Code: "resource-growth",
			Message: fmt.Sprintf("meshd has %d open files and %d goroutines; that is unusually many (please report with meshctl doctor)", st.Process.OpenFiles, st.Process.Goroutines)})
	}

	// Loop and warnings.
	passes, last, stats := d.loop.Snapshot()
	st.Reconcile = status.Reconcile{Passes: passes, LastPass: last.Started, LastDuration: last.Duration.Round(time.Millisecond).String(), Converged: last.Converged}
	for _, s := range stats {
		ri := status.ReconcilerInfo{Name: s.Name, LastChanged: s.LastChanged, Changes: s.Changes, Errors: s.Errors}
		if s.LastError != nil {
			ri.LastError = s.LastError.Error()
			st.Warnings = append(st.Warnings, status.Warning{Level: "error", Code: "reconcile-error", Message: s.LastError.Error(), Source: s.Name})
		}
		for _, w := range s.Warnings {
			st.Warnings = append(st.Warnings, status.Warning{Level: w.Level, Code: w.Code, Message: w.Message, Source: s.Name})
		}
		st.Reconcile.Reconcilers = append(st.Reconcile.Reconcilers, ri)
	}
	return st, nil
}

func (d *Daemon) collectBatman(st *status.Status, batIfindex int) {
	names := map[int]string{}
	if hifs, err := d.bat.HardIfs(batIfindex); err == nil {
		for _, h := range hifs {
			names[h.Ifindex] = h.Name
			mtu := 0
			if l, err := netlink.LinkByIndex(h.Ifindex); err == nil {
				mtu = l.Attrs().MTU
			}
			st.Batman.HardIfs = append(st.Batman.HardIfs, status.HardIf{Name: h.Name, MAC: h.Address.String(), Active: h.Active, MTU: mtu})
		}
	}
	via := func(i int) string {
		if n, ok := names[i]; ok {
			return n
		}
		return fmt.Sprint(i)
	}
	if ns, err := d.bat.Neighbors(batIfindex); err == nil {
		for _, n := range ns {
			st.Batman.Neighbors = append(st.Batman.Neighbors, status.Neighbor{MAC: n.Address.String(), Via: via(n.HardIfindex), LastSeenMS: uint32(n.LastSeen / time.Millisecond), ThroughputMbps: n.Throughput.Mbps()})
		}
	}
	if os_, err := d.bat.Originators(batIfindex); err == nil {
		for _, o := range os_ {
			st.Batman.Originators = append(st.Batman.Originators, status.Originator{MAC: o.Address.String(), NextHop: o.NextHop.String(), Via: via(o.HardIfindex), LastSeenMS: uint32(o.LastSeen / time.Millisecond), ThroughputMbps: o.Throughput.Mbps(), Best: o.Best})
		}
	}
	if gws, err := d.bat.Gateways(batIfindex); err == nil {
		for _, g := range gws {
			st.Batman.Gateways = append(st.Batman.Gateways, status.Gateway{MAC: g.Address.String(), Router: g.Router.String(), DownMbps: float64(g.BandwidthDown) / 10, UpMbps: float64(g.BandwidthUp) / 10, ThroughputMbps: g.Throughput.Mbps(), Best: g.Best})
		}
	}
	if mi, err := d.bat.GetMesh(batIfindex); err == nil {
		s := mi.Settings
		set := func(k string, v any) { st.Batman.Settings[k] = v }
		if s.AggregatedOGMs != nil {
			set("aggregated_ogms", *s.AggregatedOGMs)
		}
		if s.BridgeLoopAvoidance != nil {
			set("bridge_loop_avoidance", *s.BridgeLoopAvoidance)
		}
		if s.DistributedARPTable != nil {
			set("distributed_arp_table", *s.DistributedARPTable)
		}
		if s.Fragmentation != nil {
			set("fragmentation", *s.Fragmentation)
		}
		if s.MulticastForceflood != nil {
			set("multicast_forceflood", *s.MulticastForceflood)
			st.Batman.Multicast.Optimisations = !*s.MulticastForceflood
		}
		if s.MulticastFanout != nil {
			set("multicast_fanout", *s.MulticastFanout)
		}
		if s.GWMode != nil {
			set("gw_mode", map[uint8]string{0: "off", 1: "client", 2: "server"}[*s.GWMode])
		}
		if s.GWSelClass != nil {
			set("gw_sel_class", *s.GWSelClass)
		}
		if s.OrigIntervalMS != nil {
			set("orig_interval_ms", *s.OrigIntervalMS)
		}
		if s.HopPenalty != nil {
			set("hop_penalty", *s.HopPenalty)
		}
		if s.Bonding != nil {
			set("bonding", *s.Bonding)
		}
		if s.NetworkCoding != nil {
			set("network_coding", *s.NetworkCoding)
		}
	}
	if mf, err := d.bat.MulticastFlags(batIfindex); err == nil {
		st.Batman.Multicast.OwnFlags = mf.Own
		st.Batman.Multicast.Bridged = mf.OwnPriv&batadv.BATADV_MCAST_FLAGS_BRIDGED != 0
		st.Batman.Multicast.QuerierIPv4 = mf.OwnPriv&batadv.BATADV_MCAST_FLAGS_QUERIER_IPV4_EXISTS != 0
		st.Batman.Multicast.QuerierIPv4Shadowing = mf.OwnPriv&batadv.BATADV_MCAST_FLAGS_QUERIER_IPV4_SHADOWING != 0
		st.Batman.Multicast.QuerierIPv6 = mf.OwnPriv&batadv.BATADV_MCAST_FLAGS_QUERIER_IPV6_EXISTS != 0
		st.Batman.Multicast.QuerierIPv6Shadowing = mf.OwnPriv&batadv.BATADV_MCAST_FLAGS_QUERIER_IPV6_SHADOWING != 0
		st.Batman.Multicast.WantAllIPv4 = mf.Own&batadv.McastWantAllIPv4 != 0
		st.Batman.Multicast.WantAllIPv6 = mf.Own&batadv.McastWantAllIPv6 != 0
		for _, o := range mf.Originators {
			if !o.HasFlags {
				st.Batman.Multicast.LegacyPeers++
				continue
			}
			st.Batman.Multicast.PeersWithFlags++
			if o.Flags&batadv.McastWantAllIPv4 != 0 {
				st.Batman.Multicast.PeersWantAllIPv4++
			}
			if o.Flags&batadv.McastHaveMcPtypeCapa == 0 {
				st.Batman.Multicast.PeersNoPtype++
			}
		}
		st.Batman.Multicast.Path = mcastPath(st.Batman.Multicast)
	}
	if c, err := rtnl.EthtoolStats(BatIfname); err == nil {
		st.Batman.Multicast.Counters = status.McastCounters{TX: c["mcast_tx"], TXBytes: c["mcast_tx_bytes"], TXLocal: c["mcast_tx_local"],
			RX: c["mcast_rx"], RXLocal: c["mcast_rx_local"], Fwd: c["mcast_fwd"], FwdBytes: c["mcast_fwd_bytes"]}
	}
}

// mcastPath summarises how IPv4 multicast travels (architecture §9). Any
// node that wants all IPv4 multicast (no usable querier, or a node
// without optimisations) receives every group, which on a radio mesh is
// nearly as costly as flooding.
func mcastPath(m status.Multicast) string {
	switch {
	case !m.Optimisations:
		return "flooding"
	case m.LegacyPeers > 0:
		return "flooding"
	case !m.WantAllIPv4 && m.PeersWantAllIPv4 == 0:
		return "efficient"
	case m.PeersWantAllIPv4+boolInt(m.WantAllIPv4) > m.PeersWithFlags:
		return "flooding"
	}
	return "partial"
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// APJoin is what a phone needs to join this node's AP.
func (d *Daemon) APJoin() (*status.APJoin, error) {
	node := d.cfg
	var ap status.AP
	d.facts.Read(func(f *Facts) { ap = f.AP })
	j := &status.APJoin{Enabled: node.APEnabled(), SSID: apSSID(node), Passphrase: apPassphrase(node, d.identity()), Band: ap.Band, Channel: ap.Channel}
	j.WiFiQR = hostapd.WiFiQR(j.SSID, j.Passphrase)
	switch {
	case !j.Enabled:
		j.Note = "the client AP is disabled in node.yaml"
	case ap.Interface == "":
		j.Note = "no radio carries the client AP right now (see meshctl status)"
	case ap.Hostapd.State != "ENABLED":
		j.Note = "the AP is not up yet (hostapd state " + orDefault(ap.Hostapd.State, "unknown") + ")"
	}
	return j, nil
}

// nodeAddress computes 10.73.N.1/16 from the profile prefix and node ID.
func nodeAddress(prefix string, nodeID int) (string, error) {
	ip, ipnet, err := net.ParseCIDR(prefix)
	if err != nil {
		return "", err
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return "", errors.New("prefix is not IPv4")
	}
	ones, _ := ipnet.Mask.Size()
	addr := net.IPv4(ip4[0], ip4[1], byte(nodeID), 1)
	return fmt.Sprintf("%s/%d", addr.String(), ones), nil
}

// isNodeAddress reports whether a is shaped like a node address in prefix
// (10.73.N.1 with the prefix's length, N 1–254): one this node assigned
// under an earlier node ID.
func isNodeAddress(prefix *net.IPNet, a *net.IPNet) bool {
	ip := a.IP.To4()
	if ip == nil || !prefix.Contains(ip) {
		return false
	}
	po, _ := prefix.Mask.Size()
	ao, _ := a.Mask.Size()
	return po == ao && ip[3] == 1 && ip[2] >= 1 && ip[2] <= 254
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// ErrAlreadyRunning is returned when another meshd holds the instance lock.
var ErrAlreadyRunning = errors.New("another meshd is already running on this node")

// lockInstance takes an exclusive, non-blocking lock on path and records
// this process's pid in it. The kernel releases the lock if the process
// dies, so a crash never leaves a stale lock.
func lockInstance(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		pid, _ := os.ReadFile(path)
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w (pid %s); use meshctl to talk to it, or systemctl to restart it", ErrAlreadyRunning, strings.TrimSpace(string(pid)))
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(fmt.Sprintf("%d\n", os.Getpid())), 0)
	return func() { f.Close() }, nil
}
