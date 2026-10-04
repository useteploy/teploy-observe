package errors

import (
	"context"
	"sync"
	"testing"
)

// fakeLifecycle is an in-memory lifecycleStore.
type fakeLifecycle struct {
	mu      sync.Mutex
	cache   map[string]cachedIssue // site/hash
	issues  map[string]*Issue      // site/id
	byHash  map[string]string      // site/hash -> id
	bumped  []string
	counts  int // scopeCount calls
	created []string
}

func newFakeLifecycle() *fakeLifecycle {
	return &fakeLifecycle{cache: map[string]cachedIssue{}, issues: map[string]*Issue{}, byHash: map[string]string{}}
}

func (f *fakeLifecycle) add(site, id, hash, status string) {
	f.issues[site+"/"+id] = &Issue{IssueID: id, SiteID: site, GroupHash: hash, Status: status, Title: "T-" + id}
	f.byHash[site+"/"+hash] = id
}

func (f *fakeLifecycle) cacheGet(_ context.Context, site, hash string) (cachedIssue, bool) {
	ci, ok := f.cache[site+"/"+hash]
	return ci, ok
}
func (f *fakeLifecycle) cacheSet(_ context.Context, site, hash string, ci cachedIssue) {
	f.cache[site+"/"+hash] = ci
}
func (f *fakeLifecycle) copyOf(site, id string) *Issue {
	if is := f.issues[site+"/"+id]; is != nil {
		c := *is
		return &c
	}
	return nil
}
func (f *fakeLifecycle) findByHash(_ context.Context, site, hash string) (*Issue, error) {
	if id, ok := f.byHash[site+"/"+hash]; ok {
		return f.copyOf(site, id), nil
	}
	return nil, nil
}
func (f *fakeLifecycle) issueByID(_ context.Context, site, id string) (*Issue, error) {
	return f.copyOf(site, id), nil
}
func (f *fakeLifecycle) bump(_ context.Context, id, site string, _, _ int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bumped = append(f.bumped, id)
	if is := f.issues[site+"/"+id]; is != nil && is.Status == "resolved" {
		is.Status = "open"
	}
	return nil
}
func (f *fakeLifecycle) create(_ context.Context, id, site, hash, _, _, _, _ string, _ int64) error {
	f.created = append(f.created, id)
	f.add(site, id, hash, "open")
	return nil
}
func (f *fakeLifecycle) scopeCount(context.Context, string, []string) int64 {
	f.counts++
	return 10
}

func mergedSvc(t *testing.T, lc *fakeLifecycle, merges map[string]string) (*IssueService, *[]IssueEvent) {
	t.Helper()
	ms := newFakeStore(map[string][]string{"A": {"src", "tgt"}})
	ms.merges["A"] = merges
	s := &IssueService{merge: newMergeState(ms), store: lc}
	var evs []IssueEvent
	s.SetNotifier(func(_ context.Context, ev IssueEvent) { evs = append(evs, ev) })
	return s, &evs
}

// (a) event on a merged source whose TARGET is resolved: the target is
// reopened and a regression is notified for the target.
func TestMergedSourceResolvedTargetNotifiesTarget(t *testing.T) {
	lc := newFakeLifecycle()
	lc.add("A", "src", "hs", "open")
	lc.add("A", "tgt", "ht", "resolved")
	s, evs := mergedSvc(t, lc, map[string]string{"src": "tgt"})
	id, err := s.ResolveIssue(context.Background(), "A", "hs", "x", "c", "error", "r1", 1)
	if err != nil || id != "tgt" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	if len(*evs) != 1 || (*evs)[0].Kind != IssueEventRegression || (*evs)[0].IssueID != "tgt" {
		t.Fatalf("events: %+v", *evs)
	}
	if lc.issues["A/tgt"].Status != "open" {
		t.Fatal("target not reopened")
	}
	for _, b := range lc.bumped {
		if b == "src" {
			t.Fatal("hidden source must not be bumped")
		}
	}
}

