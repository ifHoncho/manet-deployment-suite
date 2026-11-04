// Package meshctl is the operator CLI: status, radios, doctor, config
// validation and profile generation.
package meshctl

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"meridian/trellis/internal/api"
	"meridian/trellis/internal/config"
	"meridian/trellis/internal/doctor"
	"meridian/trellis/internal/hwdb"
	"meridian/trellis/internal/status"
)

// Main runs meshctl with the given arguments (without the program name).
func Main(version string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("meshctl", flag.ContinueOnError)
	fs.SetOutput(stderr)
	socket := fs.String("socket", "", "meshd API socket (default "+config.DefaultRunDir+"/meshd.sock)")
	fs.Usage = func() {
		fmt.Fprintf(stderr, `Usage: meshctl [--socket PATH] <command> [args]

Commands:
  status [--json] [--watch]      node status from meshd
  radios [--json]                radios, capabilities and roles
  doctor [--json] [--node PATH] [--profile PATH]
                                 host preflight checks; paste the output when asking for help
  config validate [node.yaml] [profile.yaml]
                                 validate configuration files without applying them
  profile new --name NAME [--band 5g] [--channel 149] [--country US] [--mode country|unrestricted] [-o FILE [--force]]
                                 new mesh profile with a random passphrase (stdout, or FILE with mode 0600)
  profile show [FILE]            print this node's mesh profile (to copy it to other nodes)
  profile band 2g|5g|6g [--channel N] [--width MHz] [FILE]
                                 switch a profile file to another band (default file: mesh-profile.yaml)
  profile install [FILE|-]       validate a profile, install it on this node and apply it live
  node new --callsign NAME [--id N] [-o FILE [--force]]
                                 node config skeleton (stdout, or FILE)
  ap [--json]                    client AP name and passphrase (as a QR code when qrencode is installed)
  clients [--json]               devices joined to this node: AP clients and DHCP leases
  reconcile                      ask meshd for a pass now
  reload                         ask meshd to re-read its configuration
  version

`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := api.NewClient(*socket)
	var err error
	switch rest[0] {
	case "status":
		err = cmdStatus(ctx, client, rest[1:], stdout, stderr)
	case "radios":
		err = cmdRadios(ctx, client, rest[1:], stdout)
	case "doctor":
		err = cmdDoctor(version, client.Socket, rest[1:], stdout)
	case "config":
		err = cmdConfig(rest[1:], stdout)
	case "profile":
		err = cmdProfile(ctx, client, rest[1:], stdout)
	case "node":
		err = cmdNode(rest[1:], stdout)
	case "ap":
		err = cmdAP(ctx, client, rest[1:], stdout)
	case "clients":
		err = cmdClients(ctx, client, rest[1:], stdout)
	case "reconcile":
		if err = client.Reconcile(ctx); err == nil {
			fmt.Fprintln(stdout, "reconcile requested")
		}
	case "reload":
		if err = client.Reload(ctx); err == nil {
			fmt.Fprintln(stdout, "configuration reloaded")
		}
	case "driver-options":
		// For node-update.sh: modprobe.d lines from the hardware database.
		var db *hwdb.DB
		if db, err = hwdb.Load(); err == nil {
			for _, l := range db.ModprobeOptions() {
				fmt.Fprintln(stdout, l)
			}
		}
	case "version":
		fmt.Fprintln(stdout, version)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", rest[0])
		fs.Usage()
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		if errors.Is(err, api.ErrNotRunning) {
			fmt.Fprintln(stderr, "hint: start it with 'systemctl start trellis-meshd', or run 'meshctl doctor'")
		}
		return 1
	}
	return 0
}

func cmdStatus(ctx context.Context, c *api.Client, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print JSON")
	watch := fs.Bool("watch", false, "refresh every 2 seconds")
	if err := fs.Parse(args); err != nil {
		return err
	}
	for {
		st, err := c.Status(ctx)
		if err != nil {
			return err
		}
		if *asJSON {
			enc := json.NewEncoder(stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(st)
		}
		if *watch {
			fmt.Fprint(stdout, "\033[H\033[2J")
		}
		PrintStatus(stdout, st)
		if !*watch {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
}

// PrintStatus renders the human-readable status.
func PrintStatus(w io.Writer, st *status.Status) {
	fmt.Fprintf(w, "%s  node %d (%s)  mac %s  trellis %s  up %s\n", st.Node.Callsign, st.Node.NodeID, st.Node.NodeIDSource, st.Node.MAC, st.Version, time.Since(st.StartedAt).Round(time.Second))
	fmt.Fprintf(w, "mesh %q  %s ch %d (%d MHz, %d MHz wide)  prefix %s  address %s\n", st.Profile.MeshID, st.Profile.Band, st.Profile.Channel, st.Profile.FreqMHz, st.Profile.WidthMHz, st.Profile.Prefix, st.Node.Address)
	kdom := st.Regulatory.KernelAlpha2
	if kdom == "" {
		kdom = "unknown"
	}
	if st.Regulatory.RequestedMode == config.RegModeUnrestricted {
		fmt.Fprintf(w, "regulatory: unrestricted requested (NOT AVAILABLE IN THIS BUILD), using country %s, kernel domain %s\n", st.Regulatory.Country, kdom)
	} else {
		fmt.Fprintf(w, "regulatory: country %s, kernel domain %s\n", st.Regulatory.Country, kdom)
	}

	fmt.Fprintf(w, "\nRadios (%d):\n", len(st.Radios))
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  PHY\tROLE\tREC\tCHIPSET/DRIVER\tBUS\tPORT\tBANDS\tSTREAMS\tPOWER\tCAPS\tINTERFACES\tFINGERPRINT")
	for _, r := range st.Radios {
		name := r.Chipset
		if name == "" {
			name = r.Driver
		}
		if r.Label != "" {
			name = r.Label + " (" + name + ")"
		}
		bus := r.Bus
		if r.USBSpeedMbps > 0 {
			bus = fmt.Sprintf("%s %d", bus, r.USBSpeedMbps)
		}
		var powers []string
		bands := append([]string(nil), r.Bands...)
		sort.Strings(bands)
		for _, b := range bands {
			if p, ok := r.Power[b]; ok {
				powers = append(powers, fmt.Sprintf("%s %.0f(%s)", b, p.DBm, p.Source[:1]))
			}
		}
		var caps []string
		if r.MeshPoint {
			caps = append(caps, "mesh")
		}
		if r.AP {
			caps = append(caps, "ap")
		}
		if r.MeshAndAP {
			caps = append(caps, "both")
		}
		if r.Ignored {
			caps = append(caps, "IGNORED")
		}
		if r.Broken != "" {
			caps = append(caps, "no-mesh")
		}
		if r.InUse != "" {
			caps = append(caps, "SYSTEM-IN-USE")
		}
		rec := r.Recommended
		if rec == r.Role {
			rec = "="
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n", r.Phy, r.Role, rec, name, bus, r.Port, strings.Join(r.Bands, "/"), r.Streams,
			strings.Join(powers, " "), strings.Join(caps, ","), strings.Join(r.Interfaces, ","), r.Fingerprint)
	}
	tw.Flush()
	if st.Plan.GraceActive {
		fmt.Fprintln(w, "  (a radio disappeared; roles are held for a few seconds in case it returns)")
	}

	printMeshLink(w, "Mesh link", st.MeshLink, st, true)
	if st.MeshLink2 != nil {
		printMeshLink(w, "Mesh link 2", *st.MeshLink2, st, false)
	}

	b := st.Batman
	fmt.Fprintf(w, "\nbatman-adv: ")
	if !b.Present {
		fmt.Fprintf(w, "%s not present; module %q\n", b.Interface, b.ModuleVersion)
	} else {
		mismatch := ""
		if b.PinnedMismatch {
			mismatch = " (differs from pinned " + st.Profile.PinnedBatman + ")"
		}
		fmt.Fprintf(w, "%s %s mac %s mtu %d, module %s%s\n", b.Interface, b.Algo, b.MAC, b.MTU, b.ModuleVersion, mismatch)
		var hifs []string
		for _, h := range b.HardIfs {
			state := "inactive"
			if h.Active {
				state = "active"
			}
			hifs = append(hifs, fmt.Sprintf("%s(%s, mtu %d)", h.Name, state, h.MTU))
		}
		if len(hifs) == 0 {
			hifs = []string{"none"}
		}
		fmt.Fprintf(w, "  hard interfaces: %s\n", strings.Join(hifs, " "))
		// Peers of every mesh link: batman-adv routes over either.
		peers := append([]status.Peer{}, st.MeshLink.Peers...)
		if st.MeshLink2 != nil {
			peers = append(peers, st.MeshLink2.Peers...)
		}
		printRoutes(w, b, peers, nodeNames(st))
		if gs := st.Gossip; gs.Sent > 0 || gs.Received > 0 {
			fmt.Fprintf(w, "  gossip: %d nodes known; sent %d records (%s, last %d bytes), received %d (%s)", len(st.Nodes), gs.Sent, humanBytes(gs.BytesSent), gs.LastRecordBytes, gs.Received, humanBytes(gs.BytesReceived))
			if gs.AuthFailed > 0 {
				fmt.Fprintf(w, ", %d failed authentication", gs.AuthFailed)
			}
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "  multicast: %s; gateways: %d\n", mcastSummary(b.Multicast), len(b.Gateways))
	}

	br := st.Bridge
	if br.Present {
		fmt.Fprintf(w, "bridge: %s %s ports %s mtu %d\n", br.Name, br.Address, portList(br.Ports), br.MTU)
	}
	printPorts(w, st)
	printClientNet(w, st)
	printGateway(w, st)

	if st.Reconcile.LastPass.IsZero() {
		fmt.Fprintf(w, "\nreconcile: first pass still running\n")
	} else {
		fmt.Fprintf(w, "\nreconcile: %d passes, last %s ago took %s, converged=%v; meshd pid %d, %d open files\n", st.Reconcile.Passes, time.Since(st.Reconcile.LastPass).Round(time.Second), st.Reconcile.LastDuration, st.Reconcile.Converged, st.Process.PID, st.Process.OpenFiles)
	}
	if len(st.Warnings) > 0 {
		fmt.Fprintf(w, "\nWarnings (%d):\n", len(st.Warnings))
		for _, wn := range st.Warnings {
			fmt.Fprintf(w, "  [%s] %s: %s\n", strings.ToUpper(wn.Level), wn.Code, wn.Message)
		}
	}
}

func cmdRadios(ctx context.Context, c *api.Client, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("radios", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := c.Status(ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{"radios": st.Radios, "plan": st.Plan})
	}
	for _, r := range st.Radios {
		fmt.Fprintf(stdout, "%s  role=%s recommended=%s\n", r.Phy, r.Role, r.Recommended)
		fmt.Fprintf(stdout, "  fingerprint: %s (%s)\n", r.Fingerprint, r.FingerprintSource)
		fmt.Fprintf(stdout, "  hardware: %s driver %s, %s %s, id %s, perm mac %s\n", r.Chipset, r.Driver, r.Bus, r.Port, r.DeviceID, r.PermAddr)
		fmt.Fprintf(stdout, "  capabilities: mesh=%v ap=%v both=%v bands=%s streams=%d\n", r.MeshPoint, r.AP, r.MeshAndAP, strings.Join(r.Bands, "/"), r.Streams)
		for b, p := range r.Power {
			fmt.Fprintf(stdout, "  max power %s: %.1f dBm (%s)\n", b, p.DBm, p.Source)
		}
		if len(r.Interfaces) > 0 {
			fmt.Fprintf(stdout, "  interfaces: %s\n", strings.Join(r.Interfaces, ", "))
		}
		for _, q := range r.Quirks {
			fmt.Fprintf(stdout, "  quirk: %s\n", q)
		}
		if r.Broken != "" {
			fmt.Fprintf(stdout, "  broken: %s\n", r.Broken)
		}
		if r.InUse != "" {
			fmt.Fprintf(stdout, "  in use by the system, left alone: %s\n", r.InUse)
		}
	}
	for _, n := range st.Plan.Notices {
		fmt.Fprintf(stdout, "[%s] %s: %s\n", strings.ToUpper(n.Level), n.Code, n.Message)
	}
	return nil
}

func cmdDoctor(version, socket string, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print JSON")
	nodePath := fs.String("node", config.DefaultNodePath, "node config to validate")
	profilePath := fs.String("profile", "", "mesh profile to validate (default: from the node config)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rep := doctor.Run(doctor.Options{Version: version, NodePath: *nodePath, ProfilePath: *profilePath, Socket: socket})
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	fmt.Fprint(stdout, rep.Text())
	if rep.Fail > 0 {
		return fmt.Errorf("%d check(s) failed", rep.Fail)
	}
	return nil
}

func cmdConfig(args []string, stdout io.Writer) error {
	if len(args) == 0 || args[0] != "validate" {
		return errors.New("usage: meshctl config validate [node.yaml] [profile.yaml]")
	}
	nodePath, profilePath := config.DefaultNodePath, ""
	if len(args) > 1 {
		nodePath = args[1]
	}
	if len(args) > 2 {
		profilePath = args[2]
	}
	failed := false
	node, err := config.LoadNode(nodePath)
	if err != nil {
		fmt.Fprintf(stdout, "node config %s: INVALID\n  %s\n", nodePath, strings.ReplaceAll(err.Error(), "\n", "\n  "))
		failed = true
	} else {
		fmt.Fprintf(stdout, "node config %s: OK (callsign %s)\n", nodePath, node.Callsign)
		if profilePath == "" {
			profilePath = node.ProfilePath
		}
	}
	if profilePath != "" {
		if p, err := config.LoadProfile(profilePath); err != nil {
			fmt.Fprintf(stdout, "mesh profile %s: INVALID\n  %s\n", profilePath, strings.ReplaceAll(err.Error(), "\n", "\n  "))
			failed = true
		} else {
			fmt.Fprintf(stdout, "mesh profile %s: OK (mesh %q, %s channel %d)\n", profilePath, p.MeshID, p.Radio.Band, p.Radio.Channel)
		}
	}
	if failed {
		return errors.New("configuration is invalid")
	}
	return nil
}

const passphraseAlphabet = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"

func randomPassphrase(n int) (string, error) {
	var b strings.Builder
	for i := 0; i < n; i++ {
		if i > 0 && i%6 == 0 {
			b.WriteByte('-')
		}
		idx, err := rand.Int(rand.Reader, big.NewInt(int64(len(passphraseAlphabet))))
		if err != nil {
			return "", err
		}
		b.WriteByte(passphraseAlphabet[idx.Int64()])
	}
	return b.String(), nil
}

// DefaultChannel is the channel a band switch picks when none is given:
// non-DFS, usable in the US and most of Europe.
var DefaultChannel = map[string]int{config.Band2G: 6, config.Band5G: 149, config.Band6G: 37}

func cmdProfile(ctx context.Context, client *api.Client, args []string, stdout io.Writer) error {
	if len(args) > 0 && args[0] == "band" {
		return cmdProfileBand(args[1:], stdout)
	}
	if len(args) > 0 && args[0] == "install" {
		return cmdProfileInstall(ctx, client, args[1:], stdout)
	}
	if len(args) > 0 && args[0] == "show" {
		path := config.DefaultProfilePath
		if len(args) > 1 {
			path = args[1]
		}
		if _, err := config.LoadProfile(path); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		_, err = stdout.Write(data)
		return err
	}
	if len(args) == 0 || args[0] != "new" {
		return errors.New("usage: meshctl profile new --name NAME [--band 5g] [--channel 149] [--country US] [--mode country]")
	}
	fs := flag.NewFlagSet("profile new", flag.ContinueOnError)
	name := fs.String("name", "", "mesh name (required)")
	band := fs.String("band", "5g", "mesh band: 2g, 5g or 6g")
	channel := fs.Int("channel", 0, "channel (default 149 on 5g, 6 on 2g)")
	country := fs.String("country", "US", "ISO country code")
	mode := fs.String("mode", config.RegModeCountry, "regulatory mode: country or unrestricted")
	width := fs.Int("width", 20, "channel width in MHz")
	out := fs.String("o", "", "write to this file (mode 0600) instead of stdout")
	force := fs.Bool("force", false, "overwrite an existing file")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *name == "" {
		return errors.New("--name is required")
	}
	if *channel == 0 {
		switch *band {
		case "2g":
			*channel = 6
		case "6g":
			*channel = 37
		default:
			*channel = 149
		}
	}
	pass, err := randomPassphrase(24)
	if err != nil {
		return err
	}
	p := config.Profile{
		Schema: config.SchemaVersion, Name: *name, MeshID: "trellis-" + *name, SAEPassphrase: pass,
		Regulatory: config.Regulatory{Mode: *mode, Country: strings.ToUpper(*country)},
		Radio:      config.RadioPlan{Band: *band, Channel: *channel, Width: *width},
	}
	p.ApplyDefaults()
	if err := p.Validate(); err != nil {
		return err
	}
	return emit(stdout, *out, *force, 0o600, renderProfile(&p))
}

// renderProfile writes a profile as commented YAML.
func renderProfile(p *config.Profile) string {
	var buf strings.Builder
	fmt.Fprintf(&buf, `# Trellis mesh profile. Share this file (or its QR code) with every node in
# the mesh. It holds the mesh secret: keep it private.
schema: %d
name: %s
mesh_id: %s
sae_passphrase: %s
regulatory:
  mode: %s          # unrestricted needs the Trellis regulatory database (not yet shipped); country mode applies %s limits
  country: %s
radio:
  band: %s
  channel: %d
  width: %d
prefix: %s
batman:
  routing_algo: %s
  orig_interval_ms: %d
  min_version: "%s"
  pinned_version: "%s"
`, p.Schema, p.Name, p.MeshID, p.SAEPassphrase, p.Regulatory.Mode, p.Regulatory.Country, p.Regulatory.Country,
		p.Radio.Band, p.Radio.Channel, p.Radio.Width, p.Prefix, p.Batman.RoutingAlgo, p.Batman.OrigIntervalMS, p.Batman.MinVersion, p.Batman.PinnedVersion)
	return buf.String()
}

// emit prints content, or writes it to path atomically with the given mode.
// An existing file is only replaced with force.
func emit(stdout io.Writer, path string, force bool, mode os.FileMode, content string) error {
	if path == "" {
		_, err := io.WriteString(stdout, content)
		return err
	}
	if _, err := os.Stat(path); err == nil && !force {
		return fmt.Errorf("%s already exists; not overwriting it (use --force to replace it)", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), mode); err != nil {
		if errors.Is(err, os.ErrPermission) {
			return fmt.Errorf("%w (run with sudo)", err)
		}
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	fmt.Fprintf(stdout, "wrote %s\n", path)
	return nil
}

func cmdNode(args []string, stdout io.Writer) error {
	if len(args) == 0 || args[0] != "new" {
		return errors.New("usage: meshctl node new --callsign NAME [--id N]")
	}
	fs := flag.NewFlagSet("node new", flag.ContinueOnError)
	callsign := fs.String("callsign", "", "node callsign, a DNS label (required)")
	id := fs.Int("id", 0, "pinned node id 1–254 (0 = automatic)")
	out := fs.String("o", "", "write to this file instead of stdout")
	force := fs.Bool("force", false, "overwrite an existing file")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *callsign == "" {
		return errors.New("--callsign is required")
	}
	n := config.Node{Schema: config.SchemaVersion, Callsign: *callsign, NodeID: *id}
	n.ApplyDefaults()
	if err := n.Validate(); err != nil {
		return err
	}
	var buf strings.Builder
	fmt.Fprintf(&buf, `# Trellis node configuration for one node.
schema: %d
callsign: %s
node_id: %d            # 0 = claim automatically (derived from the node MAC until gossip lands)
profile: %s

radios:
  roles:
    mesh: auto         # auto | disabled | {radio: <fingerprint>} | {port: usb:1-1.2}
    ap: auto
  devices: []          # per-radio: {match: {fingerprint: ...}, label: ..., ignore: false, tx_power_dbm: 20}

ports: []              # - {name: eth0, role: auto}   roles: auto | wan | client | mesh (wired backhaul)

ap:
  enabled: true        # activated in M2
  country: US
  band: 5g

# tx_power_dbm: 20     # cap the mesh radio below the regulatory maximum
mesh_mtu: %d
allow_old_batman: false
`, n.Schema, n.Callsign, n.NodeID, n.ProfilePath, n.MeshMTU)
	return emit(stdout, *out, *force, 0o644, buf.String())
}

// PrintJSON is a helper for the main package.
func PrintJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// cmdProfileBand switches a profile file to another band.
func cmdProfileBand(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("profile band", flag.ContinueOnError)
	channel := fs.Int("channel", 0, "channel (default: 6 on 2g, 149 on 5g, 37 on 6g)")
	width := fs.Int("width", 0, "channel width in MHz (default: keep, or 20 if the band does not allow it)")
	// Accept "band 2g [flags] [FILE]" in any order.
	var pos []string
	rest := args
	for len(rest) > 0 {
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	if len(pos) == 0 || len(pos) > 2 {
		return errors.New("usage: meshctl profile band 2g|5g|6g [--channel N] [--width MHz] [FILE (default mesh-profile.yaml)]")
	}
	band, path := pos[0], "mesh-profile.yaml"
	if len(pos) == 2 {
		path = pos[1]
	}
	p, err := config.LoadProfile(path)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	old := p.Radio
	p.Radio.Band = band
	p.Radio.Channel = *channel
	if p.Radio.Channel == 0 {
		p.Radio.Channel = DefaultChannel[band]
	}
	if *width != 0 {
		p.Radio.Width = *width
	} else if band == config.Band2G && p.Radio.Width > 40 {
		p.Radio.Width = 20
	}
	if err := p.Validate(); err != nil {
		return err
	}
	if p.Radio == old {
		fmt.Fprintf(stdout, "%s already uses %s channel %d (%d MHz wide)\n", path, band, p.Radio.Channel, p.Radio.Width)
		return nil
	}
	if err := emit(io.Discard, path, true, 0o600, renderProfile(p)); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s: %s ch %d (%d MHz) -> %s ch %d (%d MHz)\n", path, old.Band, old.Channel, old.Width, p.Radio.Band, p.Radio.Channel, p.Radio.Width)
	return nil
}

// cmdProfileInstall validates a profile and installs it on this node, then
// asks meshd to apply it.
func cmdProfileInstall(ctx context.Context, client *api.Client, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("profile install", flag.ContinueOnError)
	noReload := fs.Bool("no-reload", false, "only write the file")
	target := fs.String("target", "", "where to install (default: the profile path in node.yaml)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	src := "-"
	if fs.NArg() > 0 {
		src = fs.Arg(0)
	}
	var data []byte
	var err error
	if src == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(src)
	}
	if err != nil {
		return err
	}
	p, err := config.ParseProfile(data)
	if err != nil {
		return fmt.Errorf("profile rejected, nothing installed: %w", err)
	}
	dst := *target
	if dst == "" {
		dst = config.DefaultProfilePath
		if n, err := config.LoadNode(config.DefaultNodePath); err == nil && n.ProfilePath != "" {
			dst = n.ProfilePath
		}
	}
	if cur, err := os.ReadFile(dst); err == nil && bytes.Equal(cur, data) {
		fmt.Fprintf(stdout, "%s unchanged (%s ch %d)\n", dst, p.Radio.Band, p.Radio.Channel)
		return nil
	}
	if err := emit(io.Discard, dst, true, 0o600, string(data)); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "installed %s (mesh %q, %s ch %d, %d MHz)\n", dst, p.MeshID, p.Radio.Band, p.Radio.Channel, p.Radio.Width)
	if *noReload {
		return nil
	}
	if err := client.Reload(ctx); err != nil {
		if errors.Is(err, api.ErrNotRunning) {
			fmt.Fprintln(stdout, "meshd is not running; the profile applies when it starts")
			return nil
		}
		return fmt.Errorf("installed, but meshd rejected the reload: %w", err)
	}
	fmt.Fprintln(stdout, "meshd is applying it now")
	return nil
}

// meshRadioMaxDBm returns the known maximum transmit power of the mesh radio
// on the mesh band.
func meshRadioMaxDBm(st *status.Status) (float64, bool) {
	for _, r := range st.Radios {
		if !strings.Contains(r.Role, "mesh") {
			continue
		}
		if pf, ok := r.Power[st.Profile.Band]; ok && pf.DBm > 0 {
			return pf.DBm, true
		}
	}
	return 0, false
}

// saeSummary condenses SAE events to one line per peer. Events from before a
// peer's current established link are dropped: they are history, not a
// problem.
func saeSummary(events []status.SAEEvent, peers []status.Peer, now time.Time) []string {
	estabSince := map[string]time.Time{}
	for _, p := range peers {
		if p.Established {
			estabSince[p.MAC] = now.Add(-time.Duration(p.ConnectedSeconds) * time.Second)
		}
	}
	type agg struct {
		failures    int
		last        time.Time
		blocked     bool
		blockedTime time.Time
	}
	var order []string
	byMAC := map[string]*agg{}
	for _, e := range events {
		if since, ok := estabSince[e.MAC]; ok && !e.Time.After(since) {
			continue
		}
		a := byMAC[e.MAC]
		if a == nil {
			a = &agg{}
			byMAC[e.MAC] = a
			order = append(order, e.MAC)
		}
		if e.Kind == "blocked" {
			a.blocked, a.blockedTime = true, e.Time
		} else {
			a.failures++
			if e.Time.After(a.last) {
				a.last = e.Time
			}
		}
	}
	ago := func(t time.Time) string { return now.Sub(t).Round(time.Second).String() + " ago" }
	var out []string
	for _, mac := range order {
		a := byMAC[mac]
		line := "SAE " + mac + ":"
		if a.failures > 0 {
			noun := "failure"
			if a.failures > 1 {
				noun = "failures"
			}
			line += fmt.Sprintf(" %d authentication %s, last %s", a.failures, noun, ago(a.last))
		}
		if a.blocked {
			if a.failures > 0 {
				line += ";"
			}
			line += " blocked for 300 s " + ago(a.blockedTime)
		}
		line += ". Usually a different passphrase (that node needs this mesh profile); if the profiles match, suspect that node's driver"
		out = append(out, line)
	}
	return out
}

func portList(ports []string) string {
	if len(ports) == 0 {
		return "none"
	}
	return strings.Join(ports, ",")
}

// printLinks shows each direct link once: the 802.11s peer link (radio
// layer) next to batman-adv's estimate of what it can carry. batman
// neighbours without a radio peer (wired ports, or a peer that has just
// gone) are listed with blank radio fields.
func printLinks(w io.Writer, peers []status.Peer, neighbors []status.Neighbor, names map[string]string) {
	if len(peers) == 0 && len(neighbors) == 0 {
		return
	}
	est := map[string]status.Neighbor{}
	for _, n := range neighbors {
		est[n.MAC] = n
	}
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  DIRECT LINK\tNODE\tSTATE\tSIGNAL\tRADIO TX/RX\tBATMAN EST.\tINACTIVE\tCONNECTED")
	seen := map[string]bool{}
	for _, p := range peers {
		seen[p.MAC] = true
		sig := "-"
		if p.SignalDBm != 0 {
			sig = fmt.Sprintf("%d dBm", p.SignalDBm)
		}
		inactive := fmt.Sprintf("%d ms", p.InactiveMS)
		if p.InactiveMS >= 10000 {
			inactive = (time.Duration(p.InactiveMS) * time.Millisecond).Round(time.Second).String()
		}
		bat := "-"
		if n, ok := est[p.MAC]; ok {
			bat = fmt.Sprintf("%.1f Mbit/s", n.ThroughputMbps)
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%.0f/%.0f Mbit/s\t%s\t%s\t%s\n", p.MAC, nameOf(names, p.MAC), p.PlinkState, sig, p.TXMbps, p.RXMbps, bat, inactive, (time.Duration(p.ConnectedSeconds) * time.Second).String())
	}
	for _, n := range neighbors {
		if seen[n.MAC] {
			continue
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t-\t-\t%.1f Mbit/s\t%d ms\t-\n", n.MAC, nameOf(names, n.MAC), "batman only ("+n.Via+")", n.ThroughputMbps, n.LastSeenMS)
	}
	tw.Flush()
}

// printRoutes shows how traffic reaches every other node: straight to it, or
// relayed through a neighbour. batman-adv only knows the first hop, so a
// relayed route may be longer than two hops; batctl tr shows the full path.
func printRoutes(w io.Writer, b status.Batman, peers []status.Peer, names map[string]string) {
	var routes []status.Originator
	for _, o := range b.Originators {
		if o.Best {
			routes = append(routes, o)
		}
	}
	if len(routes) == 0 {
		fmt.Fprintln(w, "  no other nodes seen")
		return
	}
	estab := map[string]bool{}
	for _, p := range peers {
		if p.Established {
			estab[p.MAC] = true
		}
	}
	direct := func(o status.Originator) bool { return o.NextHop == o.MAC }
	sort.SliceStable(routes, func(i, j int) bool {
		if di, dj := direct(routes[i]), direct(routes[j]); di != dj {
			return di
		}
		return routes[i].MAC < routes[j].MAC
	})
	multiIf := len(b.HardIfs) > 1
	relayed := 0
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  ROUTE TO\tNODE\tPATH\tPATH THROUGHPUT\tLAST SEEN")
	for _, o := range routes {
		path := "direct"
		if !direct(o) {
			relayed++
			path = "relayed via " + o.NextHop
			if n := names[strings.ToLower(o.NextHop)]; n != "" {
				path = "relayed via " + n
			}
			if estab[o.MAC] {
				path += " (beats the direct link)"
			}
		}
		if multiIf {
			path += " on " + o.Via
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%.1f Mbit/s\t%d ms\n", o.MAC, nameOf(names, o.MAC), path, o.ThroughputMbps, o.LastSeenMS)
	}
	tw.Flush()
	fmt.Fprintf(w, "  %d other nodes: %d direct, %d relayed (batctl tr <mac> shows every hop)\n", len(routes), len(routes)-relayed, relayed)
}

// printClientNet renders the anycast gateway, filters, DHCP and the client
// AP with its clients.
func printClientNet(w io.Writer, st *status.Status) {
	cn, ap := st.ClientNet, st.AP
	var parts []string
	if cn.Anycast.Present {
		parts = append(parts, "gateway "+cn.Anycast.Address+" on "+cn.Anycast.Interface)
	} else {
		parts = append(parts, "gateway not up")
	}
	switch {
	case cn.Filters.Applied:
		var dropped uint64
		for _, n := range cn.Filters.Drops {
			dropped += n
		}
		parts = append(parts, fmt.Sprintf("bat0 filters on (%d rules, %d frames dropped)", cn.Filters.Rules, dropped))
	case cn.Filters.Error != "":
		parts = append(parts, "bat0 filters FAILED")
	default:
		parts = append(parts, "bat0 filters off")
	}
	if cn.DHCP.Dnsmasq.Running {
		parts = append(parts, fmt.Sprintf("dnsmasq pid %d (DHCP %s–%s, DNS .%s)", cn.DHCP.Dnsmasq.PID, cn.DHCP.PoolStart, cn.DHCP.PoolEnd, cn.DHCP.Domain))
	} else {
		parts = append(parts, "dnsmasq not running")
	}
	fmt.Fprintf(w, "client network: %s\n", strings.Join(parts, "; "))

	switch {
	case !ap.Enabled:
		fmt.Fprintln(w, "Client AP: disabled in node.yaml")
	case ap.Interface == "":
		fmt.Fprintln(w, "Client AP: no radio available (see warnings)")
	case ap.Channel == 0:
		fmt.Fprintf(w, "Client AP: %s not started (see warnings)\n", ap.Interface)
	default:
		shared := ""
		if ap.SharesMeshRadio {
			shared = ", shares the mesh radio"
		}
		state := "hostapd " + orDash(ap.Hostapd.State)
		if !ap.Hostapd.Running {
			state = "hostapd not running"
			if ap.Hostapd.Restarts > 0 {
				state = fmt.Sprintf("hostapd keeps exiting (%d times, see the journal)", ap.Hostapd.Restarts)
			}
		}
		fmt.Fprintf(w, "Client AP: %s %q %s ch %d (%d MHz)%s, %s, %d clients", ap.Interface, ap.SSID, ap.Band, ap.Channel, ap.FreqMHz, shared, state, len(ap.Clients))
		if ap.TXPowerDBm > 0 {
			fmt.Fprintf(w, ", tx %.0f dBm", ap.TXPowerDBm)
		}
		fmt.Fprintln(w)
	}
	if len(ap.Clients) > 0 {
		tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "  CLIENT\tSIGNAL\tTX/RX Mbps\tCONNECTED\tADDRESS\tHOSTNAME")
		for _, c := range ap.Clients {
			fmt.Fprintf(tw, "  %s\t%d dBm\t%.0f/%.0f\t%s\t%s\t%s\n", c.MAC, c.SignalDBm, c.TXMbps, c.RXMbps,
				(time.Duration(c.ConnectedSeconds) * time.Second).String(), orDash(c.IP), orDash(c.Hostname))
		}
		tw.Flush()
	}
	wired := wiredLeases(st)
	if len(wired) > 0 {
		tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "  OTHER LEASE\tADDRESS\tHOSTNAME\tEXPIRES")
		for _, l := range wired {
			fmt.Fprintf(tw, "  %s\t%s\t%s\tin %s\n", l.MAC, l.IP, orDash(l.Hostname), time.Until(l.Expires).Round(time.Minute))
		}
		tw.Flush()
	}
}

// wiredLeases returns leases whose holder is not an AP client: wired
// devices, or phones that have left but still hold an address.
func wiredLeases(st *status.Status) []status.Lease {
	joined := map[string]bool{}
	for _, c := range st.AP.Clients {
		joined[strings.ToLower(c.MAC)] = true
	}
	var out []status.Lease
	for _, l := range st.ClientNet.Leases {
		if !joined[strings.ToLower(l.MAC)] && time.Until(l.Expires) > 0 {
			out = append(out, l)
		}
	}
	return out
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func cmdAP(ctx context.Context, c *api.Client, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("ap", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	j, err := c.APJoin(ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(j)
	}
	fmt.Fprintf(stdout, "SSID:       %s\n", j.SSID)
	fmt.Fprintf(stdout, "passphrase: %s\n", j.Passphrase)
	if j.Channel != 0 {
		fmt.Fprintf(stdout, "channel:    %s ch %d\n", j.Band, j.Channel)
	}
	if j.Note != "" {
		fmt.Fprintf(stdout, "note:       %s\n", j.Note)
	}
	fmt.Fprintf(stdout, "QR payload: %s\n", j.WiFiQR)
	if qr, err := exec.LookPath("qrencode"); err == nil {
		out, err := exec.Command(qr, "-t", "ANSIUTF8", "-m", "1", j.WiFiQR).Output()
		if err == nil {
			fmt.Fprintln(stdout)
			stdout.Write(out)
		}
	} else {
		fmt.Fprintln(stdout, "(install qrencode to see this as a QR code phones can scan)")
	}
	return nil
}

func cmdClients(ctx context.Context, c *api.Client, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("clients", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := c.Status(ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{"ap_clients": st.AP.Clients, "leases": st.ClientNet.Leases})
	}
	if len(st.AP.Clients) == 0 && len(wiredLeases(st)) == 0 {
		fmt.Fprintln(stdout, "no clients joined to this node")
		return nil
	}
	printClientNet(stdout, st)
	return nil
}

// nodeNames maps mesh-interface MACs to callsigns, from gossip.
func nodeNames(st *status.Status) map[string]string {
	m := map[string]string{}
	for _, n := range st.Nodes {
		label := n.Callsign
		if label == "" {
			label = fmt.Sprintf("node %d", n.NodeID)
		}
		for _, o := range n.Originators {
			m[strings.ToLower(o)] = label
		}
		m[strings.ToLower(n.MAC)] = label
	}
	return m
}

func nameOf(names map[string]string, mac string) string {
	if n := names[strings.ToLower(mac)]; n != "" {
		return n
	}
	return "?"
}

// printMeshLink renders one mesh link: interface, supplicant, channel,
// power and its peers. first is the profile's radio (mesh0), the link the
// driver-power hint is about.
func printMeshLink(w io.Writer, title string, ml status.MeshLink, st *status.Status, first bool) {
	fmt.Fprintf(w, "\n%s: transport %s", title, ml.Transport)
	if ml.Interface != "" {
		fmt.Fprintf(w, ", %s mtu %d", ml.Interface, ml.MTU)
		sup := "wpa_supplicant stopped"
		if ml.Supplicant.Running {
			sup = fmt.Sprintf("wpa_supplicant pid %d state %s", ml.Supplicant.PID, ml.Supplicant.State)
		}
		fmt.Fprintf(w, ", %s", sup)
		if ml.FreqMHz > 0 {
			fmt.Fprintf(w, ", %d MHz", ml.FreqMHz)
		}
		if ml.TXPowerDBm > 0 {
			fmt.Fprintf(w, ", tx %.0f dBm", ml.TXPowerDBm)
			// Some drivers (rt2800usb, for one) report the regulatory limit
			// rather than what the hardware transmits.
			if hw, ok := meshRadioMaxDBm(st); first && ok && ml.TXPowerDBm > hw {
				fmt.Fprintf(w, " (driver figure; radio max %.0f)", hw)
			}
		}
	} else if !first {
		fmt.Fprint(w, " (no radio for the second link; see the plan notices)")
	}
	if len(ml.WiredPorts) > 0 {
		fmt.Fprintf(w, ", wired %s", strings.Join(ml.WiredPorts, ","))
	}
	fmt.Fprintln(w)
	names := nodeNames(st)
	printLinks(w, ml.Peers, st.Batman.Neighbors, names)
	for _, line := range saeSummary(ml.SAEFailures, ml.Peers, time.Now()) {
		fmt.Fprintf(w, "  %s\n", line)
	}
}

// printPorts renders the wired ports: configured roles, and what auto
// ports became (architecture §7.1).
func printPorts(w io.Writer, st *status.Status) {
	if len(st.Ports) == 0 {
		return
	}
	fmt.Fprintf(w, "Ethernet ports (%d):\n", len(st.Ports))
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  PORT\tCONFIG\tROLE\tLINK\tDETAIL")
	for _, p := range st.Ports {
		cfg := p.Configured
		if cfg == "" {
			cfg = "(auto)"
		}
		link := "down"
		if p.Carrier {
			link = "up"
		}
		detail := p.Why
		if l := p.Lease; l != nil {
			detail = fmt.Sprintf("lease %s via %s from %s, expires in %s", l.Address, orDash(l.Router), l.Server, time.Until(l.Expires).Round(time.Second))
		}
		if !p.Since.IsZero() && p.Role != "in-use" {
			detail += fmt.Sprintf(" (%s for %s)", p.Role, time.Since(p.Since).Round(time.Second))
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n", p.Name, cfg, p.Role, link, strings.TrimSpace(detail))
	}
	tw.Flush()
}

// printGateway renders the Internet section: this node's WAN, the selected
// gateway, and the gateways batman-adv knows.
func printGateway(w io.Writer, st *status.Status) {
	g := st.Gateway
	if g.Mode == "" {
		return
	}
	wan := "no uplink of its own"
	if g.WAN != "" {
		switch {
		case g.WANHealthy:
			wan = fmt.Sprintf("WAN %s healthy (%.0f ms, %d%% loss)", g.WAN, g.RTTms, g.LossPct)
		case !g.WANKnown:
			wan = fmt.Sprintf("WAN %s being checked", g.WAN)
		default:
			wan = fmt.Sprintf("WAN %s DOWN (%s)", g.WAN, orDash(g.ProbeError))
		}
	}
	if g.Mode == "off" {
		wan += ", sharing off"
	}
	sel := "NONE: clients reach the mesh only"
	if s := g.Selected; s != nil {
		if s.Self {
			sel = "this node (sharing it, announced " + g.Announced + ")"
		} else {
			sel = fmt.Sprintf("%s %s, %.1f Mbit/s path", s.Callsign, s.Address, s.ThroughputMbps)
		}
		sel += fmt.Sprintf(" for %s", time.Since(s.Since).Round(time.Second))
	}
	fmt.Fprintf(w, "Internet: %s; gateway in use: %s", wan, sel)
	if len(g.DNS) > 0 {
		fmt.Fprintf(w, "; DNS via %s", strings.Join(g.DNS, ","))
	}
	fmt.Fprintln(w)
	if len(g.Candidates) > 0 {
		tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "  GATEWAY\tNODE\tADDRESS\tPATH THROUGHPUT\tLAST SEEN\tUSABLE")
		for _, c := range g.Candidates {
			usable := "yes"
			if !c.Usable {
				usable = "no: " + c.Why
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%.1f Mbit/s\t%d ms\t%s\n", c.Originator, orDash(c.Callsign), orDash(c.Address), c.ThroughputMbps, c.LastSeenMS, usable)
		}
		tw.Flush()
	}
}

// humanBytes renders a byte count with a binary unit.
func humanBytes(n uint64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// mcastSummary explains the multicast path in one line (architecture §9).
func mcastSummary(m status.Multicast) string {
	querier := func(exists, shadowing bool) string {
		switch {
		case !exists:
			return "none"
		case shadowing:
			return "shadowed"
		}
		return "ok"
	}
	path := m.Path
	if path == "" {
		path = "unknown"
	}
	var parts []string
	switch path {
	case "efficient":
		parts = append(parts, "efficient (sent only to nodes with listeners)")
	case "partial":
		n := m.PeersWantAllIPv4
		if m.WantAllIPv4 {
			n++
		}
		parts = append(parts, fmt.Sprintf("partial (%d nodes receive every group)", n))
	default:
		parts = append(parts, path)
	}
	parts = append(parts, fmt.Sprintf("IGMP querier %s, MLD querier %s", querier(m.QuerierIPv4, m.QuerierIPv4Shadowing), querier(m.QuerierIPv6, m.QuerierIPv6Shadowing)))
	if m.LegacyPeers > 0 {
		parts = append(parts, fmt.Sprintf("%d peers without optimisations", m.LegacyPeers))
	}
	if m.PeersNoPtype > 0 {
		parts = append(parts, fmt.Sprintf("%d peers without the multicast packet type", m.PeersNoPtype))
	}
	c := m.Counters
	parts = append(parts, fmt.Sprintf("packet type tx %d rx %d fwd %d", c.TX, c.RX, c.Fwd))
	return strings.Join(parts, "; ")
}
