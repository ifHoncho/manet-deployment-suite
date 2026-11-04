package proc

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"
)

func TestSupervisorRestartsAndStops(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	s := New(log, "sh", "sh", "-c", "echo hello; sleep 0.1; exit 1")
	s.MaxBackoff = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for s.Restarts() < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if s.Restarts() < 2 {
		t.Fatalf("expected restarts, got %d", s.Restarts())
	}
	if err := s.Stop(2 * time.Second); err != nil {
		t.Fatal(err)
	}
	if s.Running() || s.Alive() {
		t.Error("should be stopped")
	}
}

func TestSupervisorKillsStubborn(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	s := New(log, "sh", "sh", "-c", "trap '' TERM; while true; do sleep 1; done")
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	if err := s.Stop(300 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 3*time.Second {
		t.Error("stop took too long")
	}
	if s.Alive() {
		t.Error("should be killed")
	}
}

func TestStartFailure(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	s := New(log, "nope", "/nonexistent/binary")
	if err := s.Start(context.Background()); err == nil {
		t.Fatal("expected start error")
	}
	if s.Running() {
		t.Error("must not be running after a failed start")
	}
}

func TestStderrLevel(t *testing.T) {
	info := []string{
		"dnsmasq[84290]: using nameserver 127.0.0.53#53",
		"dnsmasq[84290]: using only locally-known addresses for mesh",
		"dnsmasq[84290]: read /run/trellis/dnsmasq.hosts - 3 names",
		"dnsmasq[116420]: exiting on receipt of SIGTERM",
	}
	warn := []string{
		"dnsmasq: failed to create listening socket for port 53: Address already in use",
		"dnsmasq: cannot open log /x",
		"nl80211: Could not configure driver mode: Device or resource busy (error -16)",
		"dnsmasq: warning: no upstream servers configured",
	}
	for _, l := range info {
		if stderrLevel(l) != slog.LevelInfo {
			t.Errorf("%q logged as a warning", l)
		}
	}
	for _, l := range warn {
		if stderrLevel(l) != slog.LevelWarn {
			t.Errorf("%q not logged as a warning", l)
		}
	}
}
