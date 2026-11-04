package dhcpc

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

type sent struct {
	mac      net.HardwareAddr
	src, dst net.IP
	p        *Packet
}

// fakeConn delivers what serve answers to each sent packet.
type fakeConn struct {
	mu    sync.Mutex
	sent  []sent
	in    chan frame
	serve func(*Packet) []*Packet
	from  net.HardwareAddr
}

type frame struct {
	b    []byte
	from net.HardwareAddr
}

func newFake(serve func(*Packet) []*Packet) *fakeConn {
	return &fakeConn{in: make(chan frame, 16), serve: serve, from: net.HardwareAddr{0, 0x11, 0x22, 0x33, 0x44, 0x55}}
}

func (f *fakeConn) Send(mac net.HardwareAddr, src, dst net.IP, payload []byte) error {
	p, err := Parse(payload)
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.sent = append(f.sent, sent{mac, src, dst, p})
	f.mu.Unlock()
	if f.serve != nil {
		for _, r := range f.serve(p) {
			f.in <- frame{r.Marshal(), f.from}
		}
	}
	return nil
}

func (f *fakeConn) Recv(deadline time.Time) ([]byte, net.HardwareAddr, error) {
	t := time.NewTimer(time.Until(deadline))
	defer t.Stop()
	select {
	case fr := <-f.in:
		return fr.b, fr.from, nil
	case <-t.C:
		return nil, nil, ErrTimeout
	}
}

func (f *fakeConn) Close() error { return nil }

var (
	clientMAC = net.HardwareAddr{0x02, 0, 0, 0, 0, 0x07}
	serverIP  = net.IPv4(192, 168, 50, 1).To4()
)

// server answers like a home router: offer .23, ACK requests for it.
func server(nak bool) func(*Packet) []*Packet {
	return func(p *Packet) []*Packet {
		r := &Packet{Op: opReply, XID: p.XID, CHAddr: p.CHAddr, YIAddr: net.IPv4(192, 168, 50, 23).To4(), Options: map[byte][]byte{
			optServerID: serverIP, optSubnetMask: {255, 255, 255, 0}, optRouter: serverIP,
			optDNS: {192, 168, 50, 1, 1, 1, 1, 1}, optLeaseTime: leaseSeconds(time.Hour),
		}}
		switch p.MsgType() {
		case Discover:
			r.Options[optMsgType] = []byte{Offer}
		case Request:
			r.Options[optMsgType] = []byte{Ack}
			if nak {
				r.Options = map[byte][]byte{optMsgType: {Nak}, optServerID: serverIP}
			}
		default:
			return nil
		}
		return []*Packet{r}
	}
}

func TestPacketRoundTrip(t *testing.T) {
	c := &Client{MAC: clientMAC, Hostname: "node-3", VendorClass: "trellis"}
	p := c.base(Discover, 0xdeadbeef)
	p.Options[optRequestedIP] = []byte{10, 0, 0, 9}
	b := p.Marshal()
	if len(b) < 300 {
		t.Fatalf("message is %d bytes, under the BOOTP minimum", len(b))
	}
	q, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if q.XID != 0xdeadbeef || q.MsgType() != Discover || q.VendorClass() != "trellis" || string(q.Options[optHostname]) != "node-3" ||
		q.CHAddr.String() != clientMAC.String() || net.IP(q.Options[optRequestedIP]).String() != "10.0.0.9" {
		t.Fatalf("round trip: %+v", q)
	}
	if _, err := Parse(b[:200]); err == nil {
		t.Error("short message accepted")
	}
}

func TestUDPFraming(t *testing.T) {
	pkt := udp4(net.IPv4zero, net.IPv4bcast, 68, 67, []byte("hello"))
	if checksum(pkt[:20]) != 0 {
		t.Error("IPv4 header checksum does not verify")
	}
	got, ok := udpPayload(pkt)
	if !ok || string(got) != "hello" {
		t.Fatalf("payload %q %v", got, ok)
	}
	if _, ok := udpPayload(pkt[:25]); ok {
		t.Error("truncated packet accepted")
	}
}

