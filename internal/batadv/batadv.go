// Package batadv drives batman-adv through its generic netlink family.
//
// Nothing here parses batctl output. Every value comes from the kernel as a
// typed attribute, with the attribute numbers generated from the kernel's
// own uapi header (consts_gen.go).
package batadv

import (
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/mdlayher/genetlink"
	"github.com/mdlayher/netlink"

	"meridian/trellis/internal/gnl"
)

// FamilyName is batman-adv's generic netlink family.
const FamilyName = "batadv"

// ErrNotLoaded is returned when the batman_adv module is not loaded.
var ErrNotLoaded = errors.New("batman_adv module is not loaded")

// Gateway modes, as BATADV_ATTR_GW_MODE values.
const (
	GWModeOff    uint8 = BATADV_GW_MODE_OFF
	GWModeClient uint8 = BATADV_GW_MODE_CLIENT
	GWModeServer uint8 = BATADV_GW_MODE_SERVER
)

// Client is a connection to the batadv generic netlink family. It
// survives failed requests: see package gnl.
type Client struct {
	c *gnl.Conn
}

// Dial opens a connection to the batadv family.
func Dial() (*Client, error) {
	c, err := gnl.Dial(FamilyName)
	if err != nil {
		if errors.Is(err, gnl.ErrFamilyNotFound) {
			return nil, ErrNotLoaded
		}
		return nil, err
	}
	return &Client{c: c}, nil
}

// Close closes the connection.
func (c *Client) Close() error { return c.c.Close() }

func (c *Client) execute(cmd uint8, flags netlink.HeaderFlags, encode func(*netlink.AttributeEncoder)) ([]genetlink.Message, error) {
	ae := netlink.NewAttributeEncoder()
	if encode != nil {
		encode(ae)
	}
	data, err := ae.Encode()
	if err != nil {
		return nil, fmt.Errorf("encode attributes: %w", err)
	}
	return c.c.Execute(cmd, 1, flags, data)
}

func (c *Client) dump(cmd uint8, meshIfindex int, each func(*netlink.AttributeDecoder) error) error {
	msgs, err := c.execute(cmd, netlink.Dump, func(ae *netlink.AttributeEncoder) {
		ae.Uint32(BATADV_ATTR_MESH_IFINDEX, uint32(meshIfindex))
	})
	if err != nil {
		return err
	}
	for _, m := range msgs {
		ad, err := netlink.NewAttributeDecoder(m.Data)
		if err != nil {
			return err
		}
		if err := each(ad); err != nil {
			return err
		}
		if err := ad.Err(); err != nil {
			return err
		}
	}
	return nil
}

// MeshInfo is what GET_MESH returns for one bat0-style interface.
type MeshInfo struct {
	Ifindex  int
	Name     string
	Address  net.HardwareAddr
	Version  string // module version as batman-adv reports it
	AlgoName string
	Settings Settings
	// Own multicast flags: batman-adv sends them with the mesh info, not
	// in the multicast flags dump (which lists only originators).
	McastFlags     *uint32 // BATADV_MCAST_* (what this node asks for)
	McastFlagsPriv *uint32 // BATADV_MCAST_FLAGS_* (bridged, querier state)
}

// Settings are the per-mesh tunables. A nil field means "not present" when
// reading, and "leave unchanged" when writing.
type Settings struct {
	AggregatedOGMs      *bool
	APIsolation         *bool
	IsolationMark       *uint32
	IsolationMask       *uint32
	Bonding             *bool
	BridgeLoopAvoidance *bool
	DistributedARPTable *bool
	Fragmentation       *bool
	GWBandwidthDown     *uint32 // in 100 kbit/s units, as batman-adv stores it
	GWBandwidthUp       *uint32
	GWMode              *uint8
	GWSelClass          *uint32
	HopPenalty          *uint8
	LogLevel            *uint32
	MulticastForceflood *bool // false = multicast optimisations on
	MulticastFanout     *uint32
	NetworkCoding       *bool
	OrigIntervalMS      *uint32
}

