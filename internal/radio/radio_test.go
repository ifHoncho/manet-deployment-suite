package radio

import (
	"net"
	"testing"

	"meridian/trellis/internal/config"
	"meridian/trellis/internal/hwdb"
	"meridian/trellis/internal/nl80211"
	"meridian/trellis/internal/sysfs"
)

func mac(s string) net.HardwareAddr {
	m, err := net.ParseMAC(s)
	if err != nil {
		panic(err)
	}
	return m
}

type spec struct {
	phy, mac, port, serial, id string
	bus                        string
	speed                      int
	mesh, ap, both             bool
	bands                      []string
	power                      map[string]float64
	streams                    int
}

func mkRadio(s spec) *Radio {
	r := &Radio{
		Phy:      s.phy,
		PermAddr: mac(s.mac),
		Device:   sysfs.Device{Bus: s.bus, Port: s.port, Serial: s.serial, USBSpeedMbps: s.speed, Path: "/sys/devices/x/" + s.phy},
		Caps:     Caps{MeshPoint: s.mesh, AP: s.ap, MeshAndAP: s.both, Bands: s.bands, Streams: s.streams},
		Power:    map[string]PowerFact{},
	}
	if s.id != "" {
		r.Device.VendorID, r.Device.ProductID = s.id[:4], s.id[5:]
	}
	if r.Device.Bus == "" {
		r.Device.Bus = sysfs.BusUSB
	}
	for b, p := range s.power {
		r.Power[b] = PowerFact{DBm: p, Source: PowerDriver}
	}
	return r
}

var (
	strong = spec{phy: "phy0", mac: "00:c0:ca:11:11:11", port: "usb:1-1", id: "0e8d:7612", bus: "usb", speed: 5000,
		mesh: true, ap: true, both: true, bands: []string{"2g", "5g"}, power: map[string]float64{"2g": 20, "5g": 23}, streams: 2}
	weak = spec{phy: "phy1", mac: "00:c0:ca:22:22:22", port: "usb:1-2", id: "0cf3:9271", bus: "usb", speed: 480,
		mesh: true, ap: true, both: true, bands: []string{"2g"}, power: map[string]float64{"2g": 17}, streams: 1}
	mid = spec{phy: "phy2", mac: "00:c0:ca:33:33:33", port: "usb:1-3", id: "148f:5572", bus: "usb", speed: 480,
		mesh: true, ap: true, both: false, bands: []string{"2g", "5g"}, power: map[string]float64{"2g": 18, "5g": 17}, streams: 2}
)

func radios(specs ...spec) []*Radio {
	var out []*Radio
	for _, s := range specs {
		out = append(out, mkRadio(s))
	}
	AssignFingerprints(out)
	return out
}

func auto() config.RoleOverrides {
	return config.RoleOverrides{Mesh: config.RoleSelection{Mode: config.RoleAuto}, AP: config.RoleSelection{Mode: config.RoleAuto}}
}

func TestFingerprintSources(t *testing.T) {
	// Good unique MACs.
	rs := radios(strong, weak)
	if rs[0].FingerprintSource != SourcePermanentMAC || rs[0].Fingerprint != "mac:00:c0:ca:11:11:11" {
		t.Errorf("good mac: %s %s", rs[0].Fingerprint, rs[0].FingerprintSource)
	}
	// Duplicated MAC across two units: both fall back; serial wins when present.
	a, b := strong, strong
	b.phy, b.port = "phy1", "usb:1-2"
	a.serial, b.serial = "SN0001", "SN0002"
	rs = radios(a, b)
	for _, r := range rs {
		if r.FingerprintSource != SourceUSBSerial {
			t.Errorf("dup mac with serial: %s %s", r.Fingerprint, r.FingerprintSource)
		}
	}
	// Duplicated MAC, bogus serials: port + id.
	a.serial, b.serial = "0000000000", "0000000000"
	rs = radios(a, b)
	for _, r := range rs {
		if r.FingerprintSource != SourcePortAndID {
			t.Errorf("dup mac no serial: %s %s", r.Fingerprint, r.FingerprintSource)
		}
	}
	if rs[0].Fingerprint == rs[1].Fingerprint {
		t.Error("fingerprints must differ")
	}
	// Random (locally administered) MAC alone: next source.
	r := strong
	r.mac = "02:11:22:33:44:55"
	rs = radios(r)
	if rs[0].FingerprintSource == SourcePermanentMAC {
		t.Errorf("locally administered MAC must not be used: %s", rs[0].Fingerprint)
	}
	// PCI radio uses its bus address.
	pci := spec{phy: "phy5", mac: "02:00:00:00:00:01", port: "pci:0000:01:00.0", bus: "pci", mesh: true, ap: true, bands: []string{"5g"}}
	rs = radios(pci)
	if rs[0].FingerprintSource != SourcePCIAddress || rs[0].Fingerprint != "pci:0000:01:00.0" {
		t.Errorf("pci: %s %s", rs[0].Fingerprint, rs[0].FingerprintSource)
	}
}

