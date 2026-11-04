// Package nl80211 is a focused client for the parts of nl80211 that Trellis
// needs: radio inventory (bands, channels, power, interface types and
// combinations), creating and deleting interfaces, the regulatory domain,
// transmit power and mesh peer statistics.
//
// Attribute and command numbers are generated from the kernel's uapi header
// (consts_gen.go) and used verbatim.
package nl80211

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sort"

	"github.com/mdlayher/genetlink"
	"github.com/mdlayher/netlink"

	"meridian/trellis/internal/gnl"
)

// FamilyName is the generic netlink family.
const FamilyName = "nl80211"

// ErrUnavailable is returned when the kernel has no nl80211 (no cfg80211).
var ErrUnavailable = errors.New("nl80211 is not available (cfg80211 not loaded)")

// Client is a connection to nl80211. It survives failed requests: see
// package gnl.
type Client struct {
	c *gnl.Conn
}

// Dial opens a connection to nl80211.
func Dial() (*Client, error) {
	c, err := gnl.Dial(FamilyName)
	if err != nil {
		if errors.Is(err, gnl.ErrFamilyNotFound) {
			return nil, ErrUnavailable
		}
		return nil, err
	}
	return &Client{c: c}, nil
}

// Close closes the connection.
func (c *Client) Close() error { return c.c.Close() }

// Resets reports how many times the socket was replaced after an error.
func (c *Client) Resets() int { return c.c.Resets }

// OnReset sets a callback for socket replacements (for logging).
func (c *Client) OnReset(fn func(error)) { c.c.OnReset = fn }

func (c *Client) execute(cmd uint8, flags netlink.HeaderFlags, encode func(*netlink.AttributeEncoder)) ([]genetlink.Message, error) {
	ae := netlink.NewAttributeEncoder()
	if encode != nil {
		encode(ae)
	}
	data, err := ae.Encode()
	if err != nil {
		return nil, fmt.Errorf("encode attributes: %w", err)
	}
	return c.c.Execute(cmd, 0, flags, data)
}

// Bands ----------------------------------------------------------------------

// Band is an nl80211_band value.
type Band uint32

// Band values (enum nl80211_band).
const (
	Band2GHz  Band = NL80211_BAND_2GHZ
	Band5GHz  Band = NL80211_BAND_5GHZ
	Band60GHz Band = NL80211_BAND_60GHZ
	Band6GHz  Band = NL80211_BAND_6GHZ
	BandS1GHz Band = NL80211_BAND_S1GHZ
	BandLC    Band = NL80211_BAND_LC
)

// String returns the short name used in configuration: 2g, 5g, 6g, 60g, s1g.
func (b Band) String() string {
	switch b {
	case Band2GHz:
		return "2g"
	case Band5GHz:
		return "5g"
	case Band60GHz:
		return "60g"
	case Band6GHz:
		return "6g"
	case BandS1GHz:
		return "s1g"
	case BandLC:
		return "lc"
	}
	return fmt.Sprintf("band%d", uint32(b))
}

// ParseBand maps a configuration band name to a Band.
func ParseBand(s string) (Band, bool) {
	switch s {
	case "2g":
		return Band2GHz, true
	case "5g":
		return Band5GHz, true
	case "6g":
		return Band6GHz, true
	case "60g":
		return Band60GHz, true
	case "s1g":
		return BandS1GHz, true
	}
	return 0, false
}

// BandOfFrequency classifies a centre frequency in MHz.
func BandOfFrequency(mhz uint32) Band {
	switch {
	case mhz < 1000:
		return BandS1GHz
	case mhz < 3000:
		return Band2GHz
	case mhz < 5925:
		return Band5GHz
	case mhz < 7200:
		return Band6GHz
	default:
		return Band60GHz
	}
}

// Frequency is one channel as the driver reports it under the active
// regulatory domain.
type Frequency struct {
	MHz           uint32
	Disabled      bool
	NoIR          bool // passive only: cannot start a mesh or AP here
	Radar         bool // DFS: CAC required before transmitting
	IndoorOnly    bool
	NoHT40Minus   bool
	NoHT40Plus    bool
	No80MHz       bool
	No160MHz      bool
	MaxTXPowerMBm int32 // 100ths of a dBm
	DFSState      uint32
}

// MaxTXPowerDBm converts the limit to dBm.
func (f Frequency) MaxTXPowerDBm() float64 { return float64(f.MaxTXPowerMBm) / 100 }

// Usable reports whether a mesh point or AP can be started on this channel.
func (f Frequency) Usable() bool { return !f.Disabled && !f.NoIR && !f.Radar }

