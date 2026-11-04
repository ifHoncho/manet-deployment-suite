package batadv

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseVersion(t *testing.T) {
	for _, c := range []struct {
		in   string
		want Version
		ok   bool
	}{
		{"2026.3", Version{2026, 3}, true},
		{"2024.0\n", Version{2024, 0}, true},
		{"2023.1-openwrt-3", Version{2023, 1}, true},
		{"7.2.13-200.fc44.x86_64", Version{}, false},
		{"", Version{}, false},
	} {
		got, err := ParseVersion(c.in)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("ParseVersion(%q) = %v, %v; want %v ok=%v", c.in, got, err, c.want, c.ok)
		}
	}
}

func TestLess(t *testing.T) {
	if !(Version{2022, 3}).Less(Version{2024, 0}) || (Version{2024, 1}).Less(Version{2024, 0}) || (Version{2024, 0}).Less(Version{2024, 0}) {
		t.Error("Less ordering wrong")
	}
}

func TestReadModuleInfo(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "version")

	if mi := readModuleInfo(p); mi.Loaded {
		t.Error("missing file must report not loaded")
	}

	os.WriteFile(p, []byte("2026.3\n"), 0o644)
	mi := readModuleInfo(p)
	v, inferred := mi.EffectiveRelease()
	if !mi.Loaded || v != (Version{2026, 3}) || inferred {
		t.Errorf("release build: %+v -> %v inferred=%v", mi, v, inferred)
	}

	os.WriteFile(p, []byte("6.1.54-00001-g1234\n"), 0o644)
	mi = readModuleInfo(p)
	v, inferred = mi.EffectiveRelease()
	if mi.InTreeKernelVersion == "" || !inferred || v != (Version{2022, 3}) {
		t.Errorf("old in-tree: %+v -> %v inferred=%v", mi, v, inferred)
	}

	os.WriteFile(p, []byte("7.2.13-200.fc44.x86_64\n"), 0o644)
	mi = readModuleInfo(p)
	v, inferred = mi.EffectiveRelease()
	if !inferred || v != (Version{2024, 0}) {
		t.Errorf("new in-tree: %+v -> %v inferred=%v", mi, v, inferred)
	}
}

func TestDiff(t *testing.T) {
	tr, fa := true, false
	var u1000, u2000 uint32 = 1000, 2000
	have := Settings{BridgeLoopAvoidance: &fa, Fragmentation: &tr, OrigIntervalMS: &u1000}
	want := Settings{BridgeLoopAvoidance: &tr, Fragmentation: &tr, OrigIntervalMS: &u2000, DistributedARPTable: &tr}
	d := Diff(have, want)
	names := d.Names()
	// DistributedARPTable is absent from have: unsupported, so skipped.
	if len(names) != 2 || d.Fragmentation != nil || d.BridgeLoopAvoidance == nil || d.OrigIntervalMS == nil || d.DistributedARPTable != nil {
		t.Errorf("Diff = %v", names)
	}
	if u := Unsupported(have, want); len(u) != 1 || u[0] != "distributed_arp_table" {
		t.Errorf("Unsupported = %v", u)
	}
	if !Diff(want, want).IsEmpty() {
		t.Error("Diff of identical settings must be empty")
	}
}