func TestPlanOrderIndependent(t *testing.T) {
	a := MakePlan(Input{Radios: radios(strong, weak, mid), MeshBand: "5g", APEnabled: true, Roles: auto()})
	b := MakePlan(Input{Radios: radios(mid, weak, strong), MeshBand: "5g", APEnabled: true, Roles: auto()})
	if a.MeshFingerprint() != b.MeshFingerprint() || a.APFingerprint() != b.APFingerprint() {
		t.Errorf("order dependent: %v vs %v", a.Roles, b.Roles)
	}
	if a.Mesh.Phy != "phy0" {
		t.Errorf("mesh should be the strongest 5g radio, got %s", a.Mesh.Phy)
	}
	// AP band is 2g (mesh on 5g); lowest power on 2g among the rest is weak (17).
	if a.AP.Phy != "phy1" {
		t.Errorf("ap should be the lowest-power 2g radio, got %s", a.AP.Phy)
	}
	if a.Roles["mac:00:c0:ca:33:33:33"] != RoleUnused {
		t.Errorf("third radio should be unused: %v", a.Roles)
	}
}

func TestPlanTieBreakStable(t *testing.T) {
	a, b := strong, strong
	b.phy, b.mac, b.port = "phy9", "00:c0:ca:00:00:01", "usb:1-9"
	p1 := MakePlan(Input{Radios: radios(a, b), MeshBand: "5g", APEnabled: true, Roles: auto()})
	p2 := MakePlan(Input{Radios: radios(b, a), MeshBand: "5g", APEnabled: true, Roles: auto()})
	if p1.MeshFingerprint() != p2.MeshFingerprint() || p1.MeshFingerprint() != "mac:00:c0:ca:00:00:01" {
		t.Errorf("tie-break by fingerprint failed: %s / %s", p1.MeshFingerprint(), p2.MeshFingerprint())
	}
}

func TestMeshLossTakesAPRadio(t *testing.T) {
	rs := radios(strong, mid)
	boot := MakePlan(Input{Radios: rs, MeshBand: "5g", APEnabled: true, Roles: auto()})
	if boot.Mesh.Phy != "phy0" || boot.AP.Phy != "phy2" {
		t.Fatalf("boot plan: %v", boot.Roles)
	}
	// Mesh radio disappears.
	after := MakePlan(Input{Radios: radios(mid), MeshBand: "5g", APEnabled: true, Roles: auto(), Current: boot.Roles})
	if after.Mesh == nil || after.Mesh.Phy != "phy2" {
		t.Fatalf("mesh should move to the remaining radio: %v", after.Roles)
	}
	// mid cannot do mesh+ap, so the AP is gone with a warning.
	if after.AP != nil || !hasNotice(after, "no-ap-radio") {
		t.Errorf("expected no AP and a warning: %+v", after.Notices)
	}
}

