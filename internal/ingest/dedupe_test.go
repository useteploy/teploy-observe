package ingest

import (
	"context"
	"testing"
	"time"

	"github.com/useteploy/teploy-observe/internal/sites"
)

// F12: the in-process admission cache must (a) recognize a repeated batch
// key within its TTL, (b) let a first-time key through, and (c) forget keys
// past their TTL so the cache is a dedupe window, not a permanent set.
func TestBatchDeduper_Window(t *testing.T) {
	now := time.Unix(1700000000, 0)
	d := NewBatchDeduper(10*time.Minute, 4)
	d.now = func() time.Time { return now }

	if d.duplicate("p1\x00b1") {
		t.Fatal("fresh key must not be a duplicate")
	}
	d.record("p1\x00b1")

	now = now.Add(time.Minute)
	if !d.duplicate("p1\x00b1") {
		t.Fatal("recorded key inside TTL must be a duplicate")
	}
	// The duplicate check refreshes the window: hammering the same key keeps
	// it recognized.
	now = now.Add(9 * time.Minute)
	if !d.duplicate("p1\x00b1") {
		t.Fatal("refreshed key must still be recognized inside its window")
	}
	// Past the refreshed TTL the key is forgotten.
	now = now.Add(11 * time.Minute)
	if d.duplicate("p1\x00b1") {
		t.Fatal("key past TTL must be forgotten")
	}
}

// F12: the ring bound evicts the oldest key at capacity - the cache cannot
// grow without limit under producer churn.
func TestBatchDeduper_Bounded(t *testing.T) {
	now := time.Unix(1700000000, 0)
	d := NewBatchDeduper(time.Hour, 3)
	d.now = func() time.Time { return now }
	for i := 0; i < 10; i++ {
		k := string(rune('a'+i)) + "-producer-key-batch"
		if d.duplicate(k) {
			t.Fatalf("key %d must be fresh", i)
		}
		d.record(k)
	}
	if len(d.seen) > 3 || len(d.order) > 3 {
		t.Fatalf("cache must stay bounded (seen=%d ring=%d)", len(d.seen), len(d.order))
	}
	// The oldest keys were evicted; the newest one is still recognized.
	if !d.duplicate("j-producer-key-batch") {
		t.Fatal("the newest recorded key must still be recognized as a duplicate")
	}
	if d.duplicate("a-producer-key-batch") {
		t.Fatal("the oldest key must have been evicted at capacity")
	}
}

func batchTestBuffer(t *testing.T) *Buffer {
	t.Helper()
	return NewBuffer(nil, 1000, 1000, time.Hour, nil)
}

// withTestUA stamps a non-bot User-Agent so prepareEvent does not silently
// drop the event as bot traffic.
func withTestUA(ctx context.Context) context.Context {
	return WithUserAgent(ctx, "observe-test/1.0")
}

// F12: a v2 batch resubmitted after this process already admitted it gets a
// Deduped ack and is NOT buffered again - the fast path for the
// response-lost retry. The key is only recorded after successful admission,
// so a refused (buffer-full) first attempt remains retryable.
func TestBatchHandler_V2DuplicateAdmission(t *testing.T) {
	buf := batchTestBuffer(t)
	d := NewBatchDeduper(DefaultBatchDedupeTTL, DefaultBatchDeduperCapacity)
	h := BatchHandler(buf, "salt", (*sites.SiteService)(nil), d)
	ctx := withTestUA(context.Background())

	input := BatchInput{
		V:          2,
		ProducerID: "prod-12345678",
		BatchID:    "batch-12345678",
		Events: []IngestInput{
			{SiteID: "s1", EventType: "pageview", EventID: "aaaa-bbbb-cccc-1"},
		},
	}

	res1, err := h(ctx, input)
	if err != nil {
		t.Fatalf("first submit: %v", err)
	}
	if res1.Deduped || res1.Accepted != 1 {
		t.Fatalf("first submit must be accepted, got %+v", res1)
	}
	if buf.Len() != 1 {
		t.Fatalf("buffer must hold the admitted event, has %d", buf.Len())
	}

	res2, err := h(ctx, input)
	if err != nil {
		t.Fatalf("retry submit: %v", err)
	}
	if !res2.Deduped {
		t.Fatalf("retry must be reported as deduped, got %+v", res2)
	}
	if buf.Len() != 1 {
		t.Fatalf("retry must not re-buffer, buffer holds %d", buf.Len())
	}
}

// F12: v1 batches (no producer identity) and malformed v2 identities keep
// today's semantics - admitted every time, dedupe deferred to the flush-time
// event-id filter (which still applies when event ids are present).
func TestBatchHandler_V1AndMalformedIdentityStillAdmit(t *testing.T) {
	buf := batchTestBuffer(t)
	d := NewBatchDeduper(DefaultBatchDedupeTTL, DefaultBatchDeduperCapacity)
	h := BatchHandler(buf, "salt", (*sites.SiteService)(nil), d)
	ctx := withTestUA(context.Background())

	v1 := BatchInput{Events: []IngestInput{{SiteID: "s1", EventType: "pageview"}}}
	for i := 0; i < 2; i++ {
		res, err := h(ctx, v1)
		if err != nil {
			t.Fatalf("v1 submit %d: %v", i, err)
		}
		if res.Deduped {
			t.Fatalf("v1 submit %d must never dedupe at admission", i)
		}
	}
	if buf.Len() != 2 {
		t.Fatalf("both v1 copies admitted, buffer holds %d", buf.Len())
	}

	// Malformed ids (too short) fall back to v1 semantics.
	buf2 := batchTestBuffer(t)
	h2 := BatchHandler(buf2, "salt", (*sites.SiteService)(nil), d)
	bad := BatchInput{V: 2, ProducerID: "short", BatchID: "also-short",
		Events: []IngestInput{{SiteID: "s1", EventType: "pageview"}}}
	for i := 0; i < 2; i++ {
		if _, err := h2(ctx, bad); err != nil {
			t.Fatalf("malformed submit %d: %v", i, err)
		}
	}
	if buf2.Len() != 2 {
		t.Fatalf("malformed identity must not dedupe, buffer holds %d", buf2.Len())
	}
}

// F12: producer-supplied event ids must be carried onto the stored Event
// unchanged (when well-formed) and replaced with server ids when not.
func TestPrepareEvent_ProducerEventID(t *testing.T) {
	ctx := withTestUA(context.Background())

	good, err := prepareEvent(ctx, IngestInput{
		SiteID: "s1", EventType: "pageview", EventID: "aaaa-bbbb-cccc-1",
	}, "salt", nil)
	if err != nil || good == nil {
		t.Fatalf("prepare: %v / %v", good, err)
	}
	if good.EventID != "aaaa-bbbb-cccc-1" {
		t.Fatalf("producer id must be kept, got %q", good.EventID)
	}

	bad, err := prepareEvent(ctx, IngestInput{
		SiteID: "s1", EventType: "pageview", EventID: "short",
	}, "salt", nil)
	if err != nil || bad == nil {
		t.Fatalf("prepare: %v / %v", bad, err)
	}
	if bad.EventID == "short" || len(bad.EventID) != 32 {
		t.Fatalf("invalid producer id must be replaced by a server id, got %q", bad.EventID)
	}
}
