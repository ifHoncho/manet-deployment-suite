package meshd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"meridian/trellis/internal/config"
	"meridian/trellis/internal/dhcpc"
	"meridian/trellis/internal/reconcile"
	"meridian/trellis/internal/rtnl"
	"meridian/trellis/internal/status"
)

// Auto port roles (architecture §7.1). A port in the auto role, or an idle
// Ethernet port nobody configured, is probed with DHCP when a cable is in:
// an offer from outside the mesh prefix makes it a WAN (meshd holds the
// lease), silence makes it a client port. An offer from inside the prefix,
// or another Trellis node probing the same cable, means the cable leads to
// another mesh node's client network; bridging it would loop the mesh, so
// such a port is left alone.
const (
	portProbing  = "probing"
	portClient   = "client"
	portWAN      = "wan"
	portPeer     = "peer"
	portNoLink   = "no-link"
	portInUse    = "in-use"
	portMissing  = "missing"
	portFailed   = "probe-failed"
	vendorClass  = "trellis-meshd"
	probeTotal   = 4 * time.Second
	nmCacheFor   = 10 * time.Second
	probeRetryIn = 30 * time.Second
)

var probeSchedule = []time.Duration{0, time.Second, 2500 * time.Millisecond}

type portsReconciler struct {
	d *Daemon

	mu    sync.Mutex
	ports map[string]*autoPort

	nmStates map[string]string // nmcli device states, cached
	nmAt     time.Time

	loaded   bool   // leftover addresses read from the run directory
	addrsOut string // what was last written there
}

// addrsFile records the addresses meshd put on auto WAN ports, so a meshd
// restarted after a crash recognises them as its own rather than as the
// system using the port. It lives in the run directory: addresses do not
// survive a reboot either.
func (r *portsReconciler) addrsFile() string {
	return filepath.Join(r.d.opts.RunDir, "auto-wan-addresses")
}

type autoPort struct {
	name   string
	listed bool // in node.yaml with role auto (the operator handed it over)
	role   string
	why    string
	since  time.Time
	lease  *dhcpc.Lease
	addr   *net.IPNet // address meshd put on the port (auto WAN)
	prevIP net.IP     // last leased address, asked for again
	failAt time.Time
	ifidx  int // interface the role was decided for; a new one is a new device

	gen    int // worker generation: results from an older worker are ignored
	cancel context.CancelFunc
	done   chan struct{}
}

func newPortsReconciler(d *Daemon) *portsReconciler {
	return &portsReconciler{d: d, ports: map[string]*autoPort{}}
}

func (r *portsReconciler) Name() string { return "ports" }

func (r *portsReconciler) Reconcile(ctx context.Context) (reconcile.Result, error) {
	var res reconcile.Result
	warn := func(level, code, format string, args ...any) {
		res.Warnings = append(res.Warnings, reconcile.Warning{Level: level, Code: code, Message: fmt.Sprintf(format, args...)})
	}
	cands := r.candidates()

	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.loaded {
		r.loaded = true
		if data, err := os.ReadFile(r.addrsFile()); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				name, cidr, ok := strings.Cut(line, " ")
				ip, n, err := net.ParseCIDR(cidr)
				if !ok || err != nil {
					continue
				}
				n.IP = ip
				r.ports[name] = &autoPort{name: name, addr: n}
			}
		}
	}
	for name, p := range r.ports {
		if _, ok := cands[name]; !ok {
			r.release(p)
			delete(r.ports, name)
			res.Changed = true
		}
	}
	names := make([]string, 0, len(cands))
	for name := range cands {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		p := r.ports[name]
		if p == nil {
			p = &autoPort{name: name}
			r.ports[name] = p
		}
		p.listed = cands[name]
		if r.step(p, &res) {
			res.Changed = true
		}
		switch {
		case p.role == portPeer:
			warn("warning", "port-peer", "port %s leads to another Trellis node (%s); not bridging it, which would loop the mesh. For a wired mesh link give it role mesh on both nodes", name, p.why)
		case p.role == portInUse && p.listed:
			warn("warning", "port-in-use", "port %s is listed with role auto but the system uses it (%s); Trellis leaves it alone", name, p.why)
		case p.role == portFailed:
			warn("warning", "port-probe", "cannot probe port %s: %s", name, p.why)
			res.RequeueAfter = probeRetryIn
		case p.role == portMissing:
			warn("warning", "port-missing", "port %s (role auto) does not exist", name)
		}
	}
	r.publish()
	return res, nil
}

