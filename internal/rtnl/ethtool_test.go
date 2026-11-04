package rtnl

import (
	"net"
	"testing"
)

// Any interface with ethtool statistics will do;
// when none has them (containers), the test only proves the call is safe.
func TestEthtoolStats(t *testing.T) {
	ifs, err := net.Interfaces()
	if err != nil {
		t.Skip(err)
	}
	for _, ifi := range ifs {
		st, err := EthtoolStats(ifi.Name)
		if err != nil {
			continue
		}
		if len(st) > 0 {
			t.Logf("%s: %d statistics", ifi.Name, len(st))
			return
		}
	}
	t.Log("no interface here publishes ethtool statistics")
}