func TestNewRadioFillsEmptyRoleOnly(t *testing.T) {
	// Boot with only the weak radio: it carries mesh (on 2g) and AP together.
	boot := MakePlan(Input{Radios: radios(weak), MeshBand: "2g", APEnabled: true, Roles: auto()})
	if boot.Roles["mac:00:c0:ca:22:22:22"] != RoleMeshAP || !hasNotice(boot, "ap-shares-mesh-radio") {
		t.Fatalf("single radio: %v %+v", boot.Roles, boot.Notices)
	}
	// Plug in the strong radio at runtime.
	rt := MakePlan(Input{Radios: radios(weak, strong), MeshBand: "2g", APEnabled: true, Roles: auto(), Current: boot.Roles})
	if rt.Mesh.Phy != "phy1" {
		t.Errorf("mesh must stay on the working radio, got %s", rt.Mesh.Phy)
	}
	if rt.AP == nil || rt.AP.Phy != "phy0" {
		t.Errorf("new radio should take the empty AP role, got %+v", rt.Roles)
	}
	if !rt.Differs || rt.Recommended["mac:00:c0:ca:11:11:11"] != RoleMesh {
		t.Errorf("recommendation should prefer the strong radio for mesh: %v differs=%v", rt.Recommended, rt.Differs)
	}
}

func TestSingleRadioWithoutCombination(t *testing.T) {
	only := mid // mesh and AP, but not together
	p := MakePlan(Input{Radios: radios(only), MeshBand: "5g", APEnabled: true, Roles: auto()})
	if p.Roles["mac:00:c0:ca:33:33:33"] != RoleMesh || !hasNotice(p, "no-ap-radio") {
		t.Errorf("mesh only expected with warning: %v %+v", p.Roles, p.Notices)
	}
}

func TestPins(t *testing.T) {
	rs := radios(strong, weak)
	pinPort := config.RoleOverrides{
		Mesh: config.RoleSelection{Mode: "port", Port: "usb:1-2"},
		AP:   config.RoleSelection{Mode: config.RoleAuto},
	}
	p := MakePlan(Input{Radios: rs, MeshBand: "2g", APEnabled: true, Roles: pinPort})
	if p.Mesh.Phy != "phy1" || p.AP.Phy != "phy0" {
		t.Errorf("pin by port: %v", p.Roles)
	}
	missing := config.RoleOverrides{
		Mesh: config.RoleSelection{Mode: "radio", Radio: "mac:de:ad:be:ef:00:00"},
		AP:   config.RoleSelection{Mode: config.RoleAuto},
	}
	p = MakePlan(Input{Radios: rs, MeshBand: "2g", APEnabled: true, Roles: missing})
	if p.Mesh != nil || !hasNotice(p, "mesh-pin-missing") {
		t.Errorf("missing pin must not fall back silently: %v %+v", p.Roles, p.Notices)
	}
}

func TestIgnoredAndIneligible(t *testing.T) {
	rs := radios(strong, weak)
	rs[0].Ignored = true
	p := MakePlan(Input{Radios: rs, MeshBand: "5g", APEnabled: true, Roles: auto()})
	if p.Mesh != nil || !hasNotice(p, "no-mesh-radio") {
		t.Errorf("ignored strong radio and 2g-only weak radio: no 5g mesh expected, got %v %+v", p.Roles, p.Notices)
	}
	p = MakePlan(Input{Radios: nil, MeshBand: "5g", APEnabled: true, Roles: auto()})
	if !hasNotice(p, "no-radios") {
		t.Error("no radios notice expected")
	}
}

func TestMeshDisabledWithWiredBackhaul(t *testing.T) {
	roles := config.RoleOverrides{Mesh: config.RoleSelection{Mode: config.RoleDisabled}, AP: config.RoleSelection{Mode: config.RoleAuto}}
	p := MakePlan(Input{Radios: radios(strong), MeshBand: "5g", APEnabled: true, Roles: roles})
	if p.Mesh != nil || p.AP == nil || p.AP.Phy != "phy0" {
		t.Errorf("mesh disabled: %v", p.Roles)
	}
}