func boolp(v uint8) *bool   { b := v != 0; return &b }
func u32p(v uint32) *uint32 { return &v }
func u8p(v uint8) *uint8    { return &v }
func b2u8(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}

// GetMesh reads the mesh interface's identity and settings.
func (c *Client) GetMesh(ifindex int) (*MeshInfo, error) {
	msgs, err := c.execute(BATADV_CMD_GET_MESH, 0, func(ae *netlink.AttributeEncoder) {
		ae.Uint32(BATADV_ATTR_MESH_IFINDEX, uint32(ifindex))
	})
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		return nil, errors.New("empty GET_MESH reply")
	}
	ad, err := netlink.NewAttributeDecoder(msgs[0].Data)
	if err != nil {
		return nil, err
	}
	mi := &MeshInfo{}
	for ad.Next() {
		switch ad.Type() {
		case BATADV_ATTR_MESH_IFINDEX:
			mi.Ifindex = int(ad.Uint32())
		case BATADV_ATTR_MESH_IFNAME:
			mi.Name = ad.String()
		case BATADV_ATTR_MESH_ADDRESS:
			mi.Address = net.HardwareAddr(append([]byte(nil), ad.Bytes()...))
		case BATADV_ATTR_VERSION:
			mi.Version = ad.String()
		case BATADV_ATTR_MCAST_FLAGS:
			mi.McastFlags = u32p(ad.Uint32())
		case BATADV_ATTR_MCAST_FLAGS_PRIV:
			mi.McastFlagsPriv = u32p(ad.Uint32())
		case BATADV_ATTR_ALGO_NAME:
			mi.AlgoName = ad.String()
		case BATADV_ATTR_AGGREGATED_OGMS_ENABLED:
			mi.Settings.AggregatedOGMs = boolp(ad.Uint8())
		case BATADV_ATTR_AP_ISOLATION_ENABLED:
			mi.Settings.APIsolation = boolp(ad.Uint8())
		case BATADV_ATTR_ISOLATION_MARK:
			mi.Settings.IsolationMark = u32p(ad.Uint32())
		case BATADV_ATTR_ISOLATION_MASK:
			mi.Settings.IsolationMask = u32p(ad.Uint32())
		case BATADV_ATTR_BONDING_ENABLED:
			mi.Settings.Bonding = boolp(ad.Uint8())
		case BATADV_ATTR_BRIDGE_LOOP_AVOIDANCE_ENABLED:
			mi.Settings.BridgeLoopAvoidance = boolp(ad.Uint8())
		case BATADV_ATTR_DISTRIBUTED_ARP_TABLE_ENABLED:
			mi.Settings.DistributedARPTable = boolp(ad.Uint8())
		case BATADV_ATTR_FRAGMENTATION_ENABLED:
			mi.Settings.Fragmentation = boolp(ad.Uint8())
		case BATADV_ATTR_GW_BANDWIDTH_DOWN:
			mi.Settings.GWBandwidthDown = u32p(ad.Uint32())
		case BATADV_ATTR_GW_BANDWIDTH_UP:
			mi.Settings.GWBandwidthUp = u32p(ad.Uint32())
		case BATADV_ATTR_GW_MODE:
			mi.Settings.GWMode = u8p(ad.Uint8())
		case BATADV_ATTR_GW_SEL_CLASS:
			mi.Settings.GWSelClass = u32p(ad.Uint32())
		case BATADV_ATTR_HOP_PENALTY:
			mi.Settings.HopPenalty = u8p(ad.Uint8())
		case BATADV_ATTR_LOG_LEVEL:
			mi.Settings.LogLevel = u32p(ad.Uint32())
		case BATADV_ATTR_MULTICAST_FORCEFLOOD_ENABLED:
			mi.Settings.MulticastForceflood = boolp(ad.Uint8())
		case BATADV_ATTR_MULTICAST_FANOUT:
			mi.Settings.MulticastFanout = u32p(ad.Uint32())
		case BATADV_ATTR_NETWORK_CODING_ENABLED:
			mi.Settings.NetworkCoding = boolp(ad.Uint8())
		case BATADV_ATTR_ORIG_INTERVAL:
			mi.Settings.OrigIntervalMS = u32p(ad.Uint32())
		}
	}
	if err := ad.Err(); err != nil {
		return nil, err
	}
	return mi, nil
}

