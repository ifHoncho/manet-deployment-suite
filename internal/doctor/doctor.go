// Package doctor runs the host preflight checks from architecture §6.1 and
// hardware.md, and produces a single paste-able report.
package doctor

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"meridian/trellis/internal/batadv"
	"meridian/trellis/internal/config"
	"meridian/trellis/internal/hwdb"
	"meridian/trellis/internal/nl80211"
	"meridian/trellis/internal/proc"
	"meridian/trellis/internal/radio"
	"meridian/trellis/internal/status"
	"meridian/trellis/internal/sysfs"
)

// Check statuses.
const (
	Pass = "PASS"
	Warn = "WARN"
	Fail = "FAIL"
	Skip = "SKIP"
	Info = "INFO"
)

// Check is one finding.
type Check struct {
	Status string `json:"status"`
	Name   string `json:"name"`
	Detail string `json:"detail,omitempty"`
	Fix    string `json:"fix,omitempty"`
}

// Report is the whole run.
type Report struct {
	Time     time.Time `json:"time"`
	Version  string    `json:"version"`
	Hostname string    `json:"hostname"`
	Kernel   string    `json:"kernel"`
	Arch     string    `json:"arch"`
	OS       string    `json:"os"`
	Checks   []Check   `json:"checks"`
	Pass     int       `json:"pass"`
	Warn     int       `json:"warn"`
	Fail     int       `json:"fail"`
}

// Options controls a run.
type Options struct {
	Version     string
	SysRoot     string // "/sys"
	NodePath    string
	ProfilePath string
	Socket      string // meshd API socket
	MinBatman   string
	PinBatman   string
}

func (r *Report) add(st, name, detail, fix string) {
	r.Checks = append(r.Checks, Check{Status: st, Name: name, Detail: detail, Fix: fix})
	switch st {
	case Pass:
		r.Pass++
	case Warn:
		r.Warn++
	case Fail:
		r.Fail++
	}
}

// Run performs every check it can.
func Run(opts Options) *Report {
	if opts.SysRoot == "" {
		opts.SysRoot = sysfs.DefaultRoot
	}
	if opts.MinBatman == "" {
		opts.MinBatman = "2024.0"
	}
	if opts.PinBatman == "" {
		opts.PinBatman = "2026.3"
	}
	r := &Report{Time: time.Now(), Version: opts.Version, Arch: runtime.GOARCH}
	r.Hostname, _ = os.Hostname()
	r.Kernel = kernelRelease()
	r.OS = osRelease()

	checkPlatform(r)
	checkKernelVersion(r)
	checkKernelConfig(r)
	checkHeaders(r)
	checkBatman(r, opts)
	checkTools(r)
	checkListeners(r)
	checkRadios(r, opts)
	checkRegulatory(r)
	checkNetworkManagers(r)
	checkConfigs(r, opts)
	checkMeshd(r, opts)
	return r
}

func kernelRelease() string {
	b, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(b))
}

func osRelease() string {
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return "unknown"
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "PRETTY_NAME=") {
			return strings.Trim(strings.TrimPrefix(line, "PRETTY_NAME="), `"`)
		}
	}
	return "unknown"
}

