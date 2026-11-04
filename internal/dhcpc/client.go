package dhcpc

import (
	"context"
	"encoding/binary"
	"errors"
	"math/rand/v2"
	"net"
	"time"
)

// Errors.
var (
	ErrNoOffer = errors.New("no DHCP server answered")
	ErrPeer    = errors.New("another Trellis node is probing this link")
	ErrNak     = errors.New("the DHCP server refused the address (NAK)")
	ErrNoAck   = errors.New("no answer to the DHCP request")
)

// Client runs DHCP exchanges over Conn.
type Client struct {
	Conn     Conn
	MAC      net.HardwareAddr
	Hostname string
	// VendorClass is sent as option 60. A DISCOVER from another client
	// with the same value is another Trellis node probing the same cable
	// (ErrPeer).
	VendorClass string
	// RequestTimeout is how long to wait for each answer to a REQUEST.
	RequestTimeout time.Duration
	Now            func() time.Time
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) base(msgType byte, xid uint32) *Packet {
	p := &Packet{Op: opRequest, XID: xid, CHAddr: c.MAC, Options: map[byte][]byte{
		optMsgType:  {msgType},
		optParams:   {optSubnetMask, optRouter, optDNS, optDomain, optLeaseTime, optT1, optT2},
		optClientID: append([]byte{1}, c.MAC...),
	}}
	if c.Hostname != "" {
		p.Options[optHostname] = []byte(c.Hostname)
	}
	if c.VendorClass != "" {
		p.Options[optVendorClass] = []byte(c.VendorClass)
	}
	return p
}

// wait receives until match accepts a packet, the deadline passes, or ctx
// ends. A DISCOVER from a Trellis peer ends the wait with ErrPeer when
// peers is set.
func (c *Client) wait(ctx context.Context, deadline time.Time, peers bool, match func(*Packet) bool) (*Packet, net.HardwareAddr, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if !c.now().Before(deadline) {
			return nil, nil, ErrTimeout
		}
		slice := time.Now().Add(250 * time.Millisecond)
		if d := time.Now().Add(deadline.Sub(c.now())); d.Before(slice) {
			slice = d
		}
		b, from, err := c.Conn.Recv(slice)
		if errors.Is(err, ErrTimeout) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		p, err := Parse(b)
		if err != nil {
			continue
		}
		if p.Op == opRequest {
			if peers && c.VendorClass != "" && p.VendorClass() == c.VendorClass && p.MsgType() == Discover && !bytesEqual(p.CHAddr, c.MAC) {
				return p, from, ErrPeer
			}
			continue
		}
		if p.Op == opReply && bytesEqual(p.CHAddr, c.MAC) && match(p) {
			return p, from, nil
		}
	}
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Probe broadcasts DISCOVERs at the offsets in schedule (from the start)
// and waits until total has passed for the first OFFER. requested, if set,
// asks for a previous address again.
func (c *Client) Probe(ctx context.Context, schedule []time.Duration, total time.Duration, requested net.IP) (*Packet, error) {
	xid := rand.Uint32()
	start := c.now()
	disc := c.base(Discover, xid)
	if v4 := requested.To4(); v4 != nil {
		disc.Options[optRequestedIP] = v4
	}
	next := 0
	for {
		if next < len(schedule) && !c.now().Before(start.Add(schedule[next])) {
			disc.Secs = uint16(c.now().Sub(start) / time.Second)
			if err := c.Conn.Send(Broadcast, net.IPv4zero, net.IPv4bcast, disc.Marshal()); err != nil {
				return nil, err
			}
			next++
		}
		until := start.Add(total)
		if next < len(schedule) && start.Add(schedule[next]).Before(until) {
			until = start.Add(schedule[next])
		}
		p, _, err := c.wait(ctx, until, true, func(p *Packet) bool { return p.XID == xid && p.MsgType() == Offer })
		switch {
		case err == nil:
			return p, nil
		case errors.Is(err, ErrTimeout):
			if !c.now().Before(start.Add(total)) {
				return nil, ErrNoOffer
			}
		default:
			return nil, err
		}
	}
}

// Request takes an offer (the SELECTING state) and returns the lease.
func (c *Client) Request(ctx context.Context, offer *Packet) (*Lease, error) {
	req := c.base(Request, offer.XID)
	req.Options[optRequestedIP] = offer.YIAddr.To4()
	if sid := offer.ServerID(); sid != nil {
		req.Options[optServerID] = sid
	}
	return c.exchange(ctx, req, Broadcast, net.IPv4zero, net.IPv4bcast, 3)
}