// step moves one port along; it reports a change.
func (r *portsReconciler) step(p *autoPort, res *reconcile.Result) bool {
	d := r.d
	link, err := rtnl.LinkByNameOrNil(p.name)
	if err != nil || link == nil {
		changed := r.release(p)
		return r.set(p, portMissing, "") || changed
	}
	if idx := link.Attrs().Index; p.ifidx != idx {
		// A new interface under the same name (an adapter replugged, a
		// cable moved faster than a pass): nothing decided before holds.
		changed := false
		if p.ifidx != 0 {
			changed = r.release(p)
			r.set(p, "", "")
			changed = true
		}
		p.ifidx = idx
		if changed {
			return r.step(p, res) || true
		}
	}
	if !r.owns(p, link) {
		if why := r.inUse(p, link); why != "" {
			changed := r.release(p)
			return r.set(p, portInUse, why) || changed
		}
	}
	attrs := link.Attrs()
	adminUp := attrs.Flags&net.FlagUp != 0
	if p.role == portClient && !adminUp {
		// The bridge holds client ports down briefly after a re-address
		// (r_bridge.go); that is not a cable change.
		return false
	}
	changed := false
	if !adminUp {
		if ch, err := rtnl.EnsureUp(link); err != nil {
			return r.set(p, portFailed, fmt.Sprintf("cannot bring it up: %v", err))
		} else if ch {
			changed = true
			d.log.Info("auto port up, waiting for a cable", "port", p.name)
		}
	}
	if attrs.RawFlags&unix.IFF_LOWER_UP == 0 {
		if p.role != portNoLink {
			changed = r.release(p) || changed
			changed = r.set(p, portNoLink, "no cable or no link partner") || changed
		}
		return changed
	}
	if p.addr != nil && p.role != portWAN && p.role != portProbing && p.role != "" {
		// A former WAN address (or one left by a crashed meshd) on a port
		// that is not a WAN now.
		if err := netlink.AddrDel(link, &netlink.Addr{IPNet: p.addr}); err == nil || rtnl.IsNotFound(err) {
			p.addr = nil
			changed = true
		}
	}
	switch p.role {
	case "", portNoLink, portInUse, portMissing:
		r.start(p, attrs.HardwareAddr)
		return true
	case portFailed:
		if time.Since(p.failAt) >= probeRetryIn {
			r.start(p, attrs.HardwareAddr)
			return true
		}
	case portWAN:
		return r.applyWANAddr(p, link) || changed
	}
	return changed
}

// set records a role; it reports a change.
func (r *portsReconciler) set(p *autoPort, role, why string) bool {
	if p.role == role && p.why == why {
		return false
	}
	if p.role != role {
		p.since = time.Now()
		if role != portMissing || p.role != "" {
			r.d.log.Info("port role", "port", p.name, "role", role, "why", why)
		}
	}
	p.role, p.why = role, why
	if role == portFailed {
		p.failAt = time.Now()
	}
	return true
}

// owns reports whether the port's current state is meshd's own doing.
func (r *portsReconciler) owns(p *autoPort, link netlink.Link) bool {
	switch p.role {
	case portClient, portWAN, portProbing, portPeer, portNoLink, portFailed:
		return true
	}
	return false
}

