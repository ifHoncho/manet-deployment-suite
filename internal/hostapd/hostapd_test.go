package hostapd

import (
	"strings"
	"testing"

	"meridian/trellis/internal/nl80211"
)

func TestRender(t *testing.T) {
	out := Render(Config{Interface: "ap0", Bridge: "br-mesh", CtrlDir: "/run/trellis/hostapd", SSID: "node-1", Passphrase: "hunter22hunter",
		Country: "US", Band: "5g", Channel: 36, HT: true, VHT: true})
	for _, want := range []string{"interface=ap0", "bridge=br-mesh", "ssid2=6e6f64652d31", "hw_mode=a", "channel=36", "ieee80211n=1", "ieee80211ac=1",
		"wpa_passphrase=hunter22hunter", "multicast_to_unicast=1", "country_code=US", "ap_isolate=0"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	out2 := Render(Config{Band: "2g", Channel: 6, VHT: true})
	if strings.Contains(out2, "ieee80211ac") || !strings.Contains(out2, "hw_mode=g") {
		t.Errorf("2.4 GHz config wrong:\n%s", out2)
	}
}

func band(freqs ...nl80211.Frequency) *nl80211.BandInfo { return &nl80211.BandInfo{Frequencies: freqs} }

func TestChoose(t *testing.T) {
	b5 := band(nl80211.Frequency{MHz: 5180}, nl80211.Frequency{MHz: 5200}, nl80211.Frequency{MHz: 5260, Radar: true}, nl80211.Frequency{MHz: 5745})
	c, err := Choose("5g", b5, 0, 0)
	if err != nil || c.Channel != 36 || c.FreqMHz != 5180 {
		t.Fatalf("got %+v, %v", c, err)
	}
	// Same band as the mesh (ch 36): the farthest usable preferred channel wins.
	c, _ = Choose("5g", b5, 0, 5180)
	if c.Channel != 149 {
		t.Errorf("avoid 5180: got channel %d, want 149", c.Channel)
	}
	if _, err := Choose("5g", b5, 52, 0); err == nil {
		t.Error("radar channel 52 accepted as a pin")
	}
	b2 := band(nl80211.Frequency{MHz: 2412}, nl80211.Frequency{MHz: 2437}, nl80211.Frequency{MHz: 2462, NoIR: true})
	c, _ = Choose("2g", b2, 0, 2437)
	if c.Channel != 1 {
		t.Errorf("avoid ch 6 with ch 11 passive: got %d, want 1", c.Channel)
	}
	if _, err := Choose("2g", band(nl80211.Frequency{MHz: 2412, Disabled: true}), 0, 0); err == nil {
		t.Error("no usable channel should be an error")
	}
}

func TestWiFiQR(t *testing.T) {
	if got := WiFiQR("a;b", "p:w"); got != `WIFI:T:WPA;S:a\;b;P:p\:w;;` {
		t.Errorf("got %q", got)
	}
}