// SetMesh applies every non-nil field of s to the mesh interface.
func (c *Client) SetMesh(ifindex int, s Settings) error {
	_, err := c.execute(BATADV_CMD_SET_MESH, netlink.Acknowledge, func(ae *netlink.AttributeEncoder) {
		ae.Uint32(BATADV_ATTR_MESH_IFINDEX, uint32(ifindex))
		if s.AggregatedOGMs != nil {
			ae.Uint8(BATADV_ATTR_AGGREGATED_OGMS_ENABLED, b2u8(*s.AggregatedOGMs))
		}
		if s.APIsolation != nil {
			ae.Uint8(BATADV_ATTR_AP_ISOLATION_ENABLED, b2u8(*s.APIsolation))
		}
		if s.IsolationMark != nil {
			ae.Uint32(BATADV_ATTR_ISOLATION_MARK, *s.IsolationMark)
		}
		if s.IsolationMask != nil {
			ae.Uint32(BATADV_ATTR_ISOLATION_MASK, *s.IsolationMask)
		}
		if s.Bonding != nil {
			ae.Uint8(BATADV_ATTR_BONDING_ENABLED, b2u8(*s.Bonding))
		}
		if s.BridgeLoopAvoidance != nil {
			ae.Uint8(BATADV_ATTR_BRIDGE_LOOP_AVOIDANCE_ENABLED, b2u8(*s.BridgeLoopAvoidance))
		}
		if s.DistributedARPTable != nil {
			ae.Uint8(BATADV_ATTR_DISTRIBUTED_ARP_TABLE_ENABLED, b2u8(*s.DistributedARPTable))
		}
		if s.Fragmentation != nil {
			ae.Uint8(BATADV_ATTR_FRAGMENTATION_ENABLED, b2u8(*s.Fragmentation))
		}
		if s.GWBandwidthDown != nil {
			ae.Uint32(BATADV_ATTR_GW_BANDWIDTH_DOWN, *s.GWBandwidthDown)
		}
		if s.GWBandwidthUp != nil {
			ae.Uint32(BATADV_ATTR_GW_BANDWIDTH_UP, *s.GWBandwidthUp)
		}
		if s.GWMode != nil {
			ae.Uint8(BATADV_ATTR_GW_MODE, *s.GWMode)
		}
		if s.GWSelClass != nil {
			ae.Uint32(BATADV_ATTR_GW_SEL_CLASS, *s.GWSelClass)
		}
		if s.HopPenalty != nil {
			ae.Uint8(BATADV_ATTR_HOP_PENALTY, *s.HopPenalty)
		}
		if s.LogLevel != nil {
			ae.Uint32(BATADV_ATTR_LOG_LEVEL, *s.LogLevel)
		}
		if s.MulticastForceflood != nil {
			ae.Uint8(BATADV_ATTR_MULTICAST_FORCEFLOOD_ENABLED, b2u8(*s.MulticastForceflood))
		}
		if s.MulticastFanout != nil {
			ae.Uint32(BATADV_ATTR_MULTICAST_FANOUT, *s.MulticastFanout)
		}
		if s.NetworkCoding != nil {
			ae.Uint8(BATADV_ATTR_NETWORK_CODING_ENABLED, b2u8(*s.NetworkCoding))
		}
		if s.OrigIntervalMS != nil {
			ae.Uint32(BATADV_ATTR_ORIG_INTERVAL, *s.OrigIntervalMS)
		}
	})
	return err
}

