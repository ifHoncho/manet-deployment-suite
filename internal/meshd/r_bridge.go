package meshd

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/vishvananda/netlink"

	"meridian/trellis/internal/reconcile"
	"meridian/trellis/internal/rtnl"
)

// bridgeReconciler owns br-mesh: bat0, the AP interface and wired client
// ports as members, and the node's own address (architecture §7.1, §7.2).
// The anycast gateway, filters, AP and DHCP have reconcilers of their own
// that run after this one.
type bridgeReconciler struct {
	d *Daemon

	idGen     uint64    // identity generation last applied
	flapUntil time.Time // client ports are held down until then
}

// clientPortFlap is how long client ports stay down after the node is
// re-addressed: long enough for a wired device to see the link drop and
// ask for a new lease when it comes back.
const clientPortFlap = 2 * time.Second

func newBridgeReconciler(d *Daemon) *bridgeReconciler { return &bridgeReconciler{d: d} }

func (b *bridgeReconciler) Name() string { return "bridge" }

func (b *bridgeReconciler) Reconcile(ctx context.Context) (reconcile.Result, error) {
	d := b.d
	var res reconcile.Result
	warn := func(level, code, format string, args ...any) {
		res.Warnings = append(res.Warnings, reconcile.Warning{Level: level, Code: code, Message: fmt.Sprintf(format, args...)})
	}
	id := d.identity()
	mac := id.HardwareAddr()

	link, err := rtnl.LinkByNameOrNil(BridgeIfname)
	if err != nil {
		return res, err
	}
	if link != nil && link.Type() != "bridge" {
		return res, fmt.Errorf("%s exists but is a %s, not a bridge; remove it", BridgeIfname, link.Type())
	}
	if link == nil {
		br := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: BridgeIfname, HardwareAddr: mac}}
		if err := netlink.LinkAdd(br); err != nil {
			return res, fmt.Errorf("create %s: %w", BridgeIfname, err)
		}
		d.log.Info("created bridge", "name", BridgeIfname, "mac", mac.String())
		res.Changed = true
		if link, err = netlink.LinkByName(BridgeIfname); err != nil {
			return res, err
		}
	}
	if ch, err := rtnl.EnsureHardwareAddr(link, mac); err != nil {
		warn("warning", "bridge-mac", "cannot set %s MAC: %v", BridgeIfname, err)
	} else if ch {
		res.Changed = true
	}

	// Members: bat0 when present, plus client ports.
	members := []string{}
	var batPresent bool
	var apIfname string
	d.facts.Read(func(f *Facts) { batPresent, apIfname = f.BatPresent, f.APIfname })
	if batPresent {
		members = append(members, BatIfname)
	}
	if apIfname != "" {
		if l, _ := rtnl.LinkByNameOrNil(apIfname); l != nil {
			members = append(members, apIfname)
		}
	}
	for _, port := range d.clientPorts() {
		if l, _ := rtnl.LinkByNameOrNil(port); l == nil {
			warn("warning", "client-port-missing", "client port %s does not exist", port)
			continue
		}
		members = append(members, port)
	}
	// After a live re-address (a lost node ID conflict, or a node_id
	// change by reload), wired clients hold leases from the old pool: drop
	// their link briefly so they ask again. The local dnsmasq NAKs the old
	// address and hands out one from the new pool (architecture §7.3).
	clientPorts := d.clientPorts()
	if gen := d.identityGen(); gen != b.idGen {
		b.idGen = gen
		if len(clientPorts) > 0 {
			b.flapUntil = time.Now().Add(clientPortFlap)
			for _, port := range clientPorts {
				if l, _ := rtnl.LinkByNameOrNil(port); l != nil {
					if err := netlink.LinkSetDown(l); err != nil {
						warn("warning", "client-port-flap", "cannot take client port %s down after re-addressing: %v", port, err)
					}
				}
			}
			d.log.Info("node re-addressed: client ports down briefly so wired clients re-lease", "ports", clientPorts, "for", clientPortFlap)
			res.Changed = true
		}
	}
	holding := time.Now().Before(b.flapUntil)
	if holding {
		res.RequeueAfter = time.Until(b.flapUntil) + 50*time.Millisecond
	}
	for _, name := range members {
		l, err := netlink.LinkByName(name)
		if err != nil {
			continue
		}
		if holding && slices.Contains(clientPorts, name) {
			if _, err := rtnl.EnsureMaster(l, link); err != nil {
				return res, fmt.Errorf("add %s to %s: %w", name, BridgeIfname, err)
			}
			continue
		}
		if ch, err := rtnl.EnsureMaster(l, link); err != nil {
			return res, fmt.Errorf("add %s to %s: %w", name, BridgeIfname, err)
		} else if ch {
			d.log.Info("added bridge port", "port", name, "bridge", BridgeIfname)
			res.Changed = true
		}
		if ch, err := rtnl.EnsureUp(l); err != nil {
			if name == apIfname {
				// The AP's radio refusing the interface must not hold up
				// the bridge, the filters and DHCP behind it.
				warn("error", "ap-interface-down", "cannot bring %s up: %v (the radio's driver refused a second interface)", name, err)
				continue
			}
			return res, err
		} else if ch {
			res.Changed = true
		}
	}

	// Address 10.73.N.1/16.
	addr, err := nodeAddress(d.profile.Prefix, id.NodeID)
	if err != nil {
		return res, err
	}
	if ch, err := rtnl.EnsureAddr(link, addr); err != nil {
		return res, fmt.Errorf("address %s on %s: %w", addr, BridgeIfname, err)
	} else if ch {
		d.log.Info("assigned node address", "address", addr, "interface", BridgeIfname)
		res.Changed = true
	}
	if addrs, err := netlink.AddrList(link, netlink.FAMILY_V4); err == nil {
		_, prefix, _ := net.ParseCIDR(d.profile.Prefix)
		for _, a := range addrs {
			if a.IPNet.String() == addr || prefix == nil || !prefix.Contains(a.IP) {
				continue
			}
			if isNodeAddress(prefix, a.IPNet) {
				// A former node address of this node (its ID changed):
				// keeping it would answer for the node that now owns it.
				if err := netlink.AddrDel(link, &a); err != nil {
					warn("warning", "old-address", "cannot remove the former node address %s from %s: %v", a.IPNet, BridgeIfname, err)
					continue
				}
				d.log.Info("removed former node address", "address", a.IPNet.String(), "interface", BridgeIfname)
				res.Changed = true
				continue
			}
			warn("warning", "extra-address", "%s also has %s inside the mesh prefix; Trellis manages %s", BridgeIfname, a.IPNet, addr)
		}
	}
	if ch, err := rtnl.EnsureUp(link); err != nil {
		return res, err
	} else if ch {
		res.Changed = true
	}
	if ch, warns := b.ensureMulticast(batPresent); ch {
		res.Changed = true
	} else {
		for _, w := range warns {
			warn("warning", "bridge-multicast", "%s", w)
		}
	}
	if link, err = netlink.LinkByName(BridgeIfname); err != nil {
		return res, err
	}
	d.facts.Update(func(f *Facts) {
		f.BridgePresent, f.BridgeIfindex, f.BridgeAddress, f.BridgeMTU = true, link.Attrs().Index, addr, link.Attrs().MTU
	})
	return res, nil
}

