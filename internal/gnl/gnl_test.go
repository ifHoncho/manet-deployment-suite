package gnl

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"testing"

	"github.com/mdlayher/genetlink"
	"github.com/mdlayher/netlink"
)

// fakeSock delivers queued datagrams, one per Receive, like the kernel does
// for a reply and its acknowledgement.
type fakeSock struct {
	seq     uint32
	queue   [][]netlink.Message
	onSend  func(f *fakeSock, seq uint32, flags netlink.HeaderFlags)
	closed  bool
	recvErr error
}

func (f *fakeSock) Send(m genetlink.Message, family uint16, flags netlink.HeaderFlags) (netlink.Message, error) {
	f.seq++
	if f.onSend != nil {
		f.onSend(f, f.seq, flags)
	}
	return netlink.Message{Header: netlink.Header{Sequence: f.seq, Flags: flags}}, nil
}

func (f *fakeSock) Receive() ([]genetlink.Message, []netlink.Message, error) {
	if f.recvErr != nil {
		return nil, nil, f.recvErr
	}
	if len(f.queue) == 0 {
		panic("Receive would block: nothing queued")
	}
	d := f.queue[0]
	f.queue = f.queue[1:]
	return nil, d, nil
}

func (f *fakeSock) Close() error { f.closed = true; return nil }

func reply(seq uint32, cmd uint8) netlink.Message {
	b, _ := (&genetlink.Message{Header: genetlink.Header{Command: cmd}}).MarshalBinary()
	return netlink.Message{Header: netlink.Header{Sequence: seq, Type: 0x1c}, Data: b}
}

func ack(seq uint32) netlink.Message {
	return netlink.Message{Header: netlink.Header{Sequence: seq, Type: netlink.Error}, Data: make([]byte, 20)}
}

func newConn(s *fakeSock) *Conn {
	c := &Conn{family: "test"}
	c.dial = func() (socket, genetlink.Family, error) { return s, genetlink.Family{ID: 99}, nil }
	c.c = s
	return c
}

func TestReplyAndAckInSeparateDatagrams(t *testing.T) {
	s := &fakeSock{}
	s.onSend = func(f *fakeSock, seq uint32, flags netlink.HeaderFlags) {
		if flags&netlink.Acknowledge != 0 {
			// NEW_INTERFACE: the new interface, then the ack separately.
			f.queue = append(f.queue, []netlink.Message{reply(seq, 7)}, []netlink.Message{ack(seq)})
		} else {
			f.queue = append(f.queue, []netlink.Message{reply(seq, 8)})
		}
	}
	c := newConn(s)
	msgs, err := c.Execute(7, 0, netlink.Acknowledge, nil)
	if err != nil || len(msgs) != 1 || msgs[0].Header.Command != 7 {
		t.Fatalf("new interface: %v %+v", err, msgs)
	}
	// The next request must not see the previous acknowledgement.
	msgs, err = c.Execute(8, 0, 0, nil)
	if err != nil || len(msgs) != 1 || msgs[0].Header.Command != 8 {
		t.Fatalf("following request: %v %+v", err, msgs)
	}
	if len(s.queue) != 0 || c.Resets != 0 {
		t.Errorf("queue left %d, resets %d", len(s.queue), c.Resets)
	}
}

func TestStaleRepliesSkipped(t *testing.T) {
	s := &fakeSock{}
	s.onSend = func(f *fakeSock, seq uint32, flags netlink.HeaderFlags) {
		// A leftover ack from an earlier request arrives first.
		f.queue = append(f.queue, []netlink.Message{ack(seq - 1)}, []netlink.Message{reply(seq, 5)})
	}
	c := newConn(s)
	s.seq = 10
	msgs, err := c.Execute(5, 0, 0, nil)
	if err != nil || len(msgs) != 1 || c.Stale != 1 {
		t.Fatalf("got %v %+v stale=%d", err, msgs, c.Stale)
	}
}

func TestEmptyDumpAndAckOnly(t *testing.T) {
	s := &fakeSock{}
	s.onSend = func(f *fakeSock, seq uint32, flags netlink.HeaderFlags) {
		if flags&netlink.Dump != 0 {
			f.queue = append(f.queue, []netlink.Message{}) // Done swallowed by Receive
		} else {
			f.queue = append(f.queue, []netlink.Message{ack(seq)})
		}
	}
	c := newConn(s)
	if msgs, err := c.Execute(1, 0, netlink.Dump, nil); err != nil || len(msgs) != 0 {
		t.Fatalf("empty dump: %v %+v", err, msgs)
	}
	if msgs, err := c.Execute(2, 0, netlink.Acknowledge, nil); err != nil || len(msgs) != 0 {
		t.Fatalf("ack-only: %v %+v", err, msgs)
	}
}

func TestInterruptedDumpRetried(t *testing.T) {
	s := &fakeSock{}
	n := 0
	s.onSend = func(f *fakeSock, seq uint32, flags netlink.HeaderFlags) {
		n++
		m := reply(seq, 3)
		if n == 1 {
			m.Header.Flags |= netlink.DumpInterrupted
		}
		f.queue = append(f.queue, []netlink.Message{m})
	}
	c := newConn(s)
	msgs, err := c.Execute(3, 0, netlink.Dump, nil)
	if err != nil || len(msgs) != 1 || n != 2 {
		t.Fatalf("got %v %+v after %d sends", err, msgs, n)
	}
}

func TestReceiveErrorResetsSocket(t *testing.T) {
	s := &fakeSock{recvErr: errors.New("device or resource busy")}
	fresh := &fakeSock{}
	fresh.onSend = func(f *fakeSock, seq uint32, flags netlink.HeaderFlags) {
		f.queue = append(f.queue, []netlink.Message{reply(seq, 4)})
	}
	c := newConn(s)
	c.dial = func() (socket, genetlink.Family, error) { return fresh, genetlink.Family{ID: 99}, nil }
	if _, err := c.Execute(4, 0, 0, nil); err == nil {
		t.Fatal("expected error")
	}
	if !s.closed || c.Resets != 1 {
		t.Fatalf("socket not replaced: closed=%v resets=%d", s.closed, c.Resets)
	}
	if msgs, err := c.Execute(4, 0, 0, nil); err != nil || len(msgs) != 1 {
		t.Fatalf("after reset: %v %+v", err, msgs)
	}
}

func TestKernelReply(t *testing.T) {
	reply := &netlink.OpError{Op: "receive", Err: syscall.ENODEV, Sequence: 7}
	cases := []struct {
		name string
		err  error
		seq  uint32
		want bool
	}{
		{"kernel error for this request", reply, 7, true},
		{"wrapped by a caller", fmt.Errorf("delete interface 33: %w", reply), 7, true},
		{"kernel error for an older request", reply, 8, false},
		{"socket failure", &netlink.OpError{Op: "receive", Err: os.NewSyscallError("recvmsg", syscall.EBADF)}, 7, false},
		{"not a netlink error", errors.New("boom"), 7, false},
	}
	for _, c := range cases {
		if got := kernelReply(c.err, c.seq); got != c.want {
			t.Errorf("%s: kernelReply = %v, want %v", c.name, got, c.want)
		}
	}
	if !errors.Is(reply, syscall.ENODEV) {
		t.Error("callers must still see ENODEV through errors.Is")
	}
}
