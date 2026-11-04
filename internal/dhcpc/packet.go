// Package dhcpc is meshd's DHCPv4 client for Ethernet ports in the auto role
// (architecture §7.1): it probes a port for a DHCP server, and on a WAN it
// holds the lease (renew, rebind, expiry). It talks over a link-layer socket,
// so it works on a port with no address and never needs UDP port 68, which
// a system DHCP client may already hold.
package dhcpc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"time"
)

// BOOTP operations and DHCP message types (RFC 2131, 2132).
const (
	opRequest = 1
	opReply   = 2

	Discover = 1
	Offer    = 2
	Request  = 3
	Decline  = 4
	Ack      = 5
	Nak      = 6
	Release  = 7
)

// Option codes used here.
const (
	optPad         = 0
	optSubnetMask  = 1
	optRouter      = 3
	optDNS         = 6
	optHostname    = 12
	optDomain      = 15
	optRequestedIP = 50
	optLeaseTime   = 51
	optMsgType     = 53
	optServerID    = 54
	optParams      = 55
	optT1          = 58
	optT2          = 59
	optVendorClass = 60
	optClientID    = 61
	optEnd         = 255
)

var magic = []byte{99, 130, 83, 99}

// Packet is one BOOTP/DHCP message.
type Packet struct {
	Op      byte
	XID     uint32
	Secs    uint16
	Flags   uint16
	CIAddr  net.IP
	YIAddr  net.IP
	SIAddr  net.IP
	GIAddr  net.IP
	CHAddr  net.HardwareAddr
	Options map[byte][]byte
}

// MsgType is option 53, or 0.
func (p *Packet) MsgType() byte {
	if v := p.Options[optMsgType]; len(v) == 1 {
		return v[0]
	}
	return 0
}

// ip4Opt returns the first IPv4 address in an option.
func (p *Packet) ip4Opt(code byte) net.IP {
	if v := p.Options[code]; len(v) >= 4 {
		return net.IPv4(v[0], v[1], v[2], v[3]).To4()
	}
	return nil
}

// ServerID is option 54.
func (p *Packet) ServerID() net.IP { return p.ip4Opt(optServerID) }

// VendorClass is option 60.
func (p *Packet) VendorClass() string { return string(p.Options[optVendorClass]) }

func (p *Packet) seconds(code byte) time.Duration {
	if v := p.Options[code]; len(v) == 4 {
		return time.Duration(binary.BigEndian.Uint32(v)) * time.Second
	}
	return 0
}

// Marshal encodes the packet.
func (p *Packet) Marshal() []byte {
	b := make([]byte, 240, 300)
	b[0], b[1], b[2] = p.Op, 1, 6 // Ethernet, 6-byte addresses
	binary.BigEndian.PutUint32(b[4:], p.XID)
	binary.BigEndian.PutUint16(b[8:], p.Secs)
	binary.BigEndian.PutUint16(b[10:], p.Flags)
	for i, ip := range []net.IP{p.CIAddr, p.YIAddr, p.SIAddr, p.GIAddr} {
		if v4 := ip.To4(); v4 != nil {
			copy(b[12+4*i:], v4)
		}
	}
	copy(b[28:44], p.CHAddr)
	copy(b[236:], magic)
	// Message type first, as some servers expect.
	if v, ok := p.Options[optMsgType]; ok {
		b = append(b, optMsgType, byte(len(v)))
		b = append(b, v...)
	}
	for code := 1; code < 255; code++ {
		v, ok := p.Options[byte(code)]
		if !ok || code == optMsgType {
			continue
		}
		b = append(b, byte(code), byte(len(v)))
		b = append(b, v...)
	}
	b = append(b, optEnd)
	for len(b) < 300 { // BOOTP minimum; some relays drop shorter
		b = append(b, optPad)
	}
	return b
}