// BandInfo is one band's capabilities.
type BandInfo struct {
	Band        Band
	Frequencies []Frequency
	HasHT       bool
	HTCapa      uint16
	HTMCSSet    []byte
	HasVHT      bool
	VHTCapa     uint32
	VHTMCSSet   []byte
}

// SpatialStreams derives the receive spatial stream count from the HT and
// VHT MCS sets; 0 if unknown.
func (b *BandInfo) SpatialStreams() int {
	n := 0
	if len(b.HTMCSSet) >= 4 {
		for i := 0; i < 4; i++ {
			if b.HTMCSSet[i] != 0 {
				n = i + 1
			}
		}
	}
	if len(b.VHTMCSSet) >= 2 {
		rx := binary.LittleEndian.Uint16(b.VHTMCSSet[0:2])
		v := 0
		for s := 0; s < 8; s++ {
			if (rx>>(2*s))&3 != 3 {
				v = s + 1
			} else {
				break
			}
		}
		if v > n {
			n = v
		}
	}
	return n
}

// Interface combinations -----------------------------------------------------

// IfaceLimit is one limit inside a combination: at most Max interfaces of Types.
type IfaceLimit struct {
	Max   uint32
	Types []uint32
}

// IfaceCombination is one allowed set of concurrent interfaces.
type IfaceCombination struct {
	Limits      []IfaceLimit
	MaxNum      uint32
	NumChannels uint32
}

func (l IfaceLimit) has(t uint32) bool {
	for _, x := range l.Types {
		if x == t {
			return true
		}
	}
	return false
}

// Allows reports whether the combination permits the given interface types
// at once on a single channel. Types may repeat for multiple interfaces.
func (ic IfaceCombination) Allows(types ...uint32) bool {
	if uint32(len(types)) > ic.MaxNum || ic.NumChannels < 1 {
		return false
	}
	used := make([]uint32, len(ic.Limits))
	var assign func(i int) bool
	assign = func(i int) bool {
		if i == len(types) {
			return true
		}
		for li, l := range ic.Limits {
			if l.has(types[i]) && used[li] < l.Max {
				used[li]++
				if assign(i + 1) {
					return true
				}
				used[li]--
			}
		}
		return false
	}
	return assign(0)
}

// Wiphy ------------------------------------------------------------------------

// Wiphy is one physical radio.
type Wiphy struct {
	Index            uint32
	Name             string
	PermAddr         net.HardwareAddr
	Bands            map[Band]*BandInfo
	SupportedIftypes map[uint32]bool
	Combinations     []IfaceCombination
	AntennaAvailTX   uint32
	AntennaAvailRX   uint32
	FeatureFlags     uint32
	SelfManagedReg   bool
}

// Supports reports whether the radio can create an interface of this type.
func (w *Wiphy) Supports(iftype uint32) bool { return w.SupportedIftypes[iftype] }

// AllowsTogether reports whether the radio can run these interface types at
// the same time on one channel. With no combinations advertised, only a
// single interface is assumed.
func (w *Wiphy) AllowsTogether(types ...uint32) bool {
	for _, t := range types {
		if !w.Supports(t) {
			return false
		}
	}
	if len(types) <= 1 {
		return true
	}
	for _, ic := range w.Combinations {
		if ic.Allows(types...) {
			return true
		}
	}
	return false
}

// HasBand reports whether the radio has any channel in the band.
func (w *Wiphy) HasBand(b Band) bool {
	bi, ok := w.Bands[b]
	return ok && len(bi.Frequencies) > 0
}

// BandNames lists the bands with channels, in enum order.
func (w *Wiphy) BandNames() []string {
	var bands []int
	for b, bi := range w.Bands {
		if len(bi.Frequencies) > 0 {
			bands = append(bands, int(b))
		}
	}
	sort.Ints(bands)
	out := make([]string, 0, len(bands))
	for _, b := range bands {
		out = append(out, Band(b).String())
	}
	return out
}

// Frequency looks up a channel by centre frequency.
func (w *Wiphy) Frequency(mhz uint32) *Frequency {
	for _, bi := range w.Bands {
		for i := range bi.Frequencies {
			if bi.Frequencies[i].MHz == mhz {
				return &bi.Frequencies[i]
			}
		}
	}
	return nil
}

