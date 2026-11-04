package nl80211

import (
	"testing"
)

// These tests talk to the running kernel. They skip when there is no
// cfg80211 or no radio.

func liveClient(t *testing.T) *Client {
	t.Helper()
	c, err := Dial()
	if err != nil {
		t.Skipf("nl80211 unavailable: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestLiveRecoversAfterError(t *testing.T) {
	c := liveClient(t)
	w1, err := c.Wiphys()
	if err != nil {
		t.Fatalf("first dump: %v", err)
	}
	if len(w1) == 0 {
		t.Skip("no radios")
	}
	// A request the kernel rejects must not break later requests. The
	// kernel's error reply is a complete answer, so the socket is kept
	// (gnl.kernelReply); the requests below prove it still works.
	if _, err := c.InterfaceByIndex(1 << 30); err == nil {
		t.Fatal("expected an error for a bogus ifindex")
	}
	if n := c.Resets(); n != 0 {
		t.Errorf("socket replaced %d times after a kernel error reply; it should be kept", n)
	}
	for i := 0; i < 20; i++ {
		w2, err := c.Wiphys()
		if err != nil {
			t.Fatalf("dump %d after error: %v", i, err)
		}
		if len(w2) != len(w1) {
			t.Fatalf("dump %d: %d radios, want %d", i, len(w2), len(w1))
		}
		if _, err := c.Interfaces(); err != nil {
			t.Fatalf("interfaces %d: %v", i, err)
		}
	}
	if _, err := c.GetReg(); err != nil {
		t.Fatalf("get reg: %v", err)
	}
}

func TestLiveConcurrent(t *testing.T) {
	c := liveClient(t)
	errs := make(chan error, 40)
	for g := 0; g < 8; g++ {
		go func() {
			for i := 0; i < 5; i++ {
				if _, err := c.Wiphys(); err != nil {
					errs <- err
					return
				}
				if _, err := c.Interfaces(); err != nil {
					errs <- err
					return
				}
			}
			errs <- nil
		}()
	}
	for g := 0; g < 8; g++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}
