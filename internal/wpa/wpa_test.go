package wpa

import (
	"context"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRender(t *testing.T) {
	out := Render(MeshConfig{CtrlDir: "/run/trellis/wpa", MeshID: "trellis-alpha", Passphrase: "correct horse", FreqMHz: 5745, WidthMHz: 80})
	for _, want := range []string{
		"ctrl_interface=DIR=/run/trellis/wpa",
		"ssid=7472656c6c69732d616c706861",
		"mode=5", "frequency=5745", "key_mgmt=SAE", `sae_password="correct horse"`,
		"ieee80211w=2", "mesh_fwding=0", "ht40=1", "vht=1", "max_oper_chwidth=1", "vht_center_freq1=5775",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	out20 := Render(MeshConfig{MeshID: "x", Passphrase: "y", FreqMHz: 2437, WidthMHz: 20})
	if !strings.Contains(out20, "disable_ht40=1") || strings.Contains(out20, "vht=1") {
		t.Errorf("20 MHz config wrong:\n%s", out20)
	}
}

func TestCenterFreq80(t *testing.T) {
	for f, want := range map[int]int{5180: 5210, 5200: 5210, 5240: 5210, 5260: 5290, 5500: 5530, 5745: 5775, 5805: 5775, 5825: 5855, 5955: 0} {
		if got := CenterFreq80(f); got != want {
			t.Errorf("CenterFreq80(%d) = %d, want %d", f, got, want)
		}
	}
}

func TestPassphraseOK(t *testing.T) {
	if err := PassphraseOK("good-passphrase 123"); err != nil {
		t.Error(err)
	}
	for _, bad := range []string{`has"quote`, `back\slash`, "tab\there", "ünïcode"} {
		if PassphraseOK(bad) == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestParseEvent(t *testing.T) {
	e, ok := ParseEvent("<3>MESH-PEER-CONNECTED aa:bb:cc:dd:ee:ff")
	if !ok || e.Level != 3 || e.Name != EvMeshPeerConnected || e.Arg() != "aa:bb:cc:dd:ee:ff" {
		t.Errorf("got %+v", e)
	}
	e, _ = ParseEvent(`<3>MESH-SAE-AUTH-BLOCKED addr=aa:bb:cc:dd:ee:ff duration=10`)
	if e.KV["addr"] != "aa:bb:cc:dd:ee:ff" || e.KV["duration"] != "10" {
		t.Errorf("kv: %+v", e)
	}
	e, _ = ParseEvent(`<3>MESH-GROUP-STARTED ssid="trellis" id=0`)
	if e.KV["ssid"] != "trellis" {
		t.Errorf("quoted kv: %+v", e)
	}
}

// fakeSupplicant answers PING/STATUS/ATTACH on a unixgram socket.
func fakeSupplicant(t *testing.T, path string) (events func(string)) {
	t.Helper()
	addr := &net.UnixAddr{Name: path, Net: "unixgram"}
	conn, err := net.ListenUnixgram("unixgram", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	var attached *net.UnixAddr
	go func() {
		buf := make([]byte, 4096)
		for {
			n, from, err := conn.ReadFromUnix(buf)
			if err != nil {
				return
			}
			cmd := string(buf[:n])
			var reply string
			switch cmd {
			case "PING":
				reply = "PONG\n"
			case "STATUS":
				reply = "bssid=00:11:22:33:44:55\nfreq=5745\nssid=trellis\nmode=mesh\nwpa_state=COMPLETED\n"
			case "ATTACH":
				attached = from
				reply = "OK\n"
			case "DETACH":
				reply = "OK\n"
			default:
				reply = "UNKNOWN COMMAND\n"
			}
			conn.WriteToUnix([]byte(reply), from)
		}
	}()
	return func(ev string) {
		if attached != nil {
			conn.WriteToUnix([]byte(ev), attached)
		}
	}
}

func TestCtrlAgainstFake(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "mesh0")
	send := fakeSupplicant(t, sock)

	c, err := DialCtrl(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	st, err := c.GetStatus(ctx)
	if err != nil || !st.Joined() || st.Freq() != 5745 {
		t.Fatalf("status: %v %v", st, err)
	}

	ev, err := DialEvents(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ev.Close()
	time.Sleep(20 * time.Millisecond)
	send("<3>MESH-PEER-CONNECTED aa:bb:cc:dd:ee:ff")
	e, err := ev.Next(time.Second)
	if err != nil || e.Name != EvMeshPeerConnected {
		t.Fatalf("event: %+v %v", e, err)
	}
	if _, err := ev.Next(50 * time.Millisecond); !IsTimeout(err) {
		t.Errorf("expected timeout, got %v", err)
	}
}