func checkPlatform(r *Report) {
	if _, err := os.Stat("/etc/openwrt_release"); err == nil {
		r.add(Fail, "operating system", "OpenWrt (for example FriendlyWrt) is not supported for self-install yet",
			"use the board vendor's Debian or Ubuntu image (FriendlyElec: Debian Core or Ubuntu Core)")
	}
	if b, err := os.ReadFile("/proc/device-tree/model"); err == nil {
		r.add(Info, "board", strings.TrimRight(string(b), "\x00\n"), "")
	}
	out, err := exec.Command("dpkg", "--print-architecture").Output()
	if err != nil {
		return
	}
	uarch := strings.TrimSpace(string(out))
	karch := runtime.GOARCH
	if u, err := exec.Command("uname", "-m").Output(); err == nil {
		karch = strings.TrimSpace(string(u))
	}
	if karch == "aarch64" && uarch == "armhf" {
		r.add(Fail, "architecture", "64-bit kernel with a 32-bit operating system: kernel modules (batman-adv) cannot be built",
			"install the 64-bit OS, or add arm_64bit=0 to /boot/firmware/config.txt and reboot")
	} else {
		r.add(Info, "architecture", "kernel "+karch+", userland "+uarch, "")
	}
	if mem := memTotalMB(); mem > 0 && mem < 700 {
		r.add(Info, "memory", fmt.Sprintf("%d MB: the batman-adv build runs one compiler job at a time and takes several minutes", mem), "")
	}
}

func memTotalMB() int {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		var kb int
		if n, _ := fmt.Sscanf(line, "MemTotal: %d kB", &kb); n == 1 {
			return kb / 1024
		}
	}
	return 0
}

var reKV = regexp.MustCompile(`^(\d+)\.(\d+)`)

func checkKernelVersion(r *Report) {
	m := reKV.FindStringSubmatch(r.Kernel)
	if m == nil {
		r.add(Warn, "kernel version", "cannot parse "+r.Kernel, "")
		return
	}
	maj, _ := strconv.Atoi(m[1])
	min_, _ := strconv.Atoi(m[2])
	if maj > 5 || (maj == 5 && min_ >= 15) {
		r.add(Pass, "kernel version", r.Kernel+" (5.15 or newer)", "")
	} else {
		r.add(Fail, "kernel version", r.Kernel+" is older than 5.15", "use an image or a Trellis-built kernel")
	}
}

// KernelConfig reads the running kernel's configuration from /proc/config.gz
// or /boot/config-<release>.
func KernelConfig(release string) (map[string]string, string, error) {
	var rd io.Reader
	var src string
	if _, err := os.Stat("/proc/config.gz"); err != nil {
		// Raspberry Pi kernels build /proc/config.gz as a module.
		_ = exec.Command("modprobe", "configs").Run()
	}
	if f, err := os.Open("/proc/config.gz"); err == nil {
		defer f.Close()
		gz, err := gzip.NewReader(f)
		if err != nil {
			return nil, "", err
		}
		defer gz.Close()
		rd, src = gz, "/proc/config.gz"
	} else {
		var f *os.File
		var err error
		for _, p := range []string{"/boot/config-" + release, "/lib/modules/" + release + "/build/.config"} {
			if f, err = os.Open(p); err == nil {
				src = p
				break
			}
		}
		if f == nil {
			return nil, "", fmt.Errorf("no /proc/config.gz, /boot/config-%s or headers .config", release)
		}
		defer f.Close()
		rd = f
	}
	cfg := map[string]string{}
	sc := bufio.NewScanner(rd)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "CONFIG_") {
			if k, v, ok := strings.Cut(line, "="); ok {
				cfg[k] = v
			}
		}
	}
	return cfg, src, sc.Err()
}

