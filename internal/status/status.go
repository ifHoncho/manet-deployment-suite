// Package status defines the JSON that meshd serves and that meshctl and webd
// consume. It is the one description of a node's state (architecture §12.7).
package status

import "time"

// Status is a complete snapshot of one node.
type Status struct {
	Version    string       `json:"version"`
	Hostname   string       `json:"hostname"`
	Time       time.Time    `json:"time"`
	StartedAt  time.Time    `json:"started_at"`
	Node       Node         `json:"node"`
	Profile    Profile      `json:"profile"`
	Regulatory Regulatory   `json:"regulatory"`
	Radios     []Radio      `json:"radios"`
	Plan       Plan         `json:"plan"`
	MeshLink   MeshLink     `json:"mesh_link"`
	MeshLink2  *MeshLink    `json:"mesh_link2,omitempty"` // second mesh link (profile radio2), when configured
	Batman     Batman       `json:"batman"`
	Bridge     Bridge       `json:"bridge"`
	AP         AP           `json:"ap"`
	ClientNet  ClientNet    `json:"client_network"`
	Gateway    GatewayState `json:"gateway"`
	Ports      []Port       `json:"ports"` // Ethernet ports and their roles (architecture §7.1)
	Nodes      []MeshNode   `json:"nodes"` // other nodes, from gossip
	Gossip     Gossip       `json:"gossip"`
	Reconcile  Reconcile    `json:"reconcile"`
	Process    Process      `json:"process"`
	Warnings   []Warning    `json:"warnings"`
}

// Node is this node's identity.
type Node struct {
	Callsign     string `json:"callsign"`
	NodeID       int    `json:"node_id"`
	NodeIDSource string `json:"node_id_source"` // config, derived or claimed
	MAC          string `json:"mac"`
	Address      string `json:"address"` // CIDR on br-mesh
}

// Profile is the mesh profile in force, without secrets.
type Profile struct {
	Name           string `json:"name"`
	MeshID         string `json:"mesh_id"`
	Band           string `json:"band"`
	Channel        int    `json:"channel"`
	FreqMHz        int    `json:"freq_mhz"`
	WidthMHz       int    `json:"width_mhz"`
	Prefix         string `json:"prefix"`
	PinnedBatman   string `json:"pinned_batman"`
	MinBatman      string `json:"min_batman"`
	OrigIntervalMS int    `json:"orig_interval_ms"`
}

// Regulatory shows the requested mode and what is actually in force.
type Regulatory struct {
	RequestedMode string `json:"requested_mode"` // unrestricted or country
	Country       string `json:"country"`
	ActiveMode    string `json:"active_mode"` // country (unrestricted is not available yet)
	KernelAlpha2  string `json:"kernel_alpha2"`
	Note          string `json:"note,omitempty"`
}

// PowerFact is a maximum transmit power figure and its source.
type PowerFact struct {
	DBm    float64 `json:"dbm"`
	Source string  `json:"source"`
}

// Radio is one physical radio as the Radios page shows it.
type Radio struct {
	Fingerprint       string               `json:"fingerprint"`
	FingerprintSource string               `json:"fingerprint_source"`
	Phy               string               `json:"phy"`
	Label             string               `json:"label,omitempty"`
	Chipset           string               `json:"chipset,omitempty"`
	Driver            string               `json:"driver"`
	Bus               string               `json:"bus"`
	Port              string               `json:"port"`
	DeviceID          string               `json:"device_id,omitempty"`
	USBSpeedMbps      int                  `json:"usb_speed_mbps,omitempty"`
	PermAddr          string               `json:"perm_addr"`
	Bands             []string             `json:"bands"`
	Streams           int                  `json:"streams"`
	MeshPoint         bool                 `json:"mesh_point"`
	AP                bool                 `json:"ap"`
	MeshAndAP         bool                 `json:"mesh_and_ap"`
	Power             map[string]PowerFact `json:"power"`
	Role              string               `json:"role"`
	Recommended       string               `json:"recommended"`
	Interfaces        []string             `json:"interfaces"`
	Quirks            []string             `json:"quirks,omitempty"`
	Ignored           bool                 `json:"ignored,omitempty"`
	Broken            string               `json:"broken,omitempty"`
	HardBlocked       bool                 `json:"hard_blocked,omitempty"`
	InUse             string               `json:"in_use,omitempty"`
	TXPowerCapDBm     *int                 `json:"tx_power_cap_dbm,omitempty"`
}

