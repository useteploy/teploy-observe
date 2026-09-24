package queryguard

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"
)

func TestLimiterGlobalBoundRefusesLabeled(t *testing.T) {
	l := NewLimiter(2, 10)
	ctx := context.Background()
	rel1, err := l.Acquire(ctx, "s1")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	rel2, err := l.Acquire(ctx, "s2")
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	_, err = l.Acquire(ctx, "s3")
	var r *Refusal
	if !errors.As(err, &r) {
		t.Fatalf("third acquire must refuse with *Refusal, got %v", err)
	}
	if r.Code != CodeConcurrencyGlobal {
		t.Fatalf("code = %q, want %q", r.Code, CodeConcurrencyGlobal)
	}
	if r.Status != 429 || r.Remedy == "" {
		t.Fatalf("refusal must carry a remedy and 429, got %+v", r)
	}
	rel1()
	if _, err := l.Acquire(ctx, "s3"); err != nil {
		t.Fatalf("acquire after release must succeed, got %v", err)
	}
	rel2()
}

func TestLimiterSiteBoundRefusesLabeled(t *testing.T) {
	l := NewLimiter(10, 1)
	ctx := context.Background()
	rel, err := l.Acquire(ctx, "site-a")
	if err != nil {
		t.Fatalf("first site acquire: %v", err)
	}
	// Another site is unaffected — the bound is per site.
	if _, err := l.Acquire(ctx, "site-b"); err != nil {
		t.Fatalf("other site must not be refused: %v", err)
	}
	_, err = l.Acquire(ctx, "site-a")
	var r *Refusal
	if !errors.As(err, &r) || r.Code != CodeConcurrencySite {
		t.Fatalf("same-site acquire must refuse labeled, got %v", err)
	}
	rel()
	if _, err := l.Acquire(ctx, "site-a"); err != nil {
		t.Fatalf("acquire after release must succeed, got %v", err)
	}
}

func TestLimiterReleaseIdempotent(t *testing.T) {
	l := NewLimiter(1, 1)
	rel, err := l.Acquire(context.Background(), "s")
	if err != nil {
		t.Fatal(err)
	}
	rel()
	rel() // double release must not drive counters negative
	snap := l.Snapshot(DefaultBudgets())
	if snap.GlobalRunning != 0 || len(snap.Sites) != 0 {
		t.Fatalf("snapshot after release = %+v, want zero in-flight", snap)
	}
}

func TestSnapshotCarriesBudgetsAndRefusals(t *testing.T) {
	l := NewLimiter(3, 2)
	RowBudgetRefusal(l, 5)
	TimeBudgetRefusal(l, time.Second)
	snap := l.Snapshot(Budgets{Timeout: 5 * time.Second, MaxScanRows: 7, MaxWindow: 48 * time.Hour})
	if snap.Refused[CodeBudgetRows] != 1 || snap.Refused[CodeBudgetTime] != 1 {
		t.Fatalf("refusal counters = %v", snap.Refused)
	}
	if snap.Budgets["timeout_ms"] != int64(5000) || snap.Budgets["max_scan_rows"] != int64(7) {
		t.Fatalf("budgets = %v", snap.Budgets)
	}
	if snap.Budgets["max_window_days"] != int64(2) {
		t.Fatalf("max_window_days = %v", snap.Budgets["max_window_days"])
	}
	if snap.SiteLimit != 2 || snap.GlobalLimit != 3 {
		t.Fatalf("limits = %v/%v", snap.GlobalLimit, snap.SiteLimit)
	}
}

func TestBudgetRefusalNilLimiterSafe(t *testing.T) {
	r := RowBudgetRefusal(nil, 10)
	if r.Code != CodeBudgetRows {
		t.Fatalf("code = %q", r.Code)
	}
	TimeBudgetRefusal(nil, time.Second) // must not panic
}

func TestLoadBudgetsFromEnv(t *testing.T) {
	env := map[string]string{
		"OBSERVE_QUERY_TIMEOUT_MS":      "1500",
		"OBSERVE_QUERY_MAX_SCAN_ROWS":   "1234",
		"OBSERVE_QUERY_MAX_WINDOW_DAYS": "30",
	}
	b := LoadBudgetsFromEnv(func(k string) string { return env[k] }, slog.Default())
	if b.Timeout != 1500*time.Millisecond || b.MaxScanRows != 1234 || b.MaxWindow != 30*24*time.Hour {
		t.Fatalf("budgets = %+v", b)
	}

	// Bad values keep defaults with no error.
	env["OBSERVE_QUERY_MAX_SCAN_ROWS"] = "-3"
	env["OBSERVE_QUERY_TIMEOUT_MS"] = "not-a-number"
	d := DefaultBudgets()
	b = LoadBudgetsFromEnv(func(k string) string { return env[k] }, nil)
	if b.MaxScanRows != d.MaxScanRows || b.Timeout != d.Timeout {
		t.Fatalf("bad knobs must keep defaults, got %+v", b)
	}
}

func TestLoadSlotsFromEnv(t *testing.T) {
	env := map[string]string{
		"OBSERVE_QUERY_GLOBAL_CONCURRENCY": "3",
		"OBSERVE_QUERY_SITE_CONCURRENCY":   "1",
	}
	g, s := LoadSlotsFromEnv(func(k string) string { return env[k] }, nil)
	if g != 3 || s != 1 {
		t.Fatalf("slots = %d/%d", g, s)
	}
	env["OBSERVE_QUERY_GLOBAL_CONCURRENCY"] = "zero"
	g, s = LoadSlotsFromEnv(func(k string) string { return env[k] }, nil)
	if g != DefaultGlobalSlots {
		t.Fatalf("bad knob must keep default, got %d", g)
	}
}

// The pinned admission shape under concurrent load: exactly N holders at
// any instant, everyone else refused labeled, counters consistent.
func TestLimiterConcurrentHoldersBounded(t *testing.T) {
	l := NewLimiter(4, 4)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	running := 0
	maxRunning := 0
	refused := 0
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				rel, err := l.Acquire(context.Background(), "s")
				if err != nil {
					mu.Lock()
					refused++
					mu.Unlock()
					continue
				}
				mu.Lock()
				running++
				if running > maxRunning {
					maxRunning = running
				}
				mu.Unlock()
				time.Sleep(time.Millisecond)
				mu.Lock()
				running--
				mu.Unlock()
				rel()
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
	if maxRunning > 4 {
		t.Fatalf("observed %d concurrent holders, limit 4", maxRunning)
	}
	if refused == 0 {
		t.Fatal("16 workers against 4 slots must produce refusals")
	}
	snap := l.Snapshot(DefaultBudgets())
	if snap.GlobalRunning != 0 {
		t.Fatalf("in-flight after quiesce = %d", snap.GlobalRunning)
	}
	if snap.Refused[CodeConcurrencyGlobal]+snap.Refused[CodeConcurrencySite] == 0 {
		t.Fatalf("refusal counters missing: %v", snap.Refused)
	}
}
