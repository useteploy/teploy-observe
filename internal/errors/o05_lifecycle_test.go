package errors

// O05 slice 1 oracle, Nucleus-gated half: the honest issue lifecycle.
// Every test names the semantic it pins. Semantics summary (the full
// table lives in AUDIT_OPEN.md):
//
//   new event on RESOLVED issue -> reopened: status 'open',
//     regression_count +1, first_regression_at recorded on the first
//     regression only, snooze_until cleared.
//   new event on IGNORED issue -> stays ignored, no regression marker.
//   new event on OPEN issue -> stays open, no regression marker.
//   snooze (resolve + until): events during the window reopen
//     immediately; expiry with no events reads as plain resolved
//     (lazy, at read time - snooze_active flips false).
//   first_seen/last_seen: ingestion-time ms, clamped (first_seen LEAST,
//     last_seen GREATEST) so out-of-order applies never regress them.
//   affected users: COUNT(DISTINCT distinct_id), session proxy only
//     when the issue has no identified events at all.
//   fingerprint_version: recorded '1' at create, stable across bumps
//     and status changes.

import (
	"context"
	"testing"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"
)

func o05Svc(t *testing.T) (*IssueService, *Service, *nucleus.Client, string) {
	t.Helper()
	dbStore, _, site := o01ErrorsFixture(t)
	t.Cleanup(func() { dbStore.Close() })
	issueSvc := NewIssueService(dbStore)
	svc := NewService(dbStore, issueSvc, NewSearchService(dbStore), nil).
		WithPrivacy(nil, "o05-test-salt")
	return issueSvc, svc, dbStore, site
}

// o05Resolve wraps ResolveIssue with a stable grouphash namespace so the
// KV cache (which outlives any table the fixture recreates) cannot leak
// between tests.
func o05Resolve(t *testing.T, svc *IssueService, site, hash string, ts int64) string {
	t.Helper()
	id, err := svc.ResolveIssue(context.Background(), site, hash, "O05 Boom", "app.go", "error", "", ts)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	return id
}

// TestO05_NewEventReopensResolvedIssueWithRegression pins the headline
// semantic: resolve -> new event -> open + regression markers.
func TestO05_NewEventReopensResolvedIssueWithRegression(t *testing.T) {
	issueSvc, _, _, site := o05Svc(t)
	ctx := context.Background()
	suffix := uniqSuffix(t)
	hash := "o05-reopen-" + suffix

	id := o05Resolve(t, issueSvc, site, hash, 1000)
	if err := issueSvc.UpdateStatus(ctx, id, site, "resolved", 0); err != nil {
		t.Fatalf("resolve issue: %v", err)
	}
	// The new event arrives after resolution.
	o05Resolve(t, issueSvc, site, hash, 2000)

	got, err := issueSvc.GetIssue(ctx, id, site)
	if err != nil || got == nil {
		t.Fatalf("get: %v (nil=%v)", err, got == nil)
	}
	if got.Status != "open" {
		t.Fatalf("a new event after resolution must reopen: status %q", got.Status)
	}
	if got.RegressionCount != 1 {
		t.Fatalf("regression_count = %d, want 1", got.RegressionCount)
	}
	if got.FirstRegressionAt.UnixMilli() != 2000 {
		t.Fatalf("first_regression_at = %d, want 2000 (the reopening event's ingestion ms)",
			got.FirstRegressionAt.UnixMilli())
	}
	if got.SnoozeActive {
		t.Fatalf("reopen must clear any snooze state")
	}
	// The reopen is visible through the status filter too: the issue
	// must be listed under open, not resolved.
	open, err := issueSvc.ListIssues(ctx, site, "open", 50, 0)
	if err != nil {
		t.Fatalf("list open: %v", err)
	}
	var listed bool
	for _, i := range open {
		if i.IssueID == id {
			listed = true
		}
	}
	if !listed {
		t.Fatalf("reopened issue missing from the open list")
	}
}

