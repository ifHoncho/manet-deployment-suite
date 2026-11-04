package meshd

import (
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/vishvananda/netlink"

	"meridian/trellis/internal/gossip"
	"meridian/trellis/internal/reconcile"
	"meridian/trellis/internal/status"
	"meridian/trellis/internal/sysctl"
)

// Gossip timing (architecture §12.5): on change, a slow refresh, and an
// early reply when a node is heard for the first time so newcomers learn
// the mesh in seconds rather than a refresh period.
const (
	gossipRefresh      = 30 * time.Second
	gossipJitter       = 5 * time.Second
	gossipWelcomeMax   = time.Second     // random delay before answering a new node
	gossipWelcomeEvery = 3 * time.Second // at most one early reply per this period
)

// gossipReconciler owns the gossip socket on br-mesh and this node's
// published record. Received records go into d.peers.
type gossipReconciler struct {
	d *Daemon

	mu       sync.Mutex
	conn     *gossip.Conn
	key      []byte
	keyFor   string
	boot     uint64
	seq      uint64
	record   *gossip.Record // last record sent
	lastSend time.Time
	welcome  time.Time // last early reply
	stop     chan struct{}
	sendNow  chan struct{}
	stats    gossipStats
}

type gossipStats struct {
	Sent, Received, AuthFailed, Ignored uint64
	BytesSent, BytesReceived            uint64
	LastRecordBytes                     int
	LastError                           string
}

func newGossipReconciler(d *Daemon) *gossipReconciler {
	var b [8]byte
	_, _ = crand.Read(b[:])
	boot := binary.BigEndian.Uint64(b[:])
	if boot == 0 {
		boot = 1
	}
	return &gossipReconciler{d: d, boot: boot, sendNow: make(chan struct{}, 1)}
}

func (g *gossipReconciler) Name() string { return "gossip" }

func (g *gossipReconciler) Reconcile(ctx context.Context) (reconcile.Result, error) {
	d := g.d
	var res reconcile.Result
	warn := func(level, code, format string, args ...any) {
		res.Warnings = append(res.Warnings, reconcile.Warning{Level: level, Code: code, Message: fmt.Sprintf(format, args...)})
	}
	var bridgeUp bool
	var batIndex int
	var gw *gossip.GW
	d.facts.Read(func(f *Facts) {
		bridgeUp, batIndex = f.BridgePresent && f.BridgeAddress != "", f.BatIfindex
		gw = f.GossipGW
	})
	if !bridgeUp {
		g.close()
		return res, nil
	}

	// Key follows the profile (a passphrase change re-keys gossip).
	keyID := d.profile.SAEPassphrase + "\x00" + d.profile.MeshID
	if g.key == nil || g.keyFor != keyID {
		k, err := gossip.Key(d.profile.SAEPassphrase, d.profile.MeshID)
		if err != nil {
			return res, err
		}
		g.mu.Lock()
		g.key, g.keyFor = k, keyID
		g.mu.Unlock()
	}

	// Two nodes with the same node ID share an address, and Linux drops a
	// packet whose source is one of its own addresses as martian. Gossip is
	// how such a conflict is found and settled (architecture §7.3), so it
	// must get through: accept local sources on br-mesh.
	if ch, err := sysctl.Set("net.ipv4.conf."+BridgeIfname+".accept_local", "1"); err != nil {
		warn("warning", "gossip-accept-local", "cannot set accept_local on %s: %v; a node ID conflict may go unnoticed", BridgeIfname, err)
	} else if ch {
		d.log.Info("sysctl", "key", "net.ipv4.conf."+BridgeIfname+".accept_local", "value", "1")
		res.Changed = true
	}

	if g.conn == nil {
		c, err := gossip.Listen(BridgeIfname, "", 0)
		if err != nil {
			warn("error", "gossip-socket", "cannot open the gossip socket on %s: %v", BridgeIfname, err)
			res.RequeueAfter = 10 * time.Second
			return res, nil
		}
		g.mu.Lock()
		g.conn = c
		g.stop = make(chan struct{})
		g.mu.Unlock()
		go g.receive(c, g.stop)
		go g.sender(g.stop)
		d.log.Info("gossip started", "interface", BridgeIfname, "group", gossip.Group, "port", gossip.Port)
		res.Changed = true
	}

	// Node ID and identity conflicts (architecture §7.3), settled before
	// this node's record goes out.
	orig := hardIfMACs(batIndex)
	if n := d.peers.Expire(); n > 0 {
		d.log.Info("nodes left the gossip view", "count", n)
		res.Changed = true
	}
	g.resolveConflicts(orig, warn, &res)
	me := d.identity()

	// This node's record. The address follows the node ID even in the
	// pass where the ID changed and the bridge has not caught up yet.
	addr, _, _ := strings.Cut(d.facts.bridgeAddress(), "/")
	if a, err := nodeAddress(d.profile.Prefix, me.NodeID); err == nil {
		addr, _, _ = strings.Cut(a, "/")
	}
	r := &gossip.Record{
		V: 1, MAC: me.MAC, ID: me.NodeID, IDSource: me.NodeIDSource, Callsign: d.cfg.Callsign,
		Addr: addr, Version: d.opts.Version, Boot: g.boot, GW: gw, Orig: orig,
	}
	g.mu.Lock()
	changed := !gossip.Same(g.record, r)
	g.mu.Unlock()
	if changed {
		if err := g.send(r); err != nil {
			warn("warning", "gossip-send", "cannot send gossip: %v", err)
			res.RequeueAfter = 5 * time.Second
		} else {
			res.Changed = true
		}
	}

	g.mu.Lock()
	if g.stats.AuthFailed > 0 && g.stats.Received == 0 {
		warn("warning", "gossip-auth", "%d gossip packets failed authentication and none passed: another node may have a different mesh profile", g.stats.AuthFailed)
	}
	g.mu.Unlock()
	return res, nil
}

