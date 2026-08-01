package explorer

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"
)

// TestExecuteRespectsTimeout is the regression harness for OBS-017: Nucleus
// does not enforce READ ONLY transactions or GRANT-restricted roles (verified
// empirically — both silently accept a write), so pgx context cancellation is
// the one real database-layer containment available. This proves it actually
// cancels a query rather than just documenting the intent. Requires a live
// Nucleus.
func TestExecuteRespectsTimeout(t *testing.T) {
	dsn := os.Getenv("OBSERVE_NUCLEUS_URL")
	if dsn == "" {
		t.Skip("no OBSERVE_NUCLEUS_URL")
	}
	db, err := nucleus.Connect(context.Background(), dsn)
	if err != nil {
		t.Skipf("connect: %v", err)
	}
	svc := NewExplorerService(db)

	pool := db.Pool()
	if _, err := pool.Exec(context.Background(), "CREATE TABLE IF NOT EXISTS explorer_timeout_test (id INTEGER)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	for i := 0; i < 500; i++ {
		if _, err := pool.Exec(context.Background(), "INSERT INTO explorer_timeout_test VALUES ($1)", i); err != nil {
			t.Fatalf("seed row %d: %v", i, err)
		}
	}

	// Temporarily shrink the package-level timeout so the test doesn't take
	// queryTimeout's full real-world duration to observe cancellation.
	orig := queryTimeout
	queryTimeout = 50 * time.Millisecond
	defer func() { queryTimeout = orig }()

	start := time.Now()
	res, err := svc.Execute(context.Background(),
		"SELECT count(*) FROM explorer_timeout_test a, explorer_timeout_test b, explorer_timeout_test c")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Execute returned a Go error (want a QueryResult.Error instead): %v", err)
	}
	if res.Error == "" {
		t.Fatalf("expected the cross-join to be cancelled by the query timeout, got a result: %+v", res)
	}
	if elapsed > 2*time.Second {
		t.Errorf("query took %v — timeout does not appear to have cancelled it", elapsed)
	}
}

// TestConcurrencyLimitBlocksExcessQueries verifies the semaphore actually
// bounds concurrent executions rather than just existing.
func TestConcurrencyLimitBlocksExcessQueries(t *testing.T) {
	dsn := os.Getenv("OBSERVE_NUCLEUS_URL")
	if dsn == "" {
		t.Skip("no OBSERVE_NUCLEUS_URL")
	}
	db, err := nucleus.Connect(context.Background(), dsn)
	if err != nil {
		t.Skipf("connect: %v", err)
	}
	svc := &ExplorerService{db: db, sem: make(chan struct{}, 2)}

	// Fill both slots, then confirm acquire blocks until context expiry rather
	// than proceeding immediately.
	release1, err := svc.acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire 1: %v", err)
	}
	defer release1()
	release2, err := svc.acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire 2: %v", err)
	}
	defer release2()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = svc.acquire(ctx)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected acquire to block and then fail once both slots were held")
	}
	if elapsed < 80*time.Millisecond {
		t.Errorf("acquire returned after %v — expected it to block near the full 100ms timeout", elapsed)
	}
}

// TestListTables_ReturnsRealSchemaNotHardcodedFallback is the regression for
// OBS-024: ListTables used to swallow the information_schema query error (and
// every row-scan error) behind a hard-coded table list, and even overrode a
// genuinely empty-but-valid result with a second hard-coded list. This proves
// it returns the actual live schema, including a table that isn't in either
// of the old hard-coded lists — a real schema divergence would previously
// have been invisible.
func TestListTables_ReturnsRealSchemaNotHardcodedFallback(t *testing.T) {
	dsn := os.Getenv("OBSERVE_NUCLEUS_URL")
	if dsn == "" {
		t.Skip("no OBSERVE_NUCLEUS_URL")
	}
	db, err := nucleus.Connect(context.Background(), dsn)
	if err != nil {
		t.Skipf("connect: %v", err)
	}
	svc := NewExplorerService(db)

	pool := db.Pool()
	if _, err := pool.Exec(context.Background(), "CREATE TABLE IF NOT EXISTS explorer_list_tables_marker (id INTEGER)"); err != nil {
		t.Fatalf("create marker table: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS explorer_list_tables_marker")

	tables, err := svc.ListTables(context.Background())
	if err != nil {
		t.Fatalf("ListTables: %v", err)
	}
	found := false
	for _, name := range tables {
		if name == "explorer_list_tables_marker" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected explorer_list_tables_marker in the live schema, got: %v", tables)
	}
}
