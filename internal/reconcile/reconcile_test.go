package reconcile

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

type fake struct {
	name    string
	calls   atomic.Int32
	changeN int32 // report Changed on the first N calls
	fail    bool
	torn    atomic.Bool
	tornAt  time.Time
	requeue time.Duration
}

func (f *fake) Name() string { return f.name }
func (f *fake) Reconcile(ctx context.Context) (Result, error) {
	n := f.calls.Add(1)
	if f.fail {
		return Result{}, errors.New("boom")
	}
	return Result{Changed: n <= f.changeN, RequeueAfter: f.requeue}, nil
}
func (f *fake) Teardown(ctx context.Context) error {
	f.torn.Store(true)
	f.tornAt = time.Now()
	return nil
}

func TestPassOrderAndConvergence(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	a := &fake{name: "a", changeN: 2}
	b := &fake{name: "b"}
	l := NewLoop(log, a, b)
	rep := l.Pass(context.Background(), "test")
	if !rep.Changed || rep.Converged {
		t.Errorf("first pass should change: %+v", rep)
	}
	l.Pass(context.Background(), "test")
	rep = l.Pass(context.Background(), "test")
	if rep.Changed || !rep.Converged {
		t.Errorf("third pass should be converged: %+v", rep)
	}
	passes, _, stats := l.Snapshot()
	if passes != 3 || stats[0].Changes != 2 || stats[1].Changes != 0 {
		t.Errorf("stats: passes=%d %+v", passes, stats)
	}
}

func TestErrorDoesNotStopOthers(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	a := &fake{name: "a", fail: true}
	b := &fake{name: "b", changeN: 1}
	l := NewLoop(log, a, b)
	rep := l.Pass(context.Background(), "test")
	if rep.Errors["a"] == nil || b.calls.Load() != 1 || !rep.Changed || rep.Converged {
		t.Errorf("report: %+v", rep)
	}
}

func TestRunKickAndTeardownOrder(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	a := &fake{name: "a"}
	b := &fake{name: "b"}
	l := NewLoop(log, a, b)
	l.Interval = time.Hour
	l.Debounce = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- l.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)
	if a.calls.Load() != 1 {
		t.Fatalf("start pass expected, calls=%d", a.calls.Load())
	}
	l.Kick("x")
	l.Kick("y")
	l.Kick("z")
	time.Sleep(100 * time.Millisecond)
	if n := a.calls.Load(); n != 2 {
		t.Errorf("kicks should coalesce into one pass, calls=%d", n)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !a.torn.Load() || !b.torn.Load() || b.tornAt.After(a.tornAt) {
		t.Error("teardown must run in reverse order")
	}
}

func TestRequeueAfter(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	a := &fake{name: "a", requeue: 20 * time.Millisecond}
	l := NewLoop(log, a)
	l.Interval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	go l.RunNoTeardown(ctx)
	time.Sleep(150 * time.Millisecond)
	cancel()
	if n := a.calls.Load(); n < 3 {
		t.Errorf("requeue should trigger extra passes, calls=%d", n)
	}
}

func TestPanicIsAnError(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	l := NewLoop(log, panicky{})
	rep := l.Pass(context.Background(), "test")
	if rep.Errors["p"] == nil {
		t.Error("panic should surface as an error")
	}
}

type panicky struct{}

func (panicky) Name() string                              { return "p" }
func (panicky) Reconcile(context.Context) (Result, error) { panic("nope") }
func (panicky) Teardown(context.Context) error            { return nil }
