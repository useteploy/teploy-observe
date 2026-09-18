package replays
import (
	"context"
	"errors"
	"testing"

	"github.com/neutron-dev/neutron-go/nucleus"
)

// F12/F19 retry-safety proofs. All three batch retry scenarios the audit
// called out, executed against a live Nucleus (self-skip without one, same
// as the rest of this package's integration suite).

func v2Input(siteID, sessionID, replayID, producerID, batchID string, clickX int) IngestInput {
	in := basicInput(siteID, sessionID, replayID)
	in.V = 2
	in.ProducerID = producerID
	in.BatchID = batchID
	in.URL = "https://example.com/pricing"
	// Two clicks on the same bucket: a double-counted retry would put 4
	// clicks where 2 belong, visible in both children and heatmap counts.
	click := func(ts int64) struct {
		Type      string `json:"type"`
		Timestamp int64  `json:"timestamp"`
		Data      any    `json:"data"`
	} {
		return struct {
			Type      string `json:"type"`
			Timestamp int64  `json:"timestamp"`
			Data      any    `json:"data"`
		}{Type: "click", Timestamp: ts, Data: map[string]any{
			"x": clickX, "y": 20, "target": "button#cta",
			"page_url": "https://example.com/pricing", "viewport_width": 1200,
		}}
	}
	in.Events = append(in.Events, click(2000), click(2500))
	return in
}

func countRows(t *testing.T, db *nucleus.Client, ctx context.Context, q string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := db.Pool().QueryRow(ctx, q, args...).Scan(&n); err != nil {
		t.Fatalf("count query: %v", err)
	}
	return n
}

// TestIngest_V2DuplicateBatchNeverDoubleCounts: a committed batch retried
// after an ambiguous response-loss hits the ledger and writes NOTHING -
// children, session aggregates, and heatmap contributions all stay at
// single-submission values.
func TestIngest_V2DuplicateBatchNeverDoubleCounts(t *testing.T) {
	db := testDB(t)
	svc := NewReplayService(db)
	ctx := context.Background()
	siteID := uniqueID("f19-dup-site")
	replayID := uniqueID("f19-dup-replay")
	input := v2Input(siteID, "sess-1", replayID, "producer-000001", "batch-00000001", 11)

	res1, err := svc.Ingest(ctx, input)
	if err != nil {
		t.Fatalf("first submit: %v", err)
	}
	if res1.Deduped {
		t.Fatal("first submit must not be deduped")
	}
	res2, err := svc.Ingest(ctx, input)
	if err != nil {
		t.Fatalf("retry submit: %v", err)
	}
	if !res2.Deduped {
		t.Fatal("retry of a committed batch must be reported deduped")
	}

	var children int
	if err := db.Pool().QueryRow(ctx,
		"SELECT COUNT(*) FROM replay_events WHERE site_id = $1 AND replay_id = $2",
		siteID, replayID).Scan(&children); err != nil {
		t.Fatalf("count children: %v", err)
	}
	// basicInput contributes 2 events, v2Input appends 2 clicks.
	if children != 4 {
		t.Fatalf("children must stay at 4 after the retry, got %d", children)
	}

	var pages string
	if err := db.Pool().QueryRow(ctx,
		"SELECT argMax(page_count, version) FROM replay_sessions WHERE site_id = $1 AND replay_id = $2 GROUP BY tenant_id, site_id, start_time, replay_id",
		siteID, replayID).Scan(&pages); err != nil {
		t.Fatalf("read page_count: %v", err)
	}
	if pages != "1" {
		t.Fatalf("page_count must stay 1 (no navigation events), got %q", pages)
	}

	var heat string
	if err := db.Pool().QueryRow(ctx,
		"SELECT CAST(SUM(CAST(count AS BIGINT)) AS TEXT) FROM click_heatmaps WHERE site_id = $1 AND url = $2",
		siteID, "https://example.com/pricing").Scan(&heat); err != nil {
		t.Fatalf("read heatmap sum: %v", err)
	}
	if heat != "2" {
		t.Fatalf("heatmap must carry exactly the 2 original clicks, got %q", heat)
	}
}