func checkKernelConfig(r *Report) {
	cfg, src, err := KernelConfig(r.Kernel)
	if err != nil {
		r.add(Warn, "kernel config", "not readable: "+err.Error()+"; the checks below are skipped", "install the kernel's config (linux-image or /proc/config.gz support)")
		return
	}
	r.add(Info, "kernel config", "read from "+src, "")
	switch cfg["CONFIG_BATMAN_ADV"] {
	case "y":
		r.add(Fail, "CONFIG_BATMAN_ADV", "=y: batman-adv is built into the kernel and cannot be replaced by the pinned module", "use a Trellis-built kernel for this board")
	case "m":
		r.add(Pass, "CONFIG_BATMAN_ADV", "=m", "")
	default:
		r.add(Pass, "CONFIG_BATMAN_ADV", "unset (the DKMS module will provide it)", "")
	}
	if v := cfg["CONFIG_BRIDGE"]; v == "y" || v == "m" {
		r.add(Pass, "CONFIG_BRIDGE", "="+v, "")
	} else {
		r.add(Fail, "CONFIG_BRIDGE", "missing", "Trellis-built kernel required")
	}
	if cfg["CONFIG_BRIDGE_IGMP_SNOOPING"] == "y" {
		r.add(Pass, "CONFIG_BRIDGE_IGMP_SNOOPING", "=y", "")
	} else {
		r.add(Fail, "CONFIG_BRIDGE_IGMP_SNOOPING", "missing: batman-adv multicast optimisations need bridge snooping", "Trellis-built kernel required")
	}
	if cfg["CONFIG_MAC80211_MESH"] == "y" {
		r.add(Pass, "CONFIG_MAC80211_MESH", "=y", "")
	} else {
		r.add(Fail, "CONFIG_MAC80211_MESH", "missing: no 802.11s mesh point support", "Trellis-built kernel required")
	}
	nft := cfg["CONFIG_NF_TABLES"]
	nftb := cfg["CONFIG_NF_TABLES_BRIDGE"]
	if (nft == "y" || nft == "m") && (nftb == "y" || nftb == "m") {
		r.add(Pass, "CONFIG_NF_TABLES_BRIDGE", "="+nftb, "")
	} else {
		r.add(Fail, "CONFIG_NF_TABLES_BRIDGE", "missing: bat0 bridge filters need nftables with the bridge family", "Trellis-built kernel required")
	}
	if cfg["CONFIG_CFG80211_REQUIRE_SIGNED_REGDB"] == "y" {
		r.add(Info, "CONFIG_CFG80211_REQUIRE_SIGNED_REGDB", "=y: a Trellis regulatory.db for unrestricted mode must be signed with a key this kernel trusts", "")
	} else {
		r.add(Info, "CONFIG_CFG80211_REQUIRE_SIGNED_REGDB", "unset: an unsigned regulatory.db is accepted", "")
	}
	if v := cfg["CONFIG_MAC80211_HWSIM"]; v == "m" || v == "y" {
		r.add(Info, "CONFIG_MAC80211_HWSIM", "="+v+" (virtual radios available for lab tests)", "")
	}
}

func checkHeaders(r *Report) {
	p := "/lib/modules/" + r.Kernel + "/build"
	if st, err := os.Stat(p); err == nil && st.IsDir() {
		if _, err := os.Stat(filepath.Join(p, "Makefile")); err == nil {
			r.add(Pass, "kernel headers", p, "")
			return
		}
		r.add(Warn, "kernel headers", p+" exists but has no Makefile", "install the headers package for this kernel")
		return
	}
	r.add(Fail, "kernel headers", p+" is missing: DKMS cannot build batman-adv", "install linux-headers for "+r.Kernel+" (or a Trellis-built kernel)")
}