// Parse decodes a BOOTP/DHCP message.
func Parse(b []byte) (*Packet, error) {
	if len(b) < 240 {
		return nil, errors.New("short BOOTP message")
	}
	if string(b[236:240]) != string(magic) {
		return nil, errors.New("no DHCP magic cookie")
	}
	p := &Packet{
		Op: b[0], XID: binary.BigEndian.Uint32(b[4:]), Secs: binary.BigEndian.Uint16(b[8:]), Flags: binary.BigEndian.Uint16(b[10:]),
		CIAddr: net.IP(append([]byte{}, b[12:16]...)), YIAddr: net.IP(append([]byte{}, b[16:20]...)),
		SIAddr: net.IP(append([]byte{}, b[20:24]...)), GIAddr: net.IP(append([]byte{}, b[24:28]...)),
		Options: map[byte][]byte{},
	}
	hlen := int(b[2])
	if hlen > 16 {
		hlen = 16
	}
	p.CHAddr = net.HardwareAddr(append([]byte{}, b[28:28+hlen]...))
	opts := b[240:]
	for i := 0; i < len(opts); {
		code := opts[i]
		if code == optPad {
			i++
			continue
		}
		if code == optEnd {
			break
		}
		if i+1 >= len(opts) || i+2+int(opts[i+1]) > len(opts) {
			return nil, fmt.Errorf("option %d overruns the message", code)
		}
		n := int(opts[i+1])
		// Concatenate repeated options (RFC 3396).
		p.Options[code] = append(p.Options[code], opts[i+2:i+2+n]...)
		i += 2 + n
	}
	return p, nil
}

// Lease is an acknowledged address.
type Lease struct {
	Addr      net.IPNet // address and mask
	Router    net.IP    // nil: none offered
	DNS       []net.IP
	Domain    string
	Server    net.IP           // server identifier
	ServerMAC net.HardwareAddr // next hop towards the server, from the ACK's frame
	Duration  time.Duration
	T1, T2    time.Duration
	Start     time.Time
}

// Subnet is the lease's network.
func (l *Lease) Subnet() *net.IPNet {
	return &net.IPNet{IP: l.Addr.IP.Mask(l.Addr.Mask), Mask: l.Addr.Mask}
}

// Expires is when the lease runs out.
func (l *Lease) Expires() time.Time { return l.Start.Add(l.Duration) }

// leaseFrom builds a lease from an ACK.
func leaseFrom(ack *Packet, from net.HardwareAddr, now time.Time) (*Lease, error) {
	ip := ack.YIAddr.To4()
	if ip == nil || ip.IsUnspecified() {
		return nil, errors.New("ACK without an address")
	}
	mask := net.IPMask(ack.Options[optSubnetMask])
	if len(mask) != 4 {
		mask = ip.DefaultMask()
	}
	l := &Lease{Addr: net.IPNet{IP: ip, Mask: mask}, Router: ack.ip4Opt(optRouter), Domain: string(ack.Options[optDomain]),
		Server: ack.ServerID(), ServerMAC: from, Duration: ack.seconds(optLeaseTime), Start: now}
	if l.Server == nil {
		l.Server = ack.SIAddr.To4()
	}
	for v := ack.Options[optDNS]; len(v) >= 4; v = v[4:] {
		l.DNS = append(l.DNS, net.IPv4(v[0], v[1], v[2], v[3]).To4())
	}
	if l.Duration == 0 {
		l.Duration = time.Hour
	}
	if l.Duration == 0xffffffff*time.Second { // infinite
		l.Duration = 365 * 24 * time.Hour
	}
	l.T1, l.T2 = ack.seconds(optT1), ack.seconds(optT2)
	if l.T1 <= 0 || l.T1 >= l.Duration {
		l.T1 = l.Duration / 2
	}
	if l.T2 <= l.T1 || l.T2 >= l.Duration {
		l.T2 = l.Duration * 7 / 8
	}
	return l, nil
}
