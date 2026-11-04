package api

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"meridian/trellis/internal/config"
	"meridian/trellis/internal/status"
)

type fakeProvider struct{ kicks int }

func (f *fakeProvider) Status(ctx context.Context) (*status.Status, error) {
	return &status.Status{Version: "test", Node: status.Node{Callsign: "n1", NodeID: 7}}, nil
}
func (f *fakeProvider) Config() (config.Node, config.Profile) {
	return config.Node{Callsign: "n1"}, config.Profile{Name: "p", SAEPassphrase: "<redacted>"}
}
func (f *fakeProvider) Kick(string)   { f.kicks++ }
func (f *fakeProvider) Reload() error { return errors.New("bad config: callsign: required") }
func (f *fakeProvider) APJoin() (*status.APJoin, error) {
	return &status.APJoin{Enabled: true, SSID: "node-1", Passphrase: "secret12", WiFiQR: "WIFI:T:WPA;S:node-1;P:secret12;;"}, nil
}

func TestServerAndClient(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "m.sock")
	p := &fakeProvider{}
	srv := &Server{Provider: p, Socket: sock}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Serve(ctx)

	c := NewClient(sock)
	deadline := time.Now().Add(3 * time.Second)
	for {
		if err := c.Health(ctx); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server did not come up")
		}
		time.Sleep(10 * time.Millisecond)
	}
	st, err := c.Status(ctx)
	if err != nil || st.Node.NodeID != 7 {
		t.Fatalf("status: %+v %v", st, err)
	}
	if err := c.Reconcile(ctx); err != nil || p.kicks != 1 {
		t.Fatalf("reconcile: %v kicks=%d", err, p.kicks)
	}
	if err := c.Reload(ctx); err == nil || err.Error() != "bad config: callsign: required" {
		t.Errorf("reload error not propagated: %v", err)
	}
	cfg, err := c.Config(ctx)
	if err != nil || cfg.Profile.SAEPassphrase != "<redacted>" {
		t.Errorf("config: %+v %v", cfg, err)
	}
}

func TestClientNotRunning(t *testing.T) {
	c := NewClient(filepath.Join(t.TempDir(), "none.sock"))
	err := c.Health(context.Background())
	if !errors.Is(err, ErrNotRunning) {
		t.Errorf("expected ErrNotRunning, got %v", err)
	}
}