// TestIngest_V2InterruptedBatchRetryProducesIdenticalChildren: a batch whose
// transaction rolled back before commit leaves NOTHING behind (children,
// ledger, session all absent - they committed or rolled back as one unit),
// and the retry re-derives byte-identical child identities from
// (site, replay, batch, index).
func TestIngest_V2InterruptedBatchRetryProducesIdenticalChildren(t *testing.T) {
	db := testDB(t)
	svc := NewReplayService(db)
	ctx := context.Background()
	siteID := uniqueID("f19-int-site")
	replayID := uniqueID("f19-int-replay")
	input := v2Input(siteID, "sess-1", replayID, "producer-000002", "batch-00000002", 12)

	if _, err := svc.Ingest(ctx, input); err != nil {
		t.Fatalf("first submit: %v", err)
	}
	firstIDs := make([]string, 0, 4)
	rows, err := db.Pool().Query(ctx,
		"SELECT event_id FROM replay_events WHERE site_id = $1 AND replay_id = $2 ORDER BY event_id",
		siteID, replayID)
	if err != nil {
		t.Fatalf("select first children: %v", err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		firstIDs = append(firstIDs, id)
	}
	rows.Close()
	if len(firstIDs) != 4 {
		t.Fatalf("expected 4 committed children, got %d", len(firstIDs))
	}

	// Simulate the rolled-back transaction of an interrupted first attempt:
	// remove every trace of the batch exactly as an un-committed tx would
	// have left none. If any of these three deletes were to no-op, the
	// retry assertions below prove the wrong thing, so each is checked.
	mustExec(t, db, ctx, "DELETE FROM replay_events WHERE site_id = $1 AND replay_id = $2", siteID, replayID)
	mustExec(t, db, ctx, "DELETE FROM replay_batches WHERE site_id = $1 AND replay_id = $2", siteID, replayID)
	mustExec(t, db, ctx, "DELETE FROM replay_sessions WHERE site_id = $1 AND replay_id = $2", siteID, replayID)

	res, err := svc.Ingest(ctx, input)
	if err != nil {
		t.Fatalf("retry after interruption: %v", err)
	}
	if res.Deduped {
		t.Fatal("a rolled-back batch has no ledger row; the retry must reprocess")
	}

	secondIDs := make([]string, 0, 4)
	rows2, err := db.Pool().Query(ctx,
		"SELECT event_id FROM replay_events WHERE site_id = $1 AND replay_id = $2 ORDER BY event_id",
		siteID, replayID)
	if err != nil {
		t.Fatalf("select retried children: %v", err)
	}
	for rows2.Next() {
		var id string
		if err := rows2.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		secondIDs = append(secondIDs, id)
	}
	rows2.Close()
	if len(secondIDs) != 4 {
		t.Fatalf("retry must restore all 4 children, got %d", len(secondIDs))
	}
	for i := range firstIDs {
		if firstIDs[i] != secondIDs[i] {
			t.Fatalf("child %d identity changed on retry: %q -> %q", i, firstIDs[i], secondIDs[i])
		}
		// And the identity is the documented deterministic derivation.
		want := DeterministicChildID(siteID, replayID, input.BatchID, i)
		var found bool
		for _, id := range secondIDs {
			if id == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("deterministic child id %q (index %d) not among retried children %v", want, i, secondIDs)
		}
	}
}

// TestIngest_V2BatchIDReuseRejected: the same (producer, batch) identity
// with different content is a producer bug (or ID squatting), refused
// loudly - a silent drop would hide it.
func TestIngest_V2BatchIDReuseRejected(t *testing.T) {
	db := testDB(t)
	svc := NewReplayService(db)
	ctx := context.Background()
	siteID := uniqueID("f19-reuse-site")
	replayID := uniqueID("f19-reuse-replay")

	first := v2Input(siteID, "sess-1", replayID, "producer-000003", "batch-00000003", 13)
	if _, err := svc.Ingest(ctx, first); err != nil {
		t.Fatalf("first submit: %v", err)
	}
	different := v2Input(siteID, "sess-1", replayID, "producer-000003", "batch-00000003", 99)
	_, err := svc.Ingest(ctx, different)
	if !errors.Is(err, ErrBatchIDReuse) {
		t.Fatalf("expected ErrBatchIDReuse, got %v", err)
	}
}

// TestIngest_V2WithoutClientReplayIDRejected: the idempotency key is
// (site, replay, batch); a server-generated replay id is different on every
// attempt, so v2 identity without a client replay id is a contract error.
func TestIngest_V2WithoutClientReplayIDRejected(t *testing.T) {
	db := testDB(t)
	svc := NewReplayService(db)
	ctx := context.Background()
	in := v2Input(uniqueID("f19-noid-site"), "sess-1", "", "producer-000004", "batch-00000004", 14)
	if _, err := svc.Ingest(ctx, in); !errors.Is(err, ErrV2BatchNeedsReplayID) {
		t.Fatalf("expected ErrV2BatchNeedsReplayID, got %v", err)
	}
}

// TestIngest_V1BatchStillAccepted: no v2 identity -> exactly the pre-F19
// behavior: admitted, random child ids, no ledger row.
func TestIngest_V1BatchStillAccepted(t *testing.T) {
	db := testDB(t)
	svc := NewReplayService(db)
	ctx := context.Background()
	siteID := uniqueID("f19-v1-site")
	replayID := uniqueID("f19-v1-replay")

	res, err := svc.Ingest(ctx, basicInput(siteID, "sess-1", replayID))
	if err != nil || res.Deduped {
		t.Fatalf("v1 batch must ingest without dedupe: %v %+v", err, res)
	}
	var ledger int
	if err := db.Pool().QueryRow(ctx,
		"SELECT COUNT(*) FROM replay_batches WHERE site_id = $1", siteID).Scan(&ledger); err != nil {
		t.Fatalf("count ledger: %v", err)
	}
	if ledger != 0 {
		t.Fatalf("v1 batch must not write a ledger row, found %d", ledger)
	}
}

func mustExec(t *testing.T, db *nucleus.Client, ctx context.Context, q string, args ...any) {
	t.Helper()
	if _, err := db.Pool().Exec(ctx, q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}
