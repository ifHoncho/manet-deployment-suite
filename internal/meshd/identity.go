package meshd

import (
	"fmt"
	"strings"

	"meridian/trellis/internal/gossip"
	"meridian/trellis/internal/nodestate"
)

// Node identity while running (architecture §7.3). The identity is read by
// the reconcile loop, the gateway monitor and the API, so it is swapped
// whole under a lock and never modified in place: a caller may keep the
// pointer it got for the rest of a pass.

// identity returns the current identity.
func (d *Daemon) identity() *nodestate.Identity {
	d.identMu.RLock()
	defer d.identMu.RUnlock()
	return d.ident
}

// identityGen counts live changes of the node ID or identity MAC. Reconcilers
// that must disturb clients after a re-address (the AP, client ports)
// compare it with the value they last saw.
func (d *Daemon) identityGen() uint64 {
	d.identMu.RLock()
	defer d.identMu.RUnlock()
	return d.identGen
}

// setIdentity persists and installs a new identity. The reconcilers apply
// it from the next pass on: bridge address, DHCP pool, gateway marks,
// gossip record, and bat0/br-mesh MAC if that changed.
func (d *Daemon) setIdentity(next *nodestate.Identity) error {
	if err := nodestate.SaveIdentity(d.opts.StateDir, next); err != nil {
		return fmt.Errorf("save identity: %w", err)
	}
	d.identMu.Lock()
	prev := d.ident
	d.ident = next
	d.identGen++
	d.identMu.Unlock()
	if d.peers != nil && (prev == nil || !strings.EqualFold(prev.MAC, next.MAC)) {
		d.peers.SetSelf(next.MAC)
	}
	return nil
}

// reapplyNodeID follows a node_id change in a reloaded node.yaml: pinned,
// unpinned (back to the derived ID) or pinned to another value.
func (d *Daemon) reapplyNodeID() {
	cur := d.identity()
	next, _, err := nodestate.EnsureIdentity(d.opts.StateDir, d.cfg.NodeID)
	if err != nil {
		d.log.Error("cannot apply the new node_id", "err", err)
		return
	}
	if next.NodeID == cur.NodeID && next.NodeIDSource == cur.NodeIDSource {
		return
	}
	if err := d.setIdentity(next); err != nil {
		d.log.Error("cannot apply the new node_id", "err", err)
		return
	}
	d.log.Info("node ID changed by configuration; re-addressing", "from", cur.NodeID, "to", next.NodeID, "source", next.NodeIDSource)
}

// idDecision is the outcome of checking this node's identity against the
// gossip view.
type idDecision struct {
	Next   *nodestate.Identity // non-nil: switch to this identity
	Reason string              // why, for the log
	Holder *gossip.Record      // the node that keeps the contested ID or MAC
}

// resolveIdentity applies the conflict rules of architecture §7.3 to this
// node. clone is a record carrying this node's identity MAC from another
// meshd (a cloned SD card), or nil. It never changes a node that keeps
// what it holds; on every other node the same rules, evaluated over the
// same gossip, pick the same single keeper.
func resolveIdentity(me *nodestate.Identity, myOrig []string, myBoot uint64, peers []gossip.Record, clone *gossip.Record) (idDecision, error) {
	if clone != nil && !nodestate.CloneKeeps(myOrig, clone.Orig, myBoot, clone.Boot) {
		mac, err := nodestate.RandomMAC()
		if err != nil {
			return idDecision{}, err
		}
		next := *me
		next.MAC = mac.String()
		if next.NodeIDSource != nodestate.IDConfig {
			// A pinned ID stays pinned; if the clone pins the same one,
			// the ID rule below settles it on the next pass.
			next.NodeID, next.NodeIDSource = nodestate.DeriveNodeID(mac), nodestate.IDDerived
		}
		return idDecision{Next: &next, Holder: clone,
			Reason: fmt.Sprintf("another node (%s) uses this node's identity MAC %s (a cloned identity file); it keeps it, this node takes a new one", clone.Callsign, me.MAC)}, nil
	}
	used := map[int]bool{}
	var holder *gossip.Record
	for i := range peers {
		p := &peers[i]
		if p.ID < 1 || p.ID > 254 {
			continue
		}
		used[p.ID] = true
		if p.ID == me.NodeID && !nodestate.KeepsID(me.NodeIDSource, me.MAC, p.IDSource, p.MAC) {
			holder = p
		}
	}
	if holder == nil {
		return idDecision{}, nil
	}
	id, ok := nodestate.ClaimNodeID(me.HardwareAddr(), used, me.NodeID)
	if !ok {
		return idDecision{}, fmt.Errorf("node ID %d is held by %s and no free node ID is left", me.NodeID, holder.Callsign)
	}
	next := *me
	next.NodeID, next.NodeIDSource = id, nodestate.IDClaimed
	return idDecision{Next: &next, Holder: holder,
		Reason: fmt.Sprintf("node %s (%s) also uses node ID %d and keeps it (%s); this node moves to %d", holder.Callsign, holder.MAC, me.NodeID, keepRule(me, holder), id)}, nil
}

// keepRule names the rule by which holder keeps the ID over me.
func keepRule(me *nodestate.Identity, holder *gossip.Record) string {
	if holder.IDSource == nodestate.IDConfig && me.NodeIDSource != nodestate.IDConfig {
		return "its ID is pinned in its configuration"
	}
	return "lower MAC"
}