// inUse explains why the system uses a port, or returns "".
func (r *portsReconciler) inUse(p *autoPort, link netlink.Link) string {
	attrs := link.Attrs()
	if attrs.MasterIndex != 0 {
		if m, err := netlink.LinkByIndex(attrs.MasterIndex); err == nil && m.Attrs().Name != BridgeIfname {
			return "it is a member of " + m.Attrs().Name
		}
	}
	if addrs, err := netlink.AddrList(link, netlink.FAMILY_ALL); err == nil {
		for _, a := range addrs {
			if p.addr != nil && a.IPNet.String() == p.addr.String() {
				continue
			}
			if a.IP.To4() != nil || a.Scope == unix.RT_SCOPE_UNIVERSE {
				return "it has the address " + a.IPNet.String()
			}
		}
	}
	if def, l, err := rtnl.MainDefault(nil); err == nil && def != nil && l != nil && l.Attrs().Index == attrs.Index {
		return "it carries the system's default route"
	}
	if p.listed {
		// Listed with role auto: the operator handed it over, and meshd has
		// told NetworkManager and systemd-networkd to leave it alone.
		return ""
	}
	if st := r.nmState(p.name); st != "" && st != "unmanaged" {
		return fmt.Sprintf("NetworkManager manages it (state %s); to hand it to Trellis, list it in node.yaml with role auto", st)
	}
	if st := networkdState(attrs.Index); st != "" && st != "unmanaged" && st != "linger" {
		return fmt.Sprintf("systemd-networkd manages it (%s); to hand it to Trellis, list it in node.yaml with role auto", st)
	}
	if ifupdownConfigures(p.name) {
		return "it is configured in /etc/network/interfaces"
	}
	if processRunning("dhcpcd") {
		return "dhcpcd is running and may manage it; to hand it to Trellis, list it in node.yaml with role auto"
	}
	return ""
}

// start launches the probe-and-hold worker. Called with r.mu held.
func (r *portsReconciler) start(p *autoPort, mac net.HardwareAddr) {
	d := r.d
	r.stopWorker(p)
	r.set(p, portProbing, "asking for a DHCP server")
	p.gen++
	gen, name, prev, callsign := p.gen, p.name, p.prevIP, d.cfg.Callsign
	_, prefix, _ := net.ParseCIDR(d.profile.Prefix)
	ctx, cancel := context.WithCancel(d.ctx)
	done := make(chan struct{})
	p.cancel, p.done = cancel, done
	report := func(role, why string, l *dhcpc.Lease, setLease bool) {
		r.mu.Lock()
		cur := r.ports[name]
		if cur == nil || cur.gen != gen {
			r.mu.Unlock()
			return
		}
		r.set(cur, role, why)
		if setLease {
			cur.lease = l
			if l != nil {
				cur.prevIP = l.Addr.IP
			}
		}
		r.mu.Unlock()
		d.Kick("port " + name + ": " + role)
	}
	go func() {
		defer close(done)
		conn, err := dhcpc.Open(name)
		if err != nil {
			report(portFailed, err.Error(), nil, false)
			return
		}
		defer conn.Close()
		c := &dhcpc.Client{Conn: conn, MAC: mac, Hostname: callsign, VendorClass: vendorClass}
		offer, err := c.Probe(ctx, probeSchedule, probeTotal, prev)
		switch {
		case ctx.Err() != nil:
			return
		case errors.Is(err, dhcpc.ErrPeer):
			report(portPeer, "another Trellis node is probing the same cable", nil, false)
			return
		case errors.Is(err, dhcpc.ErrNoOffer):
			report(portClient, "no DHCP server answered", nil, false)
			return
		case err != nil:
			report(portFailed, err.Error(), nil, false)
			return
		case prefix != nil && prefix.Contains(offer.YIAddr):
			report(portPeer, fmt.Sprintf("a DHCP server offered %s, inside the mesh prefix", offer.YIAddr), nil, false)
			return
		}
		l, err := c.Request(ctx, offer)
		if ctx.Err() != nil {
			return
		}
		server := offer.ServerID().String()
		if err != nil {
			report(portWAN, fmt.Sprintf("DHCP server %s offered %s but the request failed (%v); retrying", server, offer.YIAddr, err), nil, true)
			l = nil
		} else {
			report(portWAN, "DHCP server "+server+" leased an address outside the mesh", l, true)
		}
		c.Hold(ctx, l, func(l *dhcpc.Lease) {
			if l == nil {
				report(portWAN, "the lease ran out or was refused; asking again", nil, true)
				return
			}
			report(portWAN, "DHCP server "+l.Server.String()+" leased an address outside the mesh", l, true)
		})
	}()
}

// stopWorker ends a port's worker and waits for it. Called with r.mu held;
// the worker's report takes r.mu, so it is released while waiting.
func (r *portsReconciler) stopWorker(p *autoPort) {
	if p.cancel == nil {
		return
	}
	p.cancel()
	p.gen++ // results still in flight are now stale
	done := p.done
	p.cancel, p.done = nil, nil
	r.mu.Unlock()
	<-done
	r.mu.Lock()
}

