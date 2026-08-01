package auth

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/neutron-dev/neutron-go/nucleus"
)

// TestEnsureAdminConcurrentRace is the regression for OBS-008: EnsureAdmin
// used to be a plain COUNT-then-INSERT with no atomicity, so concurrent
// first-run setup requests could both observe zero admins and both insert,
// creating two initial administrators. The KV.SetNX claim must make exactly
// one of N concurrent callers win.
func TestEnsureAdminConcurrentRace(t *testing.T) {
	ctx, db, done := connect(t)
	defer done()

	// Start from a clean slate: a prior run (or prior finding's own bootstrap)
	// may have already claimed the KV key or inserted rows.
	if _, err := db.SQL().Exec(ctx, "DELETE FROM admin_users"); err != nil {
		t.Fatalf("cleanup admin_users: %v", err)
	}
	if _, err := db.KV().Delete(ctx, bootstrapClaimKey); err != nil {
		t.Fatalf("cleanup bootstrap claim: %v", err)
	}

	svc := testService(db)

	const n = 20
	var wg sync.WaitGroup
	var createdCount int64
	results := make([]bool, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			created, err := svc.EnsureAdmin(ctx, uniqueSite("racer"), "correcthorsebatterystaple")
			results[i] = created
			errs[i] = err
			if created {
				atomic.AddInt64(&createdCount, 1)
			}
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("caller %d: unexpected error: %v", i, err)
		}
	}
	if createdCount != 1 {
		t.Errorf("expected exactly 1 caller to win the bootstrap race, got %d", createdCount)
	}

	rows, err := nucleus.Query[countRow](ctx, db.SQL(), "SELECT COUNT(*) AS count FROM admin_users")
	if err != nil {
		t.Fatalf("count admin_users: %v", err)
	}
	if len(rows) == 0 || rows[0].Count != 1 {
		got := int64(-1)
		if len(rows) > 0 {
			got = rows[0].Count
		}
		t.Errorf("expected exactly 1 admin_users row after the race, got %d", got)
	}

	// Cleanup so this test is repeatable and doesn't leave state for others.
	db.SQL().Exec(context.Background(), "DELETE FROM admin_users")
	db.KV().Delete(context.Background(), bootstrapClaimKey)
}