func hasNotice(p *Plan, code string) bool {
	for _, n := range p.Notices {
		if n.Code == code {
			return true
		}
	}
	return false
}

func TestInUseRadioLeftAloneUnlessPinned(t *testing.T) {
	// Pi Zero 2 W: onboard brcmfmac (AP-capable, connected to the operator's
	// Wi-Fi) plus a USB mesh adapter.
	onboard := spec{phy: "phy7", mac: "b8:27:eb:00:00:01", port: "sdio:mmc1:0001:1", bus: "sdio",
		mesh: false, ap: true, bands: []string{"2g"}, power: map[string]float64{"2g": 18}}
	rs := radios(onboard, strong)
	rs[0].InUse = `wlan0 is connected to Wi-Fi network "home"`
	p := MakePlan(Input{Radios: rs, MeshBand: "5g", APEnabled: true, Roles: auto()})
	if p.Mesh == nil || p.Mesh.Phy != strong.phy {
		t.Fatalf("USB adapter should carry the mesh: %v", p.Roles)
	}
	if p.AP != nil && p.AP.Phy == "phy7" {
		t.Fatalf("connected onboard radio must not be taken for the AP: %v", p.Roles)
	}
	if !hasNotice(p, "radio-in-use") {
		t.Errorf("expected a radio-in-use notice: %+v", p.Notices)
	}
	// Explicit pin takes it over.
	pin := config.RoleOverrides{Mesh: config.RoleSelection{Mode: config.RoleAuto}, AP: config.RoleSelection{Mode: "port", Port: "sdio:mmc1:0001:1"}}
	p = MakePlan(Input{Radios: rs, MeshBand: "5g", APEnabled: true, Roles: pin})
	if p.AP == nil || p.AP.Phy != "phy7" {
		t.Errorf("pinned AP should take the in-use radio: %v %+v", p.Roles, p.Notices)
	}
}

func TestDetectInUse(t *testing.T) {
	noAddr := func(int) string { return "" }
	withAddr := func(i int) string {
		if i == 3 {
			return "192.168.1.20"
		}
		return ""
	}
	sta := nl80211.Interface{Ifindex: 3, Name: "wlan0", Type: nl80211.NL80211_IFTYPE_STATION}
	if got := DetectInUse([]nl80211.Interface{sta}, noAddr); got != "" {
		t.Errorf("idle station is not in use: %q", got)
	}
	if got := DetectInUse([]nl80211.Interface{sta}, withAddr); got == "" {
		t.Error("station with an address is in use")
	}
	sta.SSID = "home"
	if got := DetectInUse([]nl80211.Interface{sta}, noAddr); got == "" {
		t.Error("associated station is in use")
	}
	ours := nl80211.Interface{Ifindex: 3, Name: "mesh0", Type: nl80211.NL80211_IFTYPE_MESH_POINT, SSID: "x"}
	if got := DetectInUse([]nl80211.Interface{ours}, withAddr); got != "" {
		t.Errorf("Trellis interfaces are never 'in use by the system': %q", got)
	}
}

// A tested adapter wins the mesh role over an untested one whose driver
// claims more power (node-2, 2026-11-03: rtw_8821cu reporting 30 dBm beat a
// verified MT7610U at 14 dBm).
func TestMeshPrefersTestedRadio(t *testing.T) {
	tested := spec{phy: "phy1", mac: "9c:ef:d5:fb:2d:26", port: "usb:1-1", bus: "usb", speed: 480,
		mesh: true, ap: true, both: true, bands: []string{"2g", "5g"}, power: map[string]float64{"2g": 14}, streams: 1}
	untested := spec{phy: "phy0", mac: "cc:64:1a:eb:81:64", port: "usb:2-1", bus: "usb", speed: 480,
		mesh: true, ap: true, bands: []string{"2g", "5g"}, power: map[string]float64{"2g": 30}, streams: 1}
	rs := radios(tested, untested)
	rs[0].HWDB = &hwdb.Entry{MeshPoint: hwdb.Verified}
	rs[1].HWDB = &hwdb.Entry{MeshPoint: hwdb.Unknown}
	p := MakePlan(Input{Radios: rs, MeshBand: "2g", APEnabled: true, Roles: auto()})
	if p.Mesh == nil || p.Mesh.Phy != "phy1" {
		t.Fatalf("mesh radio = %v, want the tested phy1", p.Mesh)
	}
}