func TestProbeAndRequest(t *testing.T) {
	f := newFake(server(false))
	c := &Client{Conn: f, MAC: clientMAC, VendorClass: "trellis"}
	offer, err := c.Probe(context.Background(), []time.Duration{0}, time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	if offer.YIAddr.String() != "192.168.50.23" {
		t.Fatalf("offer %v", offer.YIAddr)
	}
	l, err := c.Request(context.Background(), offer)
	if err != nil {
		t.Fatal(err)
	}
	if l.Addr.String() != "192.168.50.23/24" || l.Router.String() != "192.168.50.1" || len(l.DNS) != 2 || l.DNS[1].String() != "1.1.1.1" ||
		l.Duration != time.Hour || l.T1 != 30*time.Minute || l.T2 != 52*time.Minute+30*time.Second || l.Subnet().String() != "192.168.50.0/24" {
		t.Fatalf("lease %+v", l)
	}
	if l.ServerMAC.String() != f.from.String() {
		t.Errorf("server MAC %v", l.ServerMAC)
	}
	req := f.sent[len(f.sent)-1]
	if req.p.MsgType() != Request || net.IP(req.p.Options[optServerID]).String() != "192.168.50.1" || req.mac.String() != Broadcast.String() {
		t.Errorf("selecting REQUEST %+v", req)
	}
	// Renewing is unicast to the server, from the leased address.
	if _, err := c.Renew(context.Background(), l, false); err != nil {
		t.Fatal(err)
	}
	ren := f.sent[len(f.sent)-1]
	if ren.mac.String() != f.from.String() || ren.dst.String() != "192.168.50.1" || ren.src.String() != "192.168.50.23" || ren.p.CIAddr.String() != "192.168.50.23" {
		t.Errorf("renewal %+v", ren)
	}
	if _, err := c.Renew(context.Background(), l, true); err != nil {
		t.Fatal(err)
	}
	if reb := f.sent[len(f.sent)-1]; reb.mac.String() != Broadcast.String() || !reb.dst.Equal(net.IPv4bcast) {
		t.Errorf("rebind not broadcast: %+v", reb)
	}
}

func TestProbeNoServer(t *testing.T) {
	f := newFake(nil)
	c := &Client{Conn: f, MAC: clientMAC}
	start := time.Now()
	_, err := c.Probe(context.Background(), []time.Duration{0, 100 * time.Millisecond, 200 * time.Millisecond}, 400*time.Millisecond, nil)
	if !errors.Is(err, ErrNoOffer) {
		t.Fatalf("err = %v", err)
	}
	if el := time.Since(start); el < 400*time.Millisecond || el > 2*time.Second {
		t.Errorf("probe took %v", el)
	}
	if len(f.sent) != 3 {
		t.Errorf("%d DISCOVERs sent, want 3", len(f.sent))
	}
}

func TestProbePeer(t *testing.T) {
	f := newFake(nil)
	other := &Client{MAC: net.HardwareAddr{0x02, 0, 0, 0, 0, 0x09}, VendorClass: "trellis"}
	f.in <- frame{other.base(Discover, 1).Marshal(), other.MAC}
	c := &Client{Conn: f, MAC: clientMAC, VendorClass: "trellis"}
	if _, err := c.Probe(context.Background(), []time.Duration{0}, time.Second, nil); !errors.Is(err, ErrPeer) {
		t.Fatalf("err = %v, want ErrPeer", err)
	}
	// An ordinary client's DISCOVER (a laptop) is not a peer.
	f = newFake(nil)
	laptop := &Client{MAC: net.HardwareAddr{0x02, 0, 0, 0, 0, 0x0a}, VendorClass: "MSFT 5.0"}
	f.in <- frame{laptop.base(Discover, 1).Marshal(), laptop.MAC}
	c.Conn = f
	if _, err := c.Probe(context.Background(), []time.Duration{0}, 300*time.Millisecond, nil); !errors.Is(err, ErrNoOffer) {
		t.Fatalf("err = %v, want ErrNoOffer", err)
	}
}

func TestRequestNak(t *testing.T) {
	f := newFake(server(true))
	c := &Client{Conn: f, MAC: clientMAC}
	offer, err := c.Probe(context.Background(), []time.Duration{0}, time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Request(context.Background(), offer); !errors.Is(err, ErrNak) {
		t.Fatalf("err = %v, want ErrNak", err)
	}
}