// Diff returns the settings in want that differ from have. A setting absent
// from have is not supported by the loaded module (network coding, for
// example, is not built by default) and is skipped, so a reconciler does not
// re-apply it on every pass. Use Unsupported to report those.
func Diff(have, want Settings) Settings {
	var d Settings
	if want.AggregatedOGMs != nil && have.AggregatedOGMs != nil && *have.AggregatedOGMs != *want.AggregatedOGMs {
		d.AggregatedOGMs = want.AggregatedOGMs
	}
	if want.APIsolation != nil && have.APIsolation != nil && *have.APIsolation != *want.APIsolation {
		d.APIsolation = want.APIsolation
	}
	if want.IsolationMark != nil && have.IsolationMark != nil && *have.IsolationMark != *want.IsolationMark {
		d.IsolationMark = want.IsolationMark
	}
	if want.IsolationMask != nil && have.IsolationMask != nil && *have.IsolationMask != *want.IsolationMask {
		d.IsolationMask = want.IsolationMask
	}
	if want.Bonding != nil && have.Bonding != nil && *have.Bonding != *want.Bonding {
		d.Bonding = want.Bonding
	}
	if want.BridgeLoopAvoidance != nil && have.BridgeLoopAvoidance != nil && *have.BridgeLoopAvoidance != *want.BridgeLoopAvoidance {
		d.BridgeLoopAvoidance = want.BridgeLoopAvoidance
	}
	if want.DistributedARPTable != nil && have.DistributedARPTable != nil && *have.DistributedARPTable != *want.DistributedARPTable {
		d.DistributedARPTable = want.DistributedARPTable
	}
	if want.Fragmentation != nil && have.Fragmentation != nil && *have.Fragmentation != *want.Fragmentation {
		d.Fragmentation = want.Fragmentation
	}
	if want.GWBandwidthDown != nil && have.GWBandwidthDown != nil && *have.GWBandwidthDown != *want.GWBandwidthDown {
		d.GWBandwidthDown = want.GWBandwidthDown
	}
	if want.GWBandwidthUp != nil && have.GWBandwidthUp != nil && *have.GWBandwidthUp != *want.GWBandwidthUp {
		d.GWBandwidthUp = want.GWBandwidthUp
	}
	if want.GWMode != nil && have.GWMode != nil && *have.GWMode != *want.GWMode {
		d.GWMode = want.GWMode
	}
	if want.GWSelClass != nil && have.GWSelClass != nil && *have.GWSelClass != *want.GWSelClass {
		d.GWSelClass = want.GWSelClass
	}
	if want.HopPenalty != nil && have.HopPenalty != nil && *have.HopPenalty != *want.HopPenalty {
		d.HopPenalty = want.HopPenalty
	}
	if want.LogLevel != nil && have.LogLevel != nil && *have.LogLevel != *want.LogLevel {
		d.LogLevel = want.LogLevel
	}
	if want.MulticastForceflood != nil && have.MulticastForceflood != nil && *have.MulticastForceflood != *want.MulticastForceflood {
		d.MulticastForceflood = want.MulticastForceflood
	}
	if want.MulticastFanout != nil && have.MulticastFanout != nil && *have.MulticastFanout != *want.MulticastFanout {
		d.MulticastFanout = want.MulticastFanout
	}
	if want.NetworkCoding != nil && have.NetworkCoding != nil && *have.NetworkCoding != *want.NetworkCoding {
		d.NetworkCoding = want.NetworkCoding
	}
	if want.OrigIntervalMS != nil && have.OrigIntervalMS != nil && *have.OrigIntervalMS != *want.OrigIntervalMS {
		d.OrigIntervalMS = want.OrigIntervalMS
	}
	return d
}

