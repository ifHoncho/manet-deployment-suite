package gossip

import (
	"testing"
	"time"
)

func rec(mac string, boot, seq uint64) *Record {
	return &Record{V: 1, MAC: mac, Orig: []string{"9c:ed:d5:fd:5c:f1"}, ID: 1, Callsign: "node-1", Addr: "10.73.1.1", Boot: boot, Seq: seq}
}

func TestSealOpen(t *testing.T) {
	k, err := Key("passphrase-1234", "trellis-alpha")
	if err != nil {
		t.Fatal(err)
	}
	r := rec("ea:67:61:6f:bc:8b", 7, 1)
	r.GW = &GW{Healthy: true, DownKbps: 50000}
	pkt, err := Seal(k, r)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Open(k, pkt)
	if err != nil || got.Callsign != "node-1" || got.GW == nil || !got.GW.Healthy {
		t.Fatalf("open: %+v %v", got, err)
	}
	// Tampering and a wrong key are refused.
	bad := append([]byte{}, pkt...)
	bad[10] ^= 1
	if _, err := Open(k, bad); err != ErrAuth {
		t.Errorf("tampered: %v", err)
	}
	k2, _ := Key("other-passphrase", "trellis-alpha")
	if _, err := Open(k2, pkt); err != ErrAuth {
		t.Errorf("wrong key: %v", err)
	}
	k3, _ := Key("passphrase-1234", "other-mesh")
	if _, err := Open(k3, pkt); err != ErrAuth {
		t.Errorf("other mesh id: %v", err)
	}
	if _, err := Open(k, []byte("hello")); err != ErrNotGossip {
		t.Errorf("junk: %v", err)
	}
}

func TestTable(t *testing.T) {
	now := time.Unix(1000, 0)
	tb := NewTable("66:26:2e:a5:2a:c9")
	tb.now = func() time.Time { return now }
	if tb.Update(rec("66:26:2E:A5:2A:C9", 1, 1), "x") != Ignored {
		t.Error("own record accepted")
	}
	if tb.Update(rec("ea:67:61:6f:bc:8b", 5, 3), "10.73.1.1") != New {
		t.Error("first record not New")
	}
	if tb.Update(rec("ea:67:61:6f:bc:8b", 5, 3), "10.73.1.1") != Ignored {
		t.Error("replay accepted")
	}
	if tb.Update(rec("ea:67:61:6f:bc:8b", 5, 2), "10.73.1.1") != Ignored {
		t.Error("older sequence accepted")
	}
	if tb.Update(rec("ea:67:61:6f:bc:8b", 5, 4), "10.73.1.1") != Refresh {
		t.Error("newer sequence not a refresh")
	}
	r := rec("ea:67:61:6f:bc:8b", 5, 5)
	r.GW = &GW{Healthy: true}
	if tb.Update(r, "10.73.1.1") != Changed {
		t.Error("state change not Changed")
	}
	// A restart (new boot id) is accepted even with a lower sequence.
	if tb.Update(rec("ea:67:61:6f:bc:8b", 9, 1), "10.73.1.1") != Changed {
		t.Error("new boot not accepted")
	}
	if p, ok := tb.ByOriginator("9C:ED:D5:FD:5C:F1"); !ok || p.Record.Callsign != "node-1" {
		t.Error("ByOriginator")
	}
	now = now.Add(Expiry + time.Second)
	if len(tb.Peers()) != 0 || tb.Expire() != 1 {
		t.Error("expiry")
	}
	if tb.Update(rec("ea:67:61:6f:bc:8b", 9, 2), "10.73.1.1") != New {
		t.Error("record after expiry should be New")
	}
}

func TestSameIgnoresLiveFigures(t *testing.T) {
	a := rec("ea:67:61:6f:bc:8b", 1, 1)
	a.GW = &GW{Healthy: true, RTTms: 12, LossPct: 0}
	b := rec("ea:67:61:6f:bc:8b", 1, 2)
	b.GW = &GW{Healthy: true, RTTms: 30, LossPct: 3}
	if !Same(a, b) {
		t.Error("RTT/loss changes should not count as a state change")
	}
	b.GW.Healthy = false
	if Same(a, b) {
		t.Error("health change must count")
	}
	if a.GW.RTTms != 12 {
		t.Error("Same modified its argument")
	}
}

func TestTableClone(t *testing.T) {
	self := "66:26:2e:a5:2a:c9"
	tb := NewTable(self)
	// Before the table knows its own boot value, nothing is a clone.
	if got := tb.Update(&Record{MAC: self, Boot: 9, Seq: 1}, "10.73.2.1"); got != Ignored {
		t.Fatalf("own MAC without a boot value: %d", got)
	}
	tb.SetBoot(7)
	if got := tb.Update(&Record{MAC: self, Boot: 7, Seq: 2}, "10.73.2.1"); got != Ignored {
		t.Fatalf("looped-back own record: %d", got)
	}
	if got := tb.Update(&Record{MAC: "66:26:2E:A5:2A:C9", Boot: 9, Seq: 1, Callsign: "copy"}, "10.73.2.1"); got != Clone {
		t.Fatalf("record from a clone: %d, want Clone", got)
	}
	if got := tb.Update(&Record{MAC: self, Boot: 9, Seq: 2}, "10.73.2.1"); got != Ignored {
		t.Fatalf("second record from the same clone: %d", got)
	}
	c, ok := tb.Clone()
	if !ok || c.Callsign != "" || c.Seq != 2 {
		t.Fatalf("Clone() = %+v, %v", c, ok)
	}
	if len(tb.Peers()) != 0 {
		t.Fatal("a clone must not enter the peer table")
	}
	tb.SetSelf("02:00:00:00:00:99")
	if _, ok := tb.Clone(); ok {
		t.Fatal("clone kept after the identity changed")
	}
	if got := tb.Update(&Record{MAC: self, Boot: 9, Seq: 3}, "10.73.2.1"); got != New {
		t.Fatalf("the clone's MAC is an ordinary peer after SetSelf: %d", got)
	}
}