func checkBatman(r *Report, opts Options) {
	minV, _ := batadv.ParseVersion(opts.MinBatman)
	pinV, _ := batadv.ParseVersion(opts.PinBatman)
	mi := batadv.ReadModuleInfo()
	path, modVersion := modinfo("batman-adv")
	if !mi.Loaded {
		if path == "" {
			r.add(Fail, "batman-adv module", "not loaded and modinfo finds no module", "install the pinned DKMS build: scripts/install-batman-adv-dkms.sh")
			return
		}
		mi = batadv.ModuleInfo{Raw: modVersion}
		if v, err := batadv.ParseVersion(modVersion); err == nil {
			mi.Release = v
		} else {
			mi.InTreeKernelVersion = modVersion
		}
	}
	v, inferred := mi.EffectiveRelease()
	where := ""
	if path != "" {
		where = " at " + path
		if strings.Contains(path, "/updates/") {
			where += " (DKMS build)"
		} else {
			where += " (kernel's own copy)"
		}
	}
	state := "loaded"
	if !mi.Loaded {
		state = "not loaded"
	}
	desc := fmt.Sprintf("%s, version %s%s", state, mi.Raw, where)
	if inferred {
		desc += fmt.Sprintf("; release inferred as %s from the kernel version", v)
	}
	switch {
	case v.IsZero():
		r.add(Warn, "batman-adv module", desc+"; version unknown", "")
	case v.Less(minV):
		r.add(Fail, "batman-adv module", desc+fmt.Sprintf("; below the minimum %s", minV), "install the pinned DKMS build: scripts/install-batman-adv-dkms.sh")
	case v != pinV || inferred:
		r.add(Warn, "batman-adv module", desc+fmt.Sprintf("; differs from the pinned release %s", pinV), "install the pinned DKMS build so every node runs identical code")
	default:
		r.add(Pass, "batman-adv module", desc, "")
	}
	if out, err := exec.Command("dkms", "status").CombinedOutput(); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if strings.Contains(line, "batman") {
				r.add(Info, "dkms status", strings.TrimSpace(line), "")
			}
		}
	}
}

func modinfo(name string) (path, version string) {
	out, err := exec.Command("modinfo", name).Output()
	if err != nil {
		return "", ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch k {
		case "filename":
			path = v
		case "version":
			version = v
		}
	}
	return path, version
}

func checkTools(r *Report) {
	type tool struct {
		name, why string
		required  bool
	}
	for _, t := range []tool{
		{"wpa_supplicant", "802.11s mesh with SAE", true},
		{"hostapd", "client AP", false},
		{"dnsmasq", "DHCP and DNS for clients", false},
		{"nft", "bat0 filters; without them the anycast gateway stays off", false},
		{"iw", "diagnostics", false},
		{"batctl", "diagnostics", false},
		{"dkms", "pinned batman-adv on self-install systems", false},
	} {
		p, err := proc.LookPath(t.name)
		switch {
		case err == nil && t.name == "wpa_supplicant":
			r.add(Pass, t.name, p+" "+wpaVersion(p), "")
		case err == nil:
			r.add(Pass, t.name, p, "")
		case t.required:
			r.add(Fail, t.name, "not found ("+t.why+")", "apt install "+aptName(t.name))
		default:
			r.add(Warn, t.name, "not found ("+t.why+")", "apt install "+aptName(t.name))
		}
	}
}

func aptName(tool string) string {
	switch tool {
	case "wpa_supplicant":
		return "wpasupplicant"
	case "nft":
		return "nftables"
	case "dnsmasq":
		return "dnsmasq-base"
	}
	return tool
}

// checkListeners warns about other programs holding the DNS and DHCP
// ports on every address: Trellis's dnsmasq binds its own addresses only,
// and a wildcard listener on port 53 stops it starting.
func checkListeners(r *Report) {
	wild := wildcardUDPListeners()
	if wild[53] {
		r.add(Warn, "port 53", "another DNS server listens on all addresses (a system dnsmasq or similar); Trellis's dnsmasq cannot bind the mesh addresses",
			"disable it: systemctl disable --now dnsmasq (install dnsmasq-base, not dnsmasq); systemd-resolved on 127.0.0.53 is fine")
	} else {
		r.add(Pass, "port 53", "no wildcard DNS listener", "")
	}
}

// wildcardUDPListeners returns UDP ports bound to 0.0.0.0 or ::.
func wildcardUDPListeners() map[int]bool {
	out := map[int]bool{}
	for _, f := range []string{"/proc/net/udp", "/proc/net/udp6"} {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for i, line := range strings.Split(string(data), "\n") {
			if i == 0 {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			addr, portHex, ok := strings.Cut(fields[1], ":")
			if !ok {
				continue
			}
			if strings.Trim(addr, "0") != "" {
				continue
			}
			p, err := strconv.ParseInt(portHex, 16, 32)
			if err == nil {
				out[int(p)] = true
			}
		}
	}
	return out
}

func wpaVersion(p string) string {
	out, _ := exec.Command(p, "-v").CombinedOutput()
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "wpa_supplicant v") {
			return strings.TrimPrefix(line, "wpa_supplicant ")
		}
	}
	return ""
}

