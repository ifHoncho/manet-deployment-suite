package wpa

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

var ctrlSeq atomic.Uint64

// Ctrl is a connection to a wpa_supplicant control socket for requests.
type Ctrl struct {
	conn  *net.UnixConn
	local string
}

// DialCtrl connects to the control socket of an interface, e.g.
// /run/trellis/wpa/mesh0. The local endpoint is created next to it.
func DialCtrl(sockPath string) (*Ctrl, error) {
	local := filepath.Join(filepath.Dir(sockPath), fmt.Sprintf(".trellis-%d-%d", os.Getpid(), ctrlSeq.Add(1)))
	if len(local) > 107 || len(sockPath) > 107 {
		return nil, fmt.Errorf("control socket path %q is too long for a unix socket (max 107 bytes); use a shorter run directory", sockPath)
	}
	_ = os.Remove(local)
	laddr := &net.UnixAddr{Name: local, Net: "unixgram"}
	raddr := &net.UnixAddr{Name: sockPath, Net: "unixgram"}
	conn, err := net.DialUnix("unixgram", laddr, raddr)
	if err != nil {
		_ = os.Remove(local)
		return nil, err
	}
	return &Ctrl{conn: conn, local: local}, nil
}

// Close closes the connection and removes the local socket file.
func (c *Ctrl) Close() error {
	err := c.conn.Close()
	_ = os.Remove(c.local)
	return err
}

// Request sends a command and returns the reply. Unsolicited events are
// skipped. The context's deadline bounds the wait.
func (c *Ctrl) Request(ctx context.Context, cmd string) (string, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(3 * time.Second)
	}
	if err := c.conn.SetDeadline(deadline); err != nil {
		return "", err
	}
	if _, err := c.conn.Write([]byte(cmd)); err != nil {
		return "", fmt.Errorf("wpa ctrl %q: %w", cmd, err)
	}
	buf := make([]byte, 64*1024)
	for {
		n, err := c.conn.Read(buf)
		if err != nil {
			return "", fmt.Errorf("wpa ctrl %q: %w", cmd, err)
		}
		reply := string(buf[:n])
		if strings.HasPrefix(reply, "<") { // unsolicited event: <N>...
			continue
		}
		return strings.TrimRight(reply, "\n"), nil
	}
}

// Ping checks the daemon answers.
func (c *Ctrl) Ping(ctx context.Context) error {
	r, err := c.Request(ctx, "PING")
	if err != nil {
		return err
	}
	if !strings.HasPrefix(r, "PONG") {
		return fmt.Errorf("unexpected PING reply %q", r)
	}
	return nil
}

// Status is the key=value output of STATUS.
type Status map[string]string

// State is wpa_state (COMPLETED when the mesh is joined).
func (s Status) State() string { return s["wpa_state"] }

// Joined reports whether the mesh is up.
func (s Status) Joined() bool { return s["wpa_state"] == "COMPLETED" }

// Freq is the operating frequency in MHz, or 0.
func (s Status) Freq() int {
	f, _ := strconv.Atoi(s["freq"])
	return f
}

// GetStatus runs STATUS.
func (c *Ctrl) GetStatus(ctx context.Context) (Status, error) {
	r, err := c.Request(ctx, "STATUS")
	if err != nil {
		return nil, err
	}
	st := Status{}
	for _, line := range strings.Split(r, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			st[k] = v
		}
	}
	return st, nil
}

// Events -------------------------------------------------------------------

// Event is an unsolicited control-interface message.
type Event struct {
	Level int               // syslog-style priority from the <N> prefix
	Name  string            // e.g. MESH-PEER-CONNECTED
	Args  []string          // positional words after the name
	KV    map[string]string // key=value words after the name
	Raw   string
}

// Arg returns the first positional argument, or "".
func (e Event) Arg() string {
	if len(e.Args) == 0 {
		return ""
	}
	return e.Args[0]
}

// ParseEvent parses "<3>MESH-PEER-CONNECTED aa:bb:cc:dd:ee:ff".
func ParseEvent(raw string) (Event, bool) {
	e := Event{Raw: raw, KV: map[string]string{}}
	s := raw
	if strings.HasPrefix(s, "<") {
		end := strings.IndexByte(s, '>')
		if end < 0 {
			return e, false
		}
		e.Level, _ = strconv.Atoi(s[1:end])
		s = s[end+1:]
	}
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return e, false
	}
	e.Name = fields[0]
	for _, f := range fields[1:] {
		if k, v, ok := strings.Cut(f, "="); ok {
			e.KV[k] = strings.Trim(v, "\"")
		} else {
			e.Args = append(e.Args, f)
		}
	}
	return e, true
}

// Mesh event names.
const (
	EvMeshGroupStarted     = "MESH-GROUP-STARTED"
	EvMeshGroupRemoved     = "MESH-GROUP-REMOVED"
	EvMeshPeerConnected    = "MESH-PEER-CONNECTED"
	EvMeshPeerDisconnected = "MESH-PEER-DISCONNECTED"
	EvMeshSAEAuthFailure   = "MESH-SAE-AUTH-FAILURE"
	EvMeshSAEAuthBlocked   = "MESH-SAE-AUTH-BLOCKED"
	EvTerminating          = "CTRL-EVENT-TERMINATING"
)

// EventConn is an attached control connection that receives events.
type EventConn struct {
	ctrl *Ctrl
}

// DialEvents attaches to the control socket for events.
func DialEvents(sockPath string) (*EventConn, error) {
	c, err := DialCtrl(sockPath)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r, err := c.Request(ctx, "ATTACH")
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	if !strings.HasPrefix(r, "OK") {
		_ = c.Close()
		return nil, fmt.Errorf("ATTACH: %q", r)
	}
	return &EventConn{ctrl: c}, nil
}

// Next waits for the next event. It returns an error on timeout or close.
func (e *EventConn) Next(timeout time.Duration) (Event, error) {
	if err := e.ctrl.conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return Event{}, err
	}
	buf := make([]byte, 4096)
	n, err := e.ctrl.conn.Read(buf)
	if err != nil {
		return Event{}, err
	}
	ev, ok := ParseEvent(strings.TrimSpace(string(buf[:n])))
	if !ok {
		return Event{}, errors.New("unparseable event")
	}
	return ev, nil
}

// Close detaches and closes.
func (e *EventConn) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _ = e.ctrl.Request(ctx, "DETACH")
	return e.ctrl.Close()
}

// IsTimeout reports whether err is a read deadline expiry.
func IsTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
