package hwdb

import (
	"strings"
	"testing"
)

func TestEmbeddedLoads(t *testing.T) {
	db, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(db.Entries) == 0 {
		t.Fatal("no entries")
	}
	e := db.Lookup("usb", "0E8D:7612", "mt76x2u")
	if e == nil || e.ID != "mt7612u" {
		t.Fatalf("lookup by id: %+v", e)
	}
	if e.MaxPowerDBm["5g"] != 20 {
		t.Errorf("power = %v", e.MaxPowerDBm)
	}
	if e := db.Lookup("usb", "ffff:ffff", "mt76x2u"); e == nil || e.ID != "mt7612u" {
		t.Errorf("lookup by driver: %+v", e)
	}
	if e := db.Lookup("usb", "ffff:ffff", "nothing"); e != nil {
		t.Errorf("unexpected match: %+v", e)
	}
	if e := db.Lookup("usb", "", "88XXau"); e == nil || e.MeshPoint != Broken {
		t.Errorf("broken driver: %+v", e)
	}
}

func TestModprobeOptions(t *testing.T) {
	db, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, l := range db.ModprobeOptions() {
		have[l] = true
	}
	for _, want := range []string{"options ath9k_htc nohwcrypt=1", "options rt2800usb nohwcrypt=1"} {
		if !have[want] {
			t.Errorf("%q missing: %v", want, db.ModprobeOptions())
		}
	}
	if !ParamMatches("Y", "1") || !ParamMatches("0", "0") || ParamMatches("0", "1") {
		t.Error("ParamMatches")
	}
}

func TestReferenceBuildEntries(t *testing.T) {
	db, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if e := db.Lookup("pci", "14C3:7906", "mt7915e"); e == nil || e.ID != "mt7916" {
		t.Errorf("MT7916 by PCI id: %+v", e)
	}
	if e := db.Lookup("pci", "", "mt7915e"); e == nil || e.ID != "mt7916" {
		t.Errorf("MT7916 by driver: %+v", e)
	}
	if e := db.Lookup("spi", "", "morse"); e == nil || e.ID != "mm6108" {
		t.Errorf("MM6108 by driver: %+v", e)
	}
	if len(db.ModprobeOptions()) == 0 {
		t.Fatal("existing module options lost")
	}
	for _, line := range db.ModprobeOptions() {
		if strings.Contains(line, "mt7915e") || strings.Contains(line, "morse") {
			t.Errorf("no module options are known for the reference build yet: %q", line)
		}
	}
}