func (b *bridgeReconciler) Teardown(ctx context.Context) error {
	deleted, err := rtnl.DeleteIfExists(BridgeIfname)
	if deleted {
		b.d.log.Info("removed interface", "name", BridgeIfname)
	}
	return err
}

// Multicast on br-mesh (architecture §9). batman-adv only sends a node the
// groups it has listeners for when an IGMP/MLD querier exists and is not
// "shadowing" (on this node's side of bat0). Every node runs the bridge
// querier from its own address; queries cross the mesh and the lowest
// address wins, so the querier is either this bridge or behind bat0, and
// neither shadows. bat0 is a permanent multicast-router port: all
// multicast from local clients goes into batman-adv, which knows the
// mesh-wide listeners and decides where it goes.
var bridgeMcastSettings = []struct{ file, value string }{
	{"bridge/multicast_snooping", "1"},
	{"bridge/multicast_querier", "1"},
	{"bridge/multicast_query_use_ifaddr", "1"},
}

func (b *bridgeReconciler) ensureMulticast(batPresent bool) (bool, []string) {
	d := b.d
	changed := false
	var warns []string
	set := func(path, value string) {
		cur, err := os.ReadFile(path)
		if err != nil {
			warns = append(warns, fmt.Sprintf("cannot read %s: %v", path, err))
			return
		}
		if strings.TrimSpace(string(cur)) == value {
			return
		}
		if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
			warns = append(warns, fmt.Sprintf("cannot set %s to %s: %v", path, value, err))
			return
		}
		d.log.Info("bridge multicast", "setting", strings.TrimPrefix(path, d.opts.SysRoot+"/class/net/"), "value", value)
		changed = true
	}
	for _, s := range bridgeMcastSettings {
		set(filepath.Join(d.opts.SysRoot, "class/net", BridgeIfname, s.file), s.value)
	}
	if batPresent {
		set(filepath.Join(d.opts.SysRoot, "class/net", BatIfname, "brport/multicast_router"), "2")
	}
	return changed, warns
}
