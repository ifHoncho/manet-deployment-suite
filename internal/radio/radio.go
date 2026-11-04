// Package radio turns raw nl80211 and sysfs facts into an inventory of
// physical radios with stable fingerprints, and plans which radio carries
// which role (architecture §5.2).
package radio

import (
	"fmt"
	"net"
	"sort"
	"strings"

	"github.com/vishvananda/netlink"

	"meridian/trellis/internal/config"
	"meridian/trellis/internal/hwdb"
	"meridian/trellis/internal/nl80211"
	"meridian/trellis/internal/sysfs"
)

// Fingerprint sources, in order of preference.
const (
	SourcePermanentMAC = "permanent-mac"
	SourceUSBSerial    = "usb-serial"
	SourcePCIAddress   = "pci-address"
	SourcePortAndID    = "port+id"
	SourcePath         = "path"
)

// Power sources, strongest first.
const (
	PowerOverride = "override" // operator-provided value in node config
	PowerMeasured = "measured" // bench measurement in the hardware database
	PowerDatabase = "database" // reported or datasheet value in the hardware database
	PowerDriver   = "driver"   // nl80211 per-channel maximum under the active regulatory domain
)

// Caps are the capabilities that matter for role assignment.
type Caps struct {
	MeshPoint bool     `json:"mesh_point"`
	AP        bool     `json:"ap"`
	MeshAndAP bool     `json:"mesh_and_ap"` // both at once on one channel
	Bands     []string `json:"bands"`
	Streams   int      `json:"streams"`
}

// PowerFact is a maximum transmit power figure and where it came from.
type PowerFact struct {
	DBm    float64 `json:"dbm"`
	Source string  `json:"source"`
}

// Radio is one physical radio.
type Radio struct {
	Fingerprint       string               `json:"fingerprint"`
	FingerprintSource string               `json:"fingerprint_source"`
	Phy               string               `json:"phy"`
	PhyIndex          uint32               `json:"phy_index"`
	PermAddr          net.HardwareAddr     `json:"-"`
	Device            sysfs.Device         `json:"device"`
	Caps              Caps                 `json:"caps"`
	Power             map[string]PowerFact `json:"power"` // by band
	HWDB              *hwdb.Entry          `json:"hwdb,omitempty"`
	Label             string               `json:"label,omitempty"`
	Ignored           bool                 `json:"ignored,omitempty"`
	TXPowerCapDBm     *int                 `json:"tx_power_cap_dbm,omitempty"`
	Broken            string               `json:"broken,omitempty"` // why the radio can't carry a mesh
	HardBlocked       bool                 `json:"hard_blocked,omitempty"`
	// InUse is set when the system itself uses this radio (for example the
	// Wi-Fi a headless Pi is reached through). Trellis only takes such a
	// radio when the node config pins a role to it explicitly, so it never
	// cuts off the operator (architecture §3, principle 7).
	InUse      string              `json:"in_use,omitempty"`
	Interfaces []nl80211.Interface `json:"-"`
	Wiphy      *nl80211.Wiphy      `json:"-"`
}

// PermAddrString is the permanent MAC for JSON and display.
func (r *Radio) PermAddrString() string {
	if r.PermAddr == nil {
		return ""
	}
	return r.PermAddr.String()
}

// Describe is a one-line human description.
func (r *Radio) Describe() string {
	name := r.Label
	if name == "" && r.HWDB != nil {
		name = r.HWDB.Chipset
	}
	if name == "" {
		name = r.Device.Driver
	}
	return fmt.Sprintf("%s (%s, %s, %s)", name, r.Phy, r.Device.Port, r.Fingerprint)
}

// HasBand reports whether the radio has channels in the named band.
func (r *Radio) HasBand(band string) bool {
	for _, b := range r.Caps.Bands {
		if b == band {
			return true
		}
	}
	return false
}

// PowerOn returns the best-known maximum power on a band.
func (r *Radio) PowerOn(band string) (PowerFact, bool) {
	p, ok := r.Power[band]
	return p, ok
}