// Plan is the role assignment result.
type Plan struct {
	MeshRadio   string    `json:"mesh_radio,omitempty"`
	APRadio     string    `json:"ap_radio,omitempty"`
	APBand      string    `json:"ap_band,omitempty"`
	Notices     []Warning `json:"notices"`
	Differs     bool      `json:"recommendation_differs"`
	GraceActive bool      `json:"grace_active,omitempty"` // a lost radio is being debounced
}

// MeshLink is the 802.11s link state.
type MeshLink struct {
	Interface   string     `json:"interface,omitempty"`
	Transport   string     `json:"transport"` // wifi, wired, none
	Supplicant  Supplicant `json:"supplicant"`
	FreqMHz     int        `json:"freq_mhz,omitempty"`
	MTU         int        `json:"mtu,omitempty"`
	TXPowerDBm  float64    `json:"tx_power_dbm,omitempty"`
	Peers       []Peer     `json:"peers"`
	SAEFailures []SAEEvent `json:"sae_failures,omitempty"`
	WiredPorts  []string   `json:"wired_ports,omitempty"`
}

// Supplicant is wpa_supplicant's state.
type Supplicant struct {
	Running  bool   `json:"running"`
	PID      int    `json:"pid,omitempty"`
	Restarts int    `json:"restarts,omitempty"`
	State    string `json:"state,omitempty"` // wpa_state
	Joined   bool   `json:"joined"`
}

// Peer is a mesh peer as nl80211 reports it.
type Peer struct {
	MAC              string  `json:"mac"`
	SignalDBm        int     `json:"signal_dbm"`
	TXMbps           float64 `json:"tx_mbps"`
	RXMbps           float64 `json:"rx_mbps"`
	InactiveMS       uint32  `json:"inactive_ms"`
	ConnectedSeconds uint32  `json:"connected_s"`
	PlinkState       string  `json:"plink_state"`
	Established      bool    `json:"established"`
}

// SAEEvent records a rejected peer.
type SAEEvent struct {
	Time    time.Time `json:"time"`
	MAC     string    `json:"mac"`
	Kind    string    `json:"kind"` // failure or blocked
	Message string    `json:"message"`
}

// Batman is the batman-adv state.
type Batman struct {
	Interface      string         `json:"interface"`
	Present        bool           `json:"present"`
	ModuleVersion  string         `json:"module_version"`
	ModuleInferred bool           `json:"module_inferred,omitempty"`
	ModulePath     string         `json:"module_path,omitempty"`
	PinnedMismatch bool           `json:"pinned_mismatch"`
	Algo           string         `json:"algo"`
	MAC            string         `json:"mac"`
	MTU            int            `json:"mtu"`
	MTUOK          bool           `json:"mtu_ok"`
	HardIfs        []HardIf       `json:"hard_interfaces"`
	Neighbors      []Neighbor     `json:"neighbors"`
	Originators    []Originator   `json:"originators"`
	Gateways       []Gateway      `json:"gateways"`
	Settings       map[string]any `json:"settings"`
	Multicast      Multicast      `json:"multicast"`
}

// HardIf is a batman-adv hard interface.
type HardIf struct {
	Name   string `json:"name"`
	MAC    string `json:"mac"`
	Active bool   `json:"active"`
	MTU    int    `json:"mtu"`
}

// Neighbor is a single-hop neighbour.
type Neighbor struct {
	MAC            string  `json:"mac"`
	Via            string  `json:"via"`
	LastSeenMS     uint32  `json:"last_seen_ms"`
	ThroughputMbps float64 `json:"throughput_mbps"`
}

