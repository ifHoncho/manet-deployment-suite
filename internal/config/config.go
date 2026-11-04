// Package config defines the mesh profile and node configuration (schema v0),
// loads them from YAML and validates them before anything is applied.
//
// Two documents exist (architecture §12.6):
//
//   - the mesh profile, shared by every node in a mesh;
//   - the node config, specific to one node.
//
// Validation is strict: unknown keys are rejected, and every error names the
// field it refers to, so an operator can fix a typo without reading source.
package config

import (
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// SchemaVersion is the only schema both documents accept today.
const SchemaVersion = 0

// Default paths. They can be overridden on the command line.
const (
	DefaultNodePath    = "/etc/trellis/node.yaml"
	DefaultProfilePath = "/etc/trellis/profile.yaml"
	DefaultStateDir    = "/var/lib/trellis"
	DefaultRunDir      = "/run/trellis"
)

// Regulatory modes (architecture §5.1).
const (
	RegModeUnrestricted = "unrestricted"
	RegModeCountry      = "country"
)

// Bands a mesh profile can select.
const (
	Band2G  = "2g"
	Band5G  = "5g"
	Band6G  = "6g"
	BandS1G = "s1g" // Wi-Fi HaLow; accepted by the schema, not yet supported by meshd
)

// Port roles (architecture §7.1, plus "mesh" for wired backhaul and the lab).
const (
	PortRoleAuto   = "auto"
	PortRoleWAN    = "wan"
	PortRoleClient = "client"
	PortRoleMesh   = "mesh"
)

// RoleSelection values for a radio role.
const (
	RoleAuto     = "auto"
	RoleDisabled = "disabled"
)

// Profile is the mesh profile: everything every node in one mesh must agree on.
type Profile struct {
	Schema        int        `yaml:"schema"`
	Name          string     `yaml:"name"`
	MeshID        string     `yaml:"mesh_id"` // 802.11s mesh ID on the air; defaults to Name
	SAEPassphrase string     `yaml:"sae_passphrase"`
	Regulatory    Regulatory `yaml:"regulatory"`
	Radio         RadioPlan  `yaml:"radio"`
	// Radio2 is an optional second mesh link on its own radio (mesh1),
	// typically another band: 5 GHz for capacity plus HaLow for reach.
	// batman-adv uses both and picks the faster path to each node
	// (architecture §5.2, decision D-09).
	Radio2 *RadioPlan `yaml:"radio2,omitempty"`
	Prefix string     `yaml:"prefix"` // IPv4 prefix, default 10.73.0.0/16
	Batman BatmanPlan `yaml:"batman"`
}

// Regulatory selects how transmit power and channels are limited.
type Regulatory struct {
	Mode    string `yaml:"mode"`    // unrestricted (default) or country
	Country string `yaml:"country"` // ISO 3166-1 alpha-2; required in country mode, used for the AP in both
}

// RadioPlan is the mesh radio's band and channel.
type RadioPlan struct {
	Band    string `yaml:"band"`
	Channel int    `yaml:"channel"`
	Width   int    `yaml:"width"` // MHz: 20 (default), 40 or 80
}

// BatmanPlan pins batman-adv behaviour across the mesh (architecture §6).
type BatmanPlan struct {
	RoutingAlgo    string `yaml:"routing_algo"`     // BATMAN_V only
	OrigIntervalMS int    `yaml:"orig_interval_ms"` // default 1000
	MinVersion     string `yaml:"min_version"`      // refuse to start below this; default 2024.0
	PinnedVersion  string `yaml:"pinned_version"`   // warn when different; default 2026.3
	// BridgeLoopAvoidance protects against layer-2 loops when two nodes'
	// wired client ports share a switch (architecture §6). Default on.
	BridgeLoopAvoidance *bool `yaml:"bridge_loop_avoidance"`
	// MulticastFanout: when a multicast packet cannot use batman-adv's
	// multicast packet type (too big, or a node without it), it goes as
	// separate unicasts to up to this many listener nodes, and floods
	// beyond (architecture §9). Unset: batman-adv's default, 16.
	MulticastFanout *int `yaml:"multicast_fanout"`
}

// BLA reports whether bridge loop avoidance is on (default true).
func (b BatmanPlan) BLA() bool { return b.BridgeLoopAvoidance == nil || *b.BridgeLoopAvoidance }

// Node is one node's own configuration.
type Node struct {
	Schema      int           `yaml:"schema"`
	Callsign    string        `yaml:"callsign"`
	NodeID      int           `yaml:"node_id"` // 0 = claim automatically
	ProfilePath string        `yaml:"profile"` // path of the mesh profile; default /etc/trellis/profile.yaml
	Radios      RadiosConfig  `yaml:"radios"`
	Ports       []Port        `yaml:"ports"`
	AP          APConfig      `yaml:"ap"`
	Gateway     GatewayConfig `yaml:"gateway"`
	TXPowerDBm  *int          `yaml:"tx_power_dbm"` // cap for the mesh radio, below the mode's maximum
	MeshMTU     int           `yaml:"mesh_mtu"`     // underlay MTU to request on mesh0; default 1560
	// AllowOldBatman lets meshd run on a batman-adv older than the profile's
	// minimum. For bench experiments only: such nodes make the whole mesh
	// fall back to flooding (architecture §9).
	AllowOldBatman bool `yaml:"allow_old_batman"`
}

// RadiosConfig holds role overrides and per-radio settings (architecture §5.2).
type RadiosConfig struct {
	Roles   RoleOverrides `yaml:"roles"`
	Devices []DeviceRule  `yaml:"devices"`
}

// RoleOverrides pins or disables each role.
type RoleOverrides struct {
	Mesh  RoleSelection `yaml:"mesh"`
	Mesh2 RoleSelection `yaml:"mesh2"` // the profile's second mesh link (radio2)
	AP    RoleSelection `yaml:"ap"`
}

// RoleSelection is "auto", "disabled", or a pin to one radio or one port.
// In YAML it is either a plain string or a mapping with one of the keys.
type RoleSelection struct {
	Mode   string `yaml:"-"`
	Radio  string `yaml:"radio,omitempty"` // fingerprint
	Port   string `yaml:"port,omitempty"`  // port path, e.g. usb:1-1.2
	isNode bool
}

// UnmarshalYAML accepts "auto", "disabled", {radio: fp} or {port: path}.
func (r *RoleSelection) UnmarshalYAML(n *yaml.Node) error {
	r.isNode = true
	switch n.Kind {
	case yaml.ScalarNode:
		r.Mode = n.Value
		return nil
	case yaml.MappingNode:
		var m struct {
			Radio string `yaml:"radio"`
			Port  string `yaml:"port"`
		}
		if err := n.Decode(&m); err != nil {
			return err
		}
		r.Radio, r.Port = m.Radio, m.Port
		switch {
		case m.Radio != "" && m.Port != "":
			return errors.New("set either radio or port, not both")
		case m.Radio != "":
			r.Mode = "radio"
		case m.Port != "":
			r.Mode = "port"
		default:
			return errors.New("expected radio: <fingerprint> or port: <port path>")
		}
		return nil
	default:
		return errors.New("expected a string or a mapping")
	}
}

// MarshalYAML renders the selection back in the same short forms.
func (r RoleSelection) MarshalYAML() (any, error) {
	switch r.Mode {
	case "radio":
		return map[string]string{"radio": r.Radio}, nil
	case "port":
		return map[string]string{"port": r.Port}, nil
	case "":
		return RoleAuto, nil
	default:
		return r.Mode, nil
	}
}

// DeviceRule applies per-radio settings to radios matching one selector.
type DeviceRule struct {
	Match       DeviceMatch `yaml:"match"`
	Ignore      bool        `yaml:"ignore"`
	Label       string      `yaml:"label"`
	TXPowerDBm  *int        `yaml:"tx_power_dbm"`  // cap
	MaxPowerDBm *int        `yaml:"max_power_dbm"` // overrides the hardware database for ranking
}

// DeviceMatch selects radios by fingerprint or by port path. Exactly one is set.
type DeviceMatch struct {
	Fingerprint string `yaml:"fingerprint"`
	Port        string `yaml:"port"`
}

// Port is a wired interface and its role.
type Port struct {
	Name string `yaml:"name"`
	Role string `yaml:"role"`
}

// APConfig is the client access point (architecture §10).
type APConfig struct {
	Enabled    *bool  `yaml:"enabled"`
	SSID       string `yaml:"ssid"`       // default: the callsign
	Passphrase string `yaml:"passphrase"` // default: generated once and kept in the state directory
	Country    string `yaml:"country"`    // default: the mesh profile's country
	Band       string `yaml:"band"`       // 5g preferred, 2g fallback
	Channel    int    `yaml:"channel"`    // 0 = choose (away from the mesh channel)
	// TXPowerDBm caps the AP radio's transmit power when the AP has a radio
	// of its own. Default 15: phones are close, and 50 nearby nodes should
	// not shout over each other. Not applied when the AP shares the mesh
	// radio, where it would cap the mesh too.
	TXPowerDBm *int `yaml:"tx_power_dbm"`
	// Isolate stops this node's AP clients from reaching each other
	// directly. Off by default: the client network is flat.
	Isolate bool `yaml:"isolate"`
}

// Gateway modes (architecture §8.1).
const (
	GatewayAuto = "auto" // share a healthy WAN when this node has one
	GatewayOff  = "off"  // never share; still use other nodes' gateways
)

// GatewayConfig controls sharing this node's WAN with the mesh.
type GatewayConfig struct {
	Mode string `yaml:"mode"`
	// WAN is the uplink interface. Empty: the first port with role wan,
	// else the interface of the system's own default route.
	WAN string `yaml:"wan"`
	// ProbeTargets are pinged out of the WAN every probe interval; the WAN
	// is healthy while any of them answers (architecture §8.2).
	ProbeTargets    []string `yaml:"probe_targets"`
	ProbeIntervalMS int      `yaml:"probe_interval_ms"`
	// Announced uplink capacity in Mbit/s (batman-adv gw_bandwidth).
	DownMbit int `yaml:"down_mbit"`
	UpMbit   int `yaml:"up_mbit"`
}

// DefaultProbeTargets are well-known anycast resolvers on separate networks.
var DefaultProbeTargets = []string{"1.1.1.1", "8.8.8.8", "9.9.9.9"}

// Field error -----------------------------------------------------------------

// FieldError is one validation problem, tied to the YAML path of the field.
type FieldError struct {
	Field string
	Msg   string
}

func (e *FieldError) Error() string { return e.Field + ": " + e.Msg }

// Errors is a list of field errors. It is returned as the error from Validate.
type Errors []*FieldError

func (es Errors) Error() string {
	if len(es) == 1 {
		return es[0].Error()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d problems:", len(es))
	for _, e := range es {
		b.WriteString("\n  - ")
		b.WriteString(e.Error())
	}
	return b.String()
}

func (es *Errors) add(field, format string, args ...any) {
	*es = append(*es, &FieldError{Field: field, Msg: fmt.Sprintf(format, args...)})
}

func (es Errors) orNil() error {
	if len(es) == 0 {
		return nil
	}
	sort.SliceStable(es, func(i, j int) bool { return es[i].Field < es[j].Field })
	return es
}

// Loading ---------------------------------------------------------------------

// decodeStrict decodes YAML rejecting unknown keys, and turns yaml's errors
// into field errors where it can.
func decodeStrict(data []byte, v any) error {
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return nil // empty document: defaults and validation decide
		}
		return translateYAMLError(err)
	}
	return nil
}

