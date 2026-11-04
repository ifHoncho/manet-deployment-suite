// Package nodestate persists what a node must remember across reboots and
// that is not configuration: its identity (architecture §7.3) and what it
// has learned about its radios (§5.2).
package nodestate

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"hash/fnv"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// File names under the state directory.
const (
	IdentityFile = "identity.yaml"
	RadiosFile   = "radios.yaml"
)

// Identity is the node's persistent identity.
type Identity struct {
	MAC          string    `yaml:"mac"`            // locally administered unicast; used for bat0 and br-mesh
	NodeID       int       `yaml:"node_id"`        // 1–254
	NodeIDSource string    `yaml:"node_id_source"` // IDConfig, IDDerived or IDClaimed
	Created      time.Time `yaml:"created"`
	// APPassphrase is the generated client-AP passphrase, used when the node
	// config sets none. Generated once so phones stay joined across restarts.
	APPassphrase string `yaml:"ap_passphrase,omitempty"`
}

// HardwareAddr parses the MAC.
func (id *Identity) HardwareAddr() net.HardwareAddr {
	m, _ := net.ParseMAC(id.MAC)
	return m
}

// RandomMAC returns a locally administered unicast MAC.
func RandomMAC() (net.HardwareAddr, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	b[0] = (b[0] | 0x02) &^ 0x01
	return net.HardwareAddr(b), nil
}

// passphraseAlphabet leaves out characters that are easy to misread when
// typed from a screen (0/O, 1/l/I).
const passphraseAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"

// GeneratePassphrase returns n random characters from a typing-friendly
// alphabet: about 4.95 bits each, so 12 characters give 59 bits.
func GeneratePassphrase(n int) (string, error) {
	out := make([]byte, n)
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	for i := range out {
		// Rejection-free: 31 letters, bias from 256 % 31 is negligible for
		// an AP passphrase shown to the operator anyway.
		out[i] = passphraseAlphabet[int(buf[i])%len(passphraseAlphabet)]
	}
	return string(out), nil
}

// Node ID sources.
const (
	IDConfig  = "config"  // pinned in node.yaml
	IDDerived = "derived" // hashed from the identity MAC
	IDClaimed = "claimed" // taken after losing a conflict (architecture §7.3)
)

// DeriveNodeID hashes a MAC into 1–254: a node's first choice of ID when
// none is pinned. Collisions are possible; gossip resolves them
// (KeepsID, ClaimNodeID).
func DeriveNodeID(mac net.HardwareAddr) int { return deriveNodeID(mac, 0) }

// deriveNodeID is the attempt-th ID in this MAC's own sequence. Attempt 0
// is DeriveNodeID. Each MAC walks a different sequence, so two nodes that
// lose conflicts at the same moment rarely land on the same new ID.
func deriveNodeID(mac net.HardwareAddr, attempt int) int {
	h := fnv.New32a()
	h.Write(mac)
	if attempt > 0 {
		h.Write([]byte{byte(attempt >> 8), byte(attempt)})
	}
	return int(h.Sum32()%254) + 1
}

// ClaimNodeID picks a new ID for a node that lost its ID: the first ID in
// the MAC's sequence that is not in used and is not current. When the
// sequence finds none, the lowest free ID; ok is false when all 254 are
// taken.
func ClaimNodeID(mac net.HardwareAddr, used map[int]bool, current int) (id int, ok bool) {
	for a := 1; a <= 512; a++ {
		if n := deriveNodeID(mac, a); n != current && !used[n] {
			return n, true
		}
	}
	for n := 1; n <= 254; n++ {
		if n != current && !used[n] {
			return n, true
		}
	}
	return 0, false
}

// KeepsID reports whether node a keeps a node ID that node b also claims
// (architecture §7.3): an ID pinned in configuration beats a derived or
// claimed one, and otherwise the lower identity MAC keeps it. Both nodes
// evaluate the same rule from the same gossip, so exactly one of them
// moves. Identical MACs (a cloned identity) never keep: CloneKeeps decides.
func KeepsID(aSource, aMAC, bSource, bMAC string) bool {
	ac, bc := aSource == IDConfig, bSource == IDConfig
	if ac != bc {
		return ac
	}
	return macLess(aMAC, bMAC)
}

// CloneKeeps reports whether node a keeps an identity MAC that node b also
// uses (a cloned SD card): the node with the lower mesh hard-interface MAC
// keeps it; those are burnt into the radios and differ. a and b are each
// node's hard-interface MACs. With none to compare, a tie-breaker the two
// sides also see the same way (their meshd boot values) decides.
func CloneKeeps(aOrig, bOrig []string, aBoot, bBoot uint64) bool {
	am, bm := lowestMAC(aOrig), lowestMAC(bOrig)
	if am != "" && bm != "" && am != bm {
		return macLess(am, bm)
	}
	return aBoot < bBoot
}