// resolveConflicts gives up this node's ID (or its cloned identity MAC)
// when the gossip view shows another node keeps it, and reports the
// conflicts this node wins or cannot settle.
func (g *gossipReconciler) resolveConflicts(orig []string, warn func(level, code, format string, args ...any), res *reconcile.Result) {
	d := g.d
	me := d.identity()
	peers := d.peers.Peers()
	recs := make([]gossip.Record, len(peers))
	for i, p := range peers {
		recs[i] = p.Record
	}
	var clone *gossip.Record
	if c, ok := d.peers.Clone(); ok {
		clone = &c
	}
	dec, err := resolveIdentity(me, orig, g.boot, recs, clone)
	switch {
	case err != nil:
		warn("error", "node-id-exhausted", "%v", err)
	case dec.Next != nil:
		if err := d.setIdentity(dec.Next); err != nil {
			warn("error", "node-id-conflict", "cannot take a new identity: %v", err)
			break
		}
		d.log.Warn("identity conflict: re-addressing", "reason", dec.Reason, "node_id", dec.Next.NodeID, "mac", dec.Next.MAC)
		res.Changed = true
		me = dec.Next
	}

	// What remains is shown, not acted on.
	if clone != nil && strings.EqualFold(clone.MAC, me.MAC) {
		warn("warning", "identity-clone", "node %q (%s) uses this node's identity MAC %s (a copied identity file); it takes a new one", clone.Callsign, clone.Addr, me.MAC)
	}
	for _, p := range recs {
		if p.ID == me.NodeID {
			warn("warning", "node-id-conflict", "node %q (%s) also uses node ID %d; this node keeps it and %q moves to another ID", p.Callsign, p.MAC, p.ID, p.Callsign)
		}
	}
	if pin := d.cfg.NodeID; pin != 0 && pin != me.NodeID {
		holder := "another node"
		for _, p := range recs {
			if p.ID == pin {
				holder = fmt.Sprintf("%q (%s)", p.Callsign, p.MAC)
			}
		}
		warn("error", "node-id-taken", "node_id %d in node.yaml is kept by %s; this node uses %d instead. Give one of them another node_id", pin, holder, me.NodeID)
	}
}

