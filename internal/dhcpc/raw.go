package dhcpc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// Conn carries DHCP messages on one interface.
type Conn interface {
	// Send transmits a BOOTP payload in UDP/IPv4 from src:68 to dst:67, in
	// a frame to dstMAC.
	Send(dstMAC net.HardwareAddr, src, dst net.IP, payload []byte) error
	// Recv returns the next BOOTP payload sent to UDP port 67 or 68, and
	// the sending MAC, until the deadline (ErrTimeout).
	Recv(deadline time.Time) (payload []byte, from net.HardwareAddr, err error)
	Close() error
}

// ErrTimeout is returned by Recv when nothing arrived before the deadline.
var ErrTimeout = errors.New("timeout")

// Broadcast is the Ethernet broadcast address.
var Broadcast = net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}

const (
	portServer = 67
	portClient = 68
)

type rawConn struct {
	fd      int
	ifindex int
}

func htons(v uint16) uint16 { return v<<8 | v>>8 }

// Open opens a link-layer socket on ifname that sees only UDP to ports 67
// and 68. It needs CAP_NET_RAW.
func Open(ifname string) (Conn, error) {
	ifi, err := net.InterfaceByName(ifname)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, int(htons(unix.ETH_P_IP)))
	if err != nil {
		return nil, fmt.Errorf("packet socket: %w", err)
	}
	// The filter goes on before bind, so no unfiltered traffic queues up.
	// The packet starts at the IPv4 header (SOCK_DGRAM).
	filter := []unix.SockFilter{
		{Code: unix.BPF_LD | unix.BPF_B | unix.BPF_ABS, K: 9},                 // 0: protocol
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 0, Jf: 7, K: 17}, // 1: UDP?
		{Code: unix.BPF_LD | unix.BPF_H | unix.BPF_ABS, K: 6},                 // 2: flags/fragment
		{Code: unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K, Jt: 5, Jf: 0, K: 0x1fff},
		{Code: unix.BPF_LDX | unix.BPF_B | unix.BPF_MSH, K: 0},                        // 4: X = header length
		{Code: unix.BPF_LD | unix.BPF_H | unix.BPF_IND, K: 2},                         // 5: destination port
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 1, Jf: 0, K: portClient}, // 6
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 0, Jf: 1, K: portServer}, // 7
		{Code: unix.BPF_RET | unix.BPF_K, K: 0xffff},                                  // 8: accept
		{Code: unix.BPF_RET | unix.BPF_K, K: 0},                                       // 9: drop
	}
	prog := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	if err := unix.SetsockoptSockFprog(fd, unix.SOL_SOCKET, unix.SO_ATTACH_FILTER, &prog); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("attach filter: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_IP), Ifindex: ifi.Index}); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("bind to %s: %w", ifname, err)
	}
	return &rawConn{fd: fd, ifindex: ifi.Index}, nil
}

func (c *rawConn) Send(dstMAC net.HardwareAddr, src, dst net.IP, payload []byte) error {
	pkt := udp4(src, dst, portClient, portServer, payload)
	sa := &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_IP), Ifindex: c.ifindex, Halen: 6}
	copy(sa.Addr[:], dstMAC)
	return unix.Sendto(c.fd, pkt, 0, sa)
}

func (c *rawConn) Recv(deadline time.Time) ([]byte, net.HardwareAddr, error) {
	buf := make([]byte, 2048)
	for {
		wait := time.Until(deadline)
		if wait <= 0 {
			return nil, nil, ErrTimeout
		}
		ms := int(wait / time.Millisecond)
		if ms < 1 {
			ms = 1
		}
		fds := []unix.PollFd{{Fd: int32(c.fd), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, ms)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return nil, nil, err
		}
		if n == 0 {
			return nil, nil, ErrTimeout
		}
		m, from, err := unix.Recvfrom(c.fd, buf, unix.MSG_DONTWAIT)
		if err != nil {
			if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
				continue
			}
			return nil, nil, err
		}
		ll, ok := from.(*unix.SockaddrLinklayer)
		if !ok || ll.Pkttype == unix.PACKET_OUTGOING {
			continue // our own transmissions
		}
		payload, ok := udpPayload(buf[:m])
		if !ok {
			continue
		}
		mac := net.HardwareAddr(append([]byte{}, ll.Addr[:ll.Halen]...))
		return payload, mac, nil
	}
}

func (c *rawConn) Close() error { return os.NewSyscallError("close", unix.Close(c.fd)) }

// udp4 builds an IPv4 packet holding one UDP datagram.
func udp4(src, dst net.IP, sport, dport uint16, payload []byte) []byte {
	b := make([]byte, 28+len(payload))
	b[0] = 0x45
	b[1] = 0x10 // low delay, as DHCP clients mark it
	binary.BigEndian.PutUint16(b[2:], uint16(len(b)))
	b[8] = 64
	b[9] = 17
	copy(b[12:16], src.To4())
	copy(b[16:20], dst.To4())
	binary.BigEndian.PutUint16(b[10:], checksum(b[:20]))
	binary.BigEndian.PutUint16(b[20:], sport)
	binary.BigEndian.PutUint16(b[22:], dport)
	binary.BigEndian.PutUint16(b[24:], uint16(8+len(payload)))
	copy(b[28:], payload)
	// UDP checksum 0: none, which IPv4 allows.
	return b
}

// udpPayload returns the UDP payload of an IPv4 packet.
func udpPayload(b []byte) ([]byte, bool) {
	if len(b) < 20 || b[0]>>4 != 4 {
		return nil, false
	}
	ihl := int(b[0]&0x0f) * 4
	total := int(binary.BigEndian.Uint16(b[2:]))
	if ihl < 20 || total > len(b) || total < ihl+8 || b[9] != 17 {
		return nil, false
	}
	ulen := int(binary.BigEndian.Uint16(b[ihl+4:]))
	if ulen < 8 || ihl+ulen > total {
		return nil, false
	}
	return b[ihl+8 : ihl+ulen], true
}

func checksum(b []byte) uint16 {
	var s uint32
	for i := 0; i+1 < len(b); i += 2 {
		s += uint32(binary.BigEndian.Uint16(b[i:]))
	}
	if len(b)%2 == 1 {
		s += uint32(b[len(b)-1]) << 8
	}
	for s > 0xffff {
		s = s>>16 + s&0xffff
	}
	return ^uint16(s)
}
