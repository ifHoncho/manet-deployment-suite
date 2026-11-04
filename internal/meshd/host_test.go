package meshd

import (
	"strings"
	"testing"
)

// With no ports listed, the files are exactly what earlier builds wrote, so
// an upgrade does not make NetworkManager or networkd reload.
func TestUnmanagedConfStable(t *testing.T) {
	const nm = "# Written by trellis meshd. Interfaces owned by Trellis must not be managed\n# by NetworkManager.\n[keyfile]\n" +
		"unmanaged-devices=interface-name:mesh0;interface-name:mesh1;interface-name:ap0;interface-name:bat0;interface-name:br-mesh;interface-name:anycast0\n"
	const nd = "# Written by trellis meshd. Interfaces owned by Trellis must not be managed\n# by systemd-networkd.\n[Match]\n" +
		"Name=mesh0 mesh1 ap0 bat0 br-mesh anycast0\n\n[Link]\nUnmanaged=yes\n"
	if got := nmConf(nil); got != nm {
		t.Errorf("nmConf(nil) changed:\n%s", got)
	}
	if got := networkdConf(nil); got != nd {
		t.Errorf("networkdConf(nil) changed:\n%s", got)
	}
	if got := nmConf([]string{"eth1"}); !strings.Contains(got, ";interface-name:eth1\n") {
		t.Errorf("listed port missing:\n%s", got)
	}
	if got := networkdConf([]string{"eth1"}); !strings.Contains(got, "anycast0 eth1\n") {
		t.Errorf("listed port missing:\n%s", got)
	}
}