// MaxTXPowerMBm returns the highest per-channel limit among usable channels
// of a band under the active regulatory domain, and whether any exists.
func (w *Wiphy) MaxTXPowerMBm(b Band) (int32, bool) {
	bi, ok := w.Bands[b]
	if !ok {
		return 0, false
	}
	var best int32
	found := false
	for _, f := range bi.Frequencies {
		if f.Disabled {
			continue
		}
		if !found || f.MaxTXPowerMBm > best {
			best, found = f.MaxTXPowerMBm, true
		}
	}
	return best, found
}

// SpatialStreams is the largest stream count across bands, with antenna
// count as a fallback when no MCS set is reported.
func (w *Wiphy) SpatialStreams() int {
	n := 0
	for _, bi := range w.Bands {
		if s := bi.SpatialStreams(); s > n {
			n = s
		}
	}
	if n == 0 && w.AntennaAvailRX != 0 {
		for v := w.AntennaAvailRX; v != 0; v &= v - 1 {
			n++
		}
	}
	return n
}

// Wiphys dumps every radio with full (split) capability information.
func (c *Client) Wiphys() ([]*Wiphy, error) {
	msgs, err := c.execute(NL80211_CMD_GET_WIPHY, netlink.Dump, func(ae *netlink.AttributeEncoder) {
		ae.Flag(NL80211_ATTR_SPLIT_WIPHY_DUMP, true)
	})
	if err != nil {
		return nil, fmt.Errorf("dump wiphys: %w", err)
	}
	byIndex := map[uint32]*Wiphy{}
	var order []uint32
	for _, m := range msgs {
		ad, err := netlink.NewAttributeDecoder(m.Data)
		if err != nil {
			return nil, err
		}
		// First pass: find the wiphy index so partial messages merge.
		idx, ok := peekU32(m.Data, NL80211_ATTR_WIPHY)
		if !ok {
			continue
		}
		w := byIndex[idx]
		if w == nil {
			w = &Wiphy{Index: idx, Bands: map[Band]*BandInfo{}, SupportedIftypes: map[uint32]bool{}}
			byIndex[idx] = w
			order = append(order, idx)
		}
		if err := w.decode(ad); err != nil {
			return nil, fmt.Errorf("decode wiphy %d: %w", idx, err)
		}
	}
	out := make([]*Wiphy, 0, len(order))
	for _, i := range order {
		out = append(out, byIndex[i])
	}
	return out, nil
}

// Wiphy returns a single radio by index.
func (c *Client) Wiphy(index uint32) (*Wiphy, error) {
	all, err := c.Wiphys()
	if err != nil {
		return nil, err
	}
	for _, w := range all {
		if w.Index == index {
			return w, nil
		}
	}
	return nil, fmt.Errorf("wiphy %d not found", index)
}

func peekU32(data []byte, typ uint16) (uint32, bool) {
	ad, err := netlink.NewAttributeDecoder(data)
	if err != nil {
		return 0, false
	}
	for ad.Next() {
		if ad.Type() == typ {
			return ad.Uint32(), ad.Err() == nil
		}
	}
	return 0, false
}

func (w *Wiphy) decode(ad *netlink.AttributeDecoder) error {
	for ad.Next() {
		switch ad.Type() {
		case NL80211_ATTR_WIPHY_NAME:
			w.Name = ad.String()
		case NL80211_ATTR_MAC:
			w.PermAddr = net.HardwareAddr(append([]byte(nil), ad.Bytes()...))
		case NL80211_ATTR_WIPHY_ANTENNA_AVAIL_TX:
			w.AntennaAvailTX = ad.Uint32()
		case NL80211_ATTR_WIPHY_ANTENNA_AVAIL_RX:
			w.AntennaAvailRX = ad.Uint32()
		case NL80211_ATTR_FEATURE_FLAGS:
			w.FeatureFlags = ad.Uint32()
		case NL80211_ATTR_WIPHY_SELF_MANAGED_REG:
			w.SelfManagedReg = true
		case NL80211_ATTR_SUPPORTED_IFTYPES:
			ad.Nested(func(nad *netlink.AttributeDecoder) error {
				for nad.Next() {
					w.SupportedIftypes[uint32(nad.Type())] = true
				}
				return nil
			})
		case NL80211_ATTR_WIPHY_BANDS:
			ad.Nested(func(nad *netlink.AttributeDecoder) error {
				for nad.Next() {
					b := Band(nad.Type())
					bi := w.Bands[b]
					if bi == nil {
						bi = &BandInfo{Band: b}
						w.Bands[b] = bi
					}
					nad.Nested(func(bad *netlink.AttributeDecoder) error {
						return bi.decode(bad)
					})
				}
				return nil
			})
		case NL80211_ATTR_INTERFACE_COMBINATIONS:
			ad.Nested(func(nad *netlink.AttributeDecoder) error {
				for nad.Next() {
					var ic IfaceCombination
					nad.Nested(func(cad *netlink.AttributeDecoder) error {
						for cad.Next() {
							switch cad.Type() {
							case NL80211_IFACE_COMB_MAXNUM:
								ic.MaxNum = cad.Uint32()
							case NL80211_IFACE_COMB_NUM_CHANNELS:
								ic.NumChannels = cad.Uint32()
							case NL80211_IFACE_COMB_LIMITS:
								cad.Nested(func(lad *netlink.AttributeDecoder) error {
									for lad.Next() {
										var l IfaceLimit
										lad.Nested(func(xad *netlink.AttributeDecoder) error {
											for xad.Next() {
												switch xad.Type() {
												case NL80211_IFACE_LIMIT_MAX:
													l.Max = xad.Uint32()
												case NL80211_IFACE_LIMIT_TYPES:
													xad.Nested(func(tad *netlink.AttributeDecoder) error {
														for tad.Next() {
															l.Types = append(l.Types, uint32(tad.Type()))
														}
														return nil
													})
												}
											}
											return nil
										})
										ic.Limits = append(ic.Limits, l)
									}
									return nil
								})
							}
						}
						return nil
					})
					w.Combinations = append(w.Combinations, ic)
				}
				return nil
			})
		}
	}
	return ad.Err()
}