var reUnknownField = regexp.MustCompile(`line (\d+): field (\S+) not found in type`)

func translateYAMLError(err error) error {
	msg := err.Error()
	// yaml.v3 wraps several problems into one TypeError.
	var te *yaml.TypeError
	if errors.As(err, &te) {
		var es Errors
		for _, line := range te.Errors {
			if m := reUnknownField.FindStringSubmatch(line); m != nil {
				es.add(m[2], "unknown field (line %s)", m[1])
			} else {
				es.add("(document)", "%s", strings.TrimPrefix(line, "yaml: "))
			}
		}
		return es.orNil()
	}
	return Errors{{Field: "(document)", Msg: strings.TrimPrefix(msg, "yaml: ")}}
}

// LoadProfile reads, defaults and validates a mesh profile.
func LoadProfile(path string) (*Profile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseProfile(data)
}

// ParseProfile parses, defaults and validates a mesh profile from YAML.
func ParseProfile(data []byte) (*Profile, error) {
	var p Profile
	if err := decodeStrict(data, &p); err != nil {
		return nil, err
	}
	p.ApplyDefaults()
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// LoadNode reads, defaults and validates a node config.
func LoadNode(path string) (*Node, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseNode(data)
}

// ParseNode parses, defaults and validates a node config from YAML.
func ParseNode(data []byte) (*Node, error) {
	var n Node
	if err := decodeStrict(data, &n); err != nil {
		return nil, err
	}
	n.ApplyDefaults()
	if err := n.Validate(); err != nil {
		return nil, err
	}
	return &n, nil
}

// Defaults --------------------------------------------------------------------

// ApplyDefaults fills unset fields with the documented defaults.
func (p *Profile) ApplyDefaults() {
	if p.MeshID == "" {
		p.MeshID = p.Name
	}
	if p.Regulatory.Mode == "" {
		p.Regulatory.Mode = RegModeUnrestricted
	}
	if p.Regulatory.Country == "" {
		p.Regulatory.Country = "US"
	}
	p.Regulatory.Country = strings.ToUpper(p.Regulatory.Country)
	if p.Radio.Band == "" {
		p.Radio.Band = Band5G
	}
	if p.Radio.Width == 0 {
		p.Radio.Width = 20
	}
	if p.Radio2 != nil && p.Radio2.Width == 0 {
		p.Radio2.Width = 20
	}
	if p.Prefix == "" {
		p.Prefix = "10.73.0.0/16"
	}
	if p.Batman.RoutingAlgo == "" {
		p.Batman.RoutingAlgo = "BATMAN_V"
	}
	if p.Batman.OrigIntervalMS == 0 {
		p.Batman.OrigIntervalMS = 1000
	}
	if p.Batman.MinVersion == "" {
		p.Batman.MinVersion = "2024.0"
	}
	if p.Batman.PinnedVersion == "" {
		p.Batman.PinnedVersion = "2026.3"
	}
}

// validateRadio checks one mesh link's band, channel and width.
func validateRadio(es *Errors, field string, r RadioPlan) {
	switch r.Band {
	case Band2G, Band5G, Band6G, BandS1G:
	default:
		es.add(field+".band", "must be one of 2g, 5g, 6g, s1g (got %q)", r.Band)
	}
	if r.Band != BandS1G {
		if _, err := ChannelFrequencyMHz(r.Band, r.Channel); err != nil {
			es.add(field+".channel", "%v", err)
		}
	} else if r.Channel <= 0 {
		es.add(field+".channel", "required")
	}
	switch r.Width {
	case 20, 40, 80:
	default:
		es.add(field+".width", "must be 20, 40 or 80 MHz (got %d)", r.Width)
	}
	if r.Band == Band2G && r.Width > 40 {
		es.add(field+".width", "2.4 GHz supports at most 40 MHz")
	}
}

// ApplyDefaults fills unset fields with the documented defaults.
func (n *Node) ApplyDefaults() {
	if n.ProfilePath == "" {
		n.ProfilePath = DefaultProfilePath
	}
	if n.Radios.Roles.Mesh.Mode == "" {
		n.Radios.Roles.Mesh.Mode = RoleAuto
	}
	if n.Radios.Roles.AP.Mode == "" {
		n.Radios.Roles.AP.Mode = RoleAuto
	}
	if n.Radios.Roles.Mesh2.Mode == "" {
		n.Radios.Roles.Mesh2.Mode = RoleAuto
	}
	for i := range n.Ports {
		if n.Ports[i].Role == "" {
			n.Ports[i].Role = PortRoleAuto
		}
	}
	if n.AP.Enabled == nil {
		t := true
		n.AP.Enabled = &t
	}
	n.AP.Country = strings.ToUpper(n.AP.Country)
	if n.AP.Band == "" {
		n.AP.Band = Band5G
	}
	if n.MeshMTU == 0 {
		n.MeshMTU = 1560
	}
	if n.Gateway.Mode == "" {
		n.Gateway.Mode = GatewayAuto
	}
	if len(n.Gateway.ProbeTargets) == 0 {
		n.Gateway.ProbeTargets = append([]string{}, DefaultProbeTargets...)
	}
	if n.Gateway.ProbeIntervalMS == 0 {
		n.Gateway.ProbeIntervalMS = 1000
	}
	if n.Gateway.DownMbit == 0 {
		n.Gateway.DownMbit = 20
	}
	if n.Gateway.UpMbit == 0 {
		n.Gateway.UpMbit = 5
	}
}

// Validation ------------------------------------------------------------------

var (
	reCountry  = regexp.MustCompile(`^[A-Z]{2}$`)
	reCallsign = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	reIfname   = regexp.MustCompile(`^[^/\s:]{1,15}$`)
	reVersion  = regexp.MustCompile(`^\d{4}\.\d+$`)
)

// Validate checks the profile. The returned error, if any, is an Errors list.
func (p *Profile) Validate() error {
	var es Errors
	if p.Schema != SchemaVersion {
		es.add("schema", "must be %d (got %d)", SchemaVersion, p.Schema)
	}
	if p.Name == "" {
		es.add("name", "required")
	}
	if l := len(p.MeshID); l == 0 || l > 32 {
		es.add("mesh_id", "must be 1–32 bytes (got %d)", l)
	}
	if l := len(p.SAEPassphrase); l < 8 || l > 128 {
		es.add("sae_passphrase", "must be 8–128 characters (got %d)", l)
	} else if err := printableASCII(p.SAEPassphrase); err != nil {
		es.add("sae_passphrase", "%v", err)
	}
	switch p.Regulatory.Mode {
	case RegModeUnrestricted, RegModeCountry:
	default:
		es.add("regulatory.mode", "must be %q or %q (got %q)", RegModeUnrestricted, RegModeCountry, p.Regulatory.Mode)
	}
	if !reCountry.MatchString(p.Regulatory.Country) {
		es.add("regulatory.country", "must be a two-letter ISO country code (got %q)", p.Regulatory.Country)
	}
	validateRadio(&es, "radio", p.Radio)
	if r2 := p.Radio2; r2 != nil {
		validateRadio(&es, "radio2", *r2)
		if r2.Band == p.Radio.Band && r2.Channel == p.Radio.Channel {
			es.add("radio2", "the second mesh link must not use the first link's channel (%s channel %d)", p.Radio.Band, p.Radio.Channel)
		}
	}
	if pfx, err := netip.ParsePrefix(p.Prefix); err != nil {
		es.add("prefix", "not a valid IPv4 prefix: %v", err)
	} else if !pfx.Addr().Is4() {
		es.add("prefix", "must be IPv4")
	} else if pfx.Bits() != 16 {
		es.add("prefix", "must be a /16 in this release (got /%d)", pfx.Bits())
	} else if pfx.Masked() != pfx {
		es.add("prefix", "host bits set; use %s", pfx.Masked())
	}
	if p.Batman.RoutingAlgo != "BATMAN_V" {
		es.add("batman.routing_algo", "only BATMAN_V is supported (got %q)", p.Batman.RoutingAlgo)
	}
	if f := p.Batman.MulticastFanout; f != nil && (*f < 0 || *f > 255) {
		es.add("batman.multicast_fanout", "must be 0–255 (got %d)", *f)
	}
	if p.Batman.OrigIntervalMS < 200 || p.Batman.OrigIntervalMS > 10000 {
		es.add("batman.orig_interval_ms", "must be 200–10000 (got %d)", p.Batman.OrigIntervalMS)
	}
	if !reVersion.MatchString(p.Batman.MinVersion) {
		es.add("batman.min_version", "must look like 2024.0 (got %q)", p.Batman.MinVersion)
	}
	if !reVersion.MatchString(p.Batman.PinnedVersion) {
		es.add("batman.pinned_version", "must look like 2026.3 (got %q)", p.Batman.PinnedVersion)
	}
	return es.orNil()
}

// Validate checks the node config. The returned error, if any, is an Errors list.
func (n *Node) Validate() error {
	var es Errors
	if n.Schema != SchemaVersion {
		es.add("schema", "must be %d (got %d)", SchemaVersion, n.Schema)
	}
	if n.Callsign == "" {
		es.add("callsign", "required")
	} else if !reCallsign.MatchString(n.Callsign) {
		es.add("callsign", "must be a DNS label: lowercase letters, digits and hyphens, 1–63 characters (got %q)", n.Callsign)
	}
	if n.NodeID < 0 || n.NodeID > 254 {
		es.add("node_id", "must be 1–254, or 0 to claim automatically (got %d)", n.NodeID)
	}
	if n.ProfilePath == "" {
		es.add("profile", "required")
	}
	validateRole := func(field string, r RoleSelection) {
		switch r.Mode {
		case RoleAuto, RoleDisabled:
		case "radio":
			if r.Radio == "" {
				es.add(field+".radio", "required")
			}
		case "port":
			if r.Port == "" {
				es.add(field+".port", "required")
			}
		default:
			es.add(field, "must be auto, disabled, {radio: <fingerprint>} or {port: <port>} (got %q)", r.Mode)
		}
	}
	validateRole("radios.roles.mesh", n.Radios.Roles.Mesh)
	validateRole("radios.roles.ap", n.Radios.Roles.AP)
	validateRole("radios.roles.mesh2", n.Radios.Roles.Mesh2)
	for i, d := range n.Radios.Devices {
		f := fmt.Sprintf("radios.devices[%d]", i)
		switch {
		case d.Match.Fingerprint == "" && d.Match.Port == "":
			es.add(f+".match", "set fingerprint or port")
		case d.Match.Fingerprint != "" && d.Match.Port != "":
			es.add(f+".match", "set either fingerprint or port, not both")
		}
		if d.TXPowerDBm != nil && (*d.TXPowerDBm < 0 || *d.TXPowerDBm > 40) {
			es.add(f+".tx_power_dbm", "must be 0–40 (got %d)", *d.TXPowerDBm)
		}
		if d.MaxPowerDBm != nil && (*d.MaxPowerDBm < 0 || *d.MaxPowerDBm > 40) {
			es.add(f+".max_power_dbm", "must be 0–40 (got %d)", *d.MaxPowerDBm)
		}
	}
	seen := map[string]bool{}
	for i, p := range n.Ports {
		f := fmt.Sprintf("ports[%d]", i)
		if p.Name == "" {
			es.add(f+".name", "required")
		} else if !reIfname.MatchString(p.Name) {
			es.add(f+".name", "not a valid interface name (got %q)", p.Name)
		} else if seen[p.Name] {
			es.add(f+".name", "duplicate port %q", p.Name)
		}
		seen[p.Name] = true
		switch p.Role {
		case PortRoleAuto, PortRoleWAN, PortRoleClient, PortRoleMesh:
		default:
			es.add(f+".role", "must be auto, wan, client or mesh (got %q)", p.Role)
		}
	}
	if n.AP.SSID != "" && len(n.AP.SSID) > 32 {
		es.add("ap.ssid", "must be at most 32 bytes (got %d)", len(n.AP.SSID))
	}
	if n.AP.Passphrase != "" && (len(n.AP.Passphrase) < 8 || len(n.AP.Passphrase) > 63) {
		es.add("ap.passphrase", "must be 8–63 characters (got %d)", len(n.AP.Passphrase))
	} else if err := printableASCII(n.AP.Passphrase); err != nil {
		es.add("ap.passphrase", "%v", err)
	}
	if n.AP.Country != "" && !reCountry.MatchString(n.AP.Country) {
		es.add("ap.country", "must be a two-letter ISO country code (got %q)", n.AP.Country)
	}
	switch n.AP.Band {
	case Band2G, Band5G:
		if n.AP.Channel != 0 {
			if _, err := ChannelFrequencyMHz(n.AP.Band, n.AP.Channel); err != nil {
				es.add("ap.channel", "%v", err)
			}
		}
	default:
		es.add("ap.band", "must be 2g or 5g (got %q)", n.AP.Band)
	}
	if n.AP.TXPowerDBm != nil && (*n.AP.TXPowerDBm < 0 || *n.AP.TXPowerDBm > 40) {
		es.add("ap.tx_power_dbm", "must be 0–40 (got %d)", *n.AP.TXPowerDBm)
	}
	if n.TXPowerDBm != nil && (*n.TXPowerDBm < 0 || *n.TXPowerDBm > 40) {
		es.add("tx_power_dbm", "must be 0–40 (got %d)", *n.TXPowerDBm)
	}
	if n.MeshMTU < 1280 || n.MeshMTU > 2304 {
		es.add("mesh_mtu", "must be 1280–2304 (got %d)", n.MeshMTU)
	}
	switch n.Gateway.Mode {
	case GatewayAuto, GatewayOff:
	default:
		es.add("gateway.mode", "must be auto or off (got %q)", n.Gateway.Mode)
	}
	if n.Gateway.WAN != "" && !reIfname.MatchString(n.Gateway.WAN) {
		es.add("gateway.wan", "not a valid interface name (got %q)", n.Gateway.WAN)
	}
	for i, t := range n.Gateway.ProbeTargets {
		if a, err := netip.ParseAddr(t); err != nil || !a.Is4() {
			es.add(fmt.Sprintf("gateway.probe_targets[%d]", i), "must be an IPv4 address (got %q)", t)
		}
	}
	if n.Gateway.ProbeIntervalMS < 200 || n.Gateway.ProbeIntervalMS > 10000 {
		es.add("gateway.probe_interval_ms", "must be 200–10000 (got %d)", n.Gateway.ProbeIntervalMS)
	}
	if n.Gateway.DownMbit < 1 || n.Gateway.DownMbit > 100000 {
		es.add("gateway.down_mbit", "must be 1–100000 (got %d)", n.Gateway.DownMbit)
	}
	if n.Gateway.UpMbit < 1 || n.Gateway.UpMbit > 100000 {
		es.add("gateway.up_mbit", "must be 1–100000 (got %d)", n.Gateway.UpMbit)
	}
	return es.orNil()
}

// Helpers ---------------------------------------------------------------------

// printableASCII rejects characters that cannot be written unescaped into
// wpa_supplicant and hostapd quoted strings.
func printableASCII(s string) error {
	for i, r := range s {
		if r < 0x20 || r > 0x7e {
			return fmt.Errorf("character %d is not printable ASCII", i+1)
		}
		if r == '"' || r == '\\' {
			return fmt.Errorf("character %d (%q) is not allowed", i+1, r)
		}
	}
	return nil
}

// ChannelFrequencyMHz converts a band and channel number to a centre frequency.
func ChannelFrequencyMHz(band string, ch int) (int, error) {
	switch band {
	case Band2G:
		switch {
		case ch >= 1 && ch <= 13:
			return 2407 + 5*ch, nil
		case ch == 14:
			return 2484, nil
		}
		return 0, fmt.Errorf("2.4 GHz channel must be 1–14 (got %d)", ch)
	case Band5G:
		// UNII-1 to UNII-2e run 32–144 in steps of 4; UNII-3/4 run 149–177 in steps of 4.
		if (ch >= 32 && ch <= 144 && ch%4 == 0) || (ch >= 149 && ch <= 177 && ch%4 == 1) {
			return 5000 + 5*ch, nil
		}
		return 0, fmt.Errorf("5 GHz channel must be 32–144 in steps of 4, or 149–177 in steps of 4 (got %d)", ch)
	case Band6G:
		if ch >= 1 && ch <= 233 && ch%4 == 1 {
			return 5950 + 5*ch, nil
		}
		return 0, fmt.Errorf("6 GHz channel must be 1, 5, 9 … 233 (got %d)", ch)
	}
	return 0, fmt.Errorf("unsupported band %q", band)
}

// APEnabled reports whether the client AP is enabled.
func (n *Node) APEnabled() bool { return n.AP.Enabled == nil || *n.AP.Enabled }

// MeshPorts returns the names of wired ports with the mesh role.
func (n *Node) MeshPorts() []string {
	var out []string
	for _, p := range n.Ports {
		if p.Role == PortRoleMesh {
			out = append(out, p.Name)
		}
	}
	return out
}

// ClientPorts returns the names of wired ports with the client role.
func (n *Node) ClientPorts() []string {
	var out []string
	for _, p := range n.Ports {
		if p.Role == PortRoleClient {
			out = append(out, p.Name)
		}
	}
	return out
}

// Redacted returns a copy with secrets blanked, for status output.
func (p Profile) Redacted() Profile {
	if p.SAEPassphrase != "" {
		p.SAEPassphrase = "<redacted>"
	}
	return p
}

// Redacted returns a copy with secrets blanked, for status output.
func (n Node) Redacted() Node {
	if n.AP.Passphrase != "" {
		n.AP.Passphrase = "<redacted>"
	}
	return n
}

// AutoPorts returns the names of wired ports listed with the auto role.
func (n *Node) AutoPorts() []string {
	var out []string
	for _, p := range n.Ports {
		if p.Role == PortRoleAuto {
			out = append(out, p.Name)
		}
	}
	return out
}

// OwnedPorts returns the wired ports Trellis configures itself (client,
// mesh and auto), which the system's network managers must leave alone.
// WAN ports stay with the system.
func (n *Node) OwnedPorts() []string {
	var out []string
	for _, p := range n.Ports {
		if p.Role != PortRoleWAN {
			out = append(out, p.Name)
		}
	}
	return out
}

// PortRole returns the role a port is listed with, or "".
func (n *Node) PortRole(name string) string {
	for _, p := range n.Ports {
		if p.Name == name {
			return p.Role
		}
	}
	return ""
}

// WANPorts returns the names of wired ports with the wan role.
func (n *Node) WANPorts() []string {
	var out []string
	for _, p := range n.Ports {
		if p.Role == PortRoleWAN {
			out = append(out, p.Name)
		}
	}
	return out
}