// TestO05_IgnoredIssueNotReopened pins the deliberate asymmetry: an
// ignored issue stays ignored on new events (the operator said stop).
func TestO05_IgnoredIssueNotReopened(t *testing.T) {
	issueSvc, _, _, site := o05Svc(t)
	ctx := context.Background()
	hash := "o05-ignore-" + uniqSuffix(t)

	id := o05Resolve(t, issueSvc, site, hash, 1000)
	if err := issueSvc.UpdateStatus(ctx, id, site, "ignored", 0); err != nil {
		t.Fatalf("ignore issue: %v", err)
	}
	o05Resolve(t, issueSvc, site, hash, 2000)

	got, err := issueSvc.GetIssue(ctx, id, site)
	if err != nil || got == nil {
		t.Fatalf("get: %v (nil=%v)", err, got == nil)
	}
	if got.Status != "ignored" {
		t.Fatalf("ignored issue must stay ignored on new events, got %q", got.Status)
	}
	if got.RegressionCount != 0 || !got.FirstRegressionAt.IsZero() {
		t.Fatalf("ignored issues take no regression markers: count=%d first=%v",
			got.RegressionCount, got.FirstRegressionAt)
	}
}

// TestO05_RegressionCountAccumulatesAcrossCycles pins the count and the
// first_regression_at anchor over two resolve/reopen cycles, and the
// continuous-issue contrast.
func TestO05_RegressionCountAccumulatesAcrossCycles(t *testing.T) {
	issueSvc, _, _, site := o05Svc(t)
	ctx := context.Background()
	suffix := uniqSuffix(t)
	hash := "o05-cycles-" + suffix

	id := o05Resolve(t, issueSvc, site, hash, 1000)
	if err := issueSvc.UpdateStatus(ctx, id, site, "resolved", 0); err != nil {
		t.Fatalf("resolve #1: %v", err)
	}
	o05Resolve(t, issueSvc, site, hash, 2000)
	if err := issueSvc.UpdateStatus(ctx, id, site, "resolved", 0); err != nil {
		t.Fatalf("resolve #2: %v", err)
	}
	o05Resolve(t, issueSvc, site, hash, 3000)

	got, err := issueSvc.GetIssue(ctx, id, site)
	if err != nil || got == nil {
		t.Fatalf("get: %v (nil=%v)", err, got == nil)
	}
	if got.RegressionCount != 2 {
		t.Fatalf("regression_count = %d, want 2 (one per resolved->reopened cycle)", got.RegressionCount)
	}
	if got.FirstRegressionAt.UnixMilli() != 2000 {
		t.Fatalf("first_regression_at must anchor at the FIRST regression (2000), got %d",
			got.FirstRegressionAt.UnixMilli())
	}

	// Contrast: a continuously-open issue never takes markers.
	contHash := "o05-cont-" + suffix
	cid := o05Resolve(t, issueSvc, site, contHash, 1000)
	o05Resolve(t, issueSvc, site, contHash, 2000)
	o05Resolve(t, issueSvc, site, contHash, 3000)
	cgot, err := issueSvc.GetIssue(ctx, cid, site)
	if err != nil || cgot == nil {
		t.Fatalf("get continuous: %v (nil=%v)", err, cgot == nil)
	}
	if cgot.RegressionCount != 0 || !cgot.FirstRegressionAt.IsZero() {
		t.Fatalf("continuous issue must carry no regression markers: count=%d first=%v",
			cgot.RegressionCount, cgot.FirstRegressionAt)
	}
}