// Inventory lists the radios present, with fingerprints, capabilities and
// per-radio settings from the node config applied.
func Inventory(nl *nl80211.Client, sysRoot string, db *hwdb.DB, cfg config.RadiosConfig) ([]*Radio, error) {
	wiphys, err := nl.Wiphys()
	if err != nil {
		return nil, err
	}
	ifaces, err := nl.Interfaces()
	if err != nil {
		return nil, err
	}
	radios := make([]*Radio, 0, len(wiphys))
	for _, w := range wiphys {
		r := &Radio{
			Phy:      w.Name,
			PhyIndex: w.Index,
			PermAddr: w.PermAddr,
			Wiphy:    w,
			Power:    map[string]PowerFact{},
		}
		if r.Phy == "" {
			r.Phy = fmt.Sprintf("phy%d", w.Index)
		}
		dev, err := sysfs.PhyDevice(sysRoot, r.Phy)
		if err == nil {
			r.Device = dev
		} else {
			r.Device = sysfs.Device{Bus: sysfs.BusUnknown, Port: "unknown:" + r.Phy}
		}
		r.HardBlocked = sysfs.RFKillHardBlocked(sysRoot, r.Phy)
		for _, in := range ifaces {
			if in.Wiphy == w.Index {
				r.Interfaces = append(r.Interfaces, in)
			}
		}
		r.InUse = DetectInUse(r.Interfaces, LinkAddress)
		r.Caps = Caps{
			MeshPoint: w.Supports(nl80211.NL80211_IFTYPE_MESH_POINT),
			AP:        w.Supports(nl80211.NL80211_IFTYPE_AP),
			MeshAndAP: w.AllowsTogether(nl80211.NL80211_IFTYPE_MESH_POINT, nl80211.NL80211_IFTYPE_AP),
			Bands:     w.BandNames(),
			Streams:   w.SpatialStreams(),
		}
		if db != nil {
			r.HWDB = db.Lookup(r.Device.Bus, r.Device.DeviceID(), r.Device.Driver)
		}
		if r.HWDB != nil && r.HWDB.MeshPoint == hwdb.Broken {
			r.Broken = "hardware database marks mesh point as broken for " + r.HWDB.Chipset
		}
		// Power facts: driver first, database over it.
		for _, b := range r.Caps.Bands {
			band, _ := nl80211.ParseBand(b)
			if mbm, ok := w.MaxTXPowerMBm(band); ok {
				r.Power[b] = PowerFact{DBm: float64(mbm) / 100, Source: PowerDriver}
			}
		}
		if r.HWDB != nil {
			src := PowerDatabase
			if r.HWDB.PowerSource == hwdb.SourceMeasured {
				src = PowerMeasured
			}
			for b, dbm := range r.HWDB.MaxPowerDBm {
				r.Power[b] = PowerFact{DBm: dbm, Source: src}
			}
		}
		radios = append(radios, r)
	}
	AssignFingerprints(radios)
	applyDeviceRules(radios, cfg.Devices)
	sort.Slice(radios, func(i, j int) bool { return radios[i].Fingerprint < radios[j].Fingerprint })
	return radios, nil
}

func applyDeviceRules(radios []*Radio, rules []config.DeviceRule) {
	for _, r := range radios {
		for _, rule := range rules {
			if rule.Match.Fingerprint != "" && rule.Match.Fingerprint != r.Fingerprint {
				continue
			}
			if rule.Match.Port != "" && rule.Match.Port != r.Device.Port {
				continue
			}
			if rule.Ignore {
				r.Ignored = true
			}
			if rule.Label != "" {
				r.Label = rule.Label
			}
			if rule.TXPowerDBm != nil {
				v := *rule.TXPowerDBm
				r.TXPowerCapDBm = &v
			}
			if rule.MaxPowerDBm != nil {
				for _, b := range r.Caps.Bands {
					r.Power[b] = PowerFact{DBm: float64(*rule.MaxPowerDBm), Source: PowerOverride}
				}
			}
		}
	}
}

// Fingerprints -----------------------------------------------------------------

// MACUsable reports whether a permanent MAC can identify a radio on its own:
// present, unicast, globally administered and not all zeros.
func MACUsable(mac net.HardwareAddr) bool {
	if len(mac) != 6 {
		return false
	}
	zero := true
	for _, b := range mac {
		if b != 0 {
			zero = false
		}
	}
	if zero {
		return false
	}
	if mac[0]&0x01 != 0 { // multicast
		return false
	}
	if mac[0]&0x02 != 0 { // locally administered: random or software-set
		return false
	}
	return true
}

func serialUsable(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) < 4 {
		return false
	}
	switch strings.ToLower(s) {
	case "0000000000", "00000000", "000000000000", "0123456789", "123456", "12345678", "1234567890", "serial", "none":
		return false
	}
	allSame := true
	for i := 1; i < len(s); i++ {
		if s[i] != s[0] {
			allSame = false
			break
		}
	}
	return !allSame
}

