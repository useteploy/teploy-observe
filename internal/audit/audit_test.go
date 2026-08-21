package audit

import (
	"context"
	"strings"
	"testing"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
)

// --- Pure unit tests (no DB) --------------------------------------------------

func TestBuildListQuery_Empty(t *testing.T) {
	q, args := buildListQuery(Filter{})
	if strings.Contains(q, "WHERE") {
		t.Errorf("empty filter should have no WHERE: %s", q)
	}
	if len(args) != 0 {
		t.Errorf("empty filter should have no args, got %v", args)
	}
	if !strings.Contains(q, "ORDER BY CAST(timestamp AS BIGINT) DESC") {
		t.Errorf("missing ordering: %s", q)
	}
	if !strings.HasSuffix(q, "LIMIT 200") {
		t.Errorf("expected default limit 200: %s", q)
	}
}

func TestBuildListQuery_AllFilters(t *testing.T) {
	q, args := buildListQuery(Filter{
		SiteID: "site1", Actor: "alice", Action: "auth.login", Result: "failure",
		From: 1000, To: 2000, Limit: 50,
	})
	for i, want := range []string{
		"site_id = $1", "actor = $2", "action = $3", "result = $4",
		"CAST(timestamp AS BIGINT) >= $5", "CAST(timestamp AS BIGINT) <= $6",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("cond %d missing %q in: %s", i, want, q)
		}
	}
	// Time bounds are IntParam-stringified; the four non-time filters are raw.
	if len(args) != 6 {
		t.Fatalf("expected 6 args, got %d: %v", len(args), args)
	}
	if args[0] != "site1" || args[1] != "alice" || args[2] != "auth.login" || args[3] != "failure" {
		t.Errorf("unexpected filter args: %v", args)
	}
	if args[4] != "1000" || args[5] != "2000" {
		t.Errorf("time bounds should be stringified ints: %v", args[4:])
	}
	if !strings.HasSuffix(q, "LIMIT 50") {
		t.Errorf("limit not applied: %s", q)
	}
}

func TestBuildListQuery_LimitClamp(t *testing.T) {
	if q, _ := buildListQuery(Filter{Limit: 999999}); !strings.HasSuffix(q, "LIMIT 1000") {
		t.Errorf("limit should clamp to 1000: %s", q)
	}
	if q, _ := buildListQuery(Filter{Limit: -5}); !strings.HasSuffix(q, "LIMIT 200") {
		t.Errorf("non-positive limit should default to 200: %s", q)
	}
}

func TestBuildListQuery_FromOnly(t *testing.T) {
	// A zero To must not add an upper bound; a zero From must not add a lower one.
	q, args := buildListQuery(Filter{From: 500})
	if !strings.Contains(q, ">= $1") || strings.Contains(q, "<=") {
		t.Errorf("only lower bound expected: %s", q)
	}
	if len(args) != 1 || args[0] != "500" {
		t.Errorf("unexpected args: %v", args)
	}
}

func TestMarshalMetadata(t *testing.T) {
	if got := MarshalMetadata(nil); got != "{}" {
		t.Errorf("nil metadata should be {}, got %s", got)
	}
	got := MarshalMetadata(map[string]any{"k": "v"})
	if got != `{"k":"v"}` {
		t.Errorf("unexpected marshal: %s", got)
	}
}

func TestGenID(t *testing.T) {
	a, b := genID(), genID()
	if len(a) != 32 || a == b {
		t.Errorf("genID should be unique 32-hex, got %q %q", a, b)
	}
}

// integrationAuditKey is shared by every integration test in this package.
//
// They write into one audit_events table and Verify walks all of it, so the
// whole chain has to be signed with one key. Two keys produce a "contents
// modified" verdict on untouched records — the verifier behaving correctly,
// reported as a tamper-evidence failure.
const integrationAuditKey = "test-audit-key"

// --- Integration (skips cleanly when Nucleus is unreachable) ------------------

