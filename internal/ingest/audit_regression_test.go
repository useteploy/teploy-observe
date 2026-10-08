package ingest

import (
	"context"
	"errors"
	"github.com/neutron-build/neutron/go/neutron"
	"github.com/neutron-build/neutron/go/nucleus"
	"github.com/useteploy/teploy-observe/internal/identity"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"
)

func TestPendingDedupeDoesNotMutateRetryBatch(t *testing.T) {
	chunk := []Event{{SiteID: "a", EventID: "old"}, {SiteID: "a", EventID: "new"}, {SiteID: "a", EventID: "new"}, {SiteID: "b", EventID: "new"}}
	original := append([]Event(nil), chunk...)
	got := dedupePending(chunk, map[eventKey]struct{}{{SiteID: "a", EventID: "old"}: {}})
	if !reflect.DeepEqual(chunk, original) {
		t.Fatal("retry backing storage mutated")
	}
	if len(got) != 2 || got[0].SiteID != "a" || got[1].SiteID != "b" {
		t.Fatalf("wrong dedupe: %+v", got)
	}
	if fresh := dedupePending(chunk, nil); len(fresh) != 3 {
		t.Fatalf("pending-only dedupe: %+v", fresh)
	}
}

func TestSiteCapActivationAtBucketCeilingFailsClosed(t *testing.T) {
	rl := &RateLimiter{rate: 100, interval: time.Second, burst: 200, siteLimits: make(map[string]int)}
	rl.buckets = make(map[string]*bucket, maxBuckets)
	b := &bucket{tokens: 10, rate: 5, cap: 10, lastFill: time.Now()}
	rl.buckets["site|ip"] = b
	for i := 0; len(rl.buckets) < maxBuckets; i++ {
		rl.buckets[time.Unix(int64(i), 0).String()] = b
	}
	rl.SetSiteCap("site", 5)
	if rl.Allow("site", "ip") {
		t.Fatal("required aggregate missing but request admitted")
	}
	if b.tokens != 10 {
		t.Fatal("denial consumed client token")
	}
}

