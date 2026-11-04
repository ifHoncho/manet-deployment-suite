// Package reconcile is the control loop: a set of reconcilers, each of which
// converges one slice of the system to the desired state, run in order on
// start, on events and on a slow timer (architecture §12.3).
//
// Every reconciler must be idempotent and must only report Changed when it
// actually altered something, so a converged system makes passes with no
// changes and the loop can say so.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Warning is a condition worth showing the operator.
type Warning struct {
	Level   string // info, warning, error
	Code    string
	Message string
}

// Result is what one reconciler reports from one pass.
type Result struct {
	Changed      bool
	RequeueAfter time.Duration // ask for another pass after this long
	Warnings     []Warning
}

// Reconciler converges one part of the system.
type Reconciler interface {
	Name() string
	Reconcile(ctx context.Context) (Result, error)
	// Teardown removes everything the reconciler created. It is called in
	// reverse registration order on shutdown.
	Teardown(ctx context.Context) error
}

// Stats is one reconciler's running history.
type Stats struct {
	Name        string
	LastError   error
	LastChanged time.Time
	Changes     uint64
	Errors      uint64
	Warnings    []Warning
}

// Report describes one complete pass.
type Report struct {
	Started   time.Time
	Duration  time.Duration
	Reason    string
	Changed   bool
	Converged bool // no changes and no errors
	Results   map[string]Result
	Errors    map[string]error
}

// Loop drives the reconcilers.
type Loop struct {
	Log      *slog.Logger
	Interval time.Duration // slow periodic pass; default 15 s
	Settle   time.Duration // re-run delay after a pass that changed something; default 500 ms
	Debounce time.Duration // coalesce kicks arriving within this window; default 250 ms
	OnPass   func(Report)  // called after each pass

	reconcilers []Reconciler
	kick        chan string

	mu       sync.Mutex
	stats    map[string]*Stats
	passes   uint64
	last     Report
	inPass   bool
	stopping bool
}

// NewLoop creates a loop over the reconcilers, which run in the order given.
func NewLoop(log *slog.Logger, rs ...Reconciler) *Loop {
	l := &Loop{
		Log:         log,
		Interval:    15 * time.Second,
		Settle:      500 * time.Millisecond,
		Debounce:    250 * time.Millisecond,
		reconcilers: rs,
		kick:        make(chan string, 16),
		stats:       map[string]*Stats{},
	}
	for _, r := range rs {
		l.stats[r.Name()] = &Stats{Name: r.Name()}
	}
	return l
}

// Kick requests a pass soon. Kicks are coalesced.
func (l *Loop) Kick(reason string) {
	select {
	case l.kick <- reason:
	default:
	}
}

// Pass runs every reconciler once, in order, and returns the report. A
// failing reconciler does not stop the ones after it: they may be
// independent, and their errors are recorded too.
func (l *Loop) Pass(ctx context.Context, reason string) Report {
	l.mu.Lock()
	l.inPass = true
	l.mu.Unlock()
	rep := Report{Started: time.Now(), Reason: reason, Results: map[string]Result{}, Errors: map[string]error{}}
	for _, r := range l.reconcilers {
		if ctx.Err() != nil {
			break
		}
		res, err := l.runOne(ctx, r)
		rep.Results[r.Name()] = res
		if err != nil {
			rep.Errors[r.Name()] = err
		}
		if res.Changed {
			rep.Changed = true
		}
	}
	rep.Duration = time.Since(rep.Started)
	rep.Converged = !rep.Changed && len(rep.Errors) == 0
	l.mu.Lock()
	l.passes++
	l.last = rep
	l.inPass = false
	l.mu.Unlock()
	if l.OnPass != nil {
		l.OnPass(rep)
	}
	return rep
}

func (l *Loop) runOne(ctx context.Context, r Reconciler) (res Result, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
			l.Log.Error("reconciler panicked", "reconciler", r.Name(), "panic", p)
		}
		l.mu.Lock()
		st := l.stats[r.Name()]
		st.LastError = err
		st.Warnings = res.Warnings
		if err != nil {
			st.Errors++
		}
		if res.Changed {
			st.Changes++
			st.LastChanged = time.Now()
		}
		l.mu.Unlock()
	}()
	start := time.Now()
	res, err = r.Reconcile(ctx)
	if err != nil {
		l.Log.Warn("reconcile failed", "reconciler", r.Name(), "err", err, "took", time.Since(start).Round(time.Millisecond))
	} else if res.Changed {
		l.Log.Info("reconciled", "reconciler", r.Name(), "changed", true, "took", time.Since(start).Round(time.Millisecond))
	}
	return res, err
}

// Run passes until ctx is done, then tears down in reverse order.
func (l *Loop) Run(ctx context.Context) error {
	err := l.RunNoTeardown(ctx)
	tctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if terr := l.Teardown(tctx); terr != nil {
		return errors.Join(err, terr)
	}
	return err
}

// RunNoTeardown passes until ctx is done and returns without tearing down.
func (l *Loop) RunNoTeardown(ctx context.Context) error {
	rep := l.Pass(ctx, "start")
	timer := time.NewTimer(l.nextDelay(rep))
	defer timer.Stop()
	for {
		var reason string
		select {
		case <-ctx.Done():
			return nil
		case reason = <-l.kick:
			// Coalesce a burst of events into one pass.
			d := time.NewTimer(l.Debounce)
		drain:
			for {
				select {
				case <-l.kick:
				case <-d.C:
					break drain
				case <-ctx.Done():
					d.Stop()
					return nil
				}
			}
		case <-timer.C:
			reason = "timer"
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		rep = l.Pass(ctx, reason)
		timer.Reset(l.nextDelay(rep))
	}
}

func (l *Loop) nextDelay(rep Report) time.Duration {
	d := l.Interval
	if rep.Changed || len(rep.Errors) > 0 {
		d = min(d, l.Settle)
		if len(rep.Errors) > 0 && !rep.Changed {
			d = min(l.Interval, 3*time.Second) // retry errors without spinning
		}
	}
	for _, r := range rep.Results {
		if r.RequeueAfter > 0 && r.RequeueAfter < d {
			d = r.RequeueAfter
		}
	}
	return d
}

// Teardown calls Teardown on every reconciler in reverse order and returns
// the joined errors.
func (l *Loop) Teardown(ctx context.Context) error {
	var errs []error
	for i := len(l.reconcilers) - 1; i >= 0; i-- {
		r := l.reconcilers[i]
		if err := r.Teardown(ctx); err != nil {
			l.Log.Error("teardown failed", "reconciler", r.Name(), "err", err)
			errs = append(errs, fmt.Errorf("%s: %w", r.Name(), err))
		} else {
			l.Log.Info("torn down", "reconciler", r.Name())
		}
	}
	return errors.Join(errs...)
}

// Snapshot returns the pass count, the last report and per-reconciler stats
// in registration order.
func (l *Loop) Snapshot() (passes uint64, last Report, stats []Stats) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.reconcilers {
		stats = append(stats, *l.stats[r.Name()])
	}
	return l.passes, l.last, stats
}
