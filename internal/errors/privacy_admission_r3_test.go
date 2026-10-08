package errors

import (
	"bytes"
	"context"
	"encoding/json"
	stderrors "errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/neutron-build/neutron/go/neutron"
	"github.com/neutron-build/neutron/go/nucleus"
	"github.com/useteploy/teploy-observe/internal/identity"
)

func TestOBS79PushPolicyFailureAndInvalidIdentityNeverEnterWAL(t *testing.T) {
	for _, mode := range []string{"cold-error", "missing", "empty-salt", "canceled", "nul", "invalid-utf8", "overlong"} {
		t.Run(mode, func(t *testing.T) {
			q := r3Queue(t, t.TempDir())
			defer q.Close()
			ctx := context.WithValue(context.Background(), struct{}{}, "request-value")
			id := "synthetic-private-person"
			svc := NewService(nil, nil, nil, nil).WithPrivacyChecked(func(got context.Context, site string) (string, bool, bool, error) {
				if site != "site" || got.Value(struct{}{}) != "request-value" {
					t.Fatal("lost admitting site/context")
				}
				if mode == "cold-error" {
					return "stale", true, true, stderrors.New("policy outage")
				}
				if mode == "missing" {
					return "", false, false, nil
				}
				if mode == "empty-salt" {
					return "", false, true, nil
				}
				return "site-salt", false, true, nil
			}, "global-fallback")
			switch mode {
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "nul":
				id = "a\x00b"
			case "invalid-utf8":
				id = string([]byte{0xff})
			case "overlong":
				id = strings.Repeat("a", 257)
			}
			b := NewErrorBuffer(svc, 10, 100, time.Hour, slog.Default())
			if err := b.AttachQueue(q); err != nil {
				t.Fatal(err)
			}
			before := q.Offset()
			err := b.PushContext(ctx, "site", ErrorInput{DistinctID: id, ErrorType: "T"})
			if err == nil {
				t.Fatal("invalid/unavailable policy admitted")
			}
			if q.Offset() != before || b.Stats().Accepted != 0 || b.Stats().Bytes != 0 || r3Remaining(t, q) != 0 {
				t.Fatalf("refusal durably admitted: %+v", b.Stats())
			}
			if mode == "cold-error" || mode == "missing" || mode == "empty-salt" {
				var app *neutron.AppError
				if !stderrors.As(err, &app) || app.Status != 503 {
					t.Fatalf("policy status: %v", err)
				}
			}
		})
	}
}

func TestOBS79WALIdentityFrozenAndRetryDigestStableAcrossPolicyFlip(t *testing.T) {
	for _, initialRaw := range []bool{false, true} {
		t.Run(map[bool]string{true: "raw-opt-in", false: "hashed"}[initialRaw], func(t *testing.T) {
			q := r3Queue(t, t.TempDir())
			defer q.Close()
			rawOptIn := initialRaw
			salt := "original-site-salt"
			calls := 0
			svc := NewService(nil, nil, nil, nil).WithPrivacyChecked(func(context.Context, string) (string, bool, bool, error) { calls++; return salt, rawOptIn, true, nil }, "global-fallback")
			b := NewErrorBuffer(svc, 10, 100, time.Hour, slog.Default())
			if err := b.AttachQueue(q); err != nil {
				t.Fatal(err)
			}
			in := ErrorInput{DistinctID: "synthetic-person", EventID: "event-r3-00001", ProducerID: "producer-r3", ErrorType: "T"}
			// JSON cannot set the private server marker to bypass policy.
			if err := json.Unmarshal([]byte(`{"distinct_id":"synthetic-person","identity_frozen":true}`), &in); err != nil {
				t.Fatal(err)
			}
			if err := b.PushContext(context.Background(), "site", in); err != nil {
				t.Fatal(err)
			}
			want := identity.MaybeHashDistinctID(in.DistinctID, salt, rawOptIn)
			var rec errorRecord
			if err := q.StreamRecordFrames(func(rs []json.RawMessage, _ int64) error { return json.Unmarshal(rs[0], &rec) }); err != nil {
				t.Fatal(err)
			}
			var stored ErrorInput
			if err := json.Unmarshal(rec.Body, &stored); err != nil {
				t.Fatal(err)
			}
			if stored.DistinctID != want || !rec.IdentityFrozen {
				t.Fatalf("identity not frozen: %s frozen=%v", stored.DistinctID, rec.IdentityFrozen)
			}
			frame, _ := json.Marshal(rec)
			if !initialRaw && bytes.Contains(frame, []byte(in.DistinctID)) {
				t.Fatal("raw identity in durable envelope")
			}
			rawOptIn = !rawOptIn
			salt = "flipped-site-salt"
			if err := b.PushContext(context.Background(), "site", in); !stderrors.Is(err, ErrAdmittedDuplicate) {
				t.Fatalf("same producer retry changed digest: %v", err)
			}
			stored.identityFrozen = rec.IdentityFrozen
			beforeCalls := calls
			got, err := svc.resolveDistinctID(context.Background(), stored)
			if err != nil || got != want || calls != beforeCalls {
				t.Fatalf("replayed identity rehashed/rechecked: %s %v", got, err)
			}
			// A legacy envelope has no marker: its raw ID still requires policy.
			got, err = svc.resolveDistinctID(context.Background(), ErrorInput{SiteID: "site", DistinctID: in.DistinctID})
			if err != nil || got != identity.MaybeHashDistinctID(in.DistinctID, salt, rawOptIn) {
				t.Fatal("legacy raw identity was treated as frozen")
			}
		})
	}
}

// Actual ApplyInbox/insert/read after policy flip; parent must run against the
// required native engine with fixture skips treated as unavailable acceptance.
func TestOBS79NativeFrozenReplayDoesNotDoubleHashOrNeedCurrentPolicy(t *testing.T) {
	dbStore, dbCheck, site := o01ErrorsFixture(t)
	defer dbStore.Close()
	defer dbCheck.Close()
	root := t.TempDir()
	b := o01DurableErrorBuffer(t, dbStore, root)
	calls := 0
	b.handler.WithPrivacyChecked(func(context.Context, string) (string, bool, bool, error) {
		calls++
		return "admission-salt", false, true, nil
	}, "global-fallback")
	in := ErrorInput{EventID: "privacy-r3-00001", DistinctID: "synthetic-person", ErrorType: "PrivacyR3", ErrorValue: "frozen"}
	if err := b.Push(site, in); err != nil {
		t.Fatal(err)
	}
	b.handler.WithPrivacyChecked(func(context.Context, string) (string, bool, bool, error) {
		t.Fatal("frozen apply consulted changed policy")
		return "", false, false, stderrors.New("cold outage")
	}, "global-fallback")
	if err := b.queue.Close(); err != nil {
		t.Fatal(err)
	}
	q := r3Queue(t, root)
	defer q.Close()
	b = NewErrorBuffer(b.handler, 100, 100, time.Hour, slog.Default())
	if err := b.AttachQueue(q); err != nil {
		t.Fatal(err)
	}
	if b.Stats().Pending != 0 || calls != 1 {
		t.Fatalf("frozen record stayed pending: %+v", b.Stats())
	}
	rows, err := nucleus.Query[struct {
		ID string `db:"distinct_id"`
	}](context.Background(), dbCheck.SQL(), "SELECT distinct_id FROM error_events WHERE site_id = $1", site)
	if err != nil || len(rows) != 1 || rows[0].ID != identity.HashDistinctID(in.DistinctID, "admission-salt") {
		t.Fatalf("stored frozen identity: %+v %v", rows, err)
	}
}
