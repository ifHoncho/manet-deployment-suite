// Package hwdb is the hardware database: per-adapter facts that outrank what
// drivers report, used for radio ranking and for showing quirks in the UI.
package hwdb

import (
	_ "embed"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed hwdb.yaml
var embedded []byte

// Verification states.
const (
	Verified = "verified"
	Reported = "reported"
	Broken   = "broken"
	Unknown  = "unknown"
)

// Power sources, strongest first.
const (
	SourceMeasured  = "measured"
	SourceDatasheet = "datasheet"
	SourceReported  = "reported"
)

// Match selects adapters by id or driver.
type Match struct {
	USB    []string `yaml:"usb"`
	PCI    []string `yaml:"pci"`
	Driver string   `yaml:"driver"`
}

// Entry is one adapter model.
type Entry struct {
	ID              string             `yaml:"id"`
	Chipset         string             `yaml:"chipset"`
	Match           Match              `yaml:"match"`
	MaxPowerDBm     map[string]float64 `yaml:"max_power_dbm"`
	PowerSource     string             `yaml:"power_source"`
	MeshPoint       string             `yaml:"mesh_point"`
	SAEMesh         string             `yaml:"sae_mesh"`
	MeshAPTogether  string             `yaml:"mesh_ap_together"`
	VerifiedKernels []string           `yaml:"verified_kernels"`
	Quirks          []string           `yaml:"quirks"`
	Notes           string             `yaml:"notes"`
	// ModuleParams are kernel module parameters the driver (Match.Driver)
	// needs for a working encrypted mesh. node-update.sh writes them to
	// /etc/modprobe.d and reloads the module; meshd warns while the loaded
	// module differs.
	ModuleParams map[string]string `yaml:"module_params"`
}

// DB is the loaded database.
type DB struct {
	Schema  int     `yaml:"schema"`
	Entries []Entry `yaml:"entries"`
}

// Load returns the database embedded in the binary.
func Load() (*DB, error) { return Parse(embedded) }

// Parse loads a database from YAML.
func Parse(data []byte) (*DB, error) {
	var db DB
	if err := yaml.Unmarshal(data, &db); err != nil {
		return nil, fmt.Errorf("hardware database: %w", err)
	}
	for i := range db.Entries {
		e := &db.Entries[i]
		if e.ID == "" {
			return nil, fmt.Errorf("hardware database: entry %d has no id", i)
		}
		for j, id := range e.Match.USB {
			e.Match.USB[j] = strings.ToLower(id)
		}
		for j, id := range e.Match.PCI {
			e.Match.PCI[j] = strings.ToLower(id)
		}
		if e.MeshPoint == "" {
			e.MeshPoint = Unknown
		}
		if e.SAEMesh == "" {
			e.SAEMesh = Unknown
		}
		if e.MeshAPTogether == "" {
			e.MeshAPTogether = Unknown
		}
	}
	return &db, nil
}

// Lookup finds the entry for an adapter. An id match (USB or PCI) wins over a
// driver-only match. Returns nil when nothing matches.
func (db *DB) Lookup(bus, deviceID, driver string) *Entry {
	deviceID = strings.ToLower(deviceID)
	var byDriver *Entry
	for i := range db.Entries {
		e := &db.Entries[i]
		ids := e.Match.USB
		if bus == "pci" {
			ids = e.Match.PCI
		}
		for _, id := range ids {
			if deviceID != "" && id == deviceID {
				return e
			}
		}
		if byDriver == nil && e.Match.Driver != "" && driver != "" && e.Match.Driver == driver {
			byDriver = e
		}
	}
	return byDriver
}

// ModprobeOptions returns one "options <module> k=v …" line per driver with
// required module parameters, sorted, for /etc/modprobe.d.
func (db *DB) ModprobeOptions() []string {
	byMod := map[string]map[string]string{}
	for _, e := range db.Entries {
		if e.Match.Driver == "" || len(e.ModuleParams) == 0 {
			continue
		}
		m := byMod[e.Match.Driver]
		if m == nil {
			m = map[string]string{}
			byMod[e.Match.Driver] = m
		}
		for k, v := range e.ModuleParams {
			m[k] = v
		}
	}
	var out []string
	for mod, params := range byMod {
		keys := make([]string, 0, len(params))
		for k := range params {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		line := "options " + mod
		for _, k := range keys {
			line += " " + k + "=" + params[k]
		}
		out = append(out, line)
	}
	sort.Strings(out)
	return out
}

// ParamMatches compares a module parameter as sysfs shows it with the
// wanted value: bool parameters read back as Y/N.
func ParamMatches(have, want string) bool {
	norm := func(v string) string {
		switch strings.ToUpper(strings.TrimSpace(v)) {
		case "Y", "1", "TRUE", "ON":
			return "1"
		case "N", "0", "FALSE", "OFF":
			return "0"
		}
		return strings.TrimSpace(v)
	}
	return norm(have) == norm(want)
}
