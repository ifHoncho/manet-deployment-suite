package meshd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"

	"github.com/vishvananda/netlink"

	"meridian/trellis/internal/reconcile"
	"meridian/trellis/internal/rtnl"
	"meridian/trellis/internal/status"
	"meridian/trellis/internal/sysctl"
)

// anycastReconciler owns anycast0: a macvlan on br-mesh holding the
// gateway address every node answers for (architecture §7.4), plus the
// sysctls that make two addresses on one segment behave. It refuses to run
// while the bat0 filters are not in place, because the shared MAC would
// otherwise flap across the whole mesh.
type anycastReconciler struct {
	d         *Daemon
	sysWarned bool
}

func newAnycastReconciler(d *Daemon) *anycastReconciler { return &anycastReconciler{d: d} }

func (a *anycastReconciler) Name() string { return "anycast" }

func (a *anycastReconciler) Reconcile(ctx context.Context) (reconcile.Result, error) {
	d := a.d
	var res reconcile.Result
	warn := func(level, code, format string, args ...any) {
		res.Warnings = append(res.Warnings, reconcile.Warning{Level: level, Code: code, Message: fmt.Sprintf(format, args...)})
	}
	var bridgePresent, filtersOK bool
	var brIndex int
	d.facts.Read(func(f *Facts) {
		bridgePresent, brIndex, filtersOK = f.BridgePresent, f.BridgeIfindex, f.Filters.Applied
	})
	ip, err := anycastAddress(d.profile.Prefix)
	if err != nil {
		return res, err
	}
	mac, _ := net.ParseMAC(AnycastMAC)

	if !bridgePresent || !filtersOK {
		if deleted, err := rtnl.DeleteIfExists(AnycastIfname); err != nil {
			return res, err
		} else if deleted {
			d.log.Warn("removed the anycast gateway interface", "name", AnycastIfname, "reason", "bat0 filters not in place")
			res.Changed = true
		}
		if bridgePresent {
			warn("error", "anycast-withheld", "the gateway address %s is not configured on this node because the bat0 filters are not in place; without them it would leak to the whole mesh", ip)
		}
		d.facts.Update(func(f *Facts) { f.Anycast = status.Anycast{Interface: AnycastIfname} })
		return res, nil
	}

	link, err := rtnl.LinkByNameOrNil(AnycastIfname)
	if err != nil {
		return res, err
	}
	if link != nil && (link.Type() != "macvlan" || link.Attrs().ParentIndex != brIndex) {
		if err := netlink.LinkDel(link); err != nil {
			return res, fmt.Errorf("remove stray %s: %w", AnycastIfname, err)
		}
		d.log.Info("removed interface", "name", AnycastIfname, "reason", "not our macvlan on "+BridgeIfname)
		link = nil
	}
	if link == nil {
		mv := &netlink.Macvlan{LinkAttrs: netlink.LinkAttrs{Name: AnycastIfname, ParentIndex: brIndex, HardwareAddr: mac}, Mode: netlink.MACVLAN_MODE_BRIDGE}
		if err := netlink.LinkAdd(mv); err != nil {
			return res, fmt.Errorf("create %s: %w", AnycastIfname, err)
		}
		d.log.Info("created the anycast gateway interface", "name", AnycastIfname, "address", ip, "mac", AnycastMAC)
		res.Changed = true
		if link, err = netlink.LinkByName(AnycastIfname); err != nil {
			return res, err
		}
	}
	if ch, err := rtnl.EnsureHardwareAddr(link, mac); err != nil {
		return res, err
	} else if ch {
		res.Changed = true
	}

	// Sysctls (architecture §7.4): no ICMP redirects, no ARP flux, and no
	// reverse-path drops for the EUD → anycast0 → br-mesh path.
	var sysErrs []string
	set := func(key, val string) {
		ch, err := sysctl.Set(key, val)
		if err != nil {
			sysErrs = append(sysErrs, key+": "+err.Error())
			return
		}
		if ch {
			d.log.Info("sysctl", "key", key, "value", val)
			res.Changed = true
		}
	}
	set("net.ipv4.conf.all.send_redirects", "0")
	set("net.ipv4.conf.default.send_redirects", "0")
	set("net.ipv4.conf."+BridgeIfname+".send_redirects", "0")
	set("net.ipv4.conf."+AnycastIfname+".send_redirects", "0")
	set("net.ipv4.conf.all.arp_ignore", "1")
	set("net.ipv4.conf.all.arp_announce", "2")
	set("net.ipv4.conf."+BridgeIfname+".rp_filter", "0")
	set("net.ipv4.conf."+AnycastIfname+".rp_filter", "0")
	if v, err := sysctl.Get("net.ipv4.conf.all.rp_filter"); err == nil && v == "1" {
		// Strict mode on "all" overrides the per-interface setting.
		set("net.ipv4.conf.all.rp_filter", "2")
	}
	set("net.ipv6.conf."+AnycastIfname+".disable_ipv6", "1")
	set("net.ipv6.conf."+BridgeIfname+".accept_ra", "0")
	if len(sysErrs) > 0 && !a.sysWarned {
		a.sysWarned = true
		d.log.Warn("some sysctls could not be set", "errors", sysErrs)
	}
	if len(sysErrs) > 0 {
		warn("warning", "sysctl", "%d sysctl settings failed (see the journal); the anycast gateway may misbehave", len(sysErrs))
	}

	// A permanent FDB entry makes br-mesh hand frames for the anycast MAC
	// straight to anycast0 instead of flooding them to every port first.
	// NTF_SELF addresses the bridge device itself (what "bridge fdb add …
	// dev br-mesh" sends); without it the kernel has no handler and answers
	// EOPNOTSUPP.
	fdb := &netlink.Neigh{LinkIndex: brIndex, Family: syscall.AF_BRIDGE, Flags: netlink.NTF_SELF, State: netlink.NUD_PERMANENT, HardwareAddr: mac}
	if err := netlink.NeighAdd(fdb); err != nil && !errors.Is(err, syscall.EEXIST) {
		warn("warning", "anycast-fdb", "cannot add the bridge FDB entry for %s: %v (frames for the gateway are flooded to every port)", AnycastMAC, err)
	}

	if ch, err := rtnl.EnsureAddr(link, ip+"/32"); err != nil {
		return res, fmt.Errorf("address %s on %s: %w", ip, AnycastIfname, err)
	} else if ch {
		res.Changed = true
	}
	if ch, err := rtnl.EnsureUp(link); err != nil {
		return res, err
	} else if ch {
		res.Changed = true
	}
	d.facts.Update(func(f *Facts) {
		f.Anycast = status.Anycast{Interface: AnycastIfname, Present: true, Address: ip, MAC: AnycastMAC}
	})
	return res, nil
}

func (a *anycastReconciler) Teardown(ctx context.Context) error {
	deleted, err := rtnl.DeleteIfExists(AnycastIfname)
	if deleted {
		a.d.log.Info("removed interface", "name", AnycastIfname)
	}
	return err
}

// anycastAddress is the first host of the mesh prefix: 10.73.0.1.
func anycastAddress(prefix string) (string, error) {
	ip, _, err := net.ParseCIDR(prefix)
	if err != nil {
		return "", err
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return "", errors.New("prefix is not IPv4")
	}
	return net.IPv4(ip4[0], ip4[1], 0, 1).String(), nil
}