func checkRadios(r *Report, opts Options) {
	nl, err := nl80211.Dial()
	if err != nil {
		r.add(Warn, "radios", "nl80211 unavailable: "+err.Error(), "")
		return
	}
	defer nl.Close()
	db, _ := hwdb.Load()
	radios, err := radio.Inventory(nl, opts.SysRoot, db, config.RadiosConfig{})
	if err != nil {
		r.add(Warn, "radios", "inventory failed: "+err.Error(), "")
		return
	}
	if len(radios) == 0 {
		r.add(Warn, "radios", "no Wi-Fi radios found", "plug in a mesh-capable adapter; see docs/hardware.md")
		return
	}
	meshCapable := 0
	for _, rd := range radios {
		caps := []string{}
		if rd.Caps.MeshPoint {
			caps = append(caps, "mesh point")
			meshCapable++
		}
		if rd.Caps.AP {
			caps = append(caps, "AP")
		}
		if rd.Caps.MeshAndAP {
			caps = append(caps, "mesh+AP together")
		}
		chip := rd.Device.Driver
		if rd.HWDB != nil {
			chip = rd.HWDB.Chipset + " (" + rd.Device.Driver + ")"
		}
		powers := []string{}
		for _, b := range rd.Caps.Bands {
			if p, ok := rd.Power[b]; ok {
				powers = append(powers, fmt.Sprintf("%s %.0f dBm (%s)", b, p.DBm, p.Source))
			}
		}
		ifs := []string{}
		for _, in := range rd.Interfaces {
			ifs = append(ifs, in.Name+"/"+in.TypeName())
		}
		detail := fmt.Sprintf("%s on %s (%s), id %s, bands %s, %d streams, %s; power %s; interfaces %s; fingerprint %s (%s)",
			chip, rd.Device.Port, rd.Device.Bus, rd.Device.DeviceID(), strings.Join(rd.Caps.Bands, "/"), rd.Caps.Streams,
			strings.Join(caps, ", "), strings.Join(powers, ", "), strings.Join(ifs, ","), rd.Fingerprint, rd.FingerprintSource)
		st := Pass
		if !rd.Caps.MeshPoint || rd.Broken != "" {
			st = Warn
			if rd.Broken != "" {
				detail += "; " + rd.Broken
			}
			if !rd.Caps.MeshPoint {
				detail += "; no 802.11s mesh point support, so it can only serve as the AP"
			}
		}
		if rd.InUse != "" {
			detail += "; IN USE BY THE SYSTEM (" + rd.InUse + "): meshd leaves it alone unless node.yaml pins a role to it"
		}
		r.add(st, "radio "+rd.Phy, detail, "")
	}
	if meshCapable == 0 {
		r.add(Fail, "mesh-capable radios", "none of the radios supports mesh point", "use an adapter from docs/hardware.md")
	} else {
		r.add(Pass, "mesh-capable radios", fmt.Sprint(meshCapable), "")
	}
}

