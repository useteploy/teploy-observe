package errors

import (
	"context"
	"encoding/json"
	"testing"
)

func TestNotify_NoNotifierIsNoop(t *testing.T) {
	s := &IssueService{}
	s.notify(context.Background(), IssueEvent{Kind: IssueEventNew}) // must not panic
}

func TestNotify_DeliversEventWithTimestamp(t *testing.T) {
	s := &IssueService{}
	var got IssueEvent
	s.SetNotifier(func(_ context.Context, ev IssueEvent) { got = ev })
	s.notify(context.Background(), IssueEvent{Kind: IssueEventRegression, SiteID: "s", IssueID: "i"})
	if got.Kind != IssueEventRegression || got.IssueID != "i" || got.At.IsZero() {
		t.Fatalf("event: %+v", got)
	}
	s.SetNotifier(nil)
	got = IssueEvent{}
	s.notify(context.Background(), IssueEvent{Kind: IssueEventNew})
	if got.Kind != "" {
		t.Fatal("removed notifier still called")
	}
}

// A misbehaving notifier must never fail ingest.
func TestNotify_PanicIsContained(t *testing.T) {
	s := &IssueService{}
	s.SetNotifier(func(context.Context, IssueEvent) { panic("boom") })
	s.notify(context.Background(), IssueEvent{Kind: IssueEventNew})
}

// The cache entry stays backward compatible: entries written before the
// resolved flag existed decode as not-resolved, and an unset flag is not
// serialised.
func TestCachedIssue_ResolvedFlagCompat(t *testing.T) {
	var old cachedIssue
	if err := json.Unmarshal([]byte(`{"id":"i1","ec":4}`), &old); err != nil || old.Resolved {
		t.Fatalf("old entry: %+v %v", old, err)
	}
	raw, _ := json.Marshal(cachedIssue{IssueID: "i1", EventCount: 4})
	if string(raw) != `{"id":"i1","ec":4}` {
		t.Fatalf("unset flag serialised: %s", raw)
	}
	raw, _ = json.Marshal(cachedIssue{IssueID: "i1", EventCount: 4, Resolved: true})
	if string(raw) != `{"id":"i1","ec":4,"rs":true}` {
		t.Fatalf("flag: %s", raw)
	}
}
