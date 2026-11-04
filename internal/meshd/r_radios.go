package meshd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vishvananda/netlink"

	"golang.org/x/sys/unix"

	"meridian/trellis/internal/config"
	"meridian/trellis/internal/hwdb"
	"meridian/trellis/internal/nl80211"
	"meridian/trellis/internal/radio"
	"meridian/trellis/internal/reconcile"
	"meridian/trellis/internal/status"
	"meridian/trellis/internal/sysfs"
)

// removalGrace is how long a vanished radio keeps its role before roles are
// recomputed, so a flaky USB connection does not thrash (architecture §5.2).
const removalGrace = 3 * time.Second

// Churn guard: if an interface meshd created has to be recreated this many
// times within the window, something else is reconfiguring it; meshd stops
// fighting and reports it.
const (
	churnLimit  = 3
	churnWindow = 2 * time.Minute
)

// radioReconciler owns the radio inventory, role assignment, the Trellis
// interfaces (mesh0, ap0), the regulatory domain and the mesh underlay MTU.
type radioReconciler struct {
	d            *Daemon
	booted       bool
	active       map[string]radio.Role
	missingSince map[string]time.Time
	regApplied   string
	regAppliedAt time.Time
	regFirstAsk  time.Time
	created      map[string][]time.Time // interface name -> recent creation times
	foreign      map[string]int         // foreign interfaces removed from the mesh radio
	memSaved     time.Time
}

func newRadioReconciler(d *Daemon) *radioReconciler {
	return &radioReconciler{d: d, active: map[string]radio.Role{}, missingSince: map[string]time.Time{}}
}

func (r *radioReconciler) Name() string { return "radios" }

// meshLinkSetup prepares one mesh link's interface on its radio: rf-kill,
// the underlay MTU and a sanity check of its channel. It returns the
// interface (nil when it does not exist yet), its MTU and whether it
// changed anything.
func (r *radioReconciler) meshLinkSetup(rd *radio.Radio, ifname string, rp config.RadioPlan, alpha2 string, warn func(level, code, format string, args ...any)) (*nl80211.Interface, int, bool) {
	d := r.d
	cfg := d.cfg
	if rd == nil {
		return nil, 0, false
	}
	changed := false
	var meshIf *nl80211.Interface
	for i := range rd.Interfaces {
		if rd.Interfaces[i].Name == ifname {
			meshIf = &rd.Interfaces[i]
		}
	}
	if unblocked, err := sysfs.RFKillUnblock(d.opts.SysRoot, rd.Phy); err != nil {
		warn("warning", "rfkill", "%v", err)
	} else if unblocked {
		d.log.Info("cleared soft rf-kill block", "phy", rd.Phy)
		changed = true
	}
	mtu := 0
	if meshIf != nil {
		if link, err := netlink.LinkByIndex(meshIf.Ifindex); err == nil {
			if link.Attrs().MTU != cfg.MeshMTU {
				if err := netlink.LinkSetMTU(link, cfg.MeshMTU); err != nil {
					warn("warning", "mesh-mtu", "cannot set %s MTU to %d (%v); keeping %d, batman-adv fragmentation covers the difference", ifname, cfg.MeshMTU, err, link.Attrs().MTU)
				} else {
					d.log.Info("set underlay MTU", "interface", ifname, "mtu", cfg.MeshMTU)
					changed = true
				}
			}
			if link, err = netlink.LinkByIndex(meshIf.Ifindex); err == nil {
				mtu = link.Attrs().MTU
			}
		}
	}
	if rd.Wiphy != nil {
		freq, _ := config.ChannelFrequencyMHz(rp.Band, rp.Channel)
		switch f := rd.Wiphy.Frequency(uint32(freq)); {
		case f == nil:
			warn("error", "channel-unsupported", "mesh radio %s (%s) has no channel %d (%d MHz)", rd.Phy, ifname, rp.Channel, freq)
		case f.Disabled:
			warn("error", "channel-disabled", "channel %d (%d MHz) for %s is disabled on %s under regulatory domain %s", rp.Channel, freq, ifname, rd.Phy, alpha2)
		case f.Radar:
			warn("error", "channel-dfs", "channel %d (%d MHz) for %s requires radar detection (DFS); a mesh point cannot start there, choose a non-DFS channel", rp.Channel, freq, ifname)
		case f.NoIR:
			warn("error", "channel-no-ir", "channel %d (%d MHz) for %s is passive-only (no-IR) under regulatory domain %s; a mesh cannot start there", rp.Channel, freq, ifname, alpha2)
		}
	}
	return meshIf, mtu, changed
}