func checkRegulatory(r *Report) {
	nl, err := nl80211.Dial()
	if err != nil {
		return
	}
	defer nl.Close()
	rd, err := nl.GetReg()
	if err != nil {
		r.add(Warn, "regulatory domain", err.Error(), "")
		return
	}
	note := ""
	if rd.Alpha2 == "00" {
		note = " (world domain; normal before meshd starts, which sets the profile's country)"
	}
	r.add(Info, "regulatory domain", rd.Alpha2+note, "")

	// Kernels requiring a signed database ignore every country request when
	// they reject /lib/firmware/regulatory.db, and stay in 00.
	if _, err := os.Stat("/lib/firmware/regulatory.db"); err != nil {
		r.add(Fail, "regulatory.db", "/lib/firmware/regulatory.db is missing; the kernel cannot leave the world domain", "apt install wireless-regdb")
		return
	}
	out, err := exec.Command("dmesg").Output()
	if err != nil {
		r.add(Info, "regulatory.db", "kernel log not readable as this user; signature check skipped", "sudo meshctl doctor")
		return
	}
	switch {
	case strings.Contains(string(out), "regulatory.db is malformed or signature is missing/invalid"):
		r.add(Fail, "regulatory.db", "the kernel rejected /lib/firmware/regulatory.db (malformed or signed by a key it does not trust), so it stays in the world domain", "sudo scripts/install-regdb-fallback.sh")
	case strings.Contains(string(out), "failed to load regulatory.db"):
		r.add(Fail, "regulatory.db", "the kernel could not load /lib/firmware/regulatory.db", "apt install --reinstall wireless-regdb")
	default:
		r.add(Pass, "regulatory.db", "no load or signature errors in the kernel log", "")
	}
}

func checkNetworkManagers(r *Report) {
	for _, svc := range []string{"NetworkManager", "systemd-networkd", "wpa_supplicant", "connman"} {
		out, err := exec.Command("systemctl", "is-active", svc).Output()
		if err == nil && strings.TrimSpace(string(out)) == "active" {
			msg := "active"
			switch svc {
			case "NetworkManager":
				msg += "; meshd marks its interfaces unmanaged in /etc/NetworkManager/conf.d"
			case "wpa_supplicant":
				msg += "; the system instance is left alone, meshd runs its own for mesh0"
			case "systemd-networkd":
				msg += "; meshd marks its interfaces unmanaged in /etc/systemd/network"
			}
			r.add(Info, svc, msg, "")
		}
	}
}

func checkConfigs(r *Report, opts Options) {
	if opts.NodePath != "" {
		if n, err := config.LoadNode(opts.NodePath); err != nil {
			if errors.Is(err, os.ErrPermission) {
				r.add(Info, "node config", opts.NodePath+" is not readable by this user; not checked", "sudo meshctl doctor")
			} else if os.IsNotExist(err) {
				r.add(Warn, "node config", opts.NodePath+" does not exist", "copy configs/examples/node.yaml and edit it")
			} else {
				r.add(Fail, "node config", err.Error(), "fix the fields named above")
			}
		} else {
			r.add(Pass, "node config", fmt.Sprintf("%s: callsign %s, node_id %d, profile %s", opts.NodePath, n.Callsign, n.NodeID, n.ProfilePath), "")
			if opts.ProfilePath == "" {
				opts.ProfilePath = n.ProfilePath
			}
		}
	}
	if opts.ProfilePath != "" {
		if p, err := config.LoadProfile(opts.ProfilePath); err != nil {
			if errors.Is(err, os.ErrPermission) {
				r.add(Info, "mesh profile", opts.ProfilePath+" is readable by root only (it holds the mesh secret); not checked", "sudo meshctl doctor")
			} else if os.IsNotExist(err) {
				r.add(Warn, "mesh profile", opts.ProfilePath+" does not exist", "copy configs/examples/profile.yaml and edit it")
			} else {
				r.add(Fail, "mesh profile", err.Error(), "fix the fields named above")
			}
		} else {
			r.add(Pass, "mesh profile", fmt.Sprintf("%s: mesh %q, %s channel %d, %s mode (%s)", opts.ProfilePath, p.MeshID, p.Radio.Band, p.Radio.Channel, p.Regulatory.Mode, p.Regulatory.Country), "")
			checkMeshBand(r, opts, p)
		}
	}
}