// Renew extends a lease: unicast to the server (RENEWING), or broadcast
// when rebind is set (REBINDING, after T2).
func (c *Client) Renew(ctx context.Context, l *Lease, rebind bool) (*Lease, error) {
	req := c.base(Request, rand.Uint32())
	req.CIAddr = l.Addr.IP
	mac, dst := l.ServerMAC, l.Server
	if rebind || mac == nil || dst == nil {
		mac, dst = Broadcast, net.IPv4bcast
	}
	return c.exchange(ctx, req, mac, l.Addr.IP, dst, 1)
}

func (c *Client) exchange(ctx context.Context, req *Packet, mac net.HardwareAddr, src, dst net.IP, tries int) (*Lease, error) {
	timeout := c.RequestTimeout
	if timeout == 0 {
		timeout = 4 * time.Second
	}
	for i := 0; i < tries; i++ {
		if err := c.Conn.Send(mac, src, dst, req.Marshal()); err != nil {
			return nil, err
		}
		p, from, err := c.wait(ctx, c.now().Add(timeout), false, func(p *Packet) bool {
			return p.XID == req.XID && (p.MsgType() == Ack || p.MsgType() == Nak)
		})
		if errors.Is(err, ErrTimeout) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if p.MsgType() == Nak {
			return nil, ErrNak
		}
		return leaseFrom(p, from, c.now())
	}
	return nil, ErrNoAck
}

// Hold keeps a lease until ctx ends: it renews from T1, rebinds from T2,
// and when the lease expires or is refused it reports nil and starts over
// (asking for the same address). onLease gets every lease obtained or
// extended, and nil when the lease is lost.
func (c *Client) Hold(ctx context.Context, l *Lease, onLease func(*Lease)) {
	for ctx.Err() == nil {
		if l == nil {
			l = c.acquire(ctx, nil)
			if l == nil {
				return
			}
			onLease(l)
		}
		next, lost := c.keep(ctx, l)
		if ctx.Err() != nil {
			return
		}
		if lost {
			old := l.Addr.IP
			onLease(nil)
			l = c.acquire(ctx, old)
			if l == nil {
				return
			}
			onLease(l)
			continue
		}
		l = next
		onLease(l)
	}
}

// acquire runs DORA until it gets a lease or ctx ends, backing off between
// rounds.
func (c *Client) acquire(ctx context.Context, prev net.IP) *Lease {
	backoff := 4 * time.Second
	for ctx.Err() == nil {
		offer, err := c.Probe(ctx, []time.Duration{0, 2 * time.Second, 5 * time.Second}, 9*time.Second, prev)
		if err == nil {
			if l, err := c.Request(ctx, offer); err == nil {
				return l
			}
		}
		if !sleep(ctx, backoff+time.Duration(rand.Int64N(int64(time.Second)))) {
			return nil
		}
		if backoff < time.Minute {
			backoff *= 2
		}
	}
	return nil
}

// keep waits for T1, then renews until it gets an answer: unicast until
// T2, broadcast until expiry. It returns the extended lease, or lost.
func (c *Client) keep(ctx context.Context, l *Lease) (*Lease, bool) {
	if !sleep(ctx, l.Start.Add(l.T1).Sub(c.now())) {
		return nil, false
	}
	for ctx.Err() == nil {
		now := c.now()
		if !now.Before(l.Expires()) {
			return nil, true
		}
		rebind := !now.Before(l.Start.Add(l.T2))
		nl, err := c.Renew(ctx, l, rebind)
		switch {
		case err == nil:
			return nl, false
		case errors.Is(err, ErrNak):
			return nil, true
		}
		// RFC 2131 §4.4.5: retry at half the remaining time, at least 60 s.
		limit := l.Start.Add(l.T2)
		if rebind {
			limit = l.Expires()
		}
		wait := limit.Sub(c.now()) / 2
		if wait < time.Minute {
			wait = time.Minute
		}
		if until := l.Expires().Sub(c.now()); wait > until {
			wait = until
		}
		if !sleep(ctx, wait) {
			return nil, false
		}
	}
	return nil, false
}

// leaseSeconds encodes a duration for an option (tests and servers).
func leaseSeconds(d time.Duration) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, uint32(d/time.Second))
	return b
}