// A dual-band-concurrent card (the reference build's MT7916): two wiphys on
// one PCI address, the second with a derived, locally administered MAC.
var (
	dbdcG = spec{phy: "phy0", mac: "02:0c:43:26:60:00", port: "pci:0000:01:00.0", bus: "pci", id: "14c3:7906",
		mesh: true, ap: true, both: true, bands: []string{"2g"}, power: map[string]float64{"2g": 21}, streams: 2}
	dbdcA = spec{phy: "phy1", mac: "02:0c:43:26:60:01", port: "pci:0000:01:00.0", bus: "pci", id: "14c3:7906",
		mesh: true, ap: true, both: true, bands: []string{"5g", "6g"}, power: map[string]float64{"5g": 20, "6g": 18}, streams: 2}
)

func dbdcRadios(order ...spec) []*Radio {
	rs := []*Radio{}
	for _, s := range order {
		r := mkRadio(s)
		if s.phy == "phy1" {
			r.PhyIndex = 1
		}
		rs = append(rs, r)
	}
	AssignFingerprints(rs)
	return rs
}

func TestFingerprintDualBandCard(t *testing.T) {
	rs := dbdcRadios(dbdcG, dbdcA)
	g, a := rs[0].Fingerprint, rs[1].Fingerprint
	if g == a {
		t.Fatalf("both radios of one card got fingerprint %q", g)
	}
	if g != "pci:0000:01:00.0#2g" || a != "pci:0000:01:00.0#5g+6g" {
		t.Errorf("fingerprints %q and %q; want the PCI address plus each radio's bands", g, a)
	}
	// Probe order (phy numbering) must not matter.
	rs2 := dbdcRadios(dbdcA, dbdcG)
	for _, r := range rs2 {
		want := map[string]string{"2g": g, "5g": a}[r.Caps.Bands[0]]
		if r.Fingerprint != want {
			t.Errorf("probe order changed %v's fingerprint to %q", r.Caps.Bands, r.Fingerprint)
		}
	}
	// Two radios with the same bands on one device still differ.
	twin := dbdcA
	twin.phy, twin.mac = "phy2", "02:0c:43:26:60:02"
	rs3 := dbdcRadios(dbdcA, twin)
	if rs3[0].Fingerprint == rs3[1].Fingerprint {
		t.Errorf("identical-band radios on one device collide: %q", rs3[0].Fingerprint)
	}
	// A lone PCI radio keeps its plain fingerprint (nothing changes on
	// existing nodes).
	if one := dbdcRadios(dbdcA); one[0].Fingerprint != "pci:0000:01:00.0" {
		t.Errorf("single radio fingerprint changed to %q", one[0].Fingerprint)
	}
}

func TestPlanDualBandCard(t *testing.T) {
	p := MakePlan(Input{Radios: dbdcRadios(dbdcG, dbdcA), MeshBand: "5g", APEnabled: true, Roles: auto()})
	if p.Mesh == nil || p.Mesh.Phy != "phy1" {
		t.Fatalf("mesh should be the card's 5 GHz radio, got %+v", p.Mesh)
	}
	if p.AP == nil || p.AP.Phy != "phy0" || p.APBand != "2g" {
		t.Fatalf("AP should be the card's 2.4 GHz radio on 2g, got %+v band %s", p.AP, p.APBand)
	}
	for _, n := range p.Notices {
		if n.Code == "ap-shares-mesh-radio" {
			t.Errorf("one card, two radios: the AP must not share the mesh radio (%s)", n.Message)
		}
	}
}

