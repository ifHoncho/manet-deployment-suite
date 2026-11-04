package nodestate

import (
	"net"
	"testing"
)

func TestEnsureIdentityStable(t *testing.T) {
	dir := t.TempDir()
	id1, changed, err := EnsureIdentity(dir, 0)
	if err != nil || !changed {
		t.Fatalf("first run: %v changed=%v", err, changed)
	}
	mac := id1.HardwareAddr()
	if mac[0]&0x02 == 0 || mac[0]&0x01 != 0 {
		t.Errorf("MAC must be locally administered unicast: %s", mac)
	}
	if id1.NodeID < 1 || id1.NodeID > 254 || id1.NodeIDSource != "derived" {
		t.Errorf("derived id: %+v", id1)
	}
	id2, changed, err := EnsureIdentity(dir, 0)
	if err != nil || changed || id2.MAC != id1.MAC || id2.NodeID != id1.NodeID {
		t.Errorf("second run must be a no-op: %+v changed=%v err=%v", id2, changed, err)
	}
	id3, changed, _ := EnsureIdentity(dir, 42)
	if !changed || id3.NodeID != 42 || id3.NodeIDSource != "config" || id3.MAC != id1.MAC {
		t.Errorf("pin: %+v changed=%v", id3, changed)
	}
	id4, changed, _ := EnsureIdentity(dir, 0)
	if !changed || id4.NodeID != id1.NodeID || id4.NodeIDSource != "derived" {
		t.Errorf("unpin must re-derive the same id: %+v (want %d)", id4, id1.NodeID)
	}
}

func TestDeriveNodeIDRange(t *testing.T) {
	for i := 0; i < 1000; i++ {
		mac, _ := RandomMAC()
		if n := DeriveNodeID(mac); n < 1 || n > 254 {
			t.Fatalf("id out of range: %d", n)
		}
	}
	m, _ := net.ParseMAC("02:00:00:00:00:01")
	if DeriveNodeID(m) != DeriveNodeID(m) {
		t.Error("not deterministic")
	}
}

func TestRadiosRoundTrip(t *testing.T) {
	dir := t.TempDir()
	r, err := LoadRadios(dir)
	if err != nil || len(r.Radios) != 0 {
		t.Fatal(err)
	}
	rec := r.Get("mac:00:11:22:33:44:55")
	rec.KernelIfname = "wlan0"
	rec.LastRole = "mesh"
	if err := r.Save(dir); err != nil {
		t.Fatal(err)
	}
	r2, err := LoadRadios(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := r2.Radios["mac:00:11:22:33:44:55"]
	if got == nil || got.KernelIfname != "wlan0" || got.LastRole != "mesh" || got.FirstSeen.IsZero() {
		t.Errorf("round trip: %+v", got)
	}
}