// Originator is a known node and the route to it.
type Originator struct {
	MAC            string  `json:"mac"`
	NextHop        string  `json:"next_hop"`
	Via            string  `json:"via"`
	LastSeenMS     uint32  `json:"last_seen_ms"`
	ThroughputMbps float64 `json:"throughput_mbps"`
	Best           bool    `json:"best"`
}

// Gateway is an announced internet gateway.
type Gateway struct {
	MAC            string  `json:"mac"`
	Router         string  `json:"router"`
	DownMbps       float64 `json:"down_mbps"`
	UpMbps         float64 `json:"up_mbps"`
	ThroughputMbps float64 `json:"throughput_mbps"`
	Best           bool    `json:"best"`
}

// Multicast summarises batman-adv's multicast flags.
type Multicast struct {
	Optimisations        bool   `json:"optimisations"`
	OwnFlags             uint32 `json:"own_flags"`
	Bridged              bool   `json:"bridged"`
	QuerierIPv4          bool   `json:"querier_ipv4"`
	QuerierIPv4Shadowing bool   `json:"querier_ipv4_shadowing"`
	QuerierIPv6          bool   `json:"querier_ipv6"`
	QuerierIPv6Shadowing bool   `json:"querier_ipv6_shadowing"`
	PeersWithFlags       int    `json:"peers_with_flags"`
	LegacyPeers          int    `json:"legacy_peers"` // peers without multicast optimisations
	// WantAllIPv4/IPv6: this node asks for every group (no usable querier).
	WantAllIPv4 bool `json:"want_all_ipv4"`
	WantAllIPv6 bool `json:"want_all_ipv6"`
	// PeersWantAllIPv4: nodes that receive every IPv4 group, so every
	// IPv4 multicast packet is sent to them too.
	PeersWantAllIPv4 int `json:"peers_want_all_ipv4"`
	PeersNoPtype     int `json:"peers_no_mc_ptype"` // peers without the multicast packet type
	// Path is a one-word summary: "efficient" (sent only where listeners
	// are), "partial" (some nodes still get everything) or "flooding".
	Path     string        `json:"path"`
	Counters McastCounters `json:"counters"`
}

// McastCounters are batman-adv's multicast packet type counters on this
// node, since bat0 was created ("batctl statistics"): proof that the
// efficient path is used.
type McastCounters struct {
	TX       uint64 `json:"tx"` // sent with the multicast packet type
	TXBytes  uint64 `json:"tx_bytes"`
	TXLocal  uint64 `json:"tx_local"` // of those, delivered to this node's own clients too
	RX       uint64 `json:"rx"`       // received with the multicast packet type
	RXLocal  uint64 `json:"rx_local"` // received and delivered to this node's clients
	Fwd      uint64 `json:"fwd"`      // forwarded on to further nodes
	FwdBytes uint64 `json:"fwd_bytes"`
}

// Bridge is br-mesh.
type Bridge struct {
	Name    string   `json:"name"`
	Present bool     `json:"present"`
	Ports   []string `json:"ports"`
	Address string   `json:"address,omitempty"`
	MTU     int      `json:"mtu,omitempty"`
}

// Reconcile summarises the control loop.
type Reconcile struct {
	Passes       uint64           `json:"passes"`
	LastPass     time.Time        `json:"last_pass"`
	LastDuration string           `json:"last_duration"`
	Converged    bool             `json:"converged"`
	Reconcilers  []ReconcilerInfo `json:"reconcilers"`
}

// ReconcilerInfo is one reconciler's recent history.
type ReconcilerInfo struct {
	Name        string    `json:"name"`
	LastError   string    `json:"last_error,omitempty"`
	LastChanged time.Time `json:"last_changed,omitempty"`
	Changes     uint64    `json:"changes"`
	Errors      uint64    `json:"errors"`
}