// AssignFingerprints sets Fingerprint and FingerprintSource on every radio,
// using the first source that is reliable and unique among the radios given.
func AssignFingerprints(radios []*Radio) {
	count := func(key func(*Radio) string) map[string]int {
		m := map[string]int{}
		for _, r := range radios {
			if k := key(r); k != "" {
				m[k]++
			}
		}
		return m
	}
	macKey := func(r *Radio) string {
		if MACUsable(r.PermAddr) {
			return strings.ToLower(r.PermAddr.String())
		}
		return ""
	}
	serialKey := func(r *Radio) string {
		if r.Device.Bus == sysfs.BusUSB && serialUsable(r.Device.Serial) {
			return r.Device.DeviceID() + "/" + strings.TrimSpace(r.Device.Serial)
		}
		return ""
	}
	portKey := func(r *Radio) string {
		if r.Device.Port == "" {
			return ""
		}
		if id := r.Device.DeviceID(); id != "" {
			return r.Device.Port + "/" + id
		}
		return r.Device.Port
	}
	macs, serials, ports := count(macKey), count(serialKey), count(portKey)
	for _, r := range radios {
		switch {
		case macKey(r) != "" && macs[macKey(r)] == 1:
			r.Fingerprint, r.FingerprintSource = "mac:"+macKey(r), SourcePermanentMAC
		case serialKey(r) != "" && serials[serialKey(r)] == 1:
			r.Fingerprint, r.FingerprintSource = "usb-serial:"+serialKey(r), SourceUSBSerial
		case r.Device.Bus == sysfs.BusPCI && r.Device.Port != "":
			r.Fingerprint, r.FingerprintSource = r.Device.Port, SourcePCIAddress
		case portKey(r) != "" && ports[portKey(r)] == 1:
			r.Fingerprint, r.FingerprintSource = "port:"+portKey(r), SourcePortAndID
		default:
			r.Fingerprint, r.FingerprintSource = "path:"+r.Device.Path, SourcePath
		}
	}
	disambiguate(radios)
}

// disambiguate makes fingerprints unique when one device carries several
// radios. A dual-band-concurrent card (MT7916, MT7915) shows up as two
// wiphys on one PCI address, and its second radio's MAC is usually derived
// and locally administered, so every source above gives both the same
// fingerprint. What tells them apart, and stays the same across reboots, is
// the bands each serves; phy numbering does not (it follows probe order).
// Within a group that still collides, the order of phy indexes on that
// device decides, which the kernel keeps stable for one card. Unique
// fingerprints are left exactly as they were.
func disambiguate(radios []*Radio) {
	groups := map[string][]*Radio{}
	for _, r := range radios {
		groups[r.Fingerprint] = append(groups[r.Fingerprint], r)
	}
	for fp, g := range groups {
		if len(g) < 2 {
			continue
		}
		byBands := map[string][]*Radio{}
		for _, r := range g {
			bands := append([]string{}, r.Caps.Bands...)
			sort.Strings(bands)
			key := strings.Join(bands, "+")
			if key == "" {
				key = "nobands"
			}
			byBands[key] = append(byBands[key], r)
		}
		for key, rs := range byBands {
			sort.Slice(rs, func(i, j int) bool { return rs[i].PhyIndex < rs[j].PhyIndex })
			for i, r := range rs {
				r.Fingerprint = fp + "#" + key
				if len(rs) > 1 {
					r.Fingerprint += fmt.Sprintf(".%d", i)
				}
			}
		}
	}
}

// Find returns the radio with the fingerprint, or nil.
func Find(radios []*Radio, fingerprint string) *Radio {
	for _, r := range radios {
		if r.Fingerprint == fingerprint {
			return r
		}
	}
	return nil
}

// FindByPort returns the radio in the port, or nil.
func FindByPort(radios []*Radio, port string) *Radio {
	for _, r := range radios {
		if r.Device.Port == port {
			return r
		}
	}
	return nil
}

// DetectInUse reports whether the system is using a radio through an
// interface Trellis did not create: a station that is connected (has an
// SSID or a routable address), or an access point someone else runs.
// addr returns a routable address of an interface, if it has one.
func DetectInUse(ifaces []nl80211.Interface, addr func(ifindex int) string) string {
	for _, in := range ifaces {
		if in.Ifindex == 0 || isTrellisName(in.Name) {
			continue
		}
		switch in.Type {
		case nl80211.NL80211_IFTYPE_STATION:
			if in.SSID != "" {
				return fmt.Sprintf("%s is connected to Wi-Fi network %q", in.Name, in.SSID)
			}
			if a := addr(in.Ifindex); a != "" {
				return fmt.Sprintf("%s has address %s", in.Name, a)
			}
		case nl80211.NL80211_IFTYPE_AP, nl80211.NL80211_IFTYPE_ADHOC, nl80211.NL80211_IFTYPE_MESH_POINT:
			if a := addr(in.Ifindex); a != "" || in.SSID != "" {
				return fmt.Sprintf("%s is running as %s", in.Name, in.TypeName())
			}
		}
	}
	return ""
}

func isTrellisName(name string) bool {
	return name == "mesh0" || name == "mesh1" || name == "ap0"
}

// LinkAddress returns the first global IPv4 or IPv6 address of an
// interface, or "".
func LinkAddress(ifindex int) string {
	l, err := netlink.LinkByIndex(ifindex)
	if err != nil {
		return ""
	}
	addrs, err := netlink.AddrList(l, netlink.FAMILY_ALL)
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		if a.IP.IsGlobalUnicast() {
			return a.IP.String()
		}
	}
	return ""
}
