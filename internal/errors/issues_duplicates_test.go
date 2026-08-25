package errors

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
)

const issuesColumns = `(
	issue_id       TEXT NOT NULL,
	tenant_id      TEXT NOT NULL DEFAULT 'default',
	site_id        TEXT NOT NULL,
	group_hash     TEXT NOT NULL,
	title          TEXT NOT NULL DEFAULT '',
	culprit        TEXT NOT NULL DEFAULT '',
	level          TEXT NOT NULL DEFAULT 'error',
	status         TEXT NOT NULL DEFAULT 'open',
	first_seen     TEXT NOT NULL,
	last_seen      TEXT NOT NULL,
	event_count    TEXT NOT NULL DEFAULT '1',
	user_count     TEXT NOT NULL DEFAULT '0',
	release_tag    TEXT NOT NULL DEFAULT '',
	version        BIGINT NOT NULL DEFAULT 0
)`

const issuesOrderBy = `(tenant_id, site_id, issue_id)`

func issuesTestDB(t *testing.T) *nucleus.Client {
	t.Helper()
	dsn := os.Getenv("OBSERVE_NUCLEUS_URL")
	if dsn == "" {
		t.Skip("no OBSERVE_NUCLEUS_URL")
	}
	db, err := nucleus.Connect(context.Background(), dsn)
	if err != nil {
		t.Skipf("connect: %v", err)
	}
	nucleustest.AsPlainMergeTree(t, db, "issues", issuesColumns, issuesOrderBy, "version")
	return db
}

// ResolveIssue caches grouphash -> issue in KV, and KV outlives the table this
// suite recreates, so every run needs its own site and hash.
func uniqSuffix(t *testing.T) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b)
}

func rowCount(t *testing.T, db *nucleus.Client, issueID string) int64 {
	t.Helper()
	type r struct {
		N int64 `db:"n"`
	}
	rows, err := nucleus.Query[r](context.Background(), db.SQL(),
		`SELECT COUNT(*) AS n FROM issues WHERE issue_id = $1`, issueID)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if len(rows) == 0 {
		return 0
	}
	return rows[0].N
}

// TestBumpIssueWritesOneRowPerBump pins the runaway that put 16,847,389 rows in
// the live issues table. bumpIssue was
//
//	INSERT INTO issues (...) SELECT ... FROM issues WHERE issue_id = $1
//
// which writes one row per row already present, so the physical count doubles
// on every error batch for that issue: 1, 2, 4, 8, … Without the fix this test
// sees 32 rows after five bumps.
func TestBumpIssueWritesOneRowPerBump(t *testing.T) {
	db := issuesTestDB(t)
	svc := NewIssueService(db)
	ctx := context.Background()

	site := "dupsite-" + uniqSuffix(t)
	id, err := svc.ResolveIssue(ctx, site, "hash-bump-"+site, "Boom", "app.go", "error", "v1", 1000)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := rowCount(t, db, id); got != 1 {
		t.Fatalf("after create: want 1 row, got %d", got)
	}

	const bumps = 5
	for i := 1; i <= bumps; i++ {
		if err := svc.bumpIssue(ctx, id, site, int64(1000+i), int64(1+i)); err != nil {
			t.Fatalf("bump %d: %v", i, err)
		}
	}
	if got, want := rowCount(t, db, id), int64(1+bumps); got != want {
		t.Fatalf("after %d bumps: want %d rows, got %d (doubling: the insert reads every version)", bumps, want, got)
	}
}

// TestUpdateStatusWritesOneRowAndReadsBack covers the same shape on the other
// writer, and the read side with it: with the superseded versions still on
// disk, an uncollapsed read returns an arbitrary one, so an issue marked
// resolved kept coming back as open.
func TestUpdateStatusWritesOneRowAndReadsBack(t *testing.T) {
	db := issuesTestDB(t)
	svc := NewIssueService(db)
	ctx := context.Background()

	site := "statussite-" + uniqSuffix(t)
	id, err := svc.ResolveIssue(ctx, site, "hash-status-"+site, "Boom", "app.go", "error", "v1", 1000)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// Several bumps first, so more than one version exists when the status
	// changes — that is what made both halves go wrong.
	for i := 1; i <= 3; i++ {
		if err := svc.bumpIssue(ctx, id, site, int64(1000+i), int64(1+i)); err != nil {
			t.Fatalf("bump: %v", err)
		}
	}
	before := rowCount(t, db, id)
	if err := svc.UpdateStatus(ctx, id, site, "resolved"); err != nil {
		t.Fatalf("update status: %v", err)
	}
	if got, want := rowCount(t, db, id), before+1; got != want {
		t.Fatalf("UpdateStatus: want %d rows, got %d", want, got)
	}

	issue, err := svc.GetIssue(ctx, id, site)
	if err != nil || issue == nil {
		t.Fatalf("get issue: %v (nil=%v)", err, issue == nil)
	}
	if issue.Status != "resolved" {
		t.Fatalf("GetIssue returned status %q, want resolved — the read picked an arbitrary version", issue.Status)
	}

	open, err := svc.ListIssues(ctx, site, "open", 50, 0)
	if err != nil {
		t.Fatalf("list open: %v", err)
	}
	for _, i := range open {
		if i.IssueID == id {
			t.Fatalf("resolved issue still listed as open — status was filtered before the collapse")
		}
	}
	resolved, err := svc.ListIssues(ctx, site, "resolved", 50, 0)
	if err != nil {
		t.Fatalf("list resolved: %v", err)
	}
	var found bool
	for _, i := range resolved {
		if i.IssueID == id {
			found = true
		}
	}
	if !found {
		t.Fatalf("resolved issue missing from the resolved list")
	}
}

// TestFindIssueByHashCollapses proves the grouphash lookup returns one issue
// rather than one per surviving version — ResolveIssue uses it on a cache miss,
// and a duplicate row there re-creates the issue's counter from a stale value.
func TestFindIssueByHashCollapses(t *testing.T) {
	db := issuesTestDB(t)
	svc := NewIssueService(db)
	ctx := context.Background()

	site := "hashsite-" + uniqSuffix(t)
	hash := "hash-find-" + site
	id, err := svc.ResolveIssue(ctx, site, hash, "Boom", "app.go", "error", "v1", 1000)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	for i := 1; i <= 3; i++ {
		if err := svc.bumpIssue(ctx, id, site, int64(2000+i), int64(10+i)); err != nil {
			t.Fatalf("bump: %v", err)
		}
	}
	found, err := svc.findIssueByHash(ctx, site, hash)
	if err != nil || found == nil {
		t.Fatalf("find by hash: %v (nil=%v)", err, found == nil)
	}
	if found.LastSeen.UnixMilli() != 2003 {
		t.Fatalf("find by hash returned last_seen %d, want 2003 — an older version won", found.LastSeen.UnixMilli())
	}
}