func (b *BandInfo) decode(ad *netlink.AttributeDecoder) error {
	for ad.Next() {
		switch ad.Type() {
		case NL80211_BAND_ATTR_HT_MCS_SET:
			b.HTMCSSet = append([]byte(nil), ad.Bytes()...)
			b.HasHT = true
		case NL80211_BAND_ATTR_HT_CAPA:
			b.HTCapa = ad.Uint16()
			b.HasHT = true
		case NL80211_BAND_ATTR_VHT_MCS_SET:
			b.VHTMCSSet = append([]byte(nil), ad.Bytes()...)
			b.HasVHT = true
		case NL80211_BAND_ATTR_VHT_CAPA:
			b.VHTCapa = ad.Uint32()
			b.HasVHT = true
		case NL80211_BAND_ATTR_FREQS:
			ad.Nested(func(nad *netlink.AttributeDecoder) error {
				for nad.Next() {
					var f Frequency
					nad.Nested(func(fad *netlink.AttributeDecoder) error {
						for fad.Next() {
							switch fad.Type() {
							case NL80211_FREQUENCY_ATTR_FREQ:
								f.MHz = fad.Uint32()
							case NL80211_FREQUENCY_ATTR_DISABLED:
								f.Disabled = true
							case NL80211_FREQUENCY_ATTR_NO_IR:
								f.NoIR = true
							case NL80211_FREQUENCY_ATTR_RADAR:
								f.Radar = true
							case NL80211_FREQUENCY_ATTR_INDOOR_ONLY:
								f.IndoorOnly = true
							case NL80211_FREQUENCY_ATTR_NO_HT40_MINUS:
								f.NoHT40Minus = true
							case NL80211_FREQUENCY_ATTR_NO_HT40_PLUS:
								f.NoHT40Plus = true
							case NL80211_FREQUENCY_ATTR_NO_80MHZ:
								f.No80MHz = true
							case NL80211_FREQUENCY_ATTR_NO_160MHZ:
								f.No160MHz = true
							case NL80211_FREQUENCY_ATTR_MAX_TX_POWER:
								f.MaxTXPowerMBm = int32(fad.Uint32())
							case NL80211_FREQUENCY_ATTR_DFS_STATE:
								f.DFSState = fad.Uint32()
							}
						}
						return nil
					})
					if f.MHz != 0 {
						b.Frequencies = append(b.Frequencies, f)
					}
				}
				return nil
			})
		}
	}
	return ad.Err()
}

// Interfaces -------------------------------------------------------------------

// Interface is one virtual interface on a radio.
type Interface struct {
	Ifindex      int
	Name         string
	Wiphy        uint32
	Type         uint32
	MAC          net.HardwareAddr
	Wdev         uint64
	FreqMHz      uint32 // 0 when not operating
	ChannelWidth uint32 // nl80211_chan_width
	CenterFreq1  uint32
	TXPowerMBm   int32
	HasTXPower   bool
	SSID         string
}

// TypeName is the interface type as iw prints it.
func (i Interface) TypeName() string { return IftypeName(i.Type) }