func TestRecordAndList_Integration(t *testing.T) {
	dsn := nucleustest.DSN(t)
	ctx := context.Background()
	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("nucleus not reachable at %s — skipping integration test", dsn)
	}
	defer db.Close()

	svc := NewService(db, []byte(integrationAuditKey))
	// The migration may not have run against this ad-hoc DB; create the table.
	if _, err := db.SQL().Exec(ctx, `CREATE TABLE IF NOT EXISTS audit_events (
		audit_id TEXT NOT NULL, tenant_id TEXT NOT NULL DEFAULT 'default',
		site_id TEXT NOT NULL DEFAULT 'default', timestamp BIGINT NOT NULL,
		actor TEXT NOT NULL DEFAULT '', actor_type TEXT NOT NULL DEFAULT 'user',
		action TEXT NOT NULL, target TEXT NOT NULL DEFAULT '',
		result TEXT NOT NULL DEFAULT 'success', source_ip TEXT NOT NULL DEFAULT '',
		user_agent TEXT NOT NULL DEFAULT '', metadata TEXT NOT NULL DEFAULT '{}',
		seq BIGINT NOT NULL DEFAULT 0, prev_hash TEXT NOT NULL DEFAULT '', hash TEXT NOT NULL DEFAULT ''
	) WITH (engine = 'mergetree') ORDER BY (tenant_id, site_id, timestamp)`); err != nil {
		t.Fatalf("create table: %v", err)
	}

	// Unique actor so we don't collide with other rows in a shared dev DB.
	actor := "test-" + genID()
	if err := svc.Record(ctx, AuditEvent{
		SiteID: "default", Actor: actor, Action: "auth.login", Result: ResultSuccess,
		SourceIP: "1.2.3.4",
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	got, err := svc.List(ctx, Filter{Actor: actor})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].Action != "auth.login" || got[0].SourceIP != "1.2.3.4" {
		t.Fatalf("unexpected list result: %+v", got)
	}
	if got[0].Timestamp == 0 || got[0].AuditID == "" {
		t.Errorf("defaults not filled: %+v", got[0])
	}
}

func TestTimeFilter_Integration(t *testing.T) {
	dsn := nucleustest.DSN(t)
	ctx := context.Background()
	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("nucleus not reachable at %s — skipping integration test", dsn)
	}
	defer db.Close()
	svc := NewService(db, []byte(integrationAuditKey))
	if _, err := db.SQL().Exec(ctx, `CREATE TABLE IF NOT EXISTS audit_events (
		audit_id TEXT NOT NULL, tenant_id TEXT NOT NULL DEFAULT 'default',
		site_id TEXT NOT NULL DEFAULT 'default', timestamp BIGINT NOT NULL,
		actor TEXT NOT NULL DEFAULT '', actor_type TEXT NOT NULL DEFAULT 'user',
		action TEXT NOT NULL, target TEXT NOT NULL DEFAULT '',
		result TEXT NOT NULL DEFAULT 'success', source_ip TEXT NOT NULL DEFAULT '',
		user_agent TEXT NOT NULL DEFAULT '', metadata TEXT NOT NULL DEFAULT '{}',
		seq BIGINT NOT NULL DEFAULT 0, prev_hash TEXT NOT NULL DEFAULT '', hash TEXT NOT NULL DEFAULT ''
	) WITH (engine = 'mergetree') ORDER BY (tenant_id, site_id, timestamp)`); err != nil {
		t.Fatalf("create table: %v", err)
	}

	// Two events for one actor at known, far-apart timestamps. This proves the
	// CAST(timestamp AS BIGINT) range filter works on real Nucleus (a plain
	// BIGINT comparison would silently match wrong because Nucleus returns the
	// column as text over the wire — the gotcha this design guards against).
	actor := "timefilter-" + genID()
	if err := svc.Record(ctx, AuditEvent{Actor: actor, Action: "x.old", Timestamp: 1_000}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Record(ctx, AuditEvent{Actor: actor, Action: "x.new", Timestamp: 9_000_000_000_000}); err != nil {
		t.Fatal(err)
	}

	// from=2000 must exclude the t=1000 row and keep the recent one.
	got, err := svc.List(ctx, Filter{Actor: actor, From: 2_000})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].Action != "x.new" {
		t.Fatalf("time filter wrong (CAST(BIGINT) issue?): got %+v", got)
	}

	// A [1, 5000] window must keep only the old row.
	got, err = svc.List(ctx, Filter{Actor: actor, From: 1, To: 5_000})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].Action != "x.old" {
		t.Fatalf("bounded window wrong: got %+v", got)
	}
}

func TestRecord_ActionRequired(t *testing.T) {
	// Action validation happens before any DB call, so this needs no Nucleus.
	svc := NewService(nil, nil)
	if err := svc.Record(context.Background(), AuditEvent{Actor: "x"}); err == nil {
		t.Fatal("expected error when action is empty")
	}
}
