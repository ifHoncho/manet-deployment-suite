package rtnl

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ethtool ioctl commands (linux/ethtool.h).
const (
	ethtoolGStrings  = 0x1b
	ethtoolGStats    = 0x1d
	ethtoolGSSetInfo = 0x37
	ethSSStats       = 1
	ethGStringLen    = 32
)

type ifreqData struct {
	name [unix.IFNAMSIZ]byte
	data uintptr
	_    [16]byte // rest of the union
}

func ethtoolIoctl(fd int, ifname string, buf []byte) error {
	var ifr ifreqData
	copy(ifr.name[:], ifname)
	ifr.data = uintptr(unsafe.Pointer(&buf[0]))
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), unix.SIOCETHTOOL, uintptr(unsafe.Pointer(&ifr)))
	runtime.KeepAlive(buf)
	if errno != 0 {
		return errno
	}
	return nil
}

// EthtoolStats reads an interface's ethtool statistics ("ethtool -S"), by
// name. batman-adv publishes its counters (including the multicast packet
// type's mcast_tx, mcast_rx and mcast_fwd) only this way.
func EthtoolStats(ifname string) (map[string]uint64, error) {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)
	ne := binary.NativeEndian

	// How many statistics: struct ethtool_sset_info {cmd, reserved, u64 mask, u32 data[]}.
	info := make([]byte, 4+4+8+4)
	ne.PutUint32(info[0:], ethtoolGSSetInfo)
	ne.PutUint64(info[8:], 1<<ethSSStats)
	if err := ethtoolIoctl(fd, ifname, info); err != nil {
		return nil, fmt.Errorf("ethtool sset info on %s: %w", ifname, err)
	}
	if ne.Uint64(info[8:])&(1<<ethSSStats) == 0 {
		return map[string]uint64{}, nil
	}
	n := int(ne.Uint32(info[16:]))
	if n == 0 || n > 4096 {
		return map[string]uint64{}, nil
	}

	// Names: struct ethtool_gstrings {cmd, string_set, len, u8 data[]}.
	names := make([]byte, 12+n*ethGStringLen)
	ne.PutUint32(names[0:], ethtoolGStrings)
	ne.PutUint32(names[4:], ethSSStats)
	ne.PutUint32(names[8:], uint32(n))
	if err := ethtoolIoctl(fd, ifname, names); err != nil {
		return nil, fmt.Errorf("ethtool strings on %s: %w", ifname, err)
	}

	// Values: struct ethtool_stats {cmd, n_stats, u64 data[]}.
	vals := make([]byte, 8+n*8)
	ne.PutUint32(vals[0:], ethtoolGStats)
	ne.PutUint32(vals[4:], uint32(n))
	if err := ethtoolIoctl(fd, ifname, vals); err != nil {
		return nil, fmt.Errorf("ethtool stats on %s: %w", ifname, err)
	}
	got := int(ne.Uint32(vals[4:]))
	if got < n {
		n = got
	}
	out := make(map[string]uint64, n)
	for i := 0; i < n; i++ {
		raw := names[12+i*ethGStringLen : 12+(i+1)*ethGStringLen]
		name := string(bytes.TrimRight(raw, "\x00"))
		out[name] = ne.Uint64(vals[8+i*8:])
	}
	return out, nil
}