func decodeInterface(data []byte) (*Interface, error) {
	ad, err := netlink.NewAttributeDecoder(data)
	if err != nil {
		return nil, err
	}
	var in Interface
	for ad.Next() {
		switch ad.Type() {
		case NL80211_ATTR_IFINDEX:
			in.Ifindex = int(ad.Uint32())
		case NL80211_ATTR_IFNAME:
			in.Name = ad.String()
		case NL80211_ATTR_WIPHY:
			in.Wiphy = ad.Uint32()
		case NL80211_ATTR_IFTYPE:
			in.Type = ad.Uint32()
		case NL80211_ATTR_MAC:
			in.MAC = net.HardwareAddr(append([]byte(nil), ad.Bytes()...))
		case NL80211_ATTR_WDEV:
			in.Wdev = ad.Uint64()
		case NL80211_ATTR_WIPHY_FREQ:
			in.FreqMHz = ad.Uint32()
		case NL80211_ATTR_CHANNEL_WIDTH:
			in.ChannelWidth = ad.Uint32()
		case NL80211_ATTR_CENTER_FREQ1:
			in.CenterFreq1 = ad.Uint32()
		case NL80211_ATTR_WIPHY_TX_POWER_LEVEL:
			in.TXPowerMBm = int32(ad.Uint32())
			in.HasTXPower = true
		case NL80211_ATTR_SSID:
			in.SSID = string(ad.Bytes())
		}
	}
	if err := ad.Err(); err != nil {
		return nil, err
	}
	return &in, nil
}

// Interfaces lists every nl80211 interface. Interfaces with no netdev
// (P2P-Device) have Ifindex 0.
func (c *Client) Interfaces() ([]Interface, error) {
	msgs, err := c.execute(NL80211_CMD_GET_INTERFACE, netlink.Dump, nil)
	if err != nil {
		return nil, fmt.Errorf("dump interfaces: %w", err)
	}
	out := make([]Interface, 0, len(msgs))
	for _, m := range msgs {
		in, err := decodeInterface(m.Data)
		if err != nil {
			return nil, err
		}
		out = append(out, *in)
	}
	return out, nil
}

// InterfaceByIndex reads one interface.
func (c *Client) InterfaceByIndex(ifindex int) (*Interface, error) {
	msgs, err := c.execute(NL80211_CMD_GET_INTERFACE, 0, func(ae *netlink.AttributeEncoder) {
		ae.Uint32(NL80211_ATTR_IFINDEX, uint32(ifindex))
	})
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("interface %d: empty reply", ifindex)
	}
	return decodeInterface(msgs[0].Data)
}

// NewInterface creates a virtual interface on a radio. mac may be nil.
func (c *Client) NewInterface(wiphy uint32, name string, iftype uint32, mac net.HardwareAddr) (*Interface, error) {
	msgs, err := c.execute(NL80211_CMD_NEW_INTERFACE, netlink.Acknowledge, func(ae *netlink.AttributeEncoder) {
		ae.Uint32(NL80211_ATTR_WIPHY, wiphy)
		ae.String(NL80211_ATTR_IFNAME, name)
		ae.Uint32(NL80211_ATTR_IFTYPE, iftype)
		if len(mac) == 6 {
			ae.Bytes(NL80211_ATTR_MAC, mac)
		}
	})
	if err != nil {
		return nil, fmt.Errorf("create %s interface %s on phy%d: %w", IftypeName(iftype), name, wiphy, err)
	}
	for _, m := range msgs {
		if m.Header.Command == NL80211_CMD_NEW_INTERFACE {
			return decodeInterface(m.Data)
		}
	}
	return &Interface{Name: name, Wiphy: wiphy, Type: iftype}, nil
}

// DelInterface deletes a virtual interface.
func (c *Client) DelInterface(ifindex int) error {
	_, err := c.execute(NL80211_CMD_DEL_INTERFACE, netlink.Acknowledge, func(ae *netlink.AttributeEncoder) {
		ae.Uint32(NL80211_ATTR_IFINDEX, uint32(ifindex))
	})
	if err != nil {
		return fmt.Errorf("delete interface %d: %w", ifindex, err)
	}
	return nil
}

// SetInterfaceType changes an interface's type. The interface must be down.
func (c *Client) SetInterfaceType(ifindex int, iftype uint32) error {
	_, err := c.execute(NL80211_CMD_SET_INTERFACE, netlink.Acknowledge, func(ae *netlink.AttributeEncoder) {
		ae.Uint32(NL80211_ATTR_IFINDEX, uint32(ifindex))
		ae.Uint32(NL80211_ATTR_IFTYPE, iftype)
	})
	return err
}

