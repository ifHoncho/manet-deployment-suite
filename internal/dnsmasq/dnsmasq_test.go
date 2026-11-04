package dnsmasq

import (
	"strings"
	"testing"
	"time"
)

func TestRender(t *testing.T) {
	out := Render(Config{Interfaces: []string{"br-mesh"}, NoDHCPInterfaces: []string{"anycast0"}, Domain: "mesh",
		PoolStart: "10.73.1.100", PoolEnd: "10.73.1.199", Netmask: "255.255.0.0", LeaseTime: "24h", Router: "10.73.0.1", DNS: "10.73.0.1",
		LeaseFile: "/var/lib/trellis/dnsmasq.leases", HostRecords: []HostRecord{{Name: "node-1", IP: "10.73.1.1"}}})
	for _, want := range []string{"bind-dynamic", "interface=br-mesh", "no-dhcp-interface=anycast0", "dhcp-range=10.73.1.100,10.73.1.199,255.255.0.0,24h",
		"dhcp-option=option:router,10.73.0.1", "host-record=node-1.mesh,10.73.1.1", "dhcp-authoritative", "no-resolv", "local=/mesh/"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(out, "server=") {
		t.Error("no upstream expected")
	}
}

func TestParseLeases(t *testing.T) {
	in := "1762200000 aa:bb:cc:dd:ee:ff 10.73.1.100 pixel 01:aa:bb:cc:dd:ee:ff\n1762200001 11:22:33:44:55:66 10.73.1.101 * *\nbad line\n"
	ls := ParseLeases(strings.NewReader(in))
	if len(ls) != 2 {
		t.Fatalf("got %d leases", len(ls))
	}
	if ls[0].Hostname != "pixel" || ls[0].IP != "10.73.1.100" || !ls[0].Expires.Equal(time.Unix(1762200000, 0)) {
		t.Errorf("lease 0: %+v", ls[0])
	}
	if ls[1].Hostname != "" || ls[1].ClientID != "" {
		t.Errorf("lease 1: %+v", ls[1])
	}
}

func TestFiles(t *testing.T) {
	if got := ServersFileContent([]string{"10.73.2.1"}); !strings.Contains(got, "server=10.73.2.1\n") {
		t.Errorf("servers file: %q", got)
	}
	if got := HostsFileContent("mesh", []HostRecord{{Name: "node-2", IP: "10.73.2.1"}}); !strings.Contains(got, "10.73.2.1 node-2.mesh node-2\n") {
		t.Errorf("hosts file: %q", got)
	}
	out := Render(Config{Domain: "mesh", ServersFile: "/run/x/servers", HostsFile: "/run/x/hosts"})
	if !strings.Contains(out, "servers-file=/run/x/servers") || !strings.Contains(out, "addn-hosts=/run/x/hosts") {
		t.Errorf("render: %s", out)
	}
}