// Warning is a problem the operator should know about.
type Warning struct {
	Level   string `json:"level"` // info, warning, error
	Code    string `json:"code"`
	Message string `json:"message"`
	Source  string `json:"source,omitempty"`
}

// Process is meshd's own resource use, for spotting leaks.
type Process struct {
	PID        int `json:"pid"`
	OpenFiles  int `json:"open_files"`
	Goroutines int `json:"goroutines"`
}

// Helper is the state of a supervised helper process (hostapd, dnsmasq).
type Helper struct {
	Running  bool   `json:"running"`
	PID      int    `json:"pid,omitempty"`
	Restarts int    `json:"restarts,omitempty"`
	State    string `json:"state,omitempty"`
}

// AP is the client access point (architecture §10).
type AP struct {
	Enabled         bool     `json:"enabled"`
	Interface       string   `json:"interface,omitempty"`
	Radio           string   `json:"radio,omitempty"` // fingerprint
	SharesMeshRadio bool     `json:"shares_mesh_radio"`
	SSID            string   `json:"ssid,omitempty"`
	Band            string   `json:"band,omitempty"`
	Channel         int      `json:"channel,omitempty"`
	FreqMHz         int      `json:"freq_mhz,omitempty"`
	Hostapd         Helper   `json:"hostapd"`
	TXPowerDBm      float64  `json:"tx_power_dbm,omitempty"`
	Clients         []Client `json:"clients"`
}

// Client is a station joined to the AP, with its lease when known.
type Client struct {
	MAC              string  `json:"mac"`
	SignalDBm        int     `json:"signal_dbm"`
	TXMbps           float64 `json:"tx_mbps"`
	RXMbps           float64 `json:"rx_mbps"`
	InactiveMS       uint32  `json:"inactive_ms"`
	ConnectedSeconds uint32  `json:"connected_s"`
	IP               string  `json:"ip,omitempty"`
	Hostname         string  `json:"hostname,omitempty"`
}

// ClientNet is the flat client network on this node (architecture §7).
type ClientNet struct {
	Anycast Anycast `json:"anycast"`
	Filters Filters `json:"filters"`
	DHCP    DHCP    `json:"dhcp"`
	Leases  []Lease `json:"leases"`
}

// Anycast is the gateway address every node answers for.
type Anycast struct {
	Interface string `json:"interface"`
	Present   bool   `json:"present"`
	Address   string `json:"address,omitempty"`
	MAC       string `json:"mac,omitempty"`
}

// Filters is the bridge-family nftables ruleset on bat0 and client ports.
type Filters struct {
	Applied bool              `json:"applied"`
	Rules   int               `json:"rules,omitempty"`
	Drops   map[string]uint64 `json:"drops,omitempty"` // frames dropped, by rule purpose
	Error   string            `json:"error,omitempty"`
}

// DHCP is this node's dnsmasq.
type DHCP struct {
	Dnsmasq   Helper `json:"dnsmasq"`
	PoolStart string `json:"pool_start,omitempty"`
	PoolEnd   string `json:"pool_end,omitempty"`
	Domain    string `json:"domain,omitempty"`
}

// Lease is a DHCP lease this node handed out.
type Lease struct {
	MAC      string    `json:"mac"`
	IP       string    `json:"ip"`
	Hostname string    `json:"hostname,omitempty"`
	Expires  time.Time `json:"expires"`
}

// APJoin is what a phone needs to join this node's AP.
type APJoin struct {
	Enabled    bool   `json:"enabled"`
	SSID       string `json:"ssid"`
	Passphrase string `json:"passphrase"`
	Band       string `json:"band,omitempty"`
	Channel    int    `json:"channel,omitempty"`
	WiFiQR     string `json:"wifi_qr"`
	Note       string `json:"note,omitempty"`
}

