// Package sysfs reads facts about radios that only sysfs exposes: which bus a
// radio sits on, where it is plugged in, its USB or PCI identity, and rfkill.
package sysfs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// DefaultRoot is the real sysfs mount.
const DefaultRoot = "/sys"

// Bus kinds.
const (
	BusUSB      = "usb"
	BusPCI      = "pci"
	BusSDIO     = "sdio"
	BusPlatform = "platform"
	BusVirtual  = "virtual"
	BusUnknown  = "unknown"
)

// Device describes the hardware behind one wiphy.
type Device struct {
	Bus    string
	Driver string
	// Path is the resolved sysfs path of the device providing the wiphy.
	Path string
	// Port is a stable "where is it plugged in" identifier, independent of
	// probe order: usb:1-1.2, pci:0000:01:00.0, sdio:mmc1:0001:1,
	// platform:fe300000.mmc, virtual:hwsim0.
	Port string
	// VendorID and ProductID are lower-case hex without 0x, for USB and PCI.
	VendorID  string
	ProductID string
	// Serial is the USB serial string, if the adapter has one.
	Serial string
	// USBSpeedMbps is the negotiated USB speed: 480 for USB 2, 5000+ for USB 3.
	USBSpeedMbps int
}

// DeviceID returns "vid:pid" for USB and PCI devices, empty otherwise.
func (d Device) DeviceID() string {
	if d.VendorID == "" || d.ProductID == "" {
		return ""
	}
	return d.VendorID + ":" + d.ProductID
}

var (
	rePCIAddr = regexp.MustCompile(`^[0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-7]$`)
	reUSBDev  = regexp.MustCompile(`^\d+-[\d.]+$`) // 1-1, 1-1.2 (an interface would be 1-1.2:1.0)
	reSDIO    = regexp.MustCompile(`^mmc\d+:[0-9a-f]{4}:\d+$`)
)

// PhyDevice identifies the device behind /sys/class/ieee80211/<phy>.
func PhyDevice(root, phy string) (Device, error) {
	link := filepath.Join(root, "class", "ieee80211", phy, "device")
	path, err := filepath.EvalSymlinks(link)
	if err != nil {
		return Device{}, fmt.Errorf("resolve %s: %w", link, err)
	}
	d := Device{Path: path, Bus: BusUnknown}
	if drv, err := os.Readlink(filepath.Join(path, "driver")); err == nil {
		d.Driver = filepath.Base(drv)
	}

	// Walk from the device upwards, classifying by directory name and
	// contents. The first recognisable ancestor wins.
	for p := path; p != "/" && strings.HasPrefix(p, filepath.Join(root, "devices")); p = filepath.Dir(p) {
		base := filepath.Base(p)
		switch {
		case reUSBDev.MatchString(base) && exists(filepath.Join(p, "idVendor")):
			d.Bus = BusUSB
			d.Port = "usb:" + base
			d.VendorID = readTrim(p, "idVendor")
			d.ProductID = readTrim(p, "idProduct")
			d.Serial = readTrim(p, "serial")
			d.USBSpeedMbps, _ = strconv.Atoi(readTrim(p, "speed"))
			return d, nil
		case rePCIAddr.MatchString(base) && exists(filepath.Join(p, "vendor")):
			d.Bus = BusPCI
			d.Port = "pci:" + base
			d.VendorID = strings.TrimPrefix(readTrim(p, "vendor"), "0x")
			d.ProductID = strings.TrimPrefix(readTrim(p, "device"), "0x")
			return d, nil
		case reSDIO.MatchString(base) && exists(filepath.Join(p, "vendor")):
			d.Bus = BusSDIO
			d.Port = "sdio:" + base
			d.VendorID = strings.TrimPrefix(readTrim(p, "vendor"), "0x")
			d.ProductID = strings.TrimPrefix(readTrim(p, "device"), "0x")
			return d, nil
		case strings.Contains(p, "/devices/virtual/"):
			d.Bus = BusVirtual
			d.Port = "virtual:" + base
			return d, nil
		case filepath.Base(filepath.Dir(p)) == "platform":
			d.Bus = BusPlatform
			d.Port = "platform:" + base
			return d, nil
		}
	}
	// Fall back to the raw path so the radio is still identifiable.
	d.Port = "path:" + strings.TrimPrefix(path, root)
	return d, nil
}

// Phys lists the wiphy names under /sys/class/ieee80211.
func Phys(root string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(root, "class", "ieee80211"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out, nil
}

// PhyIndex reads /sys/class/ieee80211/<phy>/index.
func PhyIndex(root, phy string) (int, error) {
	s := readTrim(filepath.Join(root, "class", "ieee80211", phy), "index")
	if s == "" {
		return 0, fmt.Errorf("no index for %s", phy)
	}
	return strconv.Atoi(s)
}

// RFKillUnblock clears the soft rfkill block on a wiphy, if it has one.
// It returns whether anything was changed.
func RFKillUnblock(root, phy string) (bool, error) {
	matches, _ := filepath.Glob(filepath.Join(root, "class", "ieee80211", phy, "rfkill*"))
	changed := false
	for _, m := range matches {
		soft := filepath.Join(m, "soft")
		if readTrim(m, "soft") == "1" {
			if err := os.WriteFile(soft, []byte("0"), 0o644); err != nil {
				return changed, fmt.Errorf("unblock %s: %w", soft, err)
			}
			changed = true
		}
	}
	return changed, nil
}

// RFKillHardBlocked reports whether a hardware switch blocks the radio.
func RFKillHardBlocked(root, phy string) bool {
	matches, _ := filepath.Glob(filepath.Join(root, "class", "ieee80211", phy, "rfkill*"))
	for _, m := range matches {
		if readTrim(m, "hard") == "1" {
			return true
		}
	}
	return false
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func readTrim(dir, file string) string {
	b, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
