package meshctl

import (
	"strings"
	"testing"
	"time"

	"meridian/trellis/internal/status"
)

func TestSAESummary(t *testing.T) {
	now := time.Date(2026, 11, 3, 3, 0, 0, 0, time.UTC)
	bad, good := "cc:64:1a:eb:81:64", "9c:ef:d5:fd:7a:ea"
	ev := func(mac, kind string, ago time.Duration) status.SAEEvent {
		return status.SAEEvent{Time: now.Add(-ago), MAC: mac, Kind: kind}
	}
	events := []status.SAEEvent{
		ev(good, "failure", 10*time.Minute), // before its current link: hidden
		ev(bad, "failure", 182*time.Second),
		ev(bad, "failure", 171*time.Second),
		ev(bad, "failure", 137*time.Second),
		ev(bad, "blocked", 137*time.Second),
	}
	peers := []status.Peer{{MAC: good, Established: true, ConnectedSeconds: 197}}
	got := saeSummary(events, peers, now)
	if len(got) != 1 {
		t.Fatalf("want one line, got %q", got)
	}
	for _, want := range []string{bad, "3 authentication failures, last 2m17s ago", "blocked for 300 s 2m17s ago"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("%q lacks %q", got[0], want)
		}
	}
}