// Transmit power ---------------------------------------------------------------

// TX power settings (enum nl80211_tx_power_setting).
const (
	TXPowerAutomatic uint32 = NL80211_TX_POWER_AUTOMATIC
	TXPowerLimited   uint32 = NL80211_TX_POWER_LIMITED
	TXPowerFixed     uint32 = NL80211_TX_POWER_FIXED
)

// SetTXPower sets transmit power on an interface. mBm is ignored for
// TXPowerAutomatic.
func (c *Client) SetTXPower(ifindex int, setting uint32, mBm int32) error {
	_, err := c.execute(NL80211_CMD_SET_WIPHY, netlink.Acknowledge, func(ae *netlink.AttributeEncoder) {
		ae.Uint32(NL80211_ATTR_IFINDEX, uint32(ifindex))
		ae.Uint32(NL80211_ATTR_WIPHY_TX_POWER_SETTING, setting)
		if setting != TXPowerAutomatic {
			ae.Uint32(NL80211_ATTR_WIPHY_TX_POWER_LEVEL, uint32(mBm))
		}
	})
	if err != nil {
		return fmt.Errorf("set tx power on ifindex %d: %w", ifindex, err)
	}
	return nil
}

// Regulatory -------------------------------------------------------------------

// RegRule is one rule of a regulatory domain.
type RegRule struct {
	StartKHz        uint32
	EndKHz          uint32
	MaxBandwidthKHz uint32
	MaxEIRPMBm      uint32
	Flags           uint32
	DFSCACTimeMS    uint32
}

// RegDomain is the regulatory domain in force.
type RegDomain struct {
	Alpha2    string
	DFSRegion uint8
	Rules     []RegRule
}

func decodeReg(data []byte) (*RegDomain, error) {
	ad, err := netlink.NewAttributeDecoder(data)
	if err != nil {
		return nil, err
	}
	rd := &RegDomain{}
	for ad.Next() {
		switch ad.Type() {
		case NL80211_ATTR_REG_ALPHA2:
			rd.Alpha2 = ad.String()
		case NL80211_ATTR_DFS_REGION:
			rd.DFSRegion = ad.Uint8()
		case NL80211_ATTR_REG_RULES:
			ad.Nested(func(nad *netlink.AttributeDecoder) error {
				for nad.Next() {
					var r RegRule
					nad.Nested(func(rad *netlink.AttributeDecoder) error {
						for rad.Next() {
							switch rad.Type() {
							case NL80211_ATTR_REG_RULE_FLAGS:
								r.Flags = rad.Uint32()
							case NL80211_ATTR_FREQ_RANGE_START:
								r.StartKHz = rad.Uint32()
							case NL80211_ATTR_FREQ_RANGE_END:
								r.EndKHz = rad.Uint32()
							case NL80211_ATTR_FREQ_RANGE_MAX_BW:
								r.MaxBandwidthKHz = rad.Uint32()
							case NL80211_ATTR_POWER_RULE_MAX_EIRP:
								r.MaxEIRPMBm = rad.Uint32()
							case NL80211_ATTR_DFS_CAC_TIME:
								r.DFSCACTimeMS = rad.Uint32()
							}
						}
						return nil
					})
					rd.Rules = append(rd.Rules, r)
				}
				return nil
			})
		}
	}
	if err := ad.Err(); err != nil {
		return nil, err
	}
	return rd, nil
}

// GetReg reads the global regulatory domain.
func (c *Client) GetReg() (*RegDomain, error) {
	msgs, err := c.execute(NL80211_CMD_GET_REG, 0, nil)
	if err != nil {
		return nil, fmt.Errorf("get regulatory domain: %w", err)
	}
	if len(msgs) == 0 {
		return nil, errors.New("get regulatory domain: empty reply")
	}
	return decodeReg(msgs[0].Data)
}

// GetWiphyReg reads the regulatory domain of a self-managed radio.
func (c *Client) GetWiphyReg(wiphy uint32) (*RegDomain, error) {
	msgs, err := c.execute(NL80211_CMD_GET_REG, 0, func(ae *netlink.AttributeEncoder) {
		ae.Uint32(NL80211_ATTR_WIPHY, wiphy)
	})
	if err != nil {
		return nil, fmt.Errorf("get regulatory domain of phy%d: %w", wiphy, err)
	}
	if len(msgs) == 0 {
		return nil, errors.New("get regulatory domain: empty reply")
	}
	return decodeReg(msgs[0].Data)
}