// send seals and multicasts r as the next record.
func (g *gossipReconciler) send(r *gossip.Record) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.conn == nil || g.key == nil {
		return errors.New("gossip not started")
	}
	g.seq++
	r.Seq = g.seq
	pkt, err := gossip.Seal(g.key, r)
	if err != nil {
		return err
	}
	if err := g.conn.Send(pkt); err != nil {
		g.stats.LastError = err.Error()
		return err
	}
	g.record, g.lastSend = r, time.Now()
	g.stats.Sent++
	g.stats.BytesSent += uint64(len(pkt))
	g.stats.LastRecordBytes = len(pkt)
	return nil
}

// sender refreshes the record periodically and answers new nodes early.
func (g *gossipReconciler) sender(stop chan struct{}) {
	for {
		wait := gossipRefresh + time.Duration(rand.Int64N(int64(2*gossipJitter))) - gossipJitter
		select {
		case <-stop:
			return
		case <-g.sendNow:
			time.Sleep(time.Duration(rand.Int64N(int64(gossipWelcomeMax))))
		case <-time.After(wait):
		}
		g.mu.Lock()
		r := g.record
		g.mu.Unlock()
		if r != nil {
			cp := *r
			_ = g.send(&cp)
		}
	}
}

func (g *gossipReconciler) receive(c *gossip.Conn, stop chan struct{}) {
	buf := make([]byte, gossip.MaxPacket+64)
	for {
		n, from, err := c.Receive(buf)
		if err != nil {
			select {
			case <-stop:
				return
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		g.mu.Lock()
		key := g.key
		g.mu.Unlock()
		r, err := gossip.Open(key, buf[:n])
		if err != nil {
			g.mu.Lock()
			if errors.Is(err, gossip.ErrAuth) {
				g.stats.AuthFailed++
			} else {
				g.stats.Ignored++
			}
			g.mu.Unlock()
			continue
		}
		g.mu.Lock()
		g.stats.Received++
		g.stats.BytesReceived += uint64(n)
		g.mu.Unlock()
		switch g.d.peers.Update(r, from.IP.String()) {
		case gossip.New:
			g.d.log.Info("node joined the gossip view", "callsign", r.Callsign, "node_id", r.ID, "address", r.Addr, "mac", r.MAC)
			g.mu.Lock()
			early := time.Since(g.welcome) > gossipWelcomeEvery
			if early {
				g.welcome = time.Now()
			}
			g.mu.Unlock()
			if early {
				select {
				case g.sendNow <- struct{}{}:
				default:
				}
			}
			g.d.Kick("gossip: new node")
		case gossip.Changed:
			g.d.Kick("gossip: node changed")
		case gossip.Clone:
			g.d.log.Warn("another node uses this node's identity MAC", "callsign", r.Callsign, "address", r.Addr, "from", from.IP.String())
			g.d.Kick("gossip: identity clone")
		}
	}
}

func (g *gossipReconciler) close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.conn != nil {
		close(g.stop)
		g.conn.Close()
		g.conn = nil
		g.record = nil
	}
}

func (g *gossipReconciler) Teardown(ctx context.Context) error {
	g.close()
	return nil
}

// Stats returns the counters for status.
func (g *gossipReconciler) Stats() gossipStats {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.stats
}

// hardIfMACs lists the MACs of bat0's hard interfaces: the originator
// addresses other nodes see in batman-adv.
func hardIfMACs(batIndex int) []string {
	if batIndex == 0 {
		return nil
	}
	links, err := netlink.LinkList()
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range links {
		if l.Attrs().MasterIndex == batIndex && l.Attrs().HardwareAddr != nil {
			out = append(out, l.Attrs().HardwareAddr.String())
		}
	}
	return out
}

// meshNodes renders the gossip view for status.
func (d *Daemon) meshNodes() []status.MeshNode {
	out := []status.MeshNode{}
	if d.peers == nil {
		return out
	}
	now := time.Now()
	self := d.identity()
	for _, p := range d.peers.Peers() {
		r := p.Record
		n := status.MeshNode{
			Callsign: r.Callsign, NodeID: r.ID, Address: r.Addr, MAC: r.MAC, Originators: r.Orig, Version: r.Version,
			HeardAgoMS: now.Sub(p.LastHeard).Milliseconds(), IDConflict: self != nil && r.ID == self.NodeID,
		}
		if r.GW != nil {
			n.Gateway, n.WANHealthy = true, r.GW.Healthy
		}
		out = append(out, n)
	}
	return out
}
