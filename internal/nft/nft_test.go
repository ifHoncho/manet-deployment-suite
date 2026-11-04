package nft

import (
	"strings"
	"testing"
)

func TestRender(t *testing.T) {
	out := Ruleset{MeshPort: "bat0", ClientPorts: []string{"ap0", "eth2"}, AnycastIP: "10.73.0.1", AnycastMAC: "02:74:72:65:6c:01"}.Render()
	for _, want := range []string{
		"table bridge trellis\ndelete table bridge trellis\n",
		`iifname "bat0" jump from_mesh`, `iifname { "ap0", "eth2" } jump from_clients`, `oifname "bat0" jump to_mesh`,
		`arp daddr ip 10.73.0.1 counter drop comment "anycast-arp"`, `ether saddr 02:74:72:65:6c:01 counter drop comment "anycast-mac"`,
		`udp dport { 67, 68 } counter drop comment "dhcp-broadcast"`, `limit rate over 20/second`, `udp sport 67 udp dport 68 counter drop comment "rogue-dhcp-server"`,
		`ether type ip counter drop comment "ipv4-broadcast"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "allowed-broadcast") {
		t.Error("no broadcast allow-list expected")
	}
	out = Ruleset{MeshPort: "bat0", AnycastIP: "10.73.0.1", AnycastMAC: "02:74:72:65:6c:01", BroadcastUDPPorts: []int{4242}}.Render()
	if strings.Contains(out, "jump from_clients") || !strings.Contains(out, "udp dport { 4242 } accept") {
		t.Errorf("client-less ruleset wrong:\n%s", out)
	}
}

func TestParseCounters(t *testing.T) {
	js := `{"nftables":[{"metainfo":{}},{"table":{"family":"bridge","name":"trellis"}},
	{"rule":{"family":"bridge","table":"trellis","chain":"to_mesh","handle":5,"comment":"anycast-arp","expr":[{"match":{}},{"counter":{"packets":7,"bytes":294}},{"drop":null}]}},
	{"rule":{"family":"bridge","table":"trellis","chain":"to_mesh","handle":6,"comment":"anycast-arp","expr":[{"counter":{"packets":1,"bytes":42}}]}},
	{"rule":{"family":"bridge","table":"trellis","chain":"in","handle":9,"expr":[{"jump":{"target":"from_mesh"}}]}}]}`
	c, n, err := parseCounters([]byte(js))
	if err != nil || n != 3 || c["anycast-arp"] != 8 {
		t.Fatalf("got %v rules=%d err=%v", c, n, err)
	}
}

func TestGossipFilterAndRouting(t *testing.T) {
	out := Ruleset{MeshPort: "bat0", ClientPorts: []string{"ap0"}, AnycastIP: "10.73.0.1", AnycastMAC: "02:74:72:65:6c:01", GossipPort: 7373}.Render()
	for _, want := range []string{`udp dport 7373 counter drop comment "gossip-to-from-clients"`, `oifname { "ap0" } jump to_clients`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	r := Routing{AnycastIf: "anycast0", Mark: 0x7302, NAT: true, WAN: "eth0", Prefix: "10.73.0.0/16"}.Render()
	for _, want := range []string{"table inet trellis\ndelete table inet trellis\n", `iifname "anycast0" ct state new ct mark & 0xffff == 0 ct mark set ct mark | 0x7302`,
		`iifname "anycast0" meta mark set ct mark`, `oifname "eth0" ip saddr 10.73.0.0/16 counter masquerade`} {
		if !strings.Contains(r, want) {
			t.Errorf("missing %q in\n%s", want, r)
		}
	}
	if strings.Contains(r, "notrack") {
		t.Errorf("a gateway must track SYNs (NAT needs them):\n%s", r)
	}
	r = Routing{AnycastIf: "anycast0", Mark: 0x7301, UntrackSYN: true}.Render()
	if !strings.Contains(r, `iifname "anycast0" tcp flags & (syn | ack) == syn notrack`) || !strings.Contains(r, "priority raw") {
		t.Errorf("client node must leave SYNs untracked:\n%s", r)
	}
	r = Routing{AnycastIf: "anycast0"}.Render()
	if strings.Contains(r, "ct mark set") || strings.Contains(r, "masquerade") {
		t.Errorf("client-only routing table wrong:\n%s", r)
	}
}