// SetReg requests a regulatory domain by ISO alpha-2 code ("00" = world).
func (c *Client) SetReg(alpha2 string) error {
	if len(alpha2) != 2 {
		return fmt.Errorf("regulatory domain must be two letters, got %q", alpha2)
	}
	_, err := c.execute(NL80211_CMD_REQ_SET_REG, netlink.Acknowledge, func(ae *netlink.AttributeEncoder) {
		ae.String(NL80211_ATTR_REG_ALPHA2, alpha2)
		ae.Uint32(NL80211_ATTR_USER_REG_HINT_TYPE, NL80211_USER_REG_HINT_USER)
	})
	if err != nil {
		return fmt.Errorf("set regulatory domain %s: %w", alpha2, err)
	}
	return nil
}

// Stations -----------------------------------------------------------------------

// Station is a peer on an interface: a mesh peer, an AP client, or the AP.
type Station struct {
	MAC                    net.HardwareAddr
	SignalDBm              int8
	HasSignal              bool
	SignalAvgDBm           int8
	InactiveMS             uint32
	ConnectedSeconds       uint32
	TXBitrateKbps          uint32
	RXBitrateKbps          uint32
	TXPackets              uint32
	RXPackets              uint32
	TXFailed               uint32
	TXRetries              uint32
	RXBytes                uint64
	TXBytes                uint64
	ExpectedThroughputKbps uint32
	PlinkState             uint8
	HasPlinkState          bool
}

// Established reports whether a mesh peer link is fully established.
func (s Station) Established() bool {
	return !s.HasPlinkState || s.PlinkState == NL80211_PLINK_ESTAB
}

func decodeRate(ad *netlink.AttributeDecoder) uint32 {
	var kbps uint32
	ad.Nested(func(rad *netlink.AttributeDecoder) error {
		for rad.Next() {
			switch rad.Type() {
			case NL80211_RATE_INFO_BITRATE32:
				kbps = rad.Uint32() * 100
			case NL80211_RATE_INFO_BITRATE:
				if kbps == 0 {
					kbps = uint32(rad.Uint16()) * 100
				}
			}
		}
		return nil
	})
	return kbps
}

// Stations dumps the station table of an interface.
func (c *Client) Stations(ifindex int) ([]Station, error) {
	msgs, err := c.execute(NL80211_CMD_GET_STATION, netlink.Dump, func(ae *netlink.AttributeEncoder) {
		ae.Uint32(NL80211_ATTR_IFINDEX, uint32(ifindex))
	})
	if err != nil {
		return nil, fmt.Errorf("dump stations on ifindex %d: %w", ifindex, err)
	}
	var out []Station
	for _, m := range msgs {
		ad, err := netlink.NewAttributeDecoder(m.Data)
		if err != nil {
			return nil, err
		}
		var s Station
		for ad.Next() {
			switch ad.Type() {
			case NL80211_ATTR_MAC:
				s.MAC = net.HardwareAddr(append([]byte(nil), ad.Bytes()...))
			case NL80211_ATTR_STA_INFO:
				ad.Nested(func(sad *netlink.AttributeDecoder) error {
					for sad.Next() {
						switch sad.Type() {
						case NL80211_STA_INFO_INACTIVE_TIME:
							s.InactiveMS = sad.Uint32()
						case NL80211_STA_INFO_CONNECTED_TIME:
							s.ConnectedSeconds = sad.Uint32()
						case NL80211_STA_INFO_SIGNAL:
							s.SignalDBm, s.HasSignal = int8(sad.Uint8()), true
						case NL80211_STA_INFO_SIGNAL_AVG:
							s.SignalAvgDBm = int8(sad.Uint8())
						case NL80211_STA_INFO_TX_BITRATE:
							s.TXBitrateKbps = decodeRate(sad)
						case NL80211_STA_INFO_RX_BITRATE:
							s.RXBitrateKbps = decodeRate(sad)
						case NL80211_STA_INFO_TX_PACKETS:
							s.TXPackets = sad.Uint32()
						case NL80211_STA_INFO_RX_PACKETS:
							s.RXPackets = sad.Uint32()
						case NL80211_STA_INFO_TX_FAILED:
							s.TXFailed = sad.Uint32()
						case NL80211_STA_INFO_TX_RETRIES:
							s.TXRetries = sad.Uint32()
						case NL80211_STA_INFO_RX_BYTES64:
							s.RXBytes = sad.Uint64()
						case NL80211_STA_INFO_TX_BYTES64:
							s.TXBytes = sad.Uint64()
						case NL80211_STA_INFO_RX_BYTES:
							if s.RXBytes == 0 {
								s.RXBytes = uint64(sad.Uint32())
							}
						case NL80211_STA_INFO_TX_BYTES:
							if s.TXBytes == 0 {
								s.TXBytes = uint64(sad.Uint32())
							}
						case NL80211_STA_INFO_EXPECTED_THROUGHPUT:
							s.ExpectedThroughputKbps = sad.Uint32()
						case NL80211_STA_INFO_PLINK_STATE:
							s.PlinkState, s.HasPlinkState = sad.Uint8(), true
						}
					}
					return nil
				})
			}
		}
		if err := ad.Err(); err != nil {
			return nil, err
		}
		if s.MAC != nil {
			out = append(out, s)
		}
	}
	return out, nil
}