func checkMeshd(r *Report, opts Options) {
	if opts.Socket == "" {
		return
	}
	if _, err := os.Stat(opts.Socket); err != nil {
		r.add(Info, "meshd", "not running (no socket at "+opts.Socket+")", "systemctl start trellis-meshd")
		return
	}
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", opts.Socket)
	}}, Timeout: 3 * time.Second}
	resp, err := client.Get("http://meshd/v0/status")
	if err != nil && errors.Is(err, os.ErrPermission) {
		r.add(Info, "meshd", "running; its socket is readable by root and the trellis group only, so its status was not checked", "sudo meshctl doctor")
		return
	}
	if err != nil {
		r.add(Warn, "meshd", "socket present but not answering: "+err.Error(), "journalctl -u trellis-meshd")
		return
	}
	defer resp.Body.Close()
	var st status.Status
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		r.add(Warn, "meshd", "bad status reply: "+err.Error(), "")
		return
	}
	peers := 0
	for _, p := range st.MeshLink.Peers {
		if p.Established {
			peers++
		}
	}
	desc := fmt.Sprintf("running %s; node %s id %d; mesh link %s (%s), %d peers; batman %s with %d originators; %d warnings",
		st.Version, st.Node.Callsign, st.Node.NodeID, st.MeshLink.Transport, st.MeshLink.Supplicant.State, peers,
		st.Batman.ModuleVersion, len(st.Batman.Originators), len(st.Warnings))
	st2 := Pass
	for _, w := range st.Warnings {
		if w.Level == "error" {
			st2 = Warn
		}
	}
	r.add(st2, "meshd", desc, "")
	for _, w := range st.Warnings {
		lvl := Info
		if w.Level == "error" {
			lvl = Warn
		}
		r.add(lvl, "meshd: "+w.Code, w.Message, "")
	}
}

// Text renders the report for pasting into an issue or a message.
func (r *Report) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Trellis doctor %s — %s\n", r.Version, r.Time.Format(time.RFC3339))
	fmt.Fprintf(&b, "Host: %s  Kernel: %s %s  OS: %s\n\n", r.Hostname, r.Kernel, r.Arch, r.OS)
	for _, c := range r.Checks {
		fmt.Fprintf(&b, "[%s] %s", c.Status, c.Name)
		if c.Detail != "" {
			fmt.Fprintf(&b, ": %s", c.Detail)
		}
		b.WriteString("\n")
		if c.Fix != "" && (c.Status == Fail || c.Status == Warn || c.Status == Info) {
			fmt.Fprintf(&b, "       fix: %s\n", c.Fix)
		}
	}
	fmt.Fprintf(&b, "\nSummary: %d pass, %d warn, %d fail\n", r.Pass, r.Warn, r.Fail)
	return b.String()
}

// checkMeshBand reports whether any radio here can join the profile's mesh.
func checkMeshBand(r *Report, opts Options, p *config.Profile) {
	nl, err := nl80211.Dial()
	if err != nil {
		return
	}
	defer nl.Close()
	db, _ := hwdb.Load()
	radios, err := radio.Inventory(nl, opts.SysRoot, db, config.RadiosConfig{})
	if err != nil {
		return
	}
	var ok, notBand []string
	for _, rd := range radios {
		if !rd.Caps.MeshPoint || rd.Broken != "" {
			continue
		}
		if rd.HasBand(p.Radio.Band) {
			ok = append(ok, rd.Phy)
		} else {
			notBand = append(notBand, rd.Phy+" ("+strings.Join(rd.Caps.Bands, "/")+")")
		}
	}
	switch {
	case len(ok) > 0:
		r.add(Pass, "mesh band", fmt.Sprintf("%s mesh can run on %s", p.Radio.Band, strings.Join(ok, ", ")), "")
	case len(notBand) > 0:
		r.add(Fail, "mesh band", fmt.Sprintf("the profile's mesh is on %s, but the mesh-capable radios here are not: %s", p.Radio.Band, strings.Join(notBand, ", ")),
			"use an adapter with "+p.Radio.Band+" support (every node in a mesh needs one)")
	}
}
