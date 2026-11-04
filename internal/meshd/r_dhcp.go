package meshd

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"meridian/trellis/internal/dnsmasq"
	"meridian/trellis/internal/proc"
	"meridian/trellis/internal/reconcile"
	"meridian/trellis/internal/status"
)

// Client-network constants (architecture §7.2, §7.5, §7.6).
const (
	MeshDomain    = "mesh"
	DHCPPoolFirst = 100
	DHCPPoolLast  = 199
	DHCPLeaseTime = "24h"
	LeaseFileName = "dnsmasq.leases"
)

// dhcpReconciler runs this node's dnsmasq: DHCP for its own pool and DNS
// for .mesh, both answered on the anycast address.
type dhcpReconciler struct {
	d        *Daemon
	sup      *proc.Supervisor
	supSince time.Time
	path     string
}

func newDHCPReconciler(d *Daemon) *dhcpReconciler { return &dhcpReconciler{d: d} }

func (r *dhcpReconciler) Name() string { return "dhcp" }

func (r *dhcpReconciler) confPath() string { return filepath.Join(r.d.opts.RunDir, "dnsmasq.conf") }

func (r *dhcpReconciler) leaseFile() string { return filepath.Join(r.d.opts.StateDir, LeaseFileName) }

func (r *dhcpReconciler) serversPath() string {
	return filepath.Join(r.d.opts.RunDir, "dnsmasq.servers")
}

func (r *dhcpReconciler) hostsPath() string { return filepath.Join(r.d.opts.RunDir, "dnsmasq.hosts") }

func (r *dhcpReconciler) Reconcile(ctx context.Context) (reconcile.Result, error) {
	d := r.d
	var res reconcile.Result
	warn := func(level, code, format string, args ...any) {
		res.Warnings = append(res.Warnings, reconcile.Warning{Level: level, Code: code, Message: fmt.Sprintf(format, args...)})
	}
	var bridgePresent, anycastPresent bool
	d.facts.Read(func(f *Facts) { bridgePresent, anycastPresent = f.BridgePresent, f.Anycast.Present })
	if !bridgePresent {
		if r.sup != nil {
			r.stop()
			res.Changed = true
		}
		d.facts.Update(func(f *Facts) { f.DHCP = status.DHCP{} })
		return res, nil
	}
	if r.path == "" {
		p, err := proc.LookPath("dnsmasq")
		if err != nil {
			warn("error", "dhcp-no-dnsmasq", "dnsmasq is not installed, so clients get no address or DNS: apt install dnsmasq-base")
			res.RequeueAfter = 30 * time.Second
			d.facts.Update(func(f *Facts) { f.DHCP = status.DHCP{} })
			return res, nil
		}
		r.path = p
	}
	anycastIP, err := anycastAddress(d.profile.Prefix)
	if err != nil {
		return res, err
	}
	nodeAddr, err := nodeAddress(d.profile.Prefix, d.identity().NodeID)
	if err != nil {
		return res, err
	}
	nodeIP, _, _ := net.ParseCIDR(nodeAddr)
	start, end, mask, err := dhcpPool(d.profile.Prefix, d.identity().NodeID)
	if err != nil {
		return res, err
	}
	conf := dnsmasq.Render(dnsmasq.Config{
		Interfaces: []string{BridgeIfname}, NoDHCPInterfaces: []string{AnycastIfname}, Domain: MeshDomain,
		PoolStart: start, PoolEnd: end, Netmask: mask, LeaseTime: DHCPLeaseTime, Router: anycastIP, DNS: anycastIP,
		LeaseFile: r.leaseFile(), LeaseMax: DHCPPoolLast - DHCPPoolFirst + 1,
		HostRecords: []dnsmasq.HostRecord{{Name: d.cfg.Callsign, IP: nodeIP.String()}, {Name: "node", IP: anycastIP}},
		ServersFile: r.serversPath(), HostsFile: r.hostsPath(),
	})
	// Upstreams (the selected gateway) and other nodes' names: files
	// dnsmasq re-reads on SIGHUP.
	var upstream []string
	d.facts.Read(func(f *Facts) { upstream = f.DNSUpstream })
	var hosts []dnsmasq.HostRecord
	if d.peers != nil {
		for _, p := range d.peers.Peers() {
			if p.Record.Callsign != "" && p.Record.Addr != "" && p.Record.Callsign != d.cfg.Callsign {
				hosts = append(hosts, dnsmasq.HostRecord{Name: p.Record.Callsign, IP: p.Record.Addr})
			}
		}
	}
	reread := false
	for path, content := range map[string]string{
		r.serversPath(): dnsmasq.ServersFileContent(upstream),
		r.hostsPath():   dnsmasq.HostsFileContent(MeshDomain, hosts),
	} {
		if cur, err := os.ReadFile(path); err != nil || string(cur) != content {
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				return res, fmt.Errorf("write %s: %w", path, err)
			}
			reread = true
		}
	}
	restart := false
	if cur, err := os.ReadFile(r.confPath()); err != nil || string(cur) != conf {
		if err := os.WriteFile(r.confPath(), []byte(conf), 0o644); err != nil {
			return res, fmt.Errorf("write %s: %w", r.confPath(), err)
		}
		d.log.Info("wrote dnsmasq config", "path", r.confPath(), "pool", start+"-"+end, "domain", MeshDomain)
		restart = r.sup != nil
		res.Changed = true
	}
	argv := []string{r.path, "--keep-in-foreground", "--pid-file=", "--conf-file=" + r.confPath()}
	if inUserNamespace() {
		// dnsmasq drops privileges after start, which an unprivileged user
		// namespace (the lab) forbids; debug mode keeps its identity.
		argv = append(argv, "--no-daemon")
	}
	if r.sup != nil && (restart || !r.sup.SameArgv(argv)) {
		d.log.Info("restarting dnsmasq", "reason", "configuration changed")
		r.stop()
		res.Changed = true
	}
	if r.sup == nil {
		r.sup = proc.New(d.log, "dnsmasq", argv...)
		if err := r.sup.Start(d.ctx); err != nil {
			r.sup = nil
			return res, fmt.Errorf("start dnsmasq: %w", err)
		}
		r.supSince = time.Now()
		res.Changed = true
	}
	if reread && !res.Changed {
		if err := r.sup.Signal(syscall.SIGHUP); err == nil {
			d.log.Info("dnsmasq re-read upstreams and names", "upstream", upstream, "names", len(hosts))
			res.Changed = true
		}
	}
	h := status.Helper{Running: r.sup.Alive(), PID: r.sup.PID(), Restarts: r.sup.Restarts()}
	if h.Running {
		h.State = "running"
	}
	if h.Restarts > 3 {
		warn("warning", "dhcp-restarting", "dnsmasq has restarted %d times; another DNS or DHCP server may hold port 53 or 67 (see the journal and meshctl doctor)", h.Restarts)
		res.RequeueAfter = 10 * time.Second
	}
	if !anycastPresent {
		warn("warning", "dhcp-without-anycast", "clients are told to use %s as router and DNS, but that address is not up on this node yet", anycastIP)
	}
	d.facts.Update(func(f *Facts) { f.DHCP = status.DHCP{Dnsmasq: h, PoolStart: start, PoolEnd: end, Domain: MeshDomain} })
	return res, nil
}