func TestWALDirectoryFailureRefusesConstructionAndRotation(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	failure := errors.New("injected directory fsync failure")
	if q, err := newDiskQueueWithDirSync(t.TempDir(), "events", time.Hour, 600, 6000, logger, func(string) error { return failure }); err == nil {
		q.Close()
		t.Fatal("constructor ignored directory fsync")
	}
	q, err := NewDiskQueueWithLimits(t.TempDir(), "events", time.Hour, 600, 6000, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	off, err := q.Append(ev("first"))
	if err != nil {
		t.Fatal(err)
	}
	if err = q.WaitCommit(off); err != nil {
		t.Fatal(err)
	}
	q.mu.Lock()
	q.dirSyncHook = func(string) error { return failure }
	q.mu.Unlock()
	var appendErr error
	for i := 0; i < 10; i++ {
		_, appendErr = q.Append(ev("second"))
		if appendErr != nil {
			break
		}
	}
	if appendErr == nil || q.LastError() == nil {
		t.Fatal("rotation failure did not latch")
	}
	if _, err = q.Append(ev("after-failure")); err == nil {
		t.Fatal("latched queue accepted another append")
	}
}

type fakePrivacy struct{ fail bool }

func (p *fakePrivacy) PrivacyConfigChecked(context.Context, string) (string, bool, bool, error) {
	if p.fail {
		return "", false, false, errors.New("store unavailable")
	}
	return "site-salt", false, true, nil
}
func TestPrivacyLookupFailureDoesNotChangeIdentity(t *testing.T) {
	ctx := WithSiteID(context.WithValue(context.Background(), keyUserAgent, "Mozilla/5.0"), "site")
	in := IngestInput{DistinctID: "user"}
	p := &fakePrivacy{fail: true}
	if _, err := prepareEvent(ctx, in, "global", p); err == nil {
		t.Fatal("failed privacy lookup accepted")
	}
	p.fail = false
	got, err := prepareEvent(ctx, in, "global", p)
	if err != nil {
		t.Fatal(err)
	}
	if got.DistinctID != identity.HashDistinctID("user", "site-salt") {
		t.Fatalf("wrong recovered digest: %s", got.DistinctID)
	}
}

func TestHistoricalImportPreservesTimeAndIdentityForCurl(t *testing.T) {
	ctx := WithSiteID(context.WithValue(context.Background(), keyUserAgent, "curl/8.0"), "site")
	ts := time.Date(2020, 1, 1, 12, 0, 0, 0, time.UTC).UnixMilli()
	in := Event{EventID: "source-id-0001", SessionID: "old-session", Timestamp: ts, Pathname: "/landing", Browser: "Firefox", OS: "Linux", Device: "desktop"}
	got, err := prepareHistorical(ctx, in, "salt", nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got.EventID != in.EventID || got.Timestamp != ts || got.SessionID != in.SessionID || got.Pathname != in.Pathname || got.Browser != in.Browser {
		t.Fatalf("history changed: %+v", got)
	}
	buf := batchTestBuffer(t)
	h := HistoricalHandler(buf, "salt", nil)
	response, err := h(ctx, HistoricalInput{Events: []Event{in}})
	if err != nil || response.Accepted != 1 || buf.Len() != 1 {
		t.Fatalf("curl import failed: %+v %v", response, err)
	}
	in.SiteID = "other"
	if _, err := h(ctx, HistoricalInput{Events: []Event{in}}); err == nil {
		t.Fatal("cross-site import admitted")
	}
}

func TestErasureLookupRunsBeforeAdmission(t *testing.T) {
	buf := batchTestBuffer(t)
	ctx := WithSiteID(context.WithValue(context.Background(), keyUserAgent, "Mozilla/5.0"), "site")
	var seen string
	lookupErr := error(nil)
	buf.SetErasureLookup(func(_ context.Context, site, key string) (bool, error) { seen = key; return true, lookupErr })
	input := IngestInput{DistinctID: "user"}
	_, err := Handler(buf, "salt", nil)(ctx, input)
	var app *neutron.AppError
	if !errors.As(err, &app) || app.Status != 410 || buf.Len() != 0 {
		t.Fatalf("erased admitted: %v", err)
	}
	if seen != identity.HashDistinctID("user", "salt") {
		t.Fatal("lookup received unhashed key")
	}
	lookupErr = errors.New("store down")
	_, err = BatchHandler(buf, "salt", nil, nil)(ctx, BatchInput{Events: []IngestInput{{EventType: "pageview"}, input}})
	if !errors.As(err, &app) || app.Status != 503 || buf.Len() != 0 {
		t.Fatalf("privacy outage admitted prefix: %v", err)
	}
}

// A dedupe query failure returns a nil persisted set. It must still dedupe
// pending identities, without map-assignment panic or mutation of retry data.
func TestPendingDedupeHandlesFailedPersistedLookup(t *testing.T) {
	events := []Event{{SiteID: "site", EventID: "retry"}, {SiteID: "site", EventID: "retry"}}
	got := dedupePending(events, nil)
	if len(got) != 1 || len(events) != 2 {
		t.Fatalf("failed-lookup pending dedupe: %+v", got)
	}
}

type fakeIngestTransaction struct {
	rollback        bool
	commit          bool
	cleanupCanceled bool
}

func (f *fakeIngestTransaction) SQL() *nucleus.SQLModel       { return nil }
func (f *fakeIngestTransaction) Commit(context.Context) error { f.commit = true; return nil }
func (f *fakeIngestTransaction) Rollback(ctx context.Context) error {
	f.rollback = true
	f.cleanupCanceled = ctx.Err() != nil
	return nil
}
func TestIngestTransactionReleasesOnPanicAndCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, panicWork := range []bool{false, true} {
		tx := &fakeIngestTransaction{}
		func() {
			defer func() {
				if recovered := recover(); (recovered != nil) != panicWork {
					t.Fatalf("unexpected panic: %v", recovered)
				}
			}()
			err := withIngestTransaction(ctx, tx, func(*nucleus.SQLModel) error {
				if panicWork {
					panic("injected work failure")
				}
				return ctx.Err()
			})
			if !panicWork && err == nil {
				t.Fatal("work error lost")
			}
		}()
		if !tx.rollback || tx.commit || tx.cleanupCanceled {
			t.Fatalf("transaction leaked/cleanup canceled: %+v", tx)
		}
	}
}