// Events -----------------------------------------------------------------------

// Event is a configuration change notification from nl80211.
type Event struct {
	Command uint8
	Wiphy   uint32
	Ifindex int
	Ifname  string
}

// EventConn receives nl80211 "config" multicast events: new or deleted
// radios and interfaces, interface type changes and regulatory changes.
type EventConn struct {
	c *genetlink.Conn
}

// DialEvents opens a connection subscribed to the config group.
func DialEvents() (*EventConn, error) {
	c, err := genetlink.Dial(nil)
	if err != nil {
		return nil, err
	}
	fam, err := c.GetFamily(FamilyName)
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	for _, g := range fam.Groups {
		if g.Name == "config" || g.Name == "regulatory" {
			if err := c.JoinGroup(g.ID); err != nil {
				_ = c.Close()
				return nil, fmt.Errorf("join nl80211 %s group: %w", g.Name, err)
			}
		}
	}
	return &EventConn{c: c}, nil
}

// Next blocks until events arrive. Close unblocks it with an error.
func (e *EventConn) Next() ([]Event, error) {
	msgs, _, err := e.c.Receive()
	if err != nil {
		return nil, err
	}
	out := make([]Event, 0, len(msgs))
	for _, m := range msgs {
		ev := Event{Command: m.Header.Command}
		ad, err := netlink.NewAttributeDecoder(m.Data)
		if err != nil {
			continue
		}
		for ad.Next() {
			switch ad.Type() {
			case NL80211_ATTR_WIPHY:
				ev.Wiphy = ad.Uint32()
			case NL80211_ATTR_IFINDEX:
				ev.Ifindex = int(ad.Uint32())
			case NL80211_ATTR_IFNAME:
				ev.Ifname = ad.String()
			}
		}
		out = append(out, ev)
	}
	return out, nil
}

// Close closes the event connection.
func (e *EventConn) Close() error { return e.c.Close() }

// Names ----------------------------------------------------------------------------

var iftypeNames = map[uint32]string{
	NL80211_IFTYPE_UNSPECIFIED: "unspecified",
	NL80211_IFTYPE_ADHOC:       "IBSS",
	NL80211_IFTYPE_STATION:     "managed",
	NL80211_IFTYPE_AP:          "AP",
	NL80211_IFTYPE_AP_VLAN:     "AP/VLAN",
	NL80211_IFTYPE_WDS:         "WDS",
	NL80211_IFTYPE_MONITOR:     "monitor",
	NL80211_IFTYPE_MESH_POINT:  "mesh point",
	NL80211_IFTYPE_P2P_CLIENT:  "P2P-client",
	NL80211_IFTYPE_P2P_GO:      "P2P-GO",
	NL80211_IFTYPE_P2P_DEVICE:  "P2P-device",
	NL80211_IFTYPE_OCB:         "outside context of a BSS",
	NL80211_IFTYPE_NAN:         "NAN",
}

// IftypeName names an interface type the way iw does.
func IftypeName(t uint32) string {
	if n, ok := iftypeNames[t]; ok {
		return n
	}
	return fmt.Sprintf("iftype%d", t)
}

var plinkNames = map[uint8]string{
	NL80211_PLINK_LISTEN:   "LISTEN",
	NL80211_PLINK_OPN_SNT:  "OPN_SNT",
	NL80211_PLINK_OPN_RCVD: "OPN_RCVD",
	NL80211_PLINK_CNF_RCVD: "CNF_RCVD",
	NL80211_PLINK_ESTAB:    "ESTAB",
	NL80211_PLINK_HOLDING:  "HOLDING",
	NL80211_PLINK_BLOCKED:  "BLOCKED",
}

// PlinkStateName names a mesh peer link state.
func PlinkStateName(s uint8) string {
	if n, ok := plinkNames[s]; ok {
		return n
	}
	return fmt.Sprintf("plink%d", s)
}
