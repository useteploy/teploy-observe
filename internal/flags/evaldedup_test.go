package flags

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func enabledSvc(rows *[]string, mu *sync.Mutex) *FlagService {
	svc := NewFlagService(nil)
	f := FeatureFlag{FlagID: "id", SiteID: "s", FlagKey: "f", FlagType: "boolean", Enabled: true, RolloutPct: 100}
	svc.fetchFlag = func(context.Context, string, string) ([]FeatureFlag, error) { return []FeatureFlag{f}, nil }
	svc.persistEval = func(_ context.Context, siteID, flagKey, userID, variant string) error {
		mu.Lock()
		defer mu.Unlock()
		*rows = append(*rows, strings.Join([]string{siteID, flagKey, userID, variant}, "|"))
		return nil
	}
	return svc
}

func TestEvalDedupCollapsesRepeatsWithinWindow(t *testing.T) {
	var rows []string
	var mu sync.Mutex
	svc := enabledSvc(&rows, &mu)
	clock := time.Unix(1000, 0)
	svc.dedup.now = func() time.Time { return clock }
	ctx := context.Background()

	for i := 0; i < 50; i++ {
		res, err := svc.Evaluate(ctx, "s", "f", "u1", nil)
		if err != nil || !res.Enabled {
			t.Fatalf("evaluate: %v %+v", err, res)
		}
	}
	if len(rows) != 1 {
		t.Fatalf("50 repeats wrote %d rows, want 1", len(rows))
	}
	// A different user is a different key.
	_, _ = svc.Evaluate(ctx, "s", "f", "u2", nil)
	if len(rows) != 2 {
		t.Fatalf("second user wrote %d rows total, want 2", len(rows))
	}
	// A hit does not extend the window: after it elapses, one more row.
	clock = clock.Add(DefaultEvalDedupWindow + time.Second)
	_, _ = svc.Evaluate(ctx, "s", "f", "u1", nil)
	_, _ = svc.Evaluate(ctx, "s", "f", "u1", nil)
	if len(rows) != 3 {
		t.Fatalf("after window got %d rows, want 3", len(rows))
	}
	st := svc.Stats()
	if st["eval_rows_written_total"] != 3 || st["eval_rows_deduped_total"] != 49+1 {
		t.Fatalf("stats = %v", st)
	}
}

func TestEvalDedupVariantChangeIsRecorded(t *testing.T) {
	d := newEvalDedup(time.Minute, 10)
	a := evalKey("s", "f", "control", "u")
	b := evalKey("s", "f", "treatment", "u")
	if !d.admit(a) || d.admit(a) {
		t.Fatal("same variant must dedupe")
	}
	if !d.admit(b) {
		t.Fatal("a variant change must be recorded")
	}
	// Field-boundary ambiguity must not collide.
	if evalKey("s", "ab", "", "c") == evalKey("s", "a", "", "bc") {
		t.Fatal("keys collide across field boundaries")
	}
}

func TestEvalDedupDisabledWritesEveryRow(t *testing.T) {
	var rows []string
	var mu sync.Mutex
	svc := enabledSvc(&rows, &mu).WithEvalDedup(0, 10)
	for i := 0; i < 5; i++ {
		_, _ = svc.Evaluate(context.Background(), "s", "f", "u", nil)
	}
	if len(rows) != 5 {
		t.Fatalf("window 0 wrote %d rows, want 5", len(rows))
	}
}

func TestEvalDedupLRUBoundedAndCounted(t *testing.T) {
	d := newEvalDedup(time.Hour, 100)
	for i := 0; i < 1000; i++ {
		d.admit(evalKey("s", "f", "", fmt.Sprintf("u%d", i)))
	}
	if d.size() != 100 {
		t.Fatalf("size %d, want bound 100", d.size())
	}
	if d.evictedTotal() != 900 {
		t.Fatalf("evicted %d, want 900", d.evictedTotal())
	}
	// Recently touched keys survive eviction (LRU, not FIFO).
	hot := evalKey("s", "f", "", "hot")
	d.admit(hot)
	for i := 0; i < 90; i++ {
		d.admit(evalKey("s", "f", "", fmt.Sprintf("x%d", i)))
		d.admit(hot)
	}
	if d.admit(hot) {
		t.Fatal("hot key was evicted despite constant use")
	}
}

func TestEvalDedupFailedWriteRetries(t *testing.T) {
	svc := NewFlagService(nil)
	calls := 0
	svc.persistEval = func(context.Context, string, string, string, string) error {
		calls++
		if calls == 1 {
			return errors.New("store down")
		}
		return nil
	}
	svc.recordEvaluation(context.Background(), "s", "f", "u", "")
	svc.recordEvaluation(context.Background(), "s", "f", "u", "")
	svc.recordEvaluation(context.Background(), "s", "f", "u", "")
	if calls != 2 {
		t.Fatalf("persist calls = %d, want 2 (failure retried once, then deduped)", calls)
	}
	if svc.Stats()["eval_rows_failed_total"] != 1 {
		t.Fatalf("stats = %v", svc.Stats())
	}
}

func TestEvalDedupConcurrent(t *testing.T) {
	var rows []string
	var mu sync.Mutex
	svc := enabledSvc(&rows, &mu)
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				_, _ = svc.Evaluate(context.Background(), "s", "f", "shared", nil)
			}
		}()
	}
	wg.Wait()
	if len(rows) != 1 {
		t.Fatalf("concurrent identical evaluations wrote %d rows, want 1", len(rows))
	}
}

func TestLoadEvalDedupFromEnv(t *testing.T) {
	env := map[string]string{}
	get := func(k string) string { return env[k] }
	if w, m := LoadEvalDedupFromEnv(get); w != DefaultEvalDedupWindow || m != DefaultEvalDedupMax {
		t.Fatalf("defaults = %v %d", w, m)
	}
	env["OBSERVE_FLAG_EVAL_DEDUP_SECONDS"] = "0"
	env["OBSERVE_FLAG_EVAL_DEDUP_MAX"] = "123"
	if w, m := LoadEvalDedupFromEnv(get); w != 0 || m != 123 {
		t.Fatalf("explicit = %v %d", w, m)
	}
	env["OBSERVE_FLAG_EVAL_DEDUP_SECONDS"] = "-5"
	env["OBSERVE_FLAG_EVAL_DEDUP_MAX"] = "junk"
	if w, m := LoadEvalDedupFromEnv(get); w != DefaultEvalDedupWindow || m != DefaultEvalDedupMax {
		t.Fatalf("malformed must fall back, got %v %d", w, m)
	}
}