func isTrellisIfname(name string) bool {
	return name == MeshIfname || name == APIfname || name == Mesh2Ifname
}

func (r *radioReconciler) Reconcile(ctx context.Context) (reconcile.Result, error) {
	d := r.d
	cfg, profile := d.cfg, d.profile
	var res reconcile.Result
	warn := func(level, code, format string, args ...any) {
		res.Warnings = append(res.Warnings, reconcile.Warning{Level: level, Code: code, Message: fmt.Sprintf(format, args...)})
	}

	if d.nl == nil {
		transport := "none"
		if len(cfg.MeshPorts()) > 0 {
			transport = "wired"
		}
		d.facts.Update(func(f *Facts) { f.Transport = transport; f.Radios = nil; f.Plan = nil })
		if cfg.Radios.Roles.Mesh.Mode == config.RoleDisabled {
			return res, nil
		}
		return res, errors.New("nl80211 is unavailable, so no Wi-Fi radio can carry the mesh (set radios.roles.mesh: disabled for a wired-only node)")
	}

	// Regulatory domain first (architecture §5.1): it does not depend on
	// the inventory, and channel flags in the inventory depend on it.
	changed := false
	reg := status.Regulatory{RequestedMode: profile.Regulatory.Mode, Country: profile.Regulatory.Country, ActiveMode: config.RegModeCountry}
	if profile.Regulatory.Mode == config.RegModeUnrestricted {
		reg.Note = "unrestricted mode needs the Trellis regulatory database, which this build does not ship yet; operating under " + profile.Regulatory.Country + " limits"
		warn("warning", "unrestricted-unavailable", "%s", reg.Note)
	}
	if rd, err := d.nl.GetReg(); err == nil {
		reg.KernelAlpha2 = rd.Alpha2
		want := profile.Regulatory.Country
		if rd.Alpha2 == want {
			r.regFirstAsk = time.Time{}
		} else {
			first := r.regApplied != want
			if first || time.Since(r.regAppliedAt) > 30*time.Second {
				if err := d.nl.SetReg(want); err != nil {
					warn("warning", "regdomain", "cannot set regulatory domain %s: %v", want, err)
				} else {
					if first {
						d.log.Info("requested regulatory domain", "alpha2", want, "was", rd.Alpha2)
						r.regFirstAsk = time.Now()
						changed = true // retries are not changes
					}
					r.regApplied, r.regAppliedAt = want, time.Now()
				}
			}
			if !r.regFirstAsk.IsZero() && time.Since(r.regFirstAsk) > 10*time.Second {
				msg := fmt.Sprintf("the kernel is still in regulatory domain %s %s after meshd requested %s", rd.Alpha2, time.Since(r.regFirstAsk).Round(time.Second), want)
				if rd.Alpha2 == "00" {
					msg += "; it most likely rejects /lib/firmware/regulatory.db (signature). See sudo meshctl doctor, or run scripts/install-regdb-fallback.sh"
				}
				warn("error", "regdomain-not-applied", "%s", msg)
			}
		}
	} else {
		warn("warning", "regdomain", "cannot read regulatory domain: %v", err)
	}

	d.facts.Update(func(f *Facts) { f.Regulatory = reg })

	radios, err := radio.Inventory(d.nl, d.opts.SysRoot, d.hw, cfg.Radios)
	if err != nil {
		return res, fmt.Errorf("radio inventory: %w", err)
	}

	// Radio memory.
	now := time.Now()
	memChanged := false
	present := map[string]bool{}
	for _, rd := range radios {
		present[rd.Fingerprint] = true
		rec := d.radioMem.Get(rd.Fingerprint)
		if rec.KernelIfname == "" {
			for _, in := range rd.Interfaces {
				if !isTrellisIfname(in.Name) && in.Ifindex != 0 {
					rec.KernelIfname = in.Name
					memChanged = true
					break
				}
			}
		}
		if rec.Port != rd.Device.Port || rec.Driver != rd.Device.Driver {
			rec.Port, rec.Driver = rd.Device.Port, rd.Device.Driver
			memChanged = true
		}
		if now.Sub(rec.LastSeen) > time.Minute {
			rec.LastSeen = now.UTC().Truncate(time.Second)
			memChanged = true
		}
	}

	// Debounce removals of role-holding radios.
	grace := false
	var requeue time.Duration
	for fp, role := range r.active {
		if present[fp] {
			delete(r.missingSince, fp)
			continue
		}
		since, ok := r.missingSince[fp]
		if !ok {
			since = now
			r.missingSince[fp] = now
			d.log.Warn("radio disappeared; holding its role briefly in case it returns", "fingerprint", fp, "role", role, "grace", removalGrace)
		}
		if left := removalGrace - now.Sub(since); left > 0 {
			grace = true
			if requeue == 0 || left < requeue {
				requeue = left + 100*time.Millisecond
			}
		}
	}
	if grace {
		d.facts.Update(func(f *Facts) { f.Radios = radios; f.GraceActive = true })
		res.RequeueAfter = requeue
		return res, nil
	}
	for fp := range r.active {
		if !present[fp] {
			d.log.Warn("radio gone; reassigning roles", "fingerprint", fp, "role", r.active[fp])
			delete(r.active, fp)
			delete(r.missingSince, fp)
		}
	}

	var current map[string]radio.Role
	if r.booted {
		current = r.active
	}
	mesh2Band := ""
	if profile.Radio2 != nil {
		mesh2Band = profile.Radio2.Band
	}
	plan := radio.MakePlan(radio.Input{
		Radios: radios, MeshBand: profile.Radio.Band, Mesh2Band: mesh2Band, APEnabled: cfg.APEnabled(), Roles: cfg.Radios.Roles, Current: current,
	})
	r.booted = true

	// Interfaces that no longer belong where they are.
	for _, rd := range radios {
		role := plan.Roles[rd.Fingerprint]
		wantMesh := role == radio.RoleMesh || role == radio.RoleMeshAP
		wantMesh2 := role == radio.RoleMesh2 || role == radio.RoleMesh2AP
		wantAP := role == radio.RoleAP || role == radio.RoleMeshAP || role == radio.RoleMesh2AP
		for _, in := range rd.Interfaces {
			if (in.Name == MeshIfname && !wantMesh) || (in.Name == Mesh2Ifname && !wantMesh2) || (in.Name == APIfname && !wantAP) {
				if err := d.nl.DelInterface(in.Ifindex); err != nil {
					return res, err
				}
				d.log.Info("removed interface", "name", in.Name, "phy", rd.Phy, "reason", "role no longer held")
				changed = true
			}
		}
	}
	if plan.Mesh != nil {
		ch, err := r.ensureInterface(plan.Mesh, MeshIfname, nl80211.NL80211_IFTYPE_MESH_POINT, nil)
		if err != nil {
			return res, err
		}
		changed = changed || ch
	}
	if plan.Mesh2 != nil {
		ch, err := r.ensureInterface(plan.Mesh2, Mesh2Ifname, nl80211.NL80211_IFTYPE_MESH_POINT, nil)
		if err != nil {
			return res, err
		}
		changed = changed || ch
	}
	// The mesh radio is Trellis's: any other interface that appears on it
	// later (some images add a p2p or wlan interface to every new radio, and
	// a network manager then scans on it) takes the radio off the mesh
	// channel. Remove it, and say so if it keeps coming back.
	for _, meshRadio := range []*radio.Radio{plan.Mesh, plan.Mesh2} {
		if meshRadio == nil {
			continue
		}
		for _, in := range meshRadio.Interfaces {
			if isTrellisIfname(in.Name) || in.Ifindex == 0 {
				continue
			}
			if err := d.nl.DelInterface(in.Ifindex); err != nil {
				warn("warning", "foreign-interface", "cannot remove %s from the mesh radio %s: %v", in.Name, meshRadio.Phy, err)
				continue
			}
			changed = true
			if r.foreign == nil {
				r.foreign = map[string]int{}
			}
			r.foreign[in.Name]++
			d.log.Warn("removed an interface something else added to the mesh radio", "name", in.Name, "type", in.TypeName(), "phy", meshRadio.Phy, "times", r.foreign[in.Name])
			if r.foreign[in.Name] >= 3 {
				warn("warning", "foreign-interface", "%s keeps being added to the mesh radio %s (%d times); a udev rule or service on this node creates it. "+
					"Look for it with: grep -rl '%s\\|interface add' /etc/udev/rules.d /lib/udev/rules.d /etc/systemd/system", in.Name, meshRadio.Phy, r.foreign[in.Name], strings.TrimRight(in.Name, "0123456789"))
			}
		}
	}

	for _, rd := range []*radio.Radio{plan.Mesh, plan.Mesh2, plan.AP} {
		if rd == nil || rd.HWDB == nil {
			continue
		}
		for k, want := range rd.HWDB.ModuleParams {
			have, err := os.ReadFile(filepath.Join(d.opts.SysRoot, "module", rd.Device.Driver, "parameters", k))
			if err != nil || hwdb.ParamMatches(string(have), want) {
				continue
			}
			warn("error", "driver-params", "%s needs the %s module loaded with %s=%s (it has %s): without it, unicast over the mesh fails. "+
				"make deploy sets it; by hand: echo 'options %s %s=%s' | sudo tee /etc/modprobe.d/trellis.conf, then unplug and replug the adapter",
				rd.Describe(), rd.Device.Driver, k, want, strings.TrimSpace(string(have)), rd.Device.Driver, k, want)
		}
	}
	if plan.AP != nil {
		var mac net.HardwareAddr
		if plan.AP == plan.Mesh || plan.AP == plan.Mesh2 {
			mac = apMAC(plan.AP.PermAddr, plan.AP.Device.Driver)
		}
		ch, err := r.ensureInterface(plan.AP, APIfname, nl80211.NL80211_IFTYPE_AP, mac)
		if err != nil {
			return res, err
		}
		changed = changed || ch
	}
	if changed {
		if ifaces, err := d.nl.Interfaces(); err == nil {
			for _, rd := range radios {
				rd.Interfaces = rd.Interfaces[:0]
				for _, in := range ifaces {
					if in.Wiphy == rd.PhyIndex {
						rd.Interfaces = append(rd.Interfaces, in)
					}
				}
			}
		}
	}

	// Remember roles.
	r.active = map[string]radio.Role{}
	for _, rd := range radios {
		role := plan.Roles[rd.Fingerprint]
		r.active[rd.Fingerprint] = role
		rec := d.radioMem.Get(rd.Fingerprint)
		if rec.LastRole != string(role) {
			rec.LastRole = string(role)
			memChanged = true
		}
	}
	if memChanged {
		if err := d.radioMem.Save(d.opts.StateDir); err != nil {
			warn("warning", "state-write", "cannot save radio memory: %v", err)
		}
	}

	// mesh0 facts, MTU and rfkill.
	var apIf *nl80211.Interface
	if plan.AP != nil {
		for i := range plan.AP.Interfaces {
			if plan.AP.Interfaces[i].Name == APIfname {
				apIf = &plan.AP.Interfaces[i]
			}
		}
	}
	meshIf, meshMTU, ch1 := r.meshLinkSetup(plan.Mesh, MeshIfname, profile.Radio, reg.KernelAlpha2, warn)
	var mesh2If *nl80211.Interface
	mesh2MTU, ch2 := 0, false
	if profile.Radio2 != nil {
		mesh2If, mesh2MTU, ch2 = r.meshLinkSetup(plan.Mesh2, Mesh2Ifname, *profile.Radio2, reg.KernelAlpha2, warn)
	}
	changed = changed || ch1 || ch2

	transport := "none"
	if plan.Mesh != nil {
		transport = "wifi"
	}
	if len(cfg.MeshPorts()) > 0 {
		if transport == "wifi" {
			transport = "wifi+wired"
		} else {
			transport = "wired"
		}
	}

	active := r.active
	d.facts.Update(func(f *Facts) {
		f.Radios, f.Plan, f.GraceActive = radios, plan, false
		f.Roles = map[string]radio.Role{}
		for k, v := range active {
			f.Roles[k] = v
		}
		f.MeshIfname, f.MeshIfindex, f.MeshWiphy, f.MeshMTU = "", 0, 0, 0
		if meshIf != nil {
			f.MeshIfname, f.MeshIfindex, f.MeshWiphy, f.MeshMTU = MeshIfname, meshIf.Ifindex, meshIf.Wiphy, meshMTU
		}
		f.Link2.Ifname, f.Link2.Ifindex, f.Link2.Wiphy, f.Link2.MTU = "", 0, 0, 0
		if mesh2If != nil {
			f.Link2.Ifname, f.Link2.Ifindex, f.Link2.Wiphy, f.Link2.MTU = Mesh2Ifname, mesh2If.Ifindex, mesh2If.Wiphy, mesh2MTU
		}
		f.APIfname, f.APIfindex, f.APWiphy, f.APShared, f.APSharesLink2 = "", 0, 0, false, false
		if apIf != nil {
			f.APIfname, f.APIfindex, f.APWiphy = APIfname, apIf.Ifindex, apIf.Wiphy
			f.APShared = plan.AP == plan.Mesh || plan.AP == plan.Mesh2
			f.APSharesLink2 = plan.APSharesMesh2()
		}
		f.Transport = transport
	})
	res.Changed = changed
	return res, nil
}