// TestO05_SnoozeWindowAndExpiry pins both snooze edges: a new event
// DURING the window reopens immediately (regression), and an expired
// snooze with no new events reads as plain resolved.
func TestO05_SnoozeWindowAndExpiry(t *testing.T) {
	issueSvc, _, _, site := o05Svc(t)
	ctx := context.Background()
	suffix := uniqSuffix(t)

	// During-window reopen.
	hash := "o05-snooze1-" + suffix
	id := o05Resolve(t, issueSvc, site, hash, 1000)
	until := time.Now().UTC().Add(time.Hour).UnixMilli()
	if err := issueSvc.UpdateStatus(ctx, id, site, "resolved", until); err != nil {
		t.Fatalf("snooze: %v", err)
	}
	got, err := issueSvc.GetIssue(ctx, id, site)
	if err != nil || got == nil {
		t.Fatalf("get snoozed: %v (nil=%v)", err, got == nil)
	}
	if got.Status != "resolved" || !got.SnoozeActive {
		t.Fatalf("fresh snooze must read resolved + snooze_active, got status=%q active=%v",
			got.Status, got.SnoozeActive)
	}
	o05Resolve(t, issueSvc, site, hash, 2000)
	got, err = issueSvc.GetIssue(ctx, id, site)
	if err != nil || got == nil {
		t.Fatalf("get reopened-from-snooze: %v (nil=%v)", err, got == nil)
	}
	if got.Status != "open" || got.RegressionCount != 1 {
		t.Fatalf("an event during the snooze window must reopen as a regression: status=%q count=%d",
			got.Status, got.RegressionCount)
	}
	if got.SnoozeActive {
		t.Fatalf("reopen from snooze must clear the active snooze")
	}

	// Expired snooze, no events during the window: plain resolved.
	hash2 := "o05-snooze2-" + suffix
	id2 := o05Resolve(t, issueSvc, site, hash2, 1000)
	expired := time.Now().UTC().Add(-time.Minute).UnixMilli()
	if err := issueSvc.UpdateStatus(ctx, id2, site, "resolved", expired); err != nil {
		t.Fatalf("expired snooze: %v", err)
	}
	got2, err := issueSvc.GetIssue(ctx, id2, site)
	if err != nil || got2 == nil {
		t.Fatalf("get expired: %v (nil=%v)", err, got2 == nil)
	}
	if got2.Status != "resolved" {
		t.Fatalf("expired snooze with no events must read resolved, got %q", got2.Status)
	}
	if got2.SnoozeActive {
		t.Fatalf("expired snooze must not read as active")
	}
	if got2.RegressionCount != 0 {
		t.Fatalf("expiry alone is not a regression, got count=%d", got2.RegressionCount)
	}
}

// TestO05_FirstLastSeenClamped pins ingestion-time first/last seen with
// out-of-order applies: an older event (PENDING retry, WAL replay)
// widens nothing and never regresses last_seen.
func TestO05_FirstLastSeenClamped(t *testing.T) {
	issueSvc, _, _, site := o05Svc(t)
	ctx := context.Background()
	hash := "o05-seen-" + uniqSuffix(t)

	id := o05Resolve(t, issueSvc, site, hash, 1000)
	o05Resolve(t, issueSvc, site, hash, 5000)
	got, err := issueSvc.GetIssue(ctx, id, site)
	if err != nil || got == nil {
		t.Fatalf("get: %v (nil=%v)", err, got == nil)
	}
	if got.FirstSeen.UnixMilli() != 1000 || got.LastSeen.UnixMilli() != 5000 {
		t.Fatalf("seen window = [%d, %d], want [1000, 5000]",
			got.FirstSeen.UnixMilli(), got.LastSeen.UnixMilli())
	}
	// The out-of-order apply: an event ingested earlier but applied
	// later (retry/replay) must not move either edge backwards.
	o05Resolve(t, issueSvc, site, hash, 2000)
	got, err = issueSvc.GetIssue(ctx, id, site)
	if err != nil || got == nil {
		t.Fatalf("re-get: %v (nil=%v)", err, got == nil)
	}
	if got.LastSeen.UnixMilli() != 5000 {
		t.Fatalf("last_seen regressed to %d, want 5000 (GREATEST clamp)", got.LastSeen.UnixMilli())
	}
	if got.FirstSeen.UnixMilli() != 1000 {
		t.Fatalf("first_seen moved to %d, want 1000 (LEAST clamp)", got.FirstSeen.UnixMilli())
	}
}

