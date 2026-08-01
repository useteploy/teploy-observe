package jobs

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// WorkerGroup runs named periodic background jobs tied to a cancellable
// lifecycle. It is the same ctx+ticker+WaitGroup pattern Scheduler already
// uses for rollups/retention (see runPeriodic), pulled out standalone so
// callers with their own, unrelated set of services — not just
// rollups/retention — can get the same shutdown-safety and drift-free
// scheduling without Scheduler growing an unrelated dependency on them.
//
// OBS-005/006: every caller that used a bare `go func() { for { ...;
// time.Sleep(iv) } }()` with context.Background() had two problems this
// fixes at once: (1) no cancellation tied to the application lifecycle, so a
// job could still be mid-run while its dependencies (DB, SMTP client) were
// closing during shutdown, and tests/restarts could leak goroutines
// indefinitely; (2) sleep-after-completion drift — a job that takes 5
// minutes on an "hourly" schedule actually runs every 65 minutes, compounding
// over time. time.Ticker fires on a fixed wall-clock cadence regardless of
// how long the previous run took, which is what "every 60s" should mean.
type WorkerGroup struct {
	logger *slog.Logger
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewWorkerGroup creates a group with no jobs running yet — call Run for each
// job, then let the caller's lifecycle hook call Stop on shutdown. There is
// no separate Start(): jobs begin as soon as Run is called, matching the
// simplest possible OnStart hook (a no-op, since Run already launched them).
func NewWorkerGroup(logger *slog.Logger) *WorkerGroup {
	ctx, cancel := context.WithCancel(context.Background())
	return &WorkerGroup{logger: logger, ctx: ctx, cancel: cancel}
}

// Run launches one named periodic job. initialDelay staggers startup (as the
// original ad hoc loops did, so several jobs don't all fire in the same
// instant right after boot); interval is the fixed cadence after that. A
// non-nil error from fn is logged with the job name — callers don't each
// need their own logging boilerplate.
func (g *WorkerGroup) Run(name string, initialDelay, interval time.Duration, fn func(context.Context) error) {
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()

		run := func() {
			if err := fn(g.ctx); err != nil {
				g.logger.Error("background job failed", "job", name, "err", err)
			}
		}

		timer := time.NewTimer(initialDelay)
		defer timer.Stop()
		select {
		case <-g.ctx.Done():
			return
		case <-timer.C:
			run()
		}

		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-g.ctx.Done():
				return
			case <-ticker.C:
				run()
			}
		}
	}()
	g.logger.Info("worker registered", "name", name, "interval", interval)
}

// Stop cancels every running job and waits for them to return.
func (g *WorkerGroup) Stop() {
	g.cancel()
	g.wg.Wait()
}
