package nodestate

import (
	"net"
	"testing"
)

func TestKeepsID(t *testing.T) {
	lo, hi := "02:00:00:00:00:0a", "0a:00:00:00:00:01"
	cases := []struct {
		aSrc, aMAC, bSrc, bMAC string
		want                   bool
	}{
		{IDDerived, lo, IDDerived, hi, true},
		{IDDerived, hi, IDDerived, lo, false},
		{IDClaimed, lo, IDDerived, hi, true}, // claimed and derived rank alike
		{IDDerived, lo, IDConfig, hi, false}, // a pin beats a lower MAC
		{IDConfig, hi, IDDerived, lo, true},
		{IDConfig, hi, IDConfig, lo, false},
		{IDConfig, lo, IDConfig, hi, true},
		{IDDerived, "02:00:00:00:00:0A", IDDerived, "02:00:00:00:00:0b", true}, // case does not matter
	}
	for _, c := range cases {
		if got := KeepsID(c.aSrc, c.aMAC, c.bSrc, c.bMAC); got != c.want {
			t.Errorf("KeepsID(%s %s, %s %s) = %v, want %v", c.aSrc, c.aMAC, c.bSrc, c.bMAC, got, c.want)
		}
		if c.aMAC != c.bMAC && KeepsID(c.aSrc, c.aMAC, c.bSrc, c.bMAC) == KeepsID(c.bSrc, c.bMAC, c.aSrc, c.aMAC) {
			t.Errorf("KeepsID is not antisymmetric for %v", c)
		}
	}
}

func TestClaimNodeID(t *testing.T) {
	mac, _ := net.ParseMAC("02:11:22:33:44:55")
	used := map[int]bool{}
	for i := 1; i <= 254; i++ {
		if i%7 != 0 {
			used[i] = true
		}
	}
	id, ok := ClaimNodeID(mac, used, 14)
	if !ok || used[id] || id == 14 || id%7 != 0 {
		t.Fatalf("ClaimNodeID = %d, %v; want a free ID other than 14", id, ok)
	}
	again, _ := ClaimNodeID(mac, used, 14)
	if again != id {
		t.Errorf("ClaimNodeID is not deterministic: %d then %d", id, again)
	}
	// Two MACs losing at once usually land on different IDs.
	other, _ := net.ParseMAC("02:11:22:33:44:56")
	a, _ := ClaimNodeID(mac, map[int]bool{5: true}, 5)
	b, _ := ClaimNodeID(other, map[int]bool{5: true}, 5)
	if a == b {
		t.Errorf("two MACs claimed the same ID %d", a)
	}
	full := map[int]bool{}
	for i := 1; i <= 254; i++ {
		full[i] = true
	}
	if _, ok := ClaimNodeID(mac, full, 3); ok {
		t.Error("claimed an ID with all 254 in use")
	}
	delete(full, 200)
	if id, ok := ClaimNodeID(mac, full, 3); !ok || id != 200 {
		t.Errorf("ClaimNodeID with only 200 free = %d, %v", id, ok)
	}
}

func TestCloneKeeps(t *testing.T) {
	a := []string{"9c:ef:d5:fb:2d:26", "00:c0:ca:00:00:01"}
	b := []string{"9c:ed:d5:fd:5c:f1"}
	if !CloneKeeps(a, b, 9, 1) || CloneKeeps(b, a, 1, 9) {
		t.Error("the node with the lowest hard-interface MAC must keep the identity")
	}
	if !CloneKeeps(nil, nil, 1, 2) || CloneKeeps(nil, nil, 2, 1) {
		t.Error("without hard interfaces the lower boot value keeps it")
	}
}

func TestEnsureIdentityKeepsClaimed(t *testing.T) {
	dir := t.TempDir()
	id, _, err := EnsureIdentity(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	id.NodeID, id.NodeIDSource = 77, IDClaimed
	if err := SaveIdentity(dir, id); err != nil {
		t.Fatal(err)
	}
	got, changed, err := EnsureIdentity(dir, 0)
	if err != nil || changed || got.NodeID != 77 || got.NodeIDSource != IDClaimed {
		t.Fatalf("claimed ID not kept across restart: %+v changed=%v err=%v", got, changed, err)
	}
	got, _, _ = EnsureIdentity(dir, 5)
	if got.NodeID != 5 || got.NodeIDSource != IDConfig {
		t.Fatalf("a pin must override a claimed ID: %+v", got)
	}
}
