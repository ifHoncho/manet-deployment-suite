package config

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
)

const goodProfile = `
schema: 0
name: alpha
sae_passphrase: correct-horse-battery
regulatory:
  mode: country
  country: us
radio:
  band: 5g
  channel: 149
`

const goodNode = `
schema: 0
callsign: renegade-1
node_id: 7
ports:
  - name: eth0
    role: auto
  - name: eth1
    role: mesh
radios:
  roles:
    mesh: auto
    ap: {port: "usb:1-1.2"}
  devices:
    - match: {fingerprint: "mac:00:11:22:33:44:55"}
      label: Alfa
      tx_power_dbm: 17
`

func TestParseProfileDefaults(t *testing.T) {
	p, err := ParseProfile([]byte(goodProfile))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.MeshID != "alpha" {
		t.Errorf("mesh_id default = %q, want name", p.MeshID)
	}
	if p.Regulatory.Country != "US" {
		t.Errorf("country not upper-cased: %q", p.Regulatory.Country)
	}
	if p.Prefix != "10.73.0.0/16" || p.Batman.OrigIntervalMS != 1000 || p.Batman.PinnedVersion != "2026.3" || p.Radio.Width != 20 {
		t.Errorf("defaults not applied: %+v", p)
	}
}

func TestParseNode(t *testing.T) {
	n, err := ParseNode([]byte(goodNode))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n.Radios.Roles.AP.Mode != "port" || n.Radios.Roles.AP.Port != "usb:1-1.2" {
		t.Errorf("ap role selection = %+v", n.Radios.Roles.AP)
	}
	if n.Radios.Roles.Mesh.Mode != RoleAuto {
		t.Errorf("mesh role = %+v", n.Radios.Roles.Mesh)
	}
	if got := n.MeshPorts(); len(got) != 1 || got[0] != "eth1" {
		t.Errorf("MeshPorts = %v", got)
	}
	if !n.APEnabled() || n.AP.Country != "" || n.AP.Band != Band5G || n.MeshMTU != 1560 {
		t.Errorf("defaults not applied: %+v", n)
	}
	if d := n.Radios.Devices[0]; d.TXPowerDBm == nil || *d.TXPowerDBm != 17 {
		t.Errorf("device rule = %+v", d)
	}
}

func fieldErrors(t *testing.T, err error) map[string]string {
	t.Helper()
	var es Errors
	if !errors.As(err, &es) {
		t.Fatalf("expected Errors, got %T: %v", err, err)
	}
	m := map[string]string{}
	for _, e := range es {
		m[e.Field] = e.Msg
	}
	return m
}

func TestProfileErrorsNameFields(t *testing.T) {
	_, err := ParseProfile([]byte(`
schema: 1
name: ""
sae_passphrase: short
regulatory: {mode: lenient, country: USA}
radio: {band: 5g, channel: 150, width: 160}
prefix: 10.73.1.0/24
batman: {routing_algo: BATMAN_IV, orig_interval_ms: 50, min_version: new}
`))
	if err == nil {
		t.Fatal("expected error")
	}
	m := fieldErrors(t, err)
	for _, f := range []string{"schema", "name", "sae_passphrase", "regulatory.mode", "regulatory.country",
		"radio.channel", "radio.width", "prefix", "batman.routing_algo", "batman.orig_interval_ms", "batman.min_version"} {
		if _, ok := m[f]; !ok {
			t.Errorf("missing error for %s; got %v", f, m)
		}
	}
}

func TestUnknownFieldRejected(t *testing.T) {
	_, err := ParseProfile([]byte(goodProfile + "\nsae_pasphrase: typo\n"))
	if err == nil {
		t.Fatal("expected error")
	}
	m := fieldErrors(t, err)
	if msg, ok := m["sae_pasphrase"]; !ok || !strings.Contains(msg, "unknown field") {
		t.Errorf("unknown field not reported by name: %v", m)
	}
}

func TestNodeErrorsNameFields(t *testing.T) {
	_, err := ParseNode([]byte(`
schema: 0
callsign: "Bad Callsign"
node_id: 300
ports:
  - name: eth0
    role: uplink
  - name: eth0
radios:
  roles:
    mesh: sometimes
  devices:
    - match: {}
      tx_power_dbm: 99
ap: {country: U, band: 6g, passphrase: short}
mesh_mtu: 100
`))
	if err == nil {
		t.Fatal("expected error")
	}
	m := fieldErrors(t, err)
	for _, f := range []string{"callsign", "node_id", "ports[0].role", "ports[1].name", "radios.roles.mesh",
		"radios.devices[0].match", "radios.devices[0].tx_power_dbm", "ap.country", "ap.band", "ap.passphrase", "mesh_mtu"} {
		if _, ok := m[f]; !ok {
			t.Errorf("missing error for %s; got %v", f, m)
		}
	}
}

