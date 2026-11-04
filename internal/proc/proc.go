// Package proc supervises helper processes (wpa_supplicant, hostapd,
// dnsmasq): it starts them, forwards their output to the log, restarts them
// with backoff when they die, and stops them cleanly on request.
package proc

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Supervisor runs one command and keeps it running.
type Supervisor struct {
	Name string
	Argv []string
	Log  *slog.Logger
	// MaxBackoff caps the restart delay. Default 30 s.
	MaxBackoff time.Duration

	mu       sync.Mutex
	cmd      *exec.Cmd
	running  bool
	stopping bool
	restarts int
	lastExit error
	started  time.Time
	cancel   context.CancelFunc
	done     chan struct{}
}

// New creates a supervisor for argv[0] with arguments argv[1:].
func New(log *slog.Logger, name string, argv ...string) *Supervisor {
	return &Supervisor{Name: name, Argv: argv, Log: log.With("proc", name), MaxBackoff: 30 * time.Second}
}

// Start launches the process and the restart loop. It returns once the
// first start attempt has been made.
func (s *Supervisor) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return nil
	}
	ctx, s.cancel = context.WithCancel(ctx)
	s.done = make(chan struct{})
	s.stopping = false
	s.running = true
	first := make(chan error, 1)
	go s.loop(ctx, first)
	err := <-first
	if err != nil {
		s.running = false
	}
	return err
}

func (s *Supervisor) loop(ctx context.Context, first chan<- error) {
	defer close(s.done)
	backoff := time.Second
	reported := false
	for {
		cmd := exec.CommandContext(ctx, s.Argv[0], s.Argv[1:]...)
		// Pdeathsig: helpers die with meshd even when it is killed outright
		// (kill -9, OOM), instead of lingering and holding ports, radios or
		// sockets the next meshd needs.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGTERM}
		cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
		cmd.WaitDelay = 5 * time.Second
		stdout, _ := cmd.StdoutPipe()
		stderr, _ := cmd.StderrPipe()
		err := cmd.Start()
		if !reported {
			reported = true
			first <- err
		}
		if err != nil {
			s.Log.Error("start failed", "err", err, "argv", strings.Join(s.Argv, " "))
			if !s.sleep(ctx, backoff) {
				return
			}
			backoff = s.next(backoff)
			continue
		}
		s.mu.Lock()
		s.cmd = cmd
		s.started = time.Now()
		s.mu.Unlock()
		s.Log.Info("started", "pid", cmd.Process.Pid, "argv", strings.Join(s.Argv, " "))

		var wg sync.WaitGroup
		wg.Add(2)
		go s.pipe(&wg, stdout, slog.LevelInfo)
		go s.pipe(&wg, stderr, levelStderr)
		wg.Wait()
		werr := cmd.Wait()

		s.mu.Lock()
		s.lastExit = werr
		s.cmd = nil
		stopping := s.stopping
		ran := time.Since(s.started)
		s.mu.Unlock()

		if stopping || ctx.Err() != nil {
			s.Log.Info("stopped", "err", werr)
			return
		}
		s.Log.Warn("exited unexpectedly", "err", werr, "ran", ran.Round(time.Millisecond), "restart_in", backoff)
		s.mu.Lock()
		s.restarts++
		s.mu.Unlock()
		if ran > time.Minute {
			backoff = time.Second
		}
		if !s.sleep(ctx, backoff) {
			return
		}
		backoff = s.next(backoff)
	}
}

func (s *Supervisor) next(d time.Duration) time.Duration {
	d *= 2
	if d > s.MaxBackoff {
		d = s.MaxBackoff
	}
	return d
}

func (s *Supervisor) sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// levelStderr marks a stream whose lines are classified one by one: helpers
// such as dnsmasq write all their routine messages to stderr, and logging
// those as warnings buries the real ones.
const levelStderr = slog.Level(-100)

// stderrLevel is WARN for lines that read like a problem, INFO otherwise.
func stderrLevel(line string) slog.Level {
	l := strings.ToLower(line)
	for _, w := range []string{"error", "fail", "cannot", "can't", "unable", "warning", "illegal", "refused", "denied", "invalid", "fatal", "no such", "not found"} {
		if strings.Contains(l, w) {
			return slog.LevelWarn
		}
	}
	return slog.LevelInfo
}

func (s *Supervisor) pipe(wg *sync.WaitGroup, r io.Reader, level slog.Level) {
	defer wg.Done()
	if r == nil {
		return
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 256*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		lv := level
		if lv == levelStderr {
			lv = stderrLevel(line)
		}
		s.Log.Log(context.Background(), lv, line)
	}
}

// Stop terminates the process (SIGTERM, then SIGKILL after timeout) and
// stops the restart loop.
func (s *Supervisor) Stop(timeout time.Duration) error {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return nil
	}
	s.stopping = true
	cmd := s.cmd
	cancel := s.cancel
	done := s.done
	s.mu.Unlock()

	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
		if cmd != nil && cmd.Process != nil {
			s.Log.Warn("did not exit in time, killing", "timeout", timeout)
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		cancel()
		<-done
	}
	cancel()
	s.mu.Lock()
	s.running = false
	s.mu.Unlock()
	return nil
}

// Signal sends sig to the running child, if any.
func (s *Supervisor) Signal(sig syscall.Signal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd == nil || s.cmd.Process == nil {
		return errors.New("not running")
	}
	return s.cmd.Process.Signal(sig)
}

// Running reports whether the supervisor is active (the process may be
// between restarts).
func (s *Supervisor) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// Alive reports whether a child process currently exists.
func (s *Supervisor) Alive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cmd != nil && s.cmd.Process != nil
}

// PID is the child's pid, or 0.
func (s *Supervisor) PID() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd == nil || s.cmd.Process == nil {
		return 0
	}
	return s.cmd.Process.Pid
}

// Restarts counts unexpected exits so far.
func (s *Supervisor) Restarts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.restarts
}

// SameArgv reports whether argv matches what this supervisor runs.
func (s *Supervisor) SameArgv(argv []string) bool {
	if len(argv) != len(s.Argv) {
		return false
	}
	for i := range argv {
		if argv[i] != s.Argv[i] {
			return false
		}
	}
	return true
}

// LookPath resolves a helper binary, searching the usual sbin directories
// too, since meshd may run with a minimal PATH under systemd.
func LookPath(name string) (string, error) {
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	for _, dir := range []string{"/usr/sbin", "/sbin", "/usr/local/sbin", "/usr/bin", "/bin", "/usr/local/bin"} {
		p := dir + "/" + name
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s not found in PATH or sbin directories", name)
}

// ErrNotRunning is returned by helpers that need a live child.
var ErrNotRunning = errors.New("process not running")