// MeshNode is another node as gossip describes it.
type MeshNode struct {
	Callsign    string   `json:"callsign"`
	NodeID      int      `json:"node_id"`
	Address     string   `json:"address"`
	MAC         string   `json:"mac"`
	Originators []string `json:"originators,omitempty"`
	Version     string   `json:"version,omitempty"`
	Gateway     bool     `json:"gateway"`     // announces a WAN
	WANHealthy  bool     `json:"wan_healthy"` // and it is healthy
	HeardAgoMS  int64    `json:"heard_ago_ms"`
	IDConflict  bool     `json:"id_conflict,omitempty"`
}

// GatewayState is this node's gateway role and its choice of gateway
// (architecture §8).
type GatewayState struct {
	Mode         string             `json:"mode"` // auto or off
	Role         string             `json:"role"` // gateway, client
	WAN          string             `json:"wan,omitempty"`
	WANHealthy   bool               `json:"wan_healthy"`
	WANKnown     bool               `json:"wan_known"`
	WANSince     time.Time          `json:"wan_since,omitempty"`
	RTTms        float64            `json:"rtt_ms,omitempty"`
	LossPct      int                `json:"loss_pct"`
	ProbeError   string             `json:"probe_error,omitempty"`
	ProbeTargets []string           `json:"probe_targets,omitempty"`
	Announced    string             `json:"announced,omitempty"` // e.g. 20/5 Mbit/s
	Selected     *SelectedGateway   `json:"selected,omitempty"`
	Candidates   []GatewayCandidate `json:"candidates"`
	DNS          []string           `json:"dns"`
	Switches     int                `json:"switches"`
}

// SelectedGateway is where this node's clients reach the internet.
type SelectedGateway struct {
	Self           bool      `json:"self"`
	Callsign       string    `json:"callsign"`
	NodeID         int       `json:"node_id"`
	Address        string    `json:"address"`
	ThroughputMbps float64   `json:"throughput_mbps,omitempty"`
	Since          time.Time `json:"since"`
}

// GatewayCandidate is a gateway batman-adv announces, and whether it can be
// used.
type GatewayCandidate struct {
	Originator     string  `json:"originator"`
	Callsign       string  `json:"callsign,omitempty"`
	NodeID         int     `json:"node_id,omitempty"`
	Address        string  `json:"address,omitempty"`
	ThroughputMbps float64 `json:"throughput_mbps"`
	LastSeenMS     uint32  `json:"last_seen_ms"`
	Usable         bool    `json:"usable"`
	Why            string  `json:"why,omitempty"` // why not usable
}

// Port is a wired port: its configured role and, for auto, what it became.
type Port struct {
	Name       string `json:"name"`
	Configured string `json:"configured"` // role in node.yaml; "" for an unlisted port treated as auto
	// Role in effect: wan, client, mesh, or for auto ports also probing,
	// no-link, peer (another Trellis node is on the cable), in-use (the
	// system uses it; left alone) or missing.
	Role    string     `json:"role"`
	Why     string     `json:"why,omitempty"`
	Carrier bool       `json:"carrier"`
	Since   time.Time  `json:"since,omitzero"`
	Lease   *PortLease `json:"lease,omitempty"` // auto WAN: the lease meshd holds
}

// PortLease is a DHCP lease held by meshd on an auto WAN port.
type PortLease struct {
	Address string    `json:"address"` // CIDR
	Router  string    `json:"router,omitempty"`
	DNS     []string  `json:"dns,omitempty"`
	Server  string    `json:"server,omitempty"`
	Expires time.Time `json:"expires"`
}

// Gossip is this node's gossip traffic since meshd started (architecture
// §12.5). Records and bytes are gossip packets as sent or received on
// br-mesh, before batman-adv carries them.
type Gossip struct {
	Sent            uint64 `json:"sent"`
	Received        uint64 `json:"received"`
	BytesSent       uint64 `json:"bytes_sent"`
	BytesReceived   uint64 `json:"bytes_received"`
	LastRecordBytes int    `json:"last_record_bytes"`
	AuthFailed      uint64 `json:"auth_failed"`
	Ignored         uint64 `json:"ignored"`
	LastError       string `json:"last_error,omitempty"`
}