// TestO05_AffectedUsersCountedByDistinctID seeds real events through
// IngestErrorEvent and pins user_count = distinct distinct_ids, with the
// anonymous session proxy only when no identified event exists.
func TestO05_AffectedUsersCountedByDistinctID(t *testing.T) {
	_, svc, db, site := o05Svc(t)
	ctx := context.Background()

	ingest := func(dstSite, distinct, session string) {
		t.Helper()
		// One shared message keeps every event in ONE issue (grouping
		// falls to the parameterized message without stacks).
		in := ErrorInput{
			SiteID:     dstSite,
			ErrorType:  "O05Users",
			ErrorValue: "boom",
			SessionID:  session,
			DistinctID: distinct,
		}
		if _, err := svc.IngestErrorEvent(ctx, in); err != nil {
			t.Fatalf("ingest: %v", err)
		}
	}
	siteIssue := func(dstSite string) *Issue {
		t.Helper()
		rows, err := nucleus.Query[struct {
			IssueID string `db:"issue_id"`
		}](ctx, db.SQL(),
			`SELECT issue_id FROM issues WHERE site_id = $1 LIMIT 1`, dstSite)
		if err != nil || len(rows) == 0 {
			t.Fatalf("issues for %s: %v (%d rows)", dstSite, err, len(rows))
		}
		got, err := svc.issueSvc.GetIssue(ctx, rows[0].IssueID, dstSite)
		if err != nil || got == nil {
			t.Fatalf("get issue: %v (nil=%v)", err, got == nil)
		}
		return got
	}

	// Same person, three sessions, plus a second person: 2 affected
	// users even though 4 sessions appear.
	ingest(site, "user-a", "sess-1")
	ingest(site, "user-a", "sess-2")
	ingest(site, "user-a", "sess-3")
	ingest(site, "user-b", "sess-4")
	if got := siteIssue(site); got.UserCount != 2 {
		t.Fatalf("user_count = %d, want 2 (distinct distinct_id, not sessions)", got.UserCount)
	}

	// Anonymous contrast: no distinct_id anywhere -> session proxy.
	anonSite := site + "-anon"
	for _, s := range []string{"s1", "s1", "s2"} {
		ingest(anonSite, "", s)
	}
	if got := siteIssue(anonSite); got.UserCount != 2 {
		t.Fatalf("anonymous user_count = %d, want 2 (session proxy: s1, s2)", got.UserCount)
	}

	// Mixed contrast: identified + anonymous events on one issue count
	// identified persons only — the empty distinct_id must never count
	// as a "user".
	mixSite := site + "-mix"
	ingest(mixSite, "user-m", "mx-1")
	ingest(mixSite, "", "mx-2")
	ingest(mixSite, "", "mx-3")
	if got := siteIssue(mixSite); got.UserCount != 1 {
		t.Fatalf("mixed user_count = %d, want 1 (identified person; '' is not a user)", got.UserCount)
	}
}

// TestO05_FingerprintVersionRecordedAndStable pins that the issue
// records the derivation version at create and keeps it through bumps
// and status changes (no history rewrite).
func TestO05_FingerprintVersionRecordedAndStable(t *testing.T) {
	issueSvc, _, _, site := o05Svc(t)
	ctx := context.Background()
	hash := "o05-fpv-" + uniqSuffix(t)

	id := o05Resolve(t, issueSvc, site, hash, 1000)
	got, err := issueSvc.GetIssue(ctx, id, site)
	if err != nil || got == nil {
		t.Fatalf("get: %v (nil=%v)", err, got == nil)
	}
	if got.FingerprintVersion != FingerprintVersion {
		t.Fatalf("fingerprint_version = %d, want %d at create", got.FingerprintVersion, FingerprintVersion)
	}
	o05Resolve(t, issueSvc, site, hash, 2000)
	if err := issueSvc.UpdateStatus(ctx, id, site, "resolved", 0); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	o05Resolve(t, issueSvc, site, hash, 3000)
	got, err = issueSvc.GetIssue(ctx, id, site)
	if err != nil || got == nil {
		t.Fatalf("re-get: %v (nil=%v)", err, got == nil)
	}
	if got.FingerprintVersion != FingerprintVersion {
		t.Fatalf("fingerprint_version drifted to %d after churn, want %d (recorded at create, never rewritten)",
			got.FingerprintVersion, FingerprintVersion)
	}
}
