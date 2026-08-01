package jobs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestWorkerGroup_RunsOnFixedTicker is the regression for OBS-006: the
// previous ad hoc loops slept AFTER each run completed, so a slow run
// pushed every subsequent run later (drift). A ticker fires on a fixed
// cadence regardless of how long the job itself takes, so a job that's
// slower than its interval doesn't compound delay into future runs.
func TestWorkerGroup_RunsOnFixedTicker(t *testing.T) {
	g := NewWorkerGroup(testLogger())
	var runs int32
	g.Run("slow-job", time.Millisecond, 20*time.Millisecond, func(ctx context.Context) error {
		atomic.AddInt32(&runs, 1)
		time.Sleep(5 * time.Millisecond) // "slow" relative to the interval
		return nil
	})
	time.Sleep(110 * time.Millisecond)
	g.Stop()

	got := atomic.LoadInt32(&runs)
	// ~1ms initial delay + ticks at 20ms: runs at roughly 1,21,41,61,81,101ms
	// -> about 5-6 runs in 110ms. A generous [3,8] bound avoids flaking on a
	// loaded CI box while still failing if drift silently reappeared (drift
	// would produce far fewer runs, since each 5ms job would push the next
	// start later on top of the interval).
	if got < 3 || got > 8 {
		t.Errorf("expected roughly 5-6 runs in 110ms at a 20ms interval, got %d", got)
	}
}

// TestWorkerGroup_StopCancelsPromptly is the regression for OBS-005: Stop
// must actually cancel in-flight/future runs and return once they've
// stopped, not leak a goroutine that keeps firing after shutdown.
func TestWorkerGroup_StopCancelsPromptly(t *testing.T) {
	g := NewWorkerGroup(testLogger())
	var runs int32
	g.Run("job", time.Millisecond, 5*time.Millisecond, func(ctx context.Context) error {
		atomic.AddInt32(&runs, 1)
		return nil
	})
	time.Sleep(20 * time.Millisecond)
	g.Stop()
	afterStop := atomic.LoadInt32(&runs)

	time.Sleep(50 * time.Millisecond) // long enough for several more ticks if leaked
	final := atomic.LoadInt32(&runs)

	if final != afterStop {
		t.Errorf("job kept running after Stop(): %d runs at stop, %d runs 50ms later", afterStop, final)
	}
}

// TestWorkerGroup_JobPassedCancelledContextOnShutdown confirms a job
// currently sleeping/working sees its context cancelled rather than being
// killed mid-operation with no signal — important for jobs that hold a DB
// connection or other resource that should be released cleanly.
func TestWorkerGroup_JobPassedCancelledContextOnShutdown(t *testing.T) {
	g := NewWorkerGroup(testLogger())
	started := make(chan struct{})
	sawCancel := make(chan bool, 1)
	g.Run("job", time.Millisecond, time.Hour, func(ctx context.Context) error {
		close(started)
		select {
		case <-ctx.Done():
			sawCancel <- true
		case <-time.After(2 * time.Second):
			sawCancel <- false
		}
		return nil
	})

	<-started
	g.Stop() // blocks until the job's WaitGroup Done, i.e. until it observes ctx.Done()

	select {
	case ok := <-sawCancel:
		if !ok {
			t.Error("job did not observe context cancellation before Stop returned")
		}
	default:
		t.Error("Stop() returned before the job finished — WaitGroup not actually waited on")
	}
}

// TestWorkerGroup_ErrorDoesNotStopScheduling confirms a job returning an
// error is logged (not asserted here directly) but keeps running on
// schedule — one bad tick shouldn't silently disable a background job.
func TestWorkerGroup_ErrorDoesNotStopScheduling(t *testing.T) {
	g := NewWorkerGroup(testLogger())
	var runs int32
	g.Run("flaky-job", time.Millisecond, 10*time.Millisecond, func(ctx context.Context) error {
		atomic.AddInt32(&runs, 1)
		return errors.New("simulated failure")
	})
	time.Sleep(55 * time.Millisecond)
	g.Stop()

	if got := atomic.LoadInt32(&runs); got < 3 {
		t.Errorf("expected the job to keep running despite returning errors, got only %d runs", got)
	}
}