// IsEmpty reports whether no field is set.
func (s Settings) IsEmpty() bool {
	return s.AggregatedOGMs == nil && s.APIsolation == nil && s.IsolationMark == nil && s.IsolationMask == nil &&
		s.Bonding == nil && s.BridgeLoopAvoidance == nil && s.DistributedARPTable == nil && s.Fragmentation == nil &&
		s.GWBandwidthDown == nil && s.GWBandwidthUp == nil && s.GWMode == nil && s.GWSelClass == nil &&
		s.HopPenalty == nil && s.LogLevel == nil && s.MulticastForceflood == nil && s.MulticastFanout == nil &&
		s.NetworkCoding == nil && s.OrigIntervalMS == nil
}

// Names lists the fields set in s, for logging.
func (s Settings) Names() []string {
	var n []string
	add := func(ok bool, name string) {
		if ok {
			n = append(n, name)
		}
	}
	add(s.AggregatedOGMs != nil, "aggregated_ogms")
	add(s.APIsolation != nil, "ap_isolation")
	add(s.IsolationMark != nil, "isolation_mark")
	add(s.IsolationMask != nil, "isolation_mask")
	add(s.Bonding != nil, "bonding")
	add(s.BridgeLoopAvoidance != nil, "bridge_loop_avoidance")
	add(s.DistributedARPTable != nil, "distributed_arp_table")
	add(s.Fragmentation != nil, "fragmentation")
	add(s.GWBandwidthDown != nil, "gw_bandwidth_down")
	add(s.GWBandwidthUp != nil, "gw_bandwidth_up")
	add(s.GWMode != nil, "gw_mode")
	add(s.GWSelClass != nil, "gw_sel_class")
	add(s.HopPenalty != nil, "hop_penalty")
	add(s.LogLevel != nil, "log_level")
	add(s.MulticastForceflood != nil, "multicast_forceflood")
	add(s.MulticastFanout != nil, "multicast_fanout")
	add(s.NetworkCoding != nil, "network_coding")
	add(s.OrigIntervalMS != nil, "orig_interval")
	return n
}

// HardIf is one hard interface (slave) of a mesh interface.
type HardIf struct {
	Ifindex            int
	Name               string
	Address            net.HardwareAddr
	Active             bool
	ELPIntervalMS      uint32
	ThroughputOverride uint32 // 100 kbit/s units; 0 = measure
	HopPenalty         uint8
}

// HardIfs lists the hard interfaces attached to a mesh interface.
func (c *Client) HardIfs(meshIfindex int) ([]HardIf, error) {
	var out []HardIf
	err := c.dump(BATADV_CMD_GET_HARDIF, meshIfindex, func(ad *netlink.AttributeDecoder) error {
		var h HardIf
		for ad.Next() {
			switch ad.Type() {
			case BATADV_ATTR_HARD_IFINDEX:
				h.Ifindex = int(ad.Uint32())
			case BATADV_ATTR_HARD_IFNAME:
				h.Name = ad.String()
			case BATADV_ATTR_HARD_ADDRESS:
				h.Address = net.HardwareAddr(append([]byte(nil), ad.Bytes()...))
			case BATADV_ATTR_ACTIVE:
				h.Active = true
			case BATADV_ATTR_ELP_INTERVAL:
				h.ELPIntervalMS = ad.Uint32()
			case BATADV_ATTR_THROUGHPUT_OVERRIDE:
				h.ThroughputOverride = ad.Uint32()
			case BATADV_ATTR_HOP_PENALTY:
				h.HopPenalty = ad.Uint8()
			}
		}
		if h.Ifindex != 0 {
			out = append(out, h)
		}
		return nil
	})
	return out, err
}

