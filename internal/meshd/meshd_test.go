package meshd

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLockInstanceExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meshd.lock")
	unlock, err := lockInstance(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockInstance(path); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second lock: want ErrAlreadyRunning, got %v", err)
	}
	unlock()
	unlock2, err := lockInstance(path)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	unlock2()
}

func TestChurnGuard(t *testing.T) {
	r := &radioReconciler{d: &Daemon{log: slog.New(slog.NewTextHandler(io.Discard, nil))}}
	for i := 0; i < churnLimit; i++ {
		if err := r.checkChurn("mesh0"); err != nil {
			t.Fatalf("creation %d refused: %v", i+1, err)
		}
	}
	err := r.checkChurn("mesh0")
	if err == nil || !strings.Contains(err.Error(), "NetworkManager") {
		t.Fatalf("expected a churn error naming the likely culprits, got %v", err)
	}
	if err := r.checkChurn("ap0"); err != nil {
		t.Fatalf("other interfaces are tracked separately: %v", err)
	}
	// Old creations age out of the window.
	old := time.Now().Add(-churnWindow - time.Second)
	r.created["mesh0"] = []time.Time{old, old, old}
	if err := r.checkChurn("mesh0"); err != nil {
		t.Fatalf("aged-out creations must not count: %v", err)
	}
}

func TestClientNetworkAddressing(t *testing.T) {
	if a, err := anycastAddress("10.73.0.0/16"); err != nil || a != "10.73.0.1" {
		t.Errorf("anycast = %q, %v", a, err)
	}
	s, e, m, err := dhcpPool("10.73.0.0/16", 7)
	if err != nil || s != "10.73.7.100" || e != "10.73.7.199" || m != "255.255.0.0" {
		t.Errorf("pool = %s %s %s %v", s, e, m, err)
	}
	// MT7610U: second interface differs in the first byte.
	perm, _ := net.ParseMAC("9c:ed:d5:fd:5c:f1")
	if got := apMAC(perm, "mt76x0u").String(); got != "9e:ed:d5:fd:5c:f1" {
		t.Errorf("apMAC mt76x0u = %s", got)
	}
	// RT5572: low bits of the last byte, so the hardware BSS slots differ.
	perm2, _ := net.ParseMAC("9c:ef:d5:fd:7a:ea")
	got := apMAC(perm2, "rt2800usb")
	if got.String() != "9c:ef:d5:fd:7a:eb" || got[5]&7 == perm2[5]&7 {
		t.Errorf("apMAC rt2800usb = %s", got)
	}
	local, _ := net.ParseMAC("02:11:22:33:44:55")
	if got := apMAC(local, "mt76x2u").String(); got != "06:11:22:33:44:55" {
		t.Errorf("apMAC mt76x2u of a local address = %s", got)
	}
}
