package platform

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"
)

func TestSR203ApplicationHookReportsExpiredJoinAndRetryJoins(t *testing.T) {
	// Model a worker stuck in disposition using its real lifecycle state. The
	// exact hook installed by Observe must expose incomplete join, keep restart
	// excluded, and allow a later stop to finish without spawning waiter loops.
	workerCtx, cancelWorker := context.WithCancel(context.Background())
	done := make(chan struct{})
	n := &Notifier{started: true, stopCtx: workerCtx, stopCancel: cancelWorker, workerDone: done}
	hook := n.LifecycleHook()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	err := hook.OnStop(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || workerCtx.Err() == nil || !n.started {
		t.Fatalf("incomplete hook join: %v started=%v", err, n.started)
	}
	n.Start()
	if n.workerDone != done {
		t.Fatal("timed-out stop allowed overlapping restart")
	}
	if err := hook.OnStop(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired retry: %v", err)
	}
	close(done)
	if err := hook.OnStop(context.Background()); err != nil || n.started {
		t.Fatalf("retried join: %v started=%v", err, n.started)
	}
}

func TestSR203NativeApplicationHookBlockedSendRetriedJoin(t *testing.T) {
	db := o10Fixture(t)
	clk := newO10Clock()
	e := newO10Engine(t, db, &o10Receiver{}, clk)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(func() {
		unblock()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := e.notify.StopContext(ctx); err != nil {
			t.Errorf("notification cleanup join: %v", err)
		}
	})
	e.notify.client = &http.Client{Transport: auditRoundTripper(func(r *http.Request) (*http.Response, error) {
		close(entered)
		<-release // deliberately ignore cancellation until released
		return nil, r.Context().Err()
	})}
	o10Enqueue(t, e.notify, e.site, e.server.URL)
	hook := e.notify.LifecycleHook()
	if err := hook.OnStart(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("send never entered")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	err := hook.OnStop(ctx)
	unblock()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked lifecycle was reported successful: %v", err)
	}
	join, cancelJoin := context.WithTimeout(context.Background(), time.Second)
	defer cancelJoin()
	if err := hook.OnStop(join); err != nil {
		t.Fatal(err)
	}
	for _, row := range e.intents(t, "o10-notify-rule") {
		if row.DeliveredAt != 0 || row.Attempts != 0 {
			t.Fatalf("canceled send consumed disposition: %+v", row)
		}
	}
}
