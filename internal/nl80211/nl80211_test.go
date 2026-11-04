package nl80211

import (
	"testing"

	"github.com/mdlayher/netlink"
)

func TestIfaceCombinationAllows(t *testing.T) {
	// Typical mt76: #{ managed } <= 1, #{ AP, mesh point } <= 4 on one channel.
	ic := IfaceCombination{
		Limits: []IfaceLimit{
			{Max: 1, Types: []uint32{NL80211_IFTYPE_STATION}},
			{Max: 4, Types: []uint32{NL80211_IFTYPE_AP, NL80211_IFTYPE_MESH_POINT}},
		},
		MaxNum: 4, NumChannels: 1,
	}
	if !ic.Allows(NL80211_IFTYPE_MESH_POINT, NL80211_IFTYPE_AP) {
		t.Error("mesh+AP should be allowed")
	}
	if ic.Allows(NL80211_IFTYPE_STATION, NL80211_IFTYPE_STATION) {
		t.Error("two managed interfaces should not be allowed")
	}
	if ic.Allows(NL80211_IFTYPE_MESH_POINT, NL80211_IFTYPE_AP, NL80211_IFTYPE_AP, NL80211_IFTYPE_AP, NL80211_IFTYPE_AP) {
		t.Error("5 interfaces exceeds maxnum")
	}
	// Radio that can only run one interface at a time.
	single := IfaceCombination{Limits: []IfaceLimit{{Max: 1, Types: []uint32{NL80211_IFTYPE_AP, NL80211_IFTYPE_MESH_POINT}}}, MaxNum: 1, NumChannels: 1}
	if single.Allows(NL80211_IFTYPE_MESH_POINT, NL80211_IFTYPE_AP) {
		t.Error("single-interface radio must not allow mesh+AP")
	}
}

func TestWiphyAllowsTogether(t *testing.T) {
	w := &Wiphy{SupportedIftypes: map[uint32]bool{NL80211_IFTYPE_MESH_POINT: true, NL80211_IFTYPE_AP: true}}
	if !w.AllowsTogether(NL80211_IFTYPE_MESH_POINT) {
		t.Error("single supported type is always allowed")
	}
	if w.AllowsTogether(NL80211_IFTYPE_MESH_POINT, NL80211_IFTYPE_AP) {
		t.Error("no combinations advertised: must not assume concurrency")
	}
	if w.AllowsTogether(NL80211_IFTYPE_ADHOC) {
		t.Error("unsupported type")
	}
}

func TestSpatialStreams(t *testing.T) {
	ht2 := &BandInfo{HTMCSSet: []byte{0xff, 0xff, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}}
	if ht2.SpatialStreams() != 2 {
		t.Errorf("HT 2-stream = %d", ht2.SpatialStreams())
	}
	// VHT rx map: streams 1-3 MCS0-9 (0b10), rest unsupported (0b11): 0xffea? bits: s1=10,s2=10,s3=10,s4..8=11
	// value = 0b1111111111_10_10_10 = 0xFFEA
	vht3 := &BandInfo{HTMCSSet: ht2.HTMCSSet, VHTMCSSet: []byte{0xea, 0xff, 0, 0, 0, 0, 0, 0}}
	if vht3.SpatialStreams() != 3 {
		t.Errorf("VHT 3-stream = %d", vht3.SpatialStreams())
	}
	w := &Wiphy{Bands: map[Band]*BandInfo{Band5GHz: vht3, Band2GHz: ht2}}
	if w.SpatialStreams() != 3 {
		t.Errorf("wiphy streams = %d", w.SpatialStreams())
	}
	ant := &Wiphy{AntennaAvailRX: 0b11}
	if ant.SpatialStreams() != 2 {
		t.Errorf("antenna fallback = %d", ant.SpatialStreams())
	}
}

