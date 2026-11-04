package meshd

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/vishvananda/netlink"

	"meridian/trellis/internal/batadv"
	"meridian/trellis/internal/reconcile"
	"meridian/trellis/internal/rtnl"
)

// batmanReconciler owns bat0: the module version gate, the interface, its
// settings and its hard interfaces (architecture §6).
type batmanReconciler struct {
	d           *Daemon
	modprobeAt  time.Time
	modulePath  string
	modinfoDone bool
	// loggedUnsupported records that unsupported settings were reported.
	loggedUnsupported bool
	wantAllSince      time.Time // when this node started asking for all IPv4 multicast
}

// defaultMcastFanout is batman-adv's own default for multicast_fanout.
const defaultMcastFanout = 16

// mcastGrace is how long a node may want all multicast before it is
// reported: the bridge querier needs a query interval plus its response
// time to be elected and recognised (architecture §9).
const mcastGrace = 45 * time.Second

func newBatmanReconciler(d *Daemon) *batmanReconciler { return &batmanReconciler{d: d} }

func (b *batmanReconciler) Name() string { return "batman" }

func boolp(v bool) *bool    { return &v }
func u8p(v uint8) *uint8    { return &v }
func u32p(v uint32) *uint32 { return &v }

func (b *batmanReconciler) Reconcile(ctx context.Context) (reconcile.Result, error) {
	d := b.d
	cfg, profile := d.cfg, d.profile
	var res reconcile.Result
	warn := func(level, code, format string, args ...any) {
		res.Warnings = append(res.Warnings, reconcile.Warning{Level: level, Code: code, Message: fmt.Sprintf(format, args...)})
	}

	// Module and version gate.
	mi := batadv.ReadModuleInfo()
	if !mi.Loaded {
		if time.Since(b.modprobeAt) > 10*time.Second {
			b.modprobeAt = time.Now()
			if out, err := exec.CommandContext(ctx, "modprobe", "batman-adv").CombinedOutput(); err != nil {
				d.facts.Update(func(f *Facts) { f.BatPresent = false })
				return res, fmt.Errorf("batman_adv module is not loaded and modprobe failed: %v: %s", err, strings.TrimSpace(string(out)))
			}
			res.Changed = true
			mi = batadv.ReadModuleInfo()
		}
		if !mi.Loaded {
			d.facts.Update(func(f *Facts) { f.BatPresent = false })
			return res, fmt.Errorf("batman_adv module is not loaded")
		}
	}
	if !b.modinfoDone {
		b.modinfoDone = true
		if out, err := exec.Command("modinfo", "-n", "batman-adv").Output(); err == nil {
			b.modulePath = strings.TrimSpace(string(out))
		}
	}
	v, inferred := mi.EffectiveRelease()
	minV, _ := batadv.ParseVersion(profile.Batman.MinVersion)
	pinV, _ := batadv.ParseVersion(profile.Batman.PinnedVersion)
	mismatch := inferred || v != pinV
	d.facts.Update(func(f *Facts) {
		f.BatModule, f.BatModulePath, f.BatInferred, f.BatMismatch = mi.Raw, b.modulePath, inferred, mismatch
	})
	if v.Less(minV) {
		if !cfg.AllowOldBatman {
			d.facts.Update(func(f *Facts) { f.BatPresent = false })
			return res, fmt.Errorf("batman-adv %s is below the minimum %s required by the mesh profile; install the pinned %s build (scripts/install-batman-adv-dkms.sh), or set allow_old_batman: true in node.yaml for bench experiments", v, minV, pinV)
		}
		warn("warning", "batman-old-allowed", "running batman-adv %s below the minimum %s because allow_old_batman is set; this node forces the whole mesh into multicast flooding", v, minV)
	} else if mismatch {
		if inferred {
			warn("warning", "batman-version-unknown", "batman-adv reports %q (in-kernel build, release inferred as %s); the pinned release is %s. Install the DKMS build so every node runs identical code", mi.Raw, v, pinV)
		} else {
			warn("warning", "batman-version-mismatch", "batman-adv %s differs from the pinned release %s; install the pinned build on every node", v, pinV)
		}
	}

	if d.bat == nil {
		c, err := batadv.Dial()
		if err != nil {
			return res, err
		}
		d.bat = c
	}

	// bat0.
	mac := d.identity().HardwareAddr()
	link, err := rtnl.LinkByNameOrNil(BatIfname)
	if err != nil {
		return res, err
	}
	if link != nil && link.Type() != "batadv" {
		return res, fmt.Errorf("%s exists but is a %s interface, not batman-adv; remove it", BatIfname, link.Type())
	}
	if link == nil {
		if err := rtnl.CreateBatadv(BatIfname, profile.Batman.RoutingAlgo, mac); err != nil {
			return res, err
		}
		d.log.Info("created batman-adv interface", "name", BatIfname, "algo", profile.Batman.RoutingAlgo, "mac", mac.String())
		res.Changed = true
		if link, err = netlink.LinkByName(BatIfname); err != nil {
			return res, err
		}
	}
	idx := link.Attrs().Index
	mesh, err := d.bat.GetMesh(idx)
	if err != nil {
		return res, fmt.Errorf("read %s: %w", BatIfname, err)
	}
	if mesh.AlgoName != "" && mesh.AlgoName != profile.Batman.RoutingAlgo {
		d.log.Warn("recreating bat0 with the configured routing algorithm", "have", mesh.AlgoName, "want", profile.Batman.RoutingAlgo)
		if err := netlink.LinkDel(link); err != nil {
			return res, err
		}
		if err := rtnl.CreateBatadv(BatIfname, profile.Batman.RoutingAlgo, mac); err != nil {
			return res, err
		}
		res.Changed = true
		if link, err = netlink.LinkByName(BatIfname); err != nil {
			return res, err
		}
		idx = link.Attrs().Index
		if mesh, err = d.bat.GetMesh(idx); err != nil {
			return res, err
		}
	}
	if ch, err := rtnl.EnsureHardwareAddr(link, mac); err != nil {
		return res, fmt.Errorf("set %s MAC: %w", BatIfname, err)
	} else if ch {
		d.log.Info("set interface MAC", "interface", BatIfname, "mac", mac.String())
		res.Changed = true
	}

	// Settings (architecture §6 table). gw_mode and its bandwidth belong to
	// the gateway reconciler (§8), which switches between server and
	// client; it must not be reset here. The distributed ARP table is off:
	// it only learns a client when that client's ARP traffic crosses bat0,
	// and with the gateway answered locally on every node (§7.4) it never
	// does, so other nodes could not resolve the client at all unless its
	// node happened to be one of the three DHT candidates for its address.
	want := batadv.Settings{
		AggregatedOGMs:      boolp(true),
		BridgeLoopAvoidance: boolp(d.profile.Batman.BLA()),
		DistributedARPTable: boolp(false),
		Fragmentation:       boolp(true),
		MulticastForceflood: boolp(false),
		NetworkCoding:       boolp(false),
		Bonding:             boolp(false),
		APIsolation:         boolp(false),
		OrigIntervalMS:      u32p(uint32(profile.Batman.OrigIntervalMS)),
	}
	// Always set: removing the knob from the profile must restore the
	// default, not leave the last value in force.
	fanout := uint32(defaultMcastFanout)
	if f := profile.Batman.MulticastFanout; f != nil {
		fanout = uint32(*f)
	}
	want.MulticastFanout = u32p(fanout)
	if u := batadv.Unsupported(mesh.Settings, want); len(u) > 0 && !b.loggedUnsupported {
		d.log.Info("batman-adv module does not provide these settings; skipping them", "settings", u)
		b.loggedUnsupported = true
	}
	if diff := batadv.Diff(mesh.Settings, want); !diff.IsEmpty() {
		if err := d.bat.SetMesh(idx, diff); err != nil {
			return res, fmt.Errorf("apply %s settings %v: %w", BatIfname, diff.Names(), err)
		}
		d.log.Info("applied batman-adv settings", "changed", diff.Names())
		res.Changed = true
	}

	// Hard interfaces.
	desired := map[string]bool{}
	// Both mesh links are hard interfaces: BATMAN_V compares paths across
	// them by throughput and uses the better one per destination (D-09).
	var meshIfname, mesh2Ifname string
	d.facts.Read(func(f *Facts) { meshIfname, mesh2Ifname = f.MeshIfname, f.Link2.Ifname })
	if meshIfname != "" {
		desired[meshIfname] = true
	}
	if mesh2Ifname != "" {
		desired[mesh2Ifname] = true
	}
	for _, port := range cfg.MeshPorts() {
		l, err := rtnl.LinkByNameOrNil(port)
		if err != nil {
			return res, err
		}
		if l == nil {
			warn("warning", "mesh-port-missing", "wired mesh port %s does not exist", port)
			continue
		}
		desired[port] = true
		if l.Attrs().MTU != cfg.MeshMTU {
			if err := netlink.LinkSetMTU(l, cfg.MeshMTU); err != nil {
				warn("warning", "mesh-mtu", "cannot set %s MTU to %d (%v); keeping %d", port, cfg.MeshMTU, err, l.Attrs().MTU)
			} else {
				d.log.Info("set underlay MTU", "interface", port, "mtu", cfg.MeshMTU)
				res.Changed = true
			}
		}
	}
	for name := range desired {
		l, err := netlink.LinkByName(name)
		if err != nil {
			continue
		}
		if ch, err := rtnl.EnsureMaster(l, link); err != nil {
			return res, fmt.Errorf("attach %s to %s: %w", name, BatIfname, err)
		} else if ch {
			d.log.Info("attached hard interface", "interface", name, "to", BatIfname)
			res.Changed = true
		}
		if ch, err := rtnl.EnsureUp(l); err != nil {
			return res, fmt.Errorf("bring up %s: %w", name, err)
		} else if ch {
			res.Changed = true
		}
	}
	hifs, err := d.bat.HardIfs(idx)
	if err != nil {
		return res, err
	}
	for _, h := range hifs {
		if desired[h.Name] {
			continue
		}
		if l, err := netlink.LinkByIndex(h.Ifindex); err == nil {
			if err := netlink.LinkSetNoMaster(l); err != nil {
				return res, fmt.Errorf("detach %s from %s: %w", h.Name, BatIfname, err)
			}
			d.log.Info("detached hard interface", "interface", h.Name, "from", BatIfname)
			res.Changed = true
		}
	}
	if len(desired) == 0 {
		warn("error", "no-hard-interfaces", "%s has no hard interfaces: no mesh radio and no wired mesh ports", BatIfname)
	}

	if ch, err := rtnl.EnsureUp(link); err != nil {
		return res, fmt.Errorf("bring up %s: %w", BatIfname, err)
	} else if ch {
		res.Changed = true
	}
	if link, err = netlink.LinkByName(BatIfname); err != nil {
		return res, err
	}
	mtu := link.Attrs().MTU
	if mtu < 1280 {
		warn("warning", "bat0-mtu-low", "%s MTU is %d, below the 1280 bytes the multicast packet type needs", BatIfname, mtu)
	}
	d.facts.Update(func(f *Facts) {
		f.BatPresent, f.BatIfindex, f.BatAlgo, f.BatMTU, f.BatMAC = true, idx, mesh.AlgoName, mtu, link.Attrs().HardwareAddr.String()
	})
	if mf, err := d.bat.MulticastFlags(idx); err == nil {
		wantAll := mf.Own&batadv.McastWantAllIPv4 != 0
		switch {
		case !wantAll:
			b.wantAllSince = time.Time{}
		case b.wantAllSince.IsZero():
			b.wantAllSince = time.Now()
			res.RequeueAfter = mcastGrace
		case time.Since(b.wantAllSince) >= mcastGrace:
			why := "no IGMP querier is known"
			if mf.OwnPriv&batadv.BATADV_MCAST_FLAGS_QUERIER_IPV4_SHADOWING != 0 {
				why = "an IGMP querier on this node's side of bat0 (a client port or the AP) shadows the mesh's querier"
			}
			warn("warning", "multicast-want-all", "this node receives every IPv4 multicast group, listeners or not, because %s: multicast to it is as costly as flooding (architecture §9)", why)
		}
	}
	return res, nil
}

func (b *batmanReconciler) Teardown(ctx context.Context) error {
	deleted, err := rtnl.DeleteIfExists(BatIfname)
	if deleted {
		b.d.log.Info("removed interface", "name", BatIfname)
	}
	return err
}