// ensureInterface makes sure the radio has exactly one interface with the
// given name and type, removing the kernel-created interfaces first.
func (r *radioReconciler) ensureInterface(rd *radio.Radio, name string, iftype uint32, mac net.HardwareAddr) (bool, error) {
	d := r.d
	for _, in := range rd.Interfaces {
		if in.Name == name {
			if in.Type == iftype && (mac == nil || in.MAC.String() == mac.String()) {
				return false, nil
			}
			if err := d.nl.DelInterface(in.Ifindex); err != nil {
				return false, err
			}
			if in.Type != iftype {
				d.log.Info("removed interface with the wrong type", "name", name, "type", in.TypeName())
			} else {
				d.log.Info("removed interface with the wrong address", "name", name, "had", in.MAC.String(), "want", mac.String())
			}
			break
		}
	}
	// The name may exist on another radio (role moved while we were down).
	if l, err := netlink.LinkByName(name); err == nil {
		if in, err := d.nl.InterfaceByIndex(l.Attrs().Index); err == nil && in.Wiphy != rd.PhyIndex {
			if err := d.nl.DelInterface(in.Ifindex); err != nil {
				return false, err
			}
			d.log.Info("removed interface from another radio", "name", name, "phy", in.Wiphy)
		}
	}
	// An interface of the right type under another name is most likely ours,
	// renamed by udev (Debian/Ubuntu 73-usb-net-by-mac.rules renames new
	// interfaces on USB adapters to wlx<mac>). Take it back rather than
	// deleting and recreating it on every pass.
	for i, in := range rd.Interfaces {
		if isTrellisIfname(in.Name) || in.Ifindex == 0 || in.Type != iftype {
			continue
		}
		l, err := netlink.LinkByIndex(in.Ifindex)
		if err != nil {
			break
		}
		if err := netlink.LinkSetDown(l); err != nil {
			return false, fmt.Errorf("take down %s to rename it: %w", in.Name, err)
		}
		if err := netlink.LinkSetName(l, name); err != nil {
			return false, fmt.Errorf("rename %s to %s: %w", in.Name, name, err)
		}
		d.log.Warn("renamed an interface back to its Trellis name; is /etc/udev/rules.d/70-trellis-names.rules installed?",
			"was", in.Name, "now", name, "type", nl80211.IftypeName(iftype), "phy", rd.Phy)
		rd.Interfaces[i].Name = name
		return true, nil
	}
	// Remove kernel-created interfaces, and drop them from rd.Interfaces: on
	// a radio with two roles (mesh+ap) this runs twice per pass on the same
	// snapshot, and the second call must not delete them again. One that is
	// already gone (ENODEV) is what we wanted.
	changed := false
	kept := make([]nl80211.Interface, 0, len(rd.Interfaces))
	for _, in := range rd.Interfaces {
		if isTrellisIfname(in.Name) || in.Ifindex == 0 {
			kept = append(kept, in)
			continue
		}
		if err := d.nl.DelInterface(in.Ifindex); err != nil && !errors.Is(err, unix.ENODEV) {
			return changed, fmt.Errorf("remove kernel interface %s on %s: %w", in.Name, rd.Phy, err)
		}
		d.log.Info("removed kernel-created interface", "name", in.Name, "phy", rd.Phy)
		changed = true
	}
	rd.Interfaces = kept
	if err := r.checkChurn(name); err != nil {
		return changed, err
	}
	newIf, err := d.nl.NewInterface(rd.PhyIndex, name, iftype, mac)
	if err != nil {
		return changed, err
	}
	d.log.Info("created interface", "name", name, "type", nl80211.IftypeName(iftype), "phy", rd.Phy, "radio", rd.Describe())
	rd.Interfaces = append(rd.Interfaces, *newIf)
	return true, nil
}