func lowestMAC(macs []string) string {
	low := ""
	for _, m := range macs {
		if low == "" || macLess(m, low) {
			low = m
		}
	}
	return low
}

// macLess compares MACs numerically; unparsable ones sort last.
func macLess(a, b string) bool {
	ma, ea := net.ParseMAC(a)
	mb, eb := net.ParseMAC(b)
	if ea != nil || eb != nil {
		return ea == nil && eb != nil
	}
	return bytes.Compare(ma, mb) < 0
}

// SaveIdentity writes the identity file.
func SaveIdentity(dir string, id *Identity) error {
	return writeAtomic(filepath.Join(dir, IdentityFile), id, 0o600)
}

// EnsureIdentity loads the identity file, creating it on first run, and
// reconciles the node ID with the pinned value from config (0 = none).
// It returns the identity and whether the file was written.
func EnsureIdentity(dir string, pinnedID int) (*Identity, bool, error) {
	path := filepath.Join(dir, IdentityFile)
	id := &Identity{}
	changed := false
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := yaml.Unmarshal(data, id); err != nil {
			return nil, false, fmt.Errorf("%s: %w", path, err)
		}
	case errors.Is(err, os.ErrNotExist):
		changed = true
	default:
		return nil, false, err
	}
	if _, perr := net.ParseMAC(id.MAC); perr != nil || id.MAC == "" {
		mac, err := RandomMAC()
		if err != nil {
			return nil, false, err
		}
		id.MAC = mac.String()
		id.Created = time.Now().UTC().Truncate(time.Second)
		changed = true
	}
	if id.APPassphrase == "" {
		p, err := GeneratePassphrase(12)
		if err != nil {
			return nil, false, err
		}
		id.APPassphrase, changed = p, true
	}
	switch {
	case pinnedID != 0:
		if id.NodeID != pinnedID || id.NodeIDSource != IDConfig {
			id.NodeID, id.NodeIDSource, changed = pinnedID, IDConfig, true
		}
	case id.NodeID < 1 || id.NodeID > 254 || id.NodeIDSource == IDConfig:
		// No pin (any more): derive from the MAC so it stays stable. A
		// claimed ID (won after a conflict) is kept across restarts.
		id.NodeID, id.NodeIDSource, changed = DeriveNodeID(id.HardwareAddr()), IDDerived, true
	}
	if changed {
		if err := writeAtomic(path, id, 0o600); err != nil {
			return nil, false, err
		}
	}
	return id, changed, nil
}

// RadioRecord is what the node remembers about one radio.
type RadioRecord struct {
	Fingerprint  string    `yaml:"fingerprint"`
	KernelIfname string    `yaml:"kernel_ifname,omitempty"` // interface the kernel created, removed by Trellis
	LastRole     string    `yaml:"last_role,omitempty"`
	FirstSeen    time.Time `yaml:"first_seen"`
	LastSeen     time.Time `yaml:"last_seen"`
	Port         string    `yaml:"port,omitempty"`
	Driver       string    `yaml:"driver,omitempty"`
}

// Radios is the radio memory.
type Radios struct {
	Radios map[string]*RadioRecord `yaml:"radios"`
}

// LoadRadios reads the radio memory; a missing file is an empty memory.
func LoadRadios(dir string) (*Radios, error) {
	r := &Radios{Radios: map[string]*RadioRecord{}}
	data, err := os.ReadFile(filepath.Join(dir, RadiosFile))
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(data, r); err != nil {
		return nil, fmt.Errorf("%s: %w", RadiosFile, err)
	}
	if r.Radios == nil {
		r.Radios = map[string]*RadioRecord{}
	}
	return r, nil
}

// Save writes the radio memory.
func (r *Radios) Save(dir string) error {
	return writeAtomic(filepath.Join(dir, RadiosFile), r, 0o644)
}

// Get returns the record for a fingerprint, creating it if needed.
func (r *Radios) Get(fingerprint string) *RadioRecord {
	rec := r.Radios[fingerprint]
	if rec == nil {
		rec = &RadioRecord{Fingerprint: fingerprint, FirstSeen: time.Now().UTC().Truncate(time.Second)}
		r.Radios[fingerprint] = rec
	}
	return rec
}

func writeAtomic(path string, v any, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := yaml.Marshal(v)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// FormatMAC lower-cases a MAC string.
func FormatMAC(s string) string { return strings.ToLower(s) }