// SetHardIf changes per-hard-interface settings. Nil means unchanged.
func (c *Client) SetHardIf(meshIfindex, hardIfindex int, elpIntervalMS, throughputOverride *uint32, hopPenalty *uint8) error {
	_, err := c.execute(BATADV_CMD_SET_HARDIF, netlink.Acknowledge, func(ae *netlink.AttributeEncoder) {
		ae.Uint32(BATADV_ATTR_MESH_IFINDEX, uint32(meshIfindex))
		ae.Uint32(BATADV_ATTR_HARD_IFINDEX, uint32(hardIfindex))
		if elpIntervalMS != nil {
			ae.Uint32(BATADV_ATTR_ELP_INTERVAL, *elpIntervalMS)
		}
		if throughputOverride != nil {
			ae.Uint32(BATADV_ATTR_THROUGHPUT_OVERRIDE, *throughputOverride)
		}
		if hopPenalty != nil {
			ae.Uint8(BATADV_ATTR_HOP_PENALTY, *hopPenalty)
		}
	})
	return err
}

// Originator is one row of the originator table: a known node, reached via
// a specific next hop on a specific hard interface. The row with Best set is
// the route in use.
type Originator struct {
	Address     net.HardwareAddr
	NextHop     net.HardwareAddr
	HardIfindex int
	LastSeen    time.Duration
	Throughput  Throughput // BATMAN_V
	TQ          uint8      // BATMAN_IV
	Best        bool
}

// Throughput is a BATMAN_V throughput estimate in kbit/s.
//
// batman-adv keeps throughput internally in 100 kbit/s units, but its
// netlink dumps are not consistent: neighbour and originator dumps multiply
// by 100 and export kbit/s (bat_v.c, batadv_v_neigh_dump_neigh and
// batadv_v_orig_dump_subentry), while the gateway dump exports the internal
// 100 kbit/s value. The decoders below normalise everything to kbit/s.
type Throughput uint32

// Kbps returns kbit/s.
func (t Throughput) Kbps() uint64 { return uint64(t) }

// Mbps returns Mbit/s.
func (t Throughput) Mbps() float64 { return float64(t) / 1000 }

// Originators dumps the originator table.
func (c *Client) Originators(meshIfindex int) ([]Originator, error) {
	var out []Originator
	err := c.dump(BATADV_CMD_GET_ORIGINATORS, meshIfindex, func(ad *netlink.AttributeDecoder) error {
		if o := decodeOriginator(ad); o.Address != nil {
			out = append(out, o)
		}
		return nil
	})
	return out, err
}

func decodeOriginator(ad *netlink.AttributeDecoder) Originator {
	var o Originator
	for ad.Next() {
		switch ad.Type() {
		case BATADV_ATTR_ORIG_ADDRESS:
			o.Address = net.HardwareAddr(append([]byte(nil), ad.Bytes()...))
		case BATADV_ATTR_NEIGH_ADDRESS:
			o.NextHop = net.HardwareAddr(append([]byte(nil), ad.Bytes()...))
		case BATADV_ATTR_HARD_IFINDEX:
			o.HardIfindex = int(ad.Uint32())
		case BATADV_ATTR_LAST_SEEN_MSECS:
			o.LastSeen = time.Duration(ad.Uint32()) * time.Millisecond
		case BATADV_ATTR_THROUGHPUT:
			o.Throughput = Throughput(ad.Uint32()) // already kbit/s
		case BATADV_ATTR_TQ:
			o.TQ = ad.Uint8()
		case BATADV_ATTR_FLAG_BEST:
			o.Best = true
		}
	}
	return o
}

// Neighbor is a directly reachable node on one hard interface.
type Neighbor struct {
	Address     net.HardwareAddr
	HardIfindex int
	LastSeen    time.Duration
	Throughput  Throughput
	TQ          uint8
}

// Neighbors dumps the single-hop neighbour table.
func (c *Client) Neighbors(meshIfindex int) ([]Neighbor, error) {
	var out []Neighbor
	err := c.dump(BATADV_CMD_GET_NEIGHBORS, meshIfindex, func(ad *netlink.AttributeDecoder) error {
		if n := decodeNeighbor(ad); n.Address != nil {
			out = append(out, n)
		}
		return nil
	})
	return out, err
}

