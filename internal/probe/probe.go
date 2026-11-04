// Package probe checks a WAN by sending ICMP echo requests out of one
// interface to several targets (architecture §8.2). One dead target is not a
// dead WAN: a round succeeds when any target answers.
package probe

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// Health thresholds (architecture §8.2).
const (
	FailAfter = 3 // consecutive failed rounds before the WAN is unhealthy
	OKAfter   = 3 // consecutive good rounds before it is healthy again
	Window    = 30
)

// State is the prober's view of the WAN.
type State struct {
	Healthy     bool
	Known       bool // at least OKAfter or FailAfter rounds seen
	ConsecOK    int
	ConsecFail  int
	LastRTT     time.Duration
	LossPct     int // over the last Window rounds
	Rounds      uint64
	LastError   string
	Since       time.Time // when Healthy last changed
	TargetsUsed []string
}

// Prober runs rounds on one interface until stopped.
type Prober struct {
	Ifname   string
	Targets  []string
	Interval time.Duration
	OnChange func(State) // called (from the prober goroutine) when Healthy changes

	mu      sync.Mutex
	st      State
	history []bool
	stop    chan struct{}
	done    chan struct{}
}

// Start begins probing.
func (p *Prober) Start() error {
	if len(p.Targets) == 0 {
		return errors.New("no probe targets")
	}
	if p.Interval == 0 {
		p.Interval = time.Second
	}
	fd, err := openICMP(p.Ifname)
	if err != nil {
		return err
	}
	p.stop, p.done = make(chan struct{}), make(chan struct{})
	p.st.TargetsUsed = p.Targets
	go p.loop(fd)
	return nil
}

// Stop ends probing.
func (p *Prober) Stop() {
	if p.stop == nil {
		return
	}
	close(p.stop)
	<-p.done
	p.stop = nil
}

// State returns a copy of the current state.
func (p *Prober) State() State {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.st
}

func openICMP(ifname string) (int, error) {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.IPPROTO_ICMP)
	if err != nil {
		return -1, fmt.Errorf("raw ICMP socket: %w", err)
	}
	if err := unix.BindToDevice(fd, ifname); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("bind ICMP probe to %s: %w", ifname, err)
	}
	return fd, nil
}

func (p *Prober) loop(fd int) {
	defer close(p.done)
	defer unix.Close(fd)
	id := uint16(os.Getpid()) ^ uint16(rand.Uint32())
	var seq uint16
	t := time.NewTicker(p.Interval)
	defer t.Stop()
	for {
		seq++
		rtt, err := p.round(fd, id, seq)
		p.record(err == nil, rtt, err)
		select {
		case <-p.stop:
			return
		case <-t.C:
		}
	}
}

// round sends one echo to every target and waits, up to most of the
// interval, for any matching reply.
func (p *Prober) round(fd int, id, seq uint16) (time.Duration, error) {
	start := time.Now()
	sent := 0
	var lastErr error
	for _, tgt := range p.Targets {
		ip := net.ParseIP(tgt).To4()
		if ip == nil {
			continue
		}
		pkt := echo(id, seq)
		sa := &unix.SockaddrInet4{}
		copy(sa.Addr[:], ip)
		if err := unix.Sendto(fd, pkt, 0, sa); err != nil {
			lastErr = err
			continue
		}
		sent++
	}
	if sent == 0 {
		if lastErr == nil {
			lastErr = errors.New("no IPv4 targets")
		}
		return 0, fmt.Errorf("send: %w", lastErr)
	}
	deadline := start.Add(p.Interval * 8 / 10)
	buf := make([]byte, 1500)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return 0, errors.New("no reply from any target")
		}
		tv := unix.NsecToTimeval(left.Nanoseconds())
		_ = unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv)
		n, _, err := unix.Recvfrom(fd, buf, 0)
		if err != nil {
			if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
				continue
			}
			return 0, err
		}
		if isReply(buf[:n], id, seq) {
			return time.Since(start), nil
		}
	}
}

func (p *Prober) record(ok bool, rtt time.Duration, err error) {
	p.mu.Lock()
	st := &p.st
	st.Rounds++
	p.history = append(p.history, ok)
	if len(p.history) > Window {
		p.history = p.history[len(p.history)-Window:]
	}
	lost := 0
	for _, h := range p.history {
		if !h {
			lost++
		}
	}
	st.LossPct = lost * 100 / len(p.history)
	if ok {
		st.ConsecOK++
		st.ConsecFail = 0
		st.LastRTT = rtt
		st.LastError = ""
	} else {
		st.ConsecFail++
		st.ConsecOK = 0
		if err != nil {
			st.LastError = err.Error()
		}
	}
	changed := false
	switch {
	case !st.Healthy && st.ConsecOK >= OKAfter:
		st.Healthy, st.Known, st.Since, changed = true, true, time.Now(), true
	case st.Healthy && st.ConsecFail >= FailAfter:
		st.Healthy, st.Since, changed = false, time.Now(), true
	case !st.Known && st.ConsecFail >= FailAfter:
		st.Known, st.Since, changed = true, time.Now(), true
	}
	cur := *st
	p.mu.Unlock()
	if changed && p.OnChange != nil {
		p.OnChange(cur)
	}
}

func echo(id, seq uint16) []byte {
	b := make([]byte, 16)
	b[0] = 8 // echo request
	binary.BigEndian.PutUint16(b[4:], id)
	binary.BigEndian.PutUint16(b[6:], seq)
	copy(b[8:], "trellis!")
	binary.BigEndian.PutUint16(b[2:], checksum(b))
	return b
}

// isReply checks an IPv4 packet (raw sockets deliver the IP header) for an
// echo reply with our id and sequence.
func isReply(pkt []byte, id, seq uint16) bool {
	if len(pkt) < 20 {
		return false
	}
	ihl := int(pkt[0]&0x0f) * 4
	if len(pkt) < ihl+8 {
		return false
	}
	icmp := pkt[ihl:]
	return icmp[0] == 0 && binary.BigEndian.Uint16(icmp[4:]) == id && binary.BigEndian.Uint16(icmp[6:]) == seq
}

func checksum(b []byte) uint16 {
	var s uint32
	for i := 0; i+1 < len(b); i += 2 {
		s += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if len(b)%2 == 1 {
		s += uint32(b[len(b)-1]) << 8
	}
	for s>>16 != 0 {
		s = s&0xffff + s>>16
	}
	return ^uint16(s)
}

// Ping sends a single echo to one address through any interface and waits
// up to timeout for the reply. Used to confirm a gateway that has gone
// quiet in batman-adv before dropping it (architecture §8.3).
func Ping(ifname, target string, timeout time.Duration) bool {
	fd, err := openICMP(ifname)
	if err != nil {
		return false
	}
	p := &Prober{Targets: []string{target}, Interval: timeout * 10 / 8}
	_, err = p.round(fd, uint16(rand.Uint32()), 1)
	unix.Close(fd)
	return err == nil
}
