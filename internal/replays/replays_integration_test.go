package replays

import (
	"context"
	"errors"
	"testing"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
)

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
// insert the session row exactly once.
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
		"SELECT COUNT(*) FROM replay_sessions WHERE site_id = $1 AND replay_id = $2",
		siteID, replayID).Scan(&count); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 session row, got %d", count)
	}
}

// TestIngest_DedupClaimErrorFailsClosed is the regression for OBS-029: a
// genuine KV error must not be silently treated as "not yet claimed" (which
// would let every batch during an outage attempt another session insert for
// the same replay ID). This exercises the real failure by disconnecting the
// pool the moment before Ingest calls KV.SetNX — the surest way to force a
// live KV error without a mock (no KV interface exists to mock).
func TestIngest_DedupClaimErrorFailsClosed(t *testing.T) {
	dsn := nucleustest.DSN(t)
	db, err := nucleus.Connect(context.Background(), dsn)
	if err != nil {
		t.Skipf("nucleus not reachable at %s — skipping integration test", dsn)
	}
	svc := NewReplayService(db)
	db.Close() // force every subsequent KV/SQL call on this client to fail

	_, err = svc.Ingest(context.Background(), basicInput("closed-pool-site", "sess-1", "replay-x"))
	if err == nil {
		t.Fatal("expected an error when the dedupe KV claim cannot be evaluated, got nil")
	}
	if !errors.Is(err, ErrDedupUnavailable) {
		t.Errorf("expected errors.Is(err, ErrDedupUnavailable), got: %v", err)
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
	siteID := "no-salt-test-site"

	input := basicInput(siteID, "sess-1", "replay-no-salt")
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
	siteID := "raw-optin-test-site"

	input := basicInput(siteID, "sess-1", "replay-raw-optin")
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
	siteID := "salted-test-site"

	input := basicInput(siteID, "sess-1", "replay-salted")
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
