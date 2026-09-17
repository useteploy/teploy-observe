package replays

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
)

// uniqueID keeps the integration tests repeatable against a persistent engine.
// Fixed site and replay ids meant a second `go test ./...` against the same
// Nucleus counted the previous run's rows and failed on the event count.
func uniqueID(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

func testDB(t *testing.T) *nucleus.Client {
	t.Helper()
	dsn := nucleustest.DSN(t)
	db, err := nucleus.Connect(context.Background(), dsn)
	if err != nil {
		t.Skipf("nucleus not reachable at %s — skipping integration test", dsn)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func basicInput(siteID, sessionID, replayID string) IngestInput {
	return IngestInput{
		SiteID:    siteID,
		SessionID: sessionID,
		ReplayID:  replayID,
		URL:       "https://example.com",
		Events: []struct {
			Type      string `json:"type"`
			Timestamp int64  `json:"timestamp"`
			Data      any    `json:"data"`
		}{
			{Type: "snapshot", Timestamp: 1000},
			{Type: "mutation", Timestamp: 1500},
		},
	}
}

// TestIngest_SharedReplayIDInsertsSessionOnce is the baseline the OBS-029 fix
// must not regress: multiple batches for the same stable replay_id still
// surface exactly one session. Since 039 the table is versioned (multi-batch
// upserts write a new row per batch that collapses per replay), so the count
// goes through COUNT(DISTINCT replay_id), not COUNT(*).
func TestIngest_SharedReplayIDInsertsSessionOnce(t *testing.T) {
	db := testDB(t)
	svc := NewReplayService(db)
	ctx := context.Background()
	siteID := "dedupe-test-site"
	replayID := "shared-replay-1"

	id1, err := svc.Ingest(ctx, basicInput(siteID, "sess-1", replayID))
	if err != nil {
		t.Fatalf("first batch: %v", err)
	}
	id2, err := svc.Ingest(ctx, basicInput(siteID, "sess-1", replayID))
	if err != nil {
		t.Fatalf("second batch: %v", err)
	}
	if id1 != replayID || id2 != replayID {
		t.Fatalf("expected replay id %q both times, got %q and %q", replayID, id1, id2)
	}

	var count int
	if err := db.Pool().QueryRow(ctx,
		"SELECT COUNT(DISTINCT replay_id) FROM replay_sessions WHERE site_id = $1 AND replay_id = $2",
		siteID, replayID).Scan(&count); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 visible session, got %d", count)
	}
}

// TestIngest_CrossSiteReplayIDRejected is the audit F08 regression: a caller
// authenticated for site B must not be able to append events to a replay
// owned by site A, even when both sites use the identical client replay ID.
func TestIngest_CrossSiteReplayIDRejected(t *testing.T) {
	db := testDB(t)
	svc := NewReplayService(db)
	ctx := context.Background()
	siteA := uniqueID("f08-site-a")
	siteB := uniqueID("f08-site-b")
	replayID := uniqueID("f08-replay")

	if _, err := svc.Ingest(ctx, basicInput(siteA, "sess-a", replayID)); err != nil {
		t.Fatalf("site A first batch: %v", err)
	}
	_, err := svc.Ingest(ctx, basicInput(siteB, "sess-b", replayID))
	if !errors.Is(err, ErrCrossSiteReplay) {
		t.Fatalf("expected ErrCrossSiteReplay, got %v", err)
	}

	// The rejected batch wrote nothing under site B.
	var events int
	if err := db.Pool().QueryRow(ctx,
		"SELECT COUNT(*) FROM replay_events WHERE replay_id = $1 AND site_id = $2",
		replayID, siteB).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 0 {
		t.Fatalf("cross-site batch must perform zero writes, got %d events", events)
	}
}

// TestGetReplayEvents_ScopedToOwningSite is the audit F08 read-side
// regression: two sites reusing one client replay ID have disjoint event
// lists, and a site's read never returns the other's events.
func TestGetReplayEvents_ScopedToOwningSite(t *testing.T) {
	db := testDB(t)
	svc := NewReplayService(db)
	ctx := context.Background()
	siteA := uniqueID("f08r-site-a")
	replayID := uniqueID("f08r-replay")

	if _, err := svc.Ingest(ctx, basicInput(siteA, "sess-a", replayID)); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	events, err := svc.GetReplayEvents(ctx, replayID)
	if err != nil {
		t.Fatalf("GetReplayEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected the owner's 2 events, got %d", len(events))
	}
}

// TestIngest_DBFailureFailsClosed keeps the OBS-029 posture now that the KV
// claim is gone: with the store unavailable, Ingest must return an error
// rather than proceed as if nothing was recorded.
func TestIngest_DBFailureFailsClosed(t *testing.T) {
	dsn := nucleustest.DSN(t)
	db, err := nucleus.Connect(context.Background(), dsn)
	if err != nil {
		t.Skipf("nucleus not reachable at %s — skipping integration test", dsn)
	}
	svc := NewReplayService(db)
	db.Close() // force every subsequent SQL call on this client to fail

	_, err = svc.Ingest(context.Background(), basicInput("closed-pool-site", "sess-1", "replay-x"))
	if err == nil {
		t.Fatal("expected an error when the store is unavailable, got nil")
	}
}

// TestIngest_LaterBatchesExtendMetadata is the audit F20 regression: a second
// batch must extend duration to cover its events, mark errors sticky, and
// count pages as navigations rather than raw events.
func TestIngest_LaterBatchesExtendMetadata(t *testing.T) {
	db := testDB(t)
	svc := NewReplayService(db)
	ctx := context.Background()
	siteID := uniqueID("f20-site")
	replayID := uniqueID("f20-replay")

	first := basicInput(siteID, "sess-1", replayID)
	first.Events = []struct {
		Type      string `json:"type"`
		Timestamp int64  `json:"timestamp"`
		Data      any    `json:"data"`
	}{
		{Type: "snapshot", Timestamp: 1000},
		{Type: "mouse", Timestamp: 1100},
		{Type: "mouse", Timestamp: 1200},
	}
	if _, err := svc.Ingest(ctx, first); err != nil {
		t.Fatalf("first batch: %v", err)
	}

	second := basicInput(siteID, "sess-1", replayID)
	second.HasError = true
	second.Events = []struct {
		Type      string `json:"type"`
		Timestamp int64  `json:"timestamp"`
		Data      any    `json:"data"`
	}{
		{Type: "mouse", Timestamp: 61000},
		{Type: "navigation", Timestamp: 62000},
	}
	if _, err := svc.Ingest(ctx, second); err != nil {
		t.Fatalf("second batch: %v", err)
	}

	var duration, pages int
	var hasError string
	if err := db.Pool().QueryRow(ctx, `
		SELECT CAST(duration_ms AS BIGINT), CAST(page_count AS BIGINT), has_error
		FROM (
			SELECT argMax(duration_ms, version) AS duration_ms,
			       argMax(page_count, version) AS page_count,
			       argMax(has_error, version) AS has_error
			FROM replay_sessions
			WHERE site_id = $1 AND replay_id = $2
			GROUP BY tenant_id, site_id, start_time, replay_id
		)`, siteID, replayID).Scan(&duration, &pages, &hasError); err != nil {
		t.Fatalf("read collapsed session: %v", err)
	}
	if duration != 61000 {
		t.Fatalf("duration must span both batches (61000ms), got %d", duration)
	}
	if pages != 2 {
		t.Fatalf("page_count must be initial page + 1 navigation = 2, got %d", pages)
	}
	if hasError != "true" {
		t.Fatalf("has_error must be sticky after the second batch, got %q", hasError)
	}
}

// TestIngest_NoSaltNoRawOptInDropsIdentifierNotBatch is the regression for
// OBS-030: with no salt and no raw opt-in, the raw distinct_id must never be
// stored — but the session/events themselves (real, valuable data) must
// still be ingested rather than the whole batch being rejected.
func TestIngest_NoSaltNoRawOptInDropsIdentifierNotBatch(t *testing.T) {
	db := testDB(t)
	// No WithPrivacy call: s.salt stays "", s.privacy stays nil, so the
	// lookup always resolves to salt="" and rawOptIn=false — the exact
	// fail-open scenario OBS-030 describes.
	svc := NewReplayService(db)
	ctx := context.Background()
	siteID := uniqueID("no-salt-test-site")

	input := basicInput(siteID, "sess-1", uniqueID("replay-no-salt"))
	input.DistinctID = "user-raw-identifier@example.com"

	replayID, err := svc.Ingest(ctx, input)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	var distinctID string
	if err := db.Pool().QueryRow(ctx,
		"SELECT distinct_id FROM replay_sessions WHERE site_id = $1 AND replay_id = $2",
		siteID, replayID).Scan(&distinctID); err != nil {
		t.Fatalf("query stored distinct_id: %v", err)
	}
	if distinctID == input.DistinctID {
		t.Fatalf("raw distinct_id was stored despite no salt and no raw opt-in: %q", distinctID)
	}
	if distinctID != "" {
		t.Fatalf("expected distinct_id to be dropped (empty), got %q", distinctID)
	}

	var eventCount int
	if err := db.Pool().QueryRow(ctx,
		"SELECT COUNT(*) FROM replay_events WHERE replay_id = $1", replayID).Scan(&eventCount); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if eventCount != len(input.Events) {
		t.Fatalf("expected the batch's events to still be stored (%d), got %d", len(input.Events), eventCount)
	}
}

// TestIngest_RawOptInStillStoresRawIdentifier guards against an overcorrection:
// a site that has genuinely opted into raw storage must still get raw storage
// even when no salt is configured (salt is irrelevant when rawOptIn is true).
func TestIngest_RawOptInStillStoresRawIdentifier(t *testing.T) {
	db := testDB(t)
	svc := NewReplayService(db).WithPrivacy(
		func(ctx context.Context, siteID string) (string, bool, bool) {
			return "", true, true // no salt, but explicit raw opt-in
		},
		"",
	)
	ctx := context.Background()
	siteID := uniqueID("raw-optin-test-site")

	input := basicInput(siteID, "sess-1", uniqueID("replay-raw-optin"))
	input.DistinctID = "user-raw-identifier@example.com"

	replayID, err := svc.Ingest(ctx, input)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	var distinctID string
	if err := db.Pool().QueryRow(ctx,
		"SELECT distinct_id FROM replay_sessions WHERE site_id = $1 AND replay_id = $2",
		siteID, replayID).Scan(&distinctID); err != nil {
		t.Fatalf("query stored distinct_id: %v", err)
	}
	if distinctID != input.DistinctID {
		t.Fatalf("expected raw identifier %q to be stored under explicit opt-in, got %q", input.DistinctID, distinctID)
	}
}

// TestIngest_SaltedHashesIdentifier is the ordinary case: a real salt hashes
// the identifier rather than storing it raw or dropping it.
func TestIngest_SaltedHashesIdentifier(t *testing.T) {
	db := testDB(t)
	svc := NewReplayService(db).WithPrivacy(nil, "a-real-fallback-salt")
	ctx := context.Background()
	siteID := uniqueID("salted-test-site")

	input := basicInput(siteID, "sess-1", uniqueID("replay-salted"))
	input.DistinctID = "user-raw-identifier@example.com"

	replayID, err := svc.Ingest(ctx, input)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	var distinctID string
	if err := db.Pool().QueryRow(ctx,
		"SELECT distinct_id FROM replay_sessions WHERE site_id = $1 AND replay_id = $2",
		siteID, replayID).Scan(&distinctID); err != nil {
		t.Fatalf("query stored distinct_id: %v", err)
	}
	if distinctID == "" || distinctID == input.DistinctID {
		t.Fatalf("expected a non-empty hashed value distinct from the raw input, got %q", distinctID)
	}
}