func (r *dhcpReconciler) stop() {
	if r.sup != nil {
		_ = r.sup.Stop(5 * time.Second)
		r.sup = nil
	}
}

func (r *dhcpReconciler) Teardown(ctx context.Context) error {
	r.stop()
	for _, p := range []string{r.confPath(), r.serversPath(), r.hostsPath()} {
		_ = os.Remove(p)
	}
	return nil
}

// Leases reads the lease file.
func (r *dhcpReconciler) Leases() []status.Lease {
	f, err := os.Open(r.leaseFile())
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []status.Lease
	for _, l := range dnsmasq.ParseLeases(f) {
		out = append(out, status.Lease{MAC: l.MAC, IP: l.IP, Hostname: l.Hostname, Expires: l.Expires})
	}
	return out
}

// dhcpPool is node N's range 10.73.N.100–199 and the prefix's netmask.
func dhcpPool(prefix string, nodeID int) (start, end, mask string, err error) {
	ip, ipnet, err := net.ParseCIDR(prefix)
	if err != nil {
		return "", "", "", err
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return "", "", "", fmt.Errorf("prefix is not IPv4")
	}
	start = net.IPv4(ip4[0], ip4[1], byte(nodeID), DHCPPoolFirst).String()
	end = net.IPv4(ip4[0], ip4[1], byte(nodeID), DHCPPoolLast).String()
	mask = net.IP(ipnet.Mask).String()
	return start, end, mask, nil
}

// inUserNamespace reports whether the process runs with a non-identity uid
// map, as the lab harness does.
func inUserNamespace() bool {
	b, err := os.ReadFile("/proc/self/uid_map")
	if err != nil {
		return false
	}
	f := strings.Fields(string(b))
	return !(len(f) == 3 && f[0] == "0" && f[1] == "0" && f[2] == "4294967295")
}