// release undoes what meshd did to a port: worker, bridge membership,
// address. It reports a change.
func (r *portsReconciler) release(p *autoPort) bool {
	changed := p.cancel != nil
	r.stopWorker(p)
	p.lease = nil
	link, _ := rtnl.LinkByNameOrNil(p.name)
	if link == nil {
		p.addr = nil
		return changed
	}
	if p.role == portClient && link.Attrs().MasterIndex != 0 {
		if m, err := netlink.LinkByIndex(link.Attrs().MasterIndex); err == nil && m.Attrs().Name == BridgeIfname {
			if err := netlink.LinkSetNoMaster(link); err == nil {
				r.d.log.Info("auto port left the bridge", "port", p.name)
				changed = true
			}
		}
	}
	if p.addr != nil {
		if err := netlink.AddrDel(link, &netlink.Addr{IPNet: p.addr}); err == nil {
			r.d.log.Info("removed auto WAN address", "port", p.name, "address", p.addr.String())
			changed = true
		}
		p.addr = nil
	}
	return changed
}

// applyWANAddr keeps the leased address on the port. NOPREFIXROUTE keeps
// the subnet out of the main table: routes for mesh clients go into
// meshd's own table (gateway reconciler), and the node's own traffic is
// left as the system set it up.
func (r *portsReconciler) applyWANAddr(p *autoPort, link netlink.Link) bool {
	var want *net.IPNet
	if p.lease != nil {
		a := p.lease.Addr
		want = &a
	}
	changed := false
	if p.addr != nil && (want == nil || p.addr.String() != want.String()) {
		if err := netlink.AddrDel(link, &netlink.Addr{IPNet: p.addr}); err == nil || rtnl.IsNotFound(err) {
			changed = true
		}
		p.addr = nil
	}
	if want != nil {
		have := false
		if addrs, err := netlink.AddrList(link, netlink.FAMILY_V4); err == nil {
			for _, a := range addrs {
				if a.IPNet.String() == want.String() {
					have = true
				}
			}
		}
		if !have {
			if err := netlink.AddrReplace(link, &netlink.Addr{IPNet: want, Flags: unix.IFA_F_NOPREFIXROUTE}); err != nil {
				r.d.log.Warn("cannot put the leased address on the auto WAN port", "port", p.name, "address", want.String(), "err", err)
				return changed
			}
			r.d.log.Info("auto WAN address", "port", p.name, "address", want.String(), "router", p.lease.Router)
			changed = true
		}
		p.addr = want
	}
	return changed
}

// publish writes the port view into facts. Called with r.mu held.
func (r *portsReconciler) publish() {
	d := r.d
	var out []status.Port
	var clients []string
	var wan *autoWAN
	for _, port := range d.cfg.Ports {
		if port.Role == config.PortRoleAuto {
			continue
		}
		l, _ := rtnl.LinkByNameOrNil(port.Name)
		sp := status.Port{Name: port.Name, Configured: port.Role, Role: port.Role}
		if l == nil {
			sp.Role, sp.Why = portMissing, "the interface does not exist"
		} else {
			sp.Carrier = l.Attrs().RawFlags&unix.IFF_LOWER_UP != 0
		}
		out = append(out, sp)
	}
	names := make([]string, 0, len(r.ports))
	for name := range r.ports {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		p := r.ports[name]
		sp := status.Port{Name: name, Role: p.role, Why: p.why, Since: p.since}
		if p.listed {
			sp.Configured = config.PortRoleAuto
		}
		if l, _ := rtnl.LinkByNameOrNil(name); l != nil {
			sp.Carrier = l.Attrs().RawFlags&unix.IFF_LOWER_UP != 0
		}
		if l := p.lease; l != nil {
			pl := &status.PortLease{Address: l.Addr.String(), Server: l.Server.String(), Expires: l.Expires()}
			if l.Router != nil {
				pl.Router = l.Router.String()
			}
			for _, s := range l.DNS {
				pl.DNS = append(pl.DNS, s.String())
			}
			sp.Lease = pl
		}
		out = append(out, sp)
		switch {
		case p.role == portClient:
			clients = append(clients, name)
		case p.role == portWAN && p.lease != nil && p.addr != nil && wan == nil:
			a := p.lease.Addr
			wan = &autoWAN{Port: name, Addr: &a, Router: p.lease.Router, DNS: p.lease.DNS}
		}
	}
	d.facts.Update(func(f *Facts) { f.Ports, f.AutoClients, f.AutoWAN = out, clients, wan })

	var b strings.Builder
	for _, name := range names {
		if a := r.ports[name].addr; a != nil {
			fmt.Fprintf(&b, "%s %s\n", name, a)
		}
	}
	if b.String() != r.addrsOut {
		if err := os.WriteFile(r.addrsFile(), []byte(b.String()), 0o644); err == nil {
			r.addrsOut = b.String()
		}
	}
}