// The reference build: MT7916 (two radios on one card) plus a HaLow radio.
var halow = spec{phy: "phy2", mac: "0c:bf:74:00:00:01", port: "spi:spi0.0", bus: "spi",
	mesh: true, ap: false, bands: []string{"s1g"}, power: map[string]float64{"s1g": 21}, streams: 1}

func TestPlanDualBackhaulReferenceBuild(t *testing.T) {
	rs := dbdcRadios(dbdcG, dbdcA)
	h := mkRadio(halow)
	h.PhyIndex = 2
	rs = append(rs, h)
	AssignFingerprints(rs)
	p := MakePlan(Input{Radios: rs, MeshBand: "5g", Mesh2Band: "s1g", APEnabled: true, Roles: auto()})
	if p.Mesh == nil || p.Mesh.Phy != "phy1" {
		t.Fatalf("mesh0 should be the MT7916's 5 GHz radio, got %+v", p.Mesh)
	}
	if p.Mesh2 == nil || p.Mesh2.Phy != "phy2" {
		t.Fatalf("mesh1 should be the HaLow radio, got %+v", p.Mesh2)
	}
	if p.AP == nil || p.AP.Phy != "phy0" || p.APBand != "2g" {
		t.Fatalf("AP should be the MT7916's 2.4 GHz radio, got %+v", p.AP)
	}
	if p.Roles[p.Mesh2.Fingerprint] != RoleMesh2 || hasNotice(p, "ap-shares-mesh-radio") || hasNotice(p, "radios-unused") {
		t.Errorf("roles %v notices %v", p.Roles, p.Notices)
	}
}

func TestPlanDualBackhaulTwoRadios(t *testing.T) {
	// strong: 2g+5g mesh+AP capable; weak: 2g only. Links on 5g and 2g take
	// both radios; the AP shares the first link's radio.
	p := MakePlan(Input{Radios: radios(strong, weak), MeshBand: "5g", Mesh2Band: "2g", APEnabled: true, Roles: auto()})
	if p.Mesh == nil || p.Mesh.Phy != "phy0" || p.Mesh2 == nil || p.Mesh2.Phy != "phy1" {
		t.Fatalf("links: mesh %+v mesh2 %+v", p.Mesh, p.Mesh2)
	}
	if p.AP != p.Mesh || p.Roles[p.Mesh.Fingerprint] != RoleMeshAP || !hasNotice(p, "ap-shares-mesh-radio") {
		t.Fatalf("AP should share the first link's radio: AP %+v roles %v", p.AP, p.Roles)
	}
}

func TestPlanDualBackhaulMissingRadio(t *testing.T) {
	p := MakePlan(Input{Radios: radios(strong), MeshBand: "5g", Mesh2Band: "2g", APEnabled: true, Roles: auto()})
	if p.Mesh == nil || p.Mesh2 != nil || !hasNotice(p, "no-mesh2-radio") {
		t.Fatalf("one radio: mesh %+v mesh2 %+v notices %v", p.Mesh, p.Mesh2, p.Notices)
	}
	// No second link asked for: no notice about it.
	p = MakePlan(Input{Radios: radios(strong), MeshBand: "5g", APEnabled: true, Roles: auto()})
	if hasNotice(p, "no-mesh2-radio") || p.Mesh2 != nil {
		t.Fatalf("single-link profile mentions a second link: %v", p.Notices)
	}
}

func TestPlanMesh2PinConflict(t *testing.T) {
	rs := radios(strong, weak)
	roles := auto()
	roles.Mesh2 = config.RoleSelection{Mode: "radio", Radio: rs[0].Fingerprint}
	p := MakePlan(Input{Radios: rs, MeshBand: "5g", Mesh2Band: "2g", APEnabled: true, Roles: roles})
	if p.Mesh2 != nil || !hasNotice(p, "mesh2-pin-conflict") {
		t.Fatalf("mesh2 pinned to the mesh radio: mesh2 %+v notices %v", p.Mesh2, p.Notices)
	}
}
