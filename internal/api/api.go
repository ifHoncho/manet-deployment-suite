// Package api is meshd's local HTTP/JSON API over a unix socket, and the
// client meshctl and webd use to reach it.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"time"

	"meridian/trellis/internal/config"
	"meridian/trellis/internal/status"
)

// Provider is what the server needs from meshd.
type Provider interface {
	Status(ctx context.Context) (*status.Status, error)
	Config() (config.Node, config.Profile)
	Kick(reason string)
	Reload() error
	APJoin() (*status.APJoin, error)
}

// Server serves the API.
type Server struct {
	Provider Provider
	Log      *slog.Logger
	Socket   string
	// Group, when set, owns the socket so unprivileged members can read status.
	Mode os.FileMode
}

// Handler returns the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v0/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true, "time": time.Now()})
	})
	mux.HandleFunc("GET /v0/status", func(w http.ResponseWriter, r *http.Request) {
		st, err := s.Provider.Status(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, st)
	})
	mux.HandleFunc("GET /v0/radios", func(w http.ResponseWriter, r *http.Request) {
		st, err := s.Provider.Status(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, map[string]any{"radios": st.Radios, "plan": st.Plan})
	})
	mux.HandleFunc("GET /v0/config", func(w http.ResponseWriter, r *http.Request) {
		node, profile := s.Provider.Config()
		writeJSON(w, map[string]any{"node": node, "profile": profile})
	})
	mux.HandleFunc("GET /v0/ap", func(w http.ResponseWriter, r *http.Request) {
		j, err := s.Provider.APJoin()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, j)
	})
	mux.HandleFunc("POST /v0/reconcile", func(w http.ResponseWriter, r *http.Request) {
		s.Provider.Kick("api")
		writeJSON(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /v0/reload", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Provider.Reload(); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	})
	return mux
}

// MaxSocketPath is the longest unix socket path Linux accepts (sun_path
// minus the terminator).
const MaxSocketPath = 107

// Serve listens on the unix socket until ctx is done.
func (s *Server) Serve(ctx context.Context) error {
	if len(s.Socket) > MaxSocketPath {
		return fmt.Errorf("socket path %q is %d bytes; unix sockets allow at most %d. Use a shorter --run-dir or --socket", s.Socket, len(s.Socket), MaxSocketPath)
	}
	if err := os.MkdirAll(filepath.Dir(s.Socket), 0o755); err != nil {
		return err
	}
	// A stale socket from an unclean exit must not block startup.
	if _, err := os.Stat(s.Socket); err == nil {
		if c, err := net.DialTimeout("unix", s.Socket, 500*time.Millisecond); err == nil {
			c.Close()
			return fmt.Errorf("another meshd is already listening on %s", s.Socket)
		}
		_ = os.Remove(s.Socket)
	}
	ln, err := net.Listen("unix", s.Socket)
	if err != nil {
		return err
	}
	mode := s.Mode
	if mode == 0 {
		mode = 0o660
	}
	_ = os.Chmod(s.Socket, mode)
	// Members of the "trellis" group may run meshctl without root.
	if g, err := user.LookupGroup("trellis"); err == nil {
		if gid, err := strconv.Atoi(g.Gid); err == nil {
			_ = os.Chown(s.Socket, -1, gid)
		}
	}
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	if s.Log != nil {
		s.Log.Info("api listening", "socket", s.Socket)
	}
	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
		_ = os.Remove(s.Socket)
		return ctx.Err()
	case err := <-errCh:
		_ = os.Remove(s.Socket)
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

// Client talks to meshd.
type Client struct {
	Socket string
	http   *http.Client
}

// NewClient creates a client for the socket.
func NewClient(socket string) *Client {
	if socket == "" {
		socket = filepath.Join(config.DefaultRunDir, "meshd.sock")
	}
	return &Client{
		Socket: socket,
		http: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socket)
			}},
		},
	}
}

// ErrNotRunning means the socket is absent or refuses connections.
var ErrNotRunning = errors.New("meshd is not running")

// ErrPermission means the socket exists but this user may not open it.
var ErrPermission = errors.New("permission denied")

func (c *Client) do(ctx context.Context, method, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, "http://meshd"+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return fmt.Errorf("%w: cannot open %s; run as root or add your user to the trellis group", ErrPermission, c.Socket)
		}
		var opErr *net.OpError
		if errors.As(err, &opErr) || errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w (socket %s: %v)", ErrNotRunning, c.Socket, err)
		}
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		var e struct{ Error string }
		if json.Unmarshal(body, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("%s %s: HTTP %d", method, path, resp.StatusCode)
	}
	if out != nil {
		return json.Unmarshal(body, out)
	}
	return nil
}

// Status fetches the node status.
func (c *Client) Status(ctx context.Context) (*status.Status, error) {
	var st status.Status
	if err := c.do(ctx, http.MethodGet, "/v0/status", &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// Health pings meshd.
func (c *Client) Health(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/v0/health", nil)
}

// Reconcile asks for a pass.
func (c *Client) Reconcile(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/v0/reconcile", nil)
}

// Reload asks meshd to re-read its configuration.
func (c *Client) Reload(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/v0/reload", nil)
}

// ConfigResult is the redacted configuration.
type ConfigResult struct {
	Node    config.Node    `json:"node"`
	Profile config.Profile `json:"profile"`
}

// Config fetches the redacted configuration.
// APJoin fetches the client-AP credentials.
func (c *Client) APJoin(ctx context.Context) (*status.APJoin, error) {
	var out status.APJoin
	if err := c.do(ctx, http.MethodGet, "/v0/ap", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Config(ctx context.Context) (*ConfigResult, error) {
	var out ConfigResult
	if err := c.do(ctx, http.MethodGet, "/v0/config", &out); err != nil {
		return nil, err
	}
	return &out, nil
}