// candidates are the ports this reconciler looks after, with whether each
// is listed in node.yaml (role auto) or an unlisted Ethernet port.
func (r *portsReconciler) candidates() map[string]bool {
	d := r.d
	out := map[string]bool{}
	for _, name := range d.cfg.AutoPorts() {
		out[name] = true
	}
	links, err := netlink.LinkList()
	if err != nil {
		return out
	}
	for _, l := range links {
		name := l.Attrs().Name
		if _, ok := out[name]; ok || d.cfg.PortRole(name) != "" || ownInterface(name) {
			continue
		}
		if isEthernetPort(d.opts.SysRoot, l) {
			out[name] = false
		}
	}
	return out
}

func ownInterface(name string) bool {
	switch name {
	case "lo", BridgeIfname, BatIfname, AnycastIfname, MeshIfname, APIfname:
		return true
	}
	return strings.HasPrefix(name, "mesh") || strings.HasPrefix(name, "ap")
}

// isEthernetPort: a physical, wired Ethernet interface (not wireless, not
// virtual).
func isEthernetPort(sysRoot string, l netlink.Link) bool {
	if l.Type() != "device" || l.Attrs().EncapType != "ether" {
		return false
	}
	base := filepath.Join(sysRoot, "class/net", l.Attrs().Name)
	if _, err := os.Stat(filepath.Join(base, "device")); err != nil {
		return false
	}
	for _, wifi := range []string{"wireless", "phy80211"} {
		if _, err := os.Stat(filepath.Join(base, wifi)); err == nil {
			return false
		}
	}
	return true
}

// nmState returns NetworkManager's state for a device, "" when
// NetworkManager is not running or does not know it.
func (r *portsReconciler) nmState(name string) string {
	if r.nmStates == nil || time.Since(r.nmAt) > nmCacheFor {
		r.nmStates, r.nmAt = map[string]string{}, time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		out, err := exec.CommandContext(ctx, "nmcli", "-t", "-f", "DEVICE,STATE", "device").Output()
		cancel()
		if err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				if dev, st, ok := strings.Cut(line, ":"); ok {
					r.nmStates[dev] = st
				}
			}
		}
	}
	return r.nmStates[name]
}

// networkdState returns systemd-networkd's ADMIN_STATE for a link, "" when
// networkd has no record of it.
func networkdState(ifindex int) string {
	f, err := os.Open(fmt.Sprintf("/run/systemd/netif/links/%d", ifindex))
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "ADMIN_STATE="); ok {
			return v
		}
	}
	return ""
}

// ifupdownConfigures reports whether /etc/network/interfaces (or its .d
// directory) mentions the interface.
func ifupdownConfigures(name string) bool {
	files := []string{"/etc/network/interfaces"}
	if more, err := filepath.Glob("/etc/network/interfaces.d/*"); err == nil {
		files = append(files, more...)
	}
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			f := strings.Fields(line)
			if len(f) >= 2 && (f[0] == "iface" || f[0] == "auto" || f[0] == "allow-hotplug") && slices.Contains(f[1:], name) {
				return true
			}
		}
	}
	return false
}

// processRunning reports whether a process with this command name exists.
func processRunning(comm string) bool {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.Name()[0] < '0' || e.Name()[0] > '9' {
			continue
		}
		if b, err := os.ReadFile(filepath.Join("/proc", e.Name(), "comm")); err == nil && strings.TrimSpace(string(b)) == comm {
			return true
		}
	}
	return false
}

func (r *portsReconciler) Teardown(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for name, p := range r.ports {
		r.release(p)
		delete(r.ports, name)
	}
	r.d.facts.Update(func(f *Facts) { f.Ports, f.AutoClients, f.AutoWAN = nil, nil, nil })
	return nil
}