func decodeNeighbor(ad *netlink.AttributeDecoder) Neighbor {
	var n Neighbor
	for ad.Next() {
		switch ad.Type() {
		case BATADV_ATTR_NEIGH_ADDRESS:
			n.Address = net.HardwareAddr(append([]byte(nil), ad.Bytes()...))
		case BATADV_ATTR_HARD_IFINDEX:
			n.HardIfindex = int(ad.Uint32())
		case BATADV_ATTR_LAST_SEEN_MSECS:
			n.LastSeen = time.Duration(ad.Uint32()) * time.Millisecond
		case BATADV_ATTR_THROUGHPUT:
			n.Throughput = Throughput(ad.Uint32()) // already kbit/s
		case BATADV_ATTR_TQ:
			n.TQ = ad.Uint8()
		}
	}
	return n
}

// Gateway is an entry in batman-adv's gateway list.
type Gateway struct {
	Address       net.HardwareAddr
	Router        net.HardwareAddr
	HardIfName    string
	BandwidthDown uint32 // 100 kbit/s units, as announced
	BandwidthUp   uint32 // 100 kbit/s units
	Throughput    Throughput
	TQ            uint8
	Best          bool
}

// Gateways dumps the gateway list.
func (c *Client) Gateways(meshIfindex int) ([]Gateway, error) {
	var out []Gateway
	err := c.dump(BATADV_CMD_GET_GATEWAYS, meshIfindex, func(ad *netlink.AttributeDecoder) error {
		if g := decodeGateway(ad); g.Address != nil {
			out = append(out, g)
		}
		return nil
	})
	return out, err
}

func decodeGateway(ad *netlink.AttributeDecoder) Gateway {
	var g Gateway
	for ad.Next() {
		switch ad.Type() {
		case BATADV_ATTR_ORIG_ADDRESS:
			g.Address = net.HardwareAddr(append([]byte(nil), ad.Bytes()...))
		case BATADV_ATTR_ROUTER:
			g.Router = net.HardwareAddr(append([]byte(nil), ad.Bytes()...))
		case BATADV_ATTR_HARD_IFNAME:
			g.HardIfName = ad.String()
		case BATADV_ATTR_BANDWIDTH_DOWN:
			g.BandwidthDown = ad.Uint32()
		case BATADV_ATTR_BANDWIDTH_UP:
			g.BandwidthUp = ad.Uint32()
		case BATADV_ATTR_THROUGHPUT:
			g.Throughput = Throughput(ad.Uint32() * 100) // exported in 100 kbit/s
		case BATADV_ATTR_TQ:
			g.TQ = ad.Uint8()
		case BATADV_ATTR_FLAG_BEST:
			g.Best = true
		}
	}
	return g
}

// Multicast flags a node announces (batman-adv packet.h, enum
// batadv_mcast_flags): what multicast it needs sent to it regardless of
// announced listeners.
const (
	McastWantAllUnsnoopables = 1 << 0 // link-local: always wanted by bridged nodes
	McastWantAllIPv4         = 1 << 1 // no usable IGMP querier: every IPv4 group
	McastWantAllIPv6         = 1 << 2 // no usable MLD querier: every IPv6 group
	McastWantNoRtr4          = 1 << 3 // no IPv4 multicast router behind it
	McastWantNoRtr6          = 1 << 4
	McastHaveMcPtypeCapa     = 1 << 5 // understands the multicast packet type
)

// McastFlags describes the multicast state of this node and its peers.
type McastFlags struct {
	Own         uint32 // this node's BATADV_MCAST_* flags
	OwnPriv     uint32 // BATADV_MCAST_FLAGS_* (bridged, querier presence)
	Originators []McastOriginator
}

