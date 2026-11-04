package meshctl

import (
	"bytes"
	"strings"
	"testing"

	"meridian/trellis/internal/status"
)

func routesFixture() (status.Batman, []status.Peer) {
	n1, n2, pi, wired := "9c:ed:d5:fd:5c:f1", "9c:ef:d5:fb:2d:26", "9c:ef:d5:fd:7a:ea", "02:00:00:00:00:09"
	peers := []status.Peer{
		{MAC: n2, PlinkState: "ESTAB", Established: true, SignalDBm: -46, TXMbps: 72.2, RXMbps: 72.2, InactiveMS: 40, ConnectedSeconds: 888},
		{MAC: pi, PlinkState: "ESTAB", Established: true, SignalDBm: -55, TXMbps: 72.2, RXMbps: 72.2, InactiveMS: 210, ConnectedSeconds: 868},
		{MAC: n1, PlinkState: "ESTAB", Established: true, SignalDBm: -67, TXMbps: 72.2, RXMbps: 39, InactiveMS: 247, ConnectedSeconds: 406},
	}
	b := status.Batman{
		HardIfs: []status.HardIf{{Name: "mesh0", Active: true}},
		Neighbors: []status.Neighbor{
			{MAC: n1, Via: "mesh0", ThroughputMbps: 2.1},
			{MAC: pi, Via: "mesh0", ThroughputMbps: 23.9},
			{MAC: n2, Via: "mesh0", ThroughputMbps: 23.9},
			{MAC: wired, Via: "eth1", ThroughputMbps: 1000, LastSeenMS: 12},
		},
		Originators: []status.Originator{
			{MAC: n1, NextHop: n2, Via: "mesh0", ThroughputMbps: 11.9, Best: true},
			{MAC: n1, NextHop: n1, Via: "mesh0", ThroughputMbps: 2.1},
			{MAC: pi, NextHop: pi, Via: "mesh0", ThroughputMbps: 23.9, Best: true},
			{MAC: n2, NextHop: n2, Via: "mesh0", ThroughputMbps: 23.9, Best: true},
		},
	}
	return b, peers
}

func TestPrintRoutes(t *testing.T) {
	b, peers := routesFixture()
	var buf bytes.Buffer
	names := map[string]string{"9c:ef:d5:fb:2d:26": "node-2"}
	printLinks(&buf, peers, b.Neighbors, names)
	printRoutes(&buf, b, peers, names)
	out := buf.String()
	t.Log("\n" + out)
	for _, want := range []string{
		"9c:ed:d5:fd:5c:f1  ?       relayed via node-2 (beats the direct link)",
		"3 other nodes: 2 direct, 1 relayed",
		"batman only (eth1)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q", want)
		}
	}
	// Direct routes are listed before relayed ones.
	if strings.Index(out, "9c:ef:d5:fd:7a:ea  ?       direct") > strings.Index(out, "relayed via") {
		t.Error("direct routes should come first")
	}
}