func TestDecodeWiphyMerge(t *testing.T) {
	// Build two split-dump messages for the same wiphy and merge them.
	enc := func(f func(ae *netlink.AttributeEncoder)) []byte {
		ae := netlink.NewAttributeEncoder()
		f(ae)
		b, err := ae.Encode()
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	m1 := enc(func(ae *netlink.AttributeEncoder) {
		ae.Uint32(NL80211_ATTR_WIPHY, 3)
		ae.String(NL80211_ATTR_WIPHY_NAME, "phy3")
		ae.Bytes(NL80211_ATTR_MAC, []byte{0, 0x11, 0x22, 0x33, 0x44, 0x55})
		ae.Nested(NL80211_ATTR_SUPPORTED_IFTYPES, func(n *netlink.AttributeEncoder) error {
			n.Flag(NL80211_IFTYPE_STATION, true)
			n.Flag(NL80211_IFTYPE_MESH_POINT, true)
			n.Flag(NL80211_IFTYPE_AP, true)
			return nil
		})
		ae.Nested(NL80211_ATTR_WIPHY_BANDS, func(n *netlink.AttributeEncoder) error {
			n.Nested(uint16(Band5GHz), func(b *netlink.AttributeEncoder) error {
				b.Bytes(NL80211_BAND_ATTR_HT_MCS_SET, []byte{0xff, 0xff, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
				b.Nested(NL80211_BAND_ATTR_FREQS, func(fs *netlink.AttributeEncoder) error {
					fs.Nested(1, func(f *netlink.AttributeEncoder) error {
						f.Uint32(NL80211_FREQUENCY_ATTR_FREQ, 5180)
						f.Uint32(NL80211_FREQUENCY_ATTR_MAX_TX_POWER, 2300)
						return nil
					})
					return nil
				})
				return nil
			})
			return nil
		})
	})
	m2 := enc(func(ae *netlink.AttributeEncoder) {
		ae.Uint32(NL80211_ATTR_WIPHY, 3)
		ae.Nested(NL80211_ATTR_WIPHY_BANDS, func(n *netlink.AttributeEncoder) error {
			n.Nested(uint16(Band5GHz), func(b *netlink.AttributeEncoder) error {
				b.Nested(NL80211_BAND_ATTR_FREQS, func(fs *netlink.AttributeEncoder) error {
					fs.Nested(1, func(f *netlink.AttributeEncoder) error {
						f.Uint32(NL80211_FREQUENCY_ATTR_FREQ, 5745)
						f.Uint32(NL80211_FREQUENCY_ATTR_MAX_TX_POWER, 3000)
						return nil
					})
					fs.Nested(2, func(f *netlink.AttributeEncoder) error {
						f.Uint32(NL80211_FREQUENCY_ATTR_FREQ, 5500)
						f.Flag(NL80211_FREQUENCY_ATTR_RADAR, true)
						f.Uint32(NL80211_FREQUENCY_ATTR_MAX_TX_POWER, 2300)
						return nil
					})
					return nil
				})
				return nil
			})
			return nil
		})
		ae.Nested(NL80211_ATTR_INTERFACE_COMBINATIONS, func(n *netlink.AttributeEncoder) error {
			n.Nested(1, func(c *netlink.AttributeEncoder) error {
				c.Nested(NL80211_IFACE_COMB_LIMITS, func(ls *netlink.AttributeEncoder) error {
					ls.Nested(1, func(l *netlink.AttributeEncoder) error {
						l.Uint32(NL80211_IFACE_LIMIT_MAX, 2)
						l.Nested(NL80211_IFACE_LIMIT_TYPES, func(t *netlink.AttributeEncoder) error {
							t.Flag(NL80211_IFTYPE_AP, true)
							t.Flag(NL80211_IFTYPE_MESH_POINT, true)
							return nil
						})
						return nil
					})
					return nil
				})
				c.Uint32(NL80211_IFACE_COMB_MAXNUM, 2)
				c.Uint32(NL80211_IFACE_COMB_NUM_CHANNELS, 1)
				return nil
			})
			return nil
		})
	})

	w := &Wiphy{Index: 3, Bands: map[Band]*BandInfo{}, SupportedIftypes: map[uint32]bool{}}
	for _, m := range [][]byte{m1, m2} {
		ad, err := netlink.NewAttributeDecoder(m)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.decode(ad); err != nil {
			t.Fatal(err)
		}
	}
	if w.Name != "phy3" || w.PermAddr.String() != "00:11:22:33:44:55" {
		t.Errorf("identity: %q %v", w.Name, w.PermAddr)
	}
	if !w.Supports(NL80211_IFTYPE_MESH_POINT) || !w.AllowsTogether(NL80211_IFTYPE_MESH_POINT, NL80211_IFTYPE_AP) {
		t.Error("capabilities not decoded")
	}
	if got := len(w.Bands[Band5GHz].Frequencies); got != 3 {
		t.Fatalf("frequencies merged = %d, want 3", got)
	}
	if p, ok := w.MaxTXPowerMBm(Band5GHz); !ok || p != 3000 {
		t.Errorf("max power = %d %v", p, ok)
	}
	if f := w.Frequency(5500); f == nil || !f.Radar || f.Usable() {
		t.Errorf("DFS channel: %+v", f)
	}
	if w.SpatialStreams() != 2 {
		t.Errorf("streams = %d", w.SpatialStreams())
	}
	if got := w.BandNames(); len(got) != 1 || got[0] != "5g" {
		t.Errorf("bands = %v", got)
	}
}

func TestBandOfFrequency(t *testing.T) {
	for mhz, want := range map[uint32]Band{2412: Band2GHz, 5180: Band5GHz, 5825: Band5GHz, 5955: Band6GHz, 915: BandS1GHz} {
		if got := BandOfFrequency(mhz); got != want {
			t.Errorf("%d MHz -> %v, want %v", mhz, got, want)
		}
	}
}
