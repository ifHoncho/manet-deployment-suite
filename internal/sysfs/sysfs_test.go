package sysfs

import (
	"os"
	"path/filepath"
	"testing"
)

func mk(t *testing.T, root string, rel string, files map[string]string) string {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	for k, v := range files {
		if err := os.WriteFile(filepath.Join(p, k), []byte(v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func linkPhy(t *testing.T, root, phy, target string) {
	t.Helper()
	dir := filepath.Join(root, "class", "ieee80211", phy)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "device")); err != nil {
		t.Fatal(err)
	}
}

func TestUSBDevice(t *testing.T) {
	root := t.TempDir()
	usbdev := mk(t, root, "devices/pci0000:00/0000:00:14.0/usb1/1-1/1-1.2",
		map[string]string{"idVendor": "0e8d", "idProduct": "7612", "serial": "ABC123", "speed": "5000"})
	iface := mk(t, root, "devices/pci0000:00/0000:00:14.0/usb1/1-1/1-1.2/1-1.2:1.0", nil)
	drvdir := mk(t, root, "bus/usb/drivers/mt76x2u", nil)
	os.Symlink(drvdir, filepath.Join(iface, "driver"))
	linkPhy(t, root, "phy0", iface)
	_ = usbdev

	d, err := PhyDevice(root, "phy0")
	if err != nil {
		t.Fatal(err)
	}
	if d.Bus != BusUSB || d.Port != "usb:1-1.2" || d.DeviceID() != "0e8d:7612" || d.Serial != "ABC123" || d.USBSpeedMbps != 5000 || d.Driver != "mt76x2u" {
		t.Errorf("got %+v", d)
	}
}

func TestPCIDevice(t *testing.T) {
	root := t.TempDir()
	dev := mk(t, root, "devices/pci0000:00/0000:00:1c.0/0000:01:00.0",
		map[string]string{"vendor": "0x14c3", "device": "0x7961"})
	linkPhy(t, root, "phy1", dev)
	d, err := PhyDevice(root, "phy1")
	if err != nil {
		t.Fatal(err)
	}
	if d.Bus != BusPCI || d.Port != "pci:0000:01:00.0" || d.DeviceID() != "14c3:7961" {
		t.Errorf("got %+v", d)
	}
}

func TestVirtualDevice(t *testing.T) {
	root := t.TempDir()
	dev := mk(t, root, "devices/virtual/mac80211_hwsim/hwsim3", nil)
	linkPhy(t, root, "phy3", dev)
	d, err := PhyDevice(root, "phy3")
	if err != nil {
		t.Fatal(err)
	}
	if d.Bus != BusVirtual || d.Port != "virtual:hwsim3" || d.DeviceID() != "" {
		t.Errorf("got %+v", d)
	}
}

func TestSDIODevice(t *testing.T) {
	root := t.TempDir()
	dev := mk(t, root, "devices/platform/soc/fe300000.mmc/mmc_host/mmc1/mmc1:0001/mmc1:0001:1",
		map[string]string{"vendor": "0x02d0", "device": "0xa9a6"})
	linkPhy(t, root, "phy0", dev)
	d, err := PhyDevice(root, "phy0")
	if err != nil {
		t.Fatal(err)
	}
	if d.Bus != BusSDIO || d.Port != "sdio:mmc1:0001:1" || d.DeviceID() != "02d0:a9a6" {
		t.Errorf("got %+v", d)
	}
}

func TestRFKill(t *testing.T) {
	root := t.TempDir()
	mk(t, root, "class/ieee80211/phy0/rfkill2", map[string]string{"soft": "1", "hard": "0"})
	changed, err := RFKillUnblock(root, "phy0")
	if err != nil || !changed {
		t.Fatalf("unblock: changed=%v err=%v", changed, err)
	}
	if readTrim(filepath.Join(root, "class/ieee80211/phy0/rfkill2"), "soft") != "0" {
		t.Error("soft block not cleared")
	}
	changed, _ = RFKillUnblock(root, "phy0")
	if changed {
		t.Error("second unblock must be a no-op")
	}
	if RFKillHardBlocked(root, "phy0") {
		t.Error("not hard blocked")
	}
}