// (b) the SOURCE itself is resolved: no notification for the hidden source id.
func TestMergedResolvedSourceNeverNotified(t *testing.T) {
	for _, cached := range []bool{false, true} {
		lc := newFakeLifecycle()
		lc.add("A", "src", "hs", "resolved")
		lc.add("A", "tgt", "ht", "open")
		if cached {
			lc.cache["A/hs"] = cachedIssue{IssueID: "src", EventCount: 3, Resolved: true}
		}
		s, evs := mergedSvc(t, lc, map[string]string{"src": "tgt"})
		id, _ := s.ResolveIssue(context.Background(), "A", "hs", "x", "c", "error", "", 1)
		if id != "tgt" {
			t.Fatalf("cached=%v id=%q", cached, id)
		}
		if len(*evs) != 0 {
			t.Fatalf("cached=%v: notification for open target/hidden source: %+v", cached, *evs)
		}
	}
}

// Unmerged resolved issue still notifies as before (both paths).
func TestUnmergedRegressionStillNotified(t *testing.T) {
	lc := newFakeLifecycle()
	lc.add("A", "tgt", "ht", "resolved")
	s, evs := mergedSvc(t, lc, nil)
	if id, _ := s.ResolveIssue(context.Background(), "A", "ht", "x", "c", "error", "", 1); id != "tgt" {
		t.Fatalf("id=%q", id)
	}
	if len(*evs) != 1 || (*evs)[0].IssueID != "tgt" {
		t.Fatalf("events: %+v", *evs)
	}
}

// (c)/(d) spike exemption is judged on the attribution target, and a
// snoozed issue (status resolved + snooze deadline) is a regression.
func TestSpikeExemptJudgedOnTargetAndSnoozed(t *testing.T) {
	ctx := context.Background()
	lc := newFakeLifecycle()
	lc.add("A", "src", "hs", "open")
	lc.add("A", "tgt", "ht", "resolved")
	lc.cache["A/hs"] = cachedIssue{IssueID: "src", EventCount: 1}
	s, _ := mergedSvc(t, lc, map[string]string{"src": "tgt"})
	if !s.spikeExempt(ctx, "A", "hs") {
		t.Fatal("regression of a merged target must be exempt from thinning")
	}
	lc.issues["A/tgt"].Status = "open"
	if s.spikeExempt(ctx, "A", "hs") {
		t.Fatal("open target is not exempt")
	}
	// Snoozed: resolved + snooze deadline, cache carries Resolved.
	lc.add("A", "sn", "hn", "resolved")
	lc.issues["A/sn"].SnoozeActive = true
	lc.cache["A/hn"] = cachedIssue{IssueID: "sn", EventCount: 1, Resolved: true}
	if !s.spikeExempt(ctx, "A", "hn") {
		t.Fatal("snoozed issue must be exempt from thinning")
	}
	var evs []IssueEvent
	s.SetNotifier(func(_ context.Context, ev IssueEvent) { evs = append(evs, ev) })
	if id, _ := s.ResolveIssue(ctx, "A", "hn", "x", "c", "error", "", 1); id != "sn" {
		t.Fatalf("id=%q", id)
	}
	if len(evs) != 1 || evs[0].Kind != IssueEventRegression || evs[0].IssueID != "sn" {
		t.Fatalf("snoozed reopen not notified: %+v", evs)
	}
}

// The merged-scope COUNT(*) is bounded to once per TTL, not per event.
func TestMergedCountIsCached(t *testing.T) {
	lc := newFakeLifecycle()
	lc.add("A", "src", "hs", "open")
	lc.add("A", "tgt", "ht", "open")
	s, _ := mergedSvc(t, lc, map[string]string{"src": "tgt"})
	for i := 0; i < 50; i++ {
		s.ResolveIssue(context.Background(), "A", "hs", "x", "c", "error", "", int64(i))
	}
	if lc.counts != 1 {
		t.Fatalf("scope COUNT ran %d times for 50 events, want 1", lc.counts)
	}
}
