package gossip

import (
	"context"
	"fmt"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// Conn is the gossip socket on one interface: bound to it, joined to the
// group on it, sending with TTL 1 and without loopback.
type Conn struct {
	pc    *net.UDPConn
	group *net.UDPAddr
}

// Listen opens the gossip socket on ifname. group and port can be
// overridden for tests ("" and 0 mean the defaults).
func Listen(ifname string, group string, port int) (*Conn, error) {
	if group == "" {
		group = Group
	}
	if port == 0 {
		port = Port
	}
	ifi, err := net.InterfaceByName(ifname)
	if err != nil {
		return nil, err
	}
	gip := net.ParseIP(group).To4()
	if gip == nil {
		return nil, fmt.Errorf("bad gossip group %q", group)
	}
	lc := net.ListenConfig{Control: func(network, address string, c syscall.RawConn) error {
		var serr error
		err := c.Control(func(fd uintptr) {
			s := int(fd)
			if serr = unix.SetsockoptInt(s, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); serr != nil {
				return
			}
			if serr = unix.BindToDevice(s, ifname); serr != nil {
				return
			}
			mreq := &unix.IPMreqn{Ifindex: int32(ifi.Index)}
			copy(mreq.Multiaddr[:], gip)
			if serr = unix.SetsockoptIPMreqn(s, unix.IPPROTO_IP, unix.IP_ADD_MEMBERSHIP, mreq); serr != nil {
				serr = fmt.Errorf("join %s on %s: %w", group, ifname, serr)
				return
			}
			if serr = unix.SetsockoptIPMreqn(s, unix.IPPROTO_IP, unix.IP_MULTICAST_IF, &unix.IPMreqn{Ifindex: int32(ifi.Index)}); serr != nil {
				return
			}
			if serr = unix.SetsockoptInt(s, unix.IPPROTO_IP, unix.IP_MULTICAST_TTL, 1); serr != nil {
				return
			}
			serr = unix.SetsockoptInt(s, unix.IPPROTO_IP, unix.IP_MULTICAST_LOOP, 0)
		})
		if err != nil {
			return err
		}
		return serr
	}}
	c, err := lc.ListenPacket(context.Background(), "udp4", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		return nil, err
	}
	return &Conn{pc: c.(*net.UDPConn), group: &net.UDPAddr{IP: gip, Port: port}}, nil
}

// Send multicasts one packet.
func (c *Conn) Send(pkt []byte) error {
	_, err := c.pc.WriteToUDP(pkt, c.group)
	return err
}

// Receive reads one packet.
func (c *Conn) Receive(buf []byte) (int, *net.UDPAddr, error) {
	return c.pc.ReadFromUDP(buf)
}

// Close closes the socket.
func (c *Conn) Close() error { return c.pc.Close() }