// McastOriginator is one peer's multicast flags. HasFlags false means the
// peer predates multicast optimisations and wants all multicast traffic.
type McastOriginator struct {
	Address  net.HardwareAddr
	HasFlags bool
	Flags    uint32
}

// MulticastFlags dumps the multicast flag table.
func (c *Client) MulticastFlags(meshIfindex int) (*McastFlags, error) {
	out := &McastFlags{}
	err := c.dump(BATADV_CMD_GET_MCAST_FLAGS, meshIfindex, func(ad *netlink.AttributeDecoder) error {
		var mo McastOriginator
		var own, ownPriv *uint32
		for ad.Next() {
			switch ad.Type() {
			case BATADV_ATTR_ORIG_ADDRESS:
				mo.Address = net.HardwareAddr(append([]byte(nil), ad.Bytes()...))
			case BATADV_ATTR_MCAST_FLAGS:
				v := ad.Uint32()
				mo.Flags, mo.HasFlags = v, true
				own = &v
			case BATADV_ATTR_MCAST_FLAGS_PRIV:
				v := ad.Uint32()
				ownPriv = &v
			}
		}
		if mo.Address != nil {
			out.Originators = append(out.Originators, mo)
		} else {
			if own != nil {
				out.Own = *own
			}
			if ownPriv != nil {
				out.OwnPriv = *ownPriv
			}
		}
		return nil
	})
	if err != nil {
		return out, err
	}
	// This node's own flags come with the mesh info.
	if mi, err := c.GetMesh(meshIfindex); err == nil {
		if mi.McastFlags != nil {
			out.Own = *mi.McastFlags
		}
		if mi.McastFlagsPriv != nil {
			out.OwnPriv = *mi.McastFlagsPriv
		}
	}
	return out, nil
}

// RoutingAlgos lists the routing algorithms the loaded module offers.
func (c *Client) RoutingAlgos() ([]string, error) {
	msgs, err := c.execute(BATADV_CMD_GET_ROUTING_ALGOS, netlink.Dump, nil)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, m := range msgs {
		ad, err := netlink.NewAttributeDecoder(m.Data)
		if err != nil {
			return nil, err
		}
		for ad.Next() {
			if ad.Type() == BATADV_ATTR_ALGO_NAME {
				out = append(out, ad.String())
			}
		}
		if err := ad.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Unsupported lists settings requested in want that the module does not
// report in have.
func Unsupported(have, want Settings) []string {
	var miss Settings
	if want.AggregatedOGMs != nil && have.AggregatedOGMs == nil {
		miss.AggregatedOGMs = want.AggregatedOGMs
	}
	if want.APIsolation != nil && have.APIsolation == nil {
		miss.APIsolation = want.APIsolation
	}
	if want.Bonding != nil && have.Bonding == nil {
		miss.Bonding = want.Bonding
	}
	if want.BridgeLoopAvoidance != nil && have.BridgeLoopAvoidance == nil {
		miss.BridgeLoopAvoidance = want.BridgeLoopAvoidance
	}
	if want.DistributedARPTable != nil && have.DistributedARPTable == nil {
		miss.DistributedARPTable = want.DistributedARPTable
	}
	if want.Fragmentation != nil && have.Fragmentation == nil {
		miss.Fragmentation = want.Fragmentation
	}
	if want.GWMode != nil && have.GWMode == nil {
		miss.GWMode = want.GWMode
	}
	if want.MulticastForceflood != nil && have.MulticastForceflood == nil {
		miss.MulticastForceflood = want.MulticastForceflood
	}
	if want.NetworkCoding != nil && have.NetworkCoding == nil {
		miss.NetworkCoding = want.NetworkCoding
	}
	if want.OrigIntervalMS != nil && have.OrigIntervalMS == nil {
		miss.OrigIntervalMS = want.OrigIntervalMS
	}
	return miss.Names()
}
