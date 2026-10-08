package errors

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"github.com/useteploy/teploy-observe/internal/ingest"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestOBS87NestedJSONStringFrozenWithoutSecrets(t *testing.T) {
	for _, key := range []string{"password", "authorization", "api_key"} {
		for depth := 0; depth < 7; depth++ {
			v := fmt.Sprintf(`{"%s":"FAKE_ONLY_SECRET"}`, key)
			for i := 0; i < depth; i++ {
				raw, _ := json.Marshal(map[string]string{"body": v})
				v = string(raw)
			}
			b := NewErrorBuffer(nil, 10, 100, time.Hour, slog.Default())
			if err := b.Push("site", ErrorInput{ErrorType: "T", ErrorValue: v}); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(b.events[0].Body), "FAKE_ONLY_SECRET") {
				t.Fatalf("key=%s depth=%d retained secret", key, depth)
			}
		}
	}
}
func TestOBS84FailedReplayRefusesOverBudgetWithoutInstallingTail(t *testing.T) {
	q, err := ingest.NewDiskQueue(t.TempDir(), "errors", time.Hour, 1<<20, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	body, _ := json.Marshal(ErrorInput{ErrorType: "T", ErrorValue: strings.Repeat("x", 1024)})
	for i := 0; i < 20; i++ {
		raw, _ := json.Marshal(errorRecord{SiteID: "site", Body: body})
		if _, err := q.AppendRecords([]json.RawMessage{raw}); err != nil {
			t.Fatal(err)
		}
	}
	b := NewErrorBuffer(nil, 10, 100, time.Hour, slog.Default())
	b.maxBytes = 4096
	attempts := 0
	b.apply = func(context.Context, bufferedError) bool { attempts++; return false }
	if err := b.AttachQueue(q); err == nil {
		t.Fatal("over-budget replay succeeded")
	}
	if attempts > 5 || b.usedBytes != 0 || len(b.events) != 0 || b.queue != nil {
		t.Fatalf("unbounded or partially staged replay: attempts=%d bytes=%d events=%d", attempts, b.usedBytes, len(b.events))
	}
	// Every frame stays on disk; attachment can succeed once storage recovers.
	frames := 0
	if err := q.StreamRecordFrames(func(r []json.RawMessage, _ int64) error { frames += len(r); return nil }); err != nil {
		t.Fatal(err)
	}
	if frames != 20 {
		t.Fatalf("lost WAL frames: %d", frames)
	}
	b.apply = func(context.Context, bufferedError) bool { return true }
	if err := b.AttachQueue(q); err != nil {
		t.Fatal(err)
	}
}
func TestOBS85WideMergeScopeComplete(t *testing.T) {
	lc := newFakeLifecycle()
	m := map[string]string{}
	for i := 0; i < 60; i++ {
		m[fmt.Sprintf("source-%02d", i)] = "tgt"
	}
	s, _ := mergedSvc(t, lc, m)
	ids := s.issueScope(context.Background(), "A", "tgt")
	if len(ids) != 61 {
		t.Fatalf("scope lost sources: %d", len(ids))
	}
}

type failedLifecycle struct{ *fakeLifecycle }

func (f failedLifecycle) bump(context.Context, string, string, int64, int64) error {
	return stderrors.New("isolated lifecycle failure")
}
func TestOBS88BumpFailureDoesNotReopenNotifyOrAcknowledge(t *testing.T) {
	for _, branch := range []string{"cache", "existing", "merged-cache", "merged-existing"} {
		t.Run(branch, func(t *testing.T) {
			lc := newFakeLifecycle()
			lc.add("A", "src", "hash", "resolved")
			lc.add("A", "tgt", "targethash", "resolved")
			merges := map[string]string{}
			if strings.HasPrefix(branch, "merged") {
				merges["src"] = "tgt"
			}
			s, evs := mergedSvc(t, lc, merges)
			s.store = failedLifecycle{lc}
			if strings.HasSuffix(branch, "cache") {
				lc.cacheSet(context.Background(), "A", "hash", cachedIssue{IssueID: "src", Resolved: true, EventCount: 1})
			}
			id, err := s.ResolveIssue(context.Background(), "A", "hash", "title", "culprit", "error", "release", 1000)
			if err == nil || id != "" || len(*evs) != 0 || lc.issues["A/src"].Status != "resolved" || lc.issues["A/tgt"].Status != "resolved" {
				t.Fatalf("failed lifecycle acknowledged/notified: id=%s err=%v events=%v", id, err, *evs)
			}
		})
	}
}