func TestRoleSelectionBothKeys(t *testing.T) {
	_, err := ParseNode([]byte("schema: 0\ncallsign: a\nradios:\n  roles:\n    mesh: {radio: x, port: y}\n"))
	if err == nil || !strings.Contains(err.Error(), "not both") {
		t.Errorf("expected 'not both' error, got %v", err)
	}
}

func TestChannelFrequency(t *testing.T) {
	cases := []struct {
		band string
		ch   int
		mhz  int
		ok   bool
	}{
		{Band2G, 1, 2412, true}, {Band2G, 11, 2462, true}, {Band2G, 14, 2484, true}, {Band2G, 15, 0, false},
		{Band5G, 36, 5180, true}, {Band5G, 149, 5745, true}, {Band5G, 165, 5825, true}, {Band5G, 150, 0, false},
		{Band6G, 1, 5955, true}, {Band6G, 2, 0, false},
	}
	for _, c := range cases {
		got, err := ChannelFrequencyMHz(c.band, c.ch)
		if (err == nil) != c.ok || got != c.mhz {
			t.Errorf("ChannelFrequencyMHz(%s,%d) = %d,%v; want %d ok=%v", c.band, c.ch, got, err, c.mhz, c.ok)
		}
	}
}

func TestRedacted(t *testing.T) {
	p, _ := ParseProfile([]byte(goodProfile))
	if p.Redacted().SAEPassphrase != "<redacted>" || p.SAEPassphrase == "<redacted>" {
		t.Error("Redacted must blank the copy only")
	}
}

func TestGatewayConfig(t *testing.T) {
	n, err := ParseNode([]byte("schema: 0\ncallsign: gw-1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if n.Gateway.Mode != GatewayAuto || len(n.Gateway.ProbeTargets) != 3 || n.Gateway.ProbeIntervalMS != 1000 {
		t.Errorf("gateway defaults: %+v", n.Gateway)
	}
	_, err = ParseNode([]byte("schema: 0\ncallsign: gw-1\ngateway: {mode: sometimes, probe_targets: [example.com], probe_interval_ms: 5}\n"))
	if err == nil {
		t.Fatal("bad gateway config accepted")
	}
	for _, want := range []string{"gateway.mode", "gateway.probe_targets[0]", "gateway.probe_interval_ms"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %s: %v", want, err)
		}
	}
}

func TestProfileSecondMeshLink(t *testing.T) {
	p, err := ParseProfile([]byte(goodProfile))
	if err != nil || p.Radio2 != nil {
		t.Fatalf("a profile without radio2 has a second link: %+v %v", p.Radio2, err)
	}
	p, err = ParseProfile([]byte(goodProfile + "\nradio2: {band: 2g, channel: 6}\n"))
	if err != nil {
		t.Fatalf("valid radio2 rejected: %v", err)
	}
	if p.Radio2 == nil || p.Radio2.Band != "2g" || p.Radio2.Channel != 6 || p.Radio2.Width != 20 {
		t.Fatalf("radio2 = %+v; want 2g ch 6, default width 20", p.Radio2)
	}
	for name, extra := range map[string]string{
		"same channel as radio": "\nradio2: {band: " + p.Radio.Band + ", channel: " + strconv.Itoa(p.Radio.Channel) + "}\n",
		"bad channel":           "\nradio2: {band: 5g, channel: 7}\n",
		"bad band":              "\nradio2: {band: 3g, channel: 1}\n",
		"2.4 GHz at 80 MHz":     "\nradio2: {band: 2g, channel: 6, width: 80}\n",
	} {
		_, err := ParseProfile([]byte(goodProfile + extra))
		if err == nil || !strings.Contains(err.Error(), "radio2") {
			t.Errorf("%s: want an error naming radio2, got %v", name, err)
		}
	}
}

// The shipped examples must stay valid, including with their optional
// sections switched on.
func TestExampleConfigs(t *testing.T) {
	prof, err := os.ReadFile("../../configs/examples/profile.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseProfile(prof); err != nil {
		t.Errorf("configs/examples/profile.yaml: %v", err)
	}
	// Uncomment the example radio2 block.
	on := strings.NewReplacer("# radio2:", "radio2:", "#   band:", "  band:", "#   channel:", "  channel:", "#   width:", "  width:").Replace(string(prof))
	p, err := ParseProfile([]byte(on))
	if err != nil || p.Radio2 == nil {
		t.Errorf("example profile with radio2 enabled: %+v, %v", p, err)
	}
	node, err := os.ReadFile("../../configs/examples/node.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseNode(node); err != nil {
		t.Errorf("configs/examples/node.yaml: %v", err)
	}
}
