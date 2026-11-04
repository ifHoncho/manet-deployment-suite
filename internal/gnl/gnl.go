// Package gnl wraps a generic netlink connection so that one failed request
// cannot break every later one.
//
// A request that fails part-way through a dump can leave unread replies on
// the socket, and the kernel keeps the dump marked as running. Every later
// request on that socket then fails with "mismatched sequence" or EBUSY.
// This wrapper discards the socket after any failure and opens a fresh one
// on the next request. It also retries dumps the kernel marks as
// interrupted (NLM_F_DUMP_INTR), which happens when the object list changes
// mid-dump, for example a USB radio re-enumerating.
package gnl

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"

	"github.com/mdlayher/genetlink"
	"github.com/mdlayher/netlink"
)

// ErrFamilyNotFound means the kernel has no such generic netlink family
// (usually: the module is not loaded).
var ErrFamilyNotFound = errors.New("generic netlink family not found")

// MaxDumpRetries bounds retries of an interrupted dump.
const MaxDumpRetries = 3

// maxReads bounds the reads for one request, so a confused socket cannot
// hang the caller.
const maxReads = 10000

var errTooManyReads = errors.New("too many netlink reads for one request; socket discarded")

// socket is the subset of *genetlink.Conn used here; tests substitute it.
type socket interface {
	Send(m genetlink.Message, family uint16, flags netlink.HeaderFlags) (netlink.Message, error)
	Receive() ([]genetlink.Message, []netlink.Message, error)
	Close() error
}

// Conn is a self-healing connection to one generic netlink family. It is
// safe for concurrent use; requests are serialised.
type Conn struct {
	family string
	dial   func() (socket, genetlink.Family, error)

	mu  sync.Mutex
	c   socket
	fam genetlink.Family

	// Resets counts sockets discarded after errors, for diagnostics.
	Resets int
	// Stale counts replies to earlier requests that were skipped.
	Stale int
	// OnReset, if set, is called (with the lock held) when a socket is
	// discarded, with the error that caused it.
	OnReset func(err error)
}

// Dial connects to a family. It fails if the family does not exist.
func Dial(family string) (*Conn, error) {
	c := &Conn{family: family}
	c.dial = func() (socket, genetlink.Family, error) {
		conn, err := genetlink.Dial(nil)
		if err != nil {
			return nil, genetlink.Family{}, fmt.Errorf("open generic netlink: %w", err)
		}
		fam, err := conn.GetFamily(family)
		if err != nil {
			_ = conn.Close()
			if errors.Is(err, os.ErrNotExist) {
				return nil, genetlink.Family{}, fmt.Errorf("%s: %w", family, ErrFamilyNotFound)
			}
			return nil, genetlink.Family{}, fmt.Errorf("look up %s family: %w", family, err)
		}
		return conn, fam, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.dialLocked(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Conn) dialLocked() error {
	conn, fam, err := c.dial()
	if err != nil {
		return err
	}
	c.c, c.fam = conn, fam
	return nil
}

func (c *Conn) resetLocked(cause error) {
	if c.c != nil {
		_ = c.c.Close()
	}
	c.c = nil
	c.Resets++
	if c.OnReset != nil {
		c.OnReset(cause)
	}
}

// Family returns the family as last resolved (ID, version, groups).
func (c *Conn) Family() genetlink.Family {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fam
}

// Close closes the socket.
func (c *Conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.c == nil {
		return nil
	}
	err := c.c.Close()
	c.c = nil
	return err
}

// Execute sends one request and returns the replies, excluding netlink
// acknowledgements. Flags should include netlink.Dump for dumps and
// netlink.Acknowledge for set operations; netlink.Request is added.
func (c *Conn) Execute(cmd, version uint8, flags netlink.HeaderFlags, data []byte) ([]genetlink.Message, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for attempt := 0; ; attempt++ {
		msgs, interrupted, err := c.executeLocked(cmd, version, flags, data)
		if err != nil {
			return nil, err
		}
		if interrupted && flags&netlink.Dump != 0 && attempt < MaxDumpRetries {
			continue
		}
		if interrupted && flags&netlink.Dump != 0 {
			return nil, fmt.Errorf("%s dump of command %d: interrupted %d times in a row (objects changing)", c.family, cmd, attempt+1)
		}
		return msgs, nil
	}
}

func (c *Conn) executeLocked(cmd, version uint8, flags netlink.HeaderFlags, data []byte) ([]genetlink.Message, bool, error) {
	if c.c == nil {
		if err := c.dialLocked(); err != nil {
			return nil, false, err
		}
	}
	req := genetlink.Message{Header: genetlink.Header{Command: cmd, Version: version}, Data: data}
	sent, err := c.c.Send(req, c.fam.ID, netlink.Request|flags)
	if err != nil {
		c.resetLocked(err)
		return nil, false, err
	}
	// Collect replies to this request. The kernel may deliver a reply and
	// its acknowledgement in separate datagrams (NEW_INTERFACE does), and
	// one Receive returns only one datagram unless it is part of a dump.
	// Keep reading until the request is complete; skip anything left over
	// from an earlier request rather than failing on it.
	wantAck := flags&netlink.Acknowledge != 0
	isDump := flags&netlink.Dump != 0
	interrupted := false
	out := []genetlink.Message{}
	for reads := 0; ; reads++ {
		if reads >= maxReads {
			c.resetLocked(errTooManyReads)
			return nil, false, errTooManyReads
		}
		_, nmsgs, err := c.c.Receive()
		if err != nil {
			// The kernel answering this request with an error (ENODEV for
			// an interface that is already gone, EBUSY, ...) is a complete
			// reply: the socket is fine. Anything else, or any error during
			// a dump, may leave data behind: never reuse the socket then.
			if !isDump && kernelReply(err, sent.Header.Sequence) {
				return nil, false, err
			}
			c.resetLocked(err)
			return nil, false, err
		}
		// Receive reads a whole dump and swallows its Done message, and an
		// empty dump (or an empty reply) yields no messages at all.
		done := len(nmsgs) == 0
		matched := false
		for _, nm := range nmsgs {
			if nm.Header.Sequence != sent.Header.Sequence {
				c.Stale++
				continue // left over from an earlier request
			}
			matched = true
			if nm.Header.Flags&netlink.DumpInterrupted != 0 {
				interrupted = true
			}
			switch nm.Header.Type {
			case netlink.Error: // acknowledgement (errors were returned by Receive)
				done = true
				continue
			case netlink.Done:
				done = true
				continue
			}
			var gm genetlink.Message
			if err := gm.UnmarshalBinary(nm.Data); err != nil {
				c.resetLocked(err)
				return nil, false, err
			}
			out = append(out, gm)
		}
		switch {
		case done:
		case !matched:
			continue // only stale messages so far
		case isDump:
			done = true // Receive returned the complete dump
		case wantAck:
			continue // reply arrived; its acknowledgement has not
		default:
			done = true
		}
		if done {
			break
		}
	}
	return out, interrupted, nil
}

// kernelReply reports whether err is the kernel's error reply to the request
// with sequence seq, as opposed to a failure of the socket itself.
// mdlayher/netlink returns a reply's error number as a bare syscall.Errno
// (socket errors are wrapped in os.SyscallError) and records its sequence.
func kernelReply(err error, seq uint32) bool {
	var oe *netlink.OpError
	if !errors.As(err, &oe) {
		return false
	}
	_, bare := oe.Err.(syscall.Errno)
	return bare && oe.Sequence == seq
}