// apMAC derives the AP interface's address from the radio's permanent one
// when the AP shares the mesh radio: two interfaces on one phy need
// different addresses, and drivers disagree on which differences their
// hardware can filter (bench 2026-11-03):
//
//   - mt76x0/mt76x2 (MT7610U, MT7612U) take the hardware slot for a second
//     interface from bits 1–4 of the FIRST byte and refuse the interface
//     (EBUSY on up) when that byte equals the first interface's;
//   - rt2800 (RT5572) and ath9k take it from the low 3 bits of the LAST byte
//     and silently break the first interface when those collide.
//
// So mt76x* gets the locally administered bit set in the first byte, and
// everything else the lowest bit of the last byte flipped.
func apMAC(perm net.HardwareAddr, driver string) net.HardwareAddr {
	if len(perm) != 6 {
		return nil
	}
	m := make(net.HardwareAddr, 6)
	copy(m, perm)
	if strings.HasPrefix(driver, "mt76x") {
		if m[0]&0x02 == 0 {
			m[0] |= 0x02
		} else {
			m[0] ^= 0x04
		}
		return m
	}
	m[5] ^= 0x01
	return m
}

// Teardown removes Trellis interfaces and gives radios their kernel
// interface back.
func (r *radioReconciler) Teardown(ctx context.Context) error {
	d := r.d
	if d.nl == nil {
		return nil
	}
	ifaces, err := d.nl.Interfaces()
	if err != nil {
		return err
	}
	var errs []error
	touched := map[uint32]bool{}
	for _, in := range ifaces {
		if isTrellisIfname(in.Name) && in.Ifindex != 0 {
			if err := d.nl.DelInterface(in.Ifindex); err != nil {
				errs = append(errs, err)
				continue
			}
			touched[in.Wiphy] = true
			d.log.Info("removed interface", "name", in.Name)
		}
	}
	// Restore a managed interface where we removed the kernel's one.
	remaining := map[uint32]int{}
	if ifaces, err = d.nl.Interfaces(); err == nil {
		for _, in := range ifaces {
			if in.Ifindex != 0 {
				remaining[in.Wiphy]++
			}
		}
	}
	for fp, role := range r.active {
		if role == radio.RoleUnused {
			continue
		}
		rec := d.radioMem.Radios[fp]
		if rec == nil || rec.KernelIfname == "" {
			continue
		}
		var rd *radio.Radio
		d.facts.Read(func(f *Facts) { rd = radio.Find(f.Radios, fp) })
		if rd == nil || !touched[rd.PhyIndex] || remaining[rd.PhyIndex] > 0 {
			continue
		}
		if _, err := d.nl.NewInterface(rd.PhyIndex, rec.KernelIfname, nl80211.NL80211_IFTYPE_STATION, nil); err != nil {
			d.log.Warn("could not restore kernel interface", "name", rec.KernelIfname, "phy", rd.Phy, "err", err)
		} else {
			d.log.Info("restored kernel interface", "name", rec.KernelIfname, "phy", rd.Phy)
		}
	}
	return errors.Join(errs...)
}

// checkChurn records a (re)creation of name and refuses when it has happened
// too often recently: something else keeps changing or deleting it.
func (r *radioReconciler) checkChurn(name string) error {
	if r.created == nil {
		r.created = map[string][]time.Time{}
	}
	now := time.Now()
	recent := r.created[name][:0]
	for _, t := range r.created[name] {
		if now.Sub(t) < churnWindow {
			recent = append(recent, t)
		}
	}
	if len(recent) >= churnLimit {
		r.created[name] = recent
		return fmt.Errorf("%s had to be recreated %d times in %s: something else on this node keeps changing it, "+
			"most likely NetworkManager, systemd-networkd/netplan, iwd or a system wpa_supplicant. Check: nmcli device; "+
			"networkctl; ps aux | grep -E 'wpa_supplicant|iwd'. meshd stops recreating it until %s",
			name, len(recent), churnWindow, recent[0].Add(churnWindow).Format("15:04:05"))
	}
	if n := len(recent); n > 0 {
		r.d.log.Warn("recreating an interface meshd created earlier; something removed or changed it",
			"name", name, "previous_created_ago", now.Sub(recent[n-1]).Round(time.Second), "recreations_in_window", n)
	}
	r.created[name] = append(recent, now)
	return nil
}
