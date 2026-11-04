package probe

import (
	"testing"
	"time"
)

func TestEchoChecksum(t *testing.T) {
	b := echo(0x1234, 7)
	if checksum(b) != 0 {
		t.Errorf("checksum of a packet including its checksum should be 0, got %#x", checksum(b))
	}
	reply := append(make([]byte, 20), b...)
	reply[0] = 0x45
	reply[20] = 0
	if !isReply(reply, 0x1234, 7) || isReply(reply, 0x1234, 8) {
		t.Error("isReply")
	}
}

func TestHealthTransitions(t *testing.T) {
	var changes []bool
	p := &Prober{OnChange: func(s State) { changes = append(changes, s.Healthy) }}
	for i := 0; i < OKAfter; i++ {
		p.record(true, time.Millisecond, nil)
	}
	if !p.State().Healthy {
		t.Fatal("not healthy after OKAfter good rounds")
	}
	p.record(false, 0, nil)
	p.record(false, 0, nil)
	if !p.State().Healthy {
		t.Fatal("unhealthy before FailAfter")
	}
	p.record(false, 0, nil)
	if p.State().Healthy {
		t.Fatal("still healthy after FailAfter bad rounds")
	}
	if len(changes) != 2 || !changes[0] || changes[1] {
		t.Errorf("changes = %v", changes)
	}
	if p.State().LossPct != 50 {
		t.Errorf("loss = %d", p.State().LossPct)
	}
}
