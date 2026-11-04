package gossip

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// Expiry is how long a node stays in the table after its last record:
// three refreshes plus jitter.
const Expiry = 100 * time.Second

// Peer is a node known from gossip.
type Peer struct {
	Record    Record
	From      string // source IP of the last packet
	LastHeard time.Time
}

// Table holds the latest record from every node, with replay protection.
type Table struct {
	mu       sync.Mutex
	peers    map[string]*Peer // by identity MAC
	self     string
	selfBoot uint64 // this node's boot value; 0: unknown, clones not detected
	clone    *Peer  // last record from another node carrying self's MAC
	now      func() time.Time
}

// NewTable creates a table that ignores records carrying selfMAC.
func NewTable(selfMAC string) *Table {
	return &Table{peers: map[string]*Peer{}, self: strings.ToLower(selfMAC), now: time.Now}
}

// SetSelf changes this node's identity MAC (after a cloned identity was
// replaced). A peer entry with the new MAC is dropped, and so is any
// clone seen under the old one.
func (t *Table) SetSelf(mac string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.self = strings.ToLower(mac)
	delete(t.peers, t.self)
	t.clone = nil
}

// SetBoot tells the table this node's own boot value. Records with this
// node's MAC but another boot value then come from a clone (Clone).
func (t *Table) SetBoot(boot uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.selfBoot = boot
}

// Clone returns the latest record from another node using this node's
// identity MAC, if one was heard within Expiry.
func (t *Table) Clone() (Record, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.clone == nil || t.now().Sub(t.clone.LastHeard) > Expiry {
		return Record{}, false
	}
	return t.clone.Record, true
}

// Update outcomes.
const (
	Ignored = iota // own record, replay or older
	Refresh        // same state, newer sequence
	Changed        // state differs from what was known
	New            // first record from this node
	Clone          // another node uses this node's identity MAC
)

// Update records r if it is newer than what is known.
func (t *Table) Update(r *Record, from string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	mac := strings.ToLower(r.MAC)
	if mac == t.self {
		if t.selfBoot == 0 || r.Boot == t.selfBoot {
			return Ignored // our own record, looped back
		}
		first := t.clone == nil || t.clone.Record.Boot != r.Boot
		if !first && r.Seq <= t.clone.Record.Seq {
			return Ignored
		}
		t.clone = &Peer{Record: *r, From: from, LastHeard: t.now()}
		if first {
			return Clone
		}
		return Ignored
	}
	p, ok := t.peers[mac]
	if ok && p.Record.Boot == r.Boot && r.Seq <= p.Record.Seq {
		return Ignored // replay, or reordered
	}
	now := t.now()
	if !ok || now.Sub(p.LastHeard) > Expiry {
		t.peers[mac] = &Peer{Record: *r, From: from, LastHeard: now}
		return New
	}
	changed := !Same(&p.Record, r)
	p.Record, p.From, p.LastHeard = *r, from, now
	if changed {
		return Changed
	}
	return Refresh
}

// Expire drops nodes not heard from within Expiry and returns how many.
func (t *Table) Expire() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for mac, p := range t.peers {
		if t.now().Sub(p.LastHeard) > Expiry {
			delete(t.peers, mac)
			n++
		}
	}
	return n
}

// Peers returns a copy of the live entries, sorted by node ID.
func (t *Table) Peers() []Peer {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Peer, 0, len(t.peers))
	for _, p := range t.peers {
		if t.now().Sub(p.LastHeard) <= Expiry {
			out = append(out, *p)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Record.ID != out[j].Record.ID {
			return out[i].Record.ID < out[j].Record.ID
		}
		return out[i].Record.MAC < out[j].Record.MAC
	})
	return out
}

// ByOriginator finds the node that owns a batman-adv originator (mesh
// interface) MAC.
func (t *Table) ByOriginator(orig string) (Peer, bool) {
	orig = strings.ToLower(orig)
	for _, p := range t.Peers() {
		for _, o := range p.Record.Orig {
			if strings.ToLower(o) == orig {
				return p, true
			}
		}
	}
	return Peer{}, false
}

// ByMAC finds a node by identity MAC.
func (t *Table) ByMAC(mac string) (Peer, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	p, ok := t.peers[strings.ToLower(mac)]
	if !ok || t.now().Sub(p.LastHeard) > Expiry {
		return Peer{}, false
	}
	return *p, true
}
