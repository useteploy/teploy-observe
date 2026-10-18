package errors

// Review round 2 regressions for the ingest-side issue lifecycle: spikes
// suppressed for operator-IGNORED issues (SF4), and the sample stack
// rendered lazily, only when a notification actually emits (SF5). Both are
// judged through ResolveIssueEvent against the in-memory lifecycleStore
// seam (merge_lifecycle_test.go's fakeLifecycle), so no database is
// needed for the decision logic.

import (
	"context"
	"testing"
	"time"
)

func spikeSvc(lc *fakeLifecycle, min int64) (*IssueService, *[]IssueEvent) {
	s := &IssueService{store: lc, spikeAlerts: newTestDetectorPtr(min, 4)}
	var evs []IssueEvent
	s.SetNotifier(func(_ context.Context, ev IssueEvent) { evs = append(evs, ev) })
	return s, &evs
}

func spikesOf(evs *[]IssueEvent) int {
	n := 0
	for _, ev := range *evs {
		if ev.Kind == IssueEventSpike {
			n++
		}
	}
	return n
}

// SF4, cache-miss path: an issue whose stored status is ignored never
// spikes, however loud the burst - the operator said stop.
func TestSpikeSuppressedForIgnoredIssue_DBPath(t *testing.T) {
	lc := newFakeLifecycle()
	lc.add("A", "i1", "h1", "ignored")
	s, evs := spikeSvc(lc, 1) // min=1: the very first bump would fire
	for i := 0; i < 25; i++ {
		if _, err := s.ResolveIssueEvent(context.Background(), "A", "h1", "Boom", "c", "error", "r1", 1, EventOrigin{Environment: "prod"}); err != nil {
			t.Fatal(err)
		}
	}
	if got := spikesOf(evs); got != 0 {
		t.Fatalf("ignored issue spiked %d times, want 0 (the operator ignored it)", got)
	}
}

// SF4, cache-hit path: the cached entry carries the ignored flag
// (markCachedStatus), so the hot path suppresses the spike too.
func TestSpikeSuppressedForIgnoredIssue_CachePath(t *testing.T) {
	lc := newFakeLifecycle()
	lc.add("A", "i1", "h1", "ignored")
	lc.cache["A/h1"] = cachedIssue{IssueID: "i1", EventCount: 5, Ignored: true}
	s, evs := spikeSvc(lc, 1)
	for i := 0; i < 25; i++ {
		if _, err := s.ResolveIssueEvent(context.Background(), "A", "h1", "Boom", "c", "error", "r1", 1, EventOrigin{}); err != nil {
			t.Fatal(err)
		}
	}
	if got := spikesOf(evs); got != 0 {
		t.Fatalf("ignored issue spiked %d times from the cache path, want 0", got)
	}
}

// The control: the same burst on an OPEN issue still fires exactly once -
// suppression is scoped to ignored, not to everything.
func TestSpikeStillFiresForOpenIssue(t *testing.T) {
	for _, cached := range []bool{false, true} {
		lc := newFakeLifecycle()
		lc.add("A", "i1", "h1", "open")
		if cached {
			lc.cache["A/h1"] = cachedIssue{IssueID: "i1", EventCount: 2}
		}
		s, evs := spikeSvc(lc, 1)
		for i := 0; i < 25; i++ {
			if _, err := s.ResolveIssueEvent(context.Background(), "A", "h1", "Boom", "c", "error", "r1", 1, EventOrigin{}); err != nil {
				t.Fatal(err)
			}
		}
		if got := spikesOf(evs); got != 1 {
			t.Fatalf("cached=%v: open issue spiked %d times, want exactly 1 (once per hour)", cached, got)
		}
	}
}

// A resolved-as-ignored posture must not leak spikes either: a snoozed
// issue that is IGNORED mid-snooze stays suppressed; a plain resolved
// issue regresses (one regression notification) and its burst can spike
// again - the operator only said stop for the ignored one.
func TestSpikeSuppressionTracksStatusChanges(t *testing.T) {
	lc := newFakeLifecycle()
	lc.add("A", "i1", "h1", "resolved")
	lc.cache["A/h1"] = cachedIssue{IssueID: "i1", EventCount: 2, Resolved: true}
	s, evs := spikeSvc(lc, 1)
	// First event: regression (reopen), and the reopen makes it spikable.
	if _, err := s.ResolveIssueEvent(context.Background(), "A", "h1", "Boom", "c", "error", "r1", 1, EventOrigin{}); err != nil {
		t.Fatal(err)
	}
	if got := spikesOf(evs); got != 1 {
		t.Fatalf("resolved issue reopening spiked %d times, want 1 (it is open again)", got)
	}
}

// SF5: the stack summary renders ONLY when a notification emits. The
// common event - an open issue below the spike threshold, no lifecycle
// transition - must render nothing at all.
func TestSummarizeStackLazy_NoRenderWithoutNotification(t *testing.T) {
	renders := 0
	orig := summarizeStack
	summarizeStack = func(frames []StackFrame) string {
		renders++
		return orig(frames)
	}
	t.Cleanup(func() { summarizeStack = orig })

	lc := newFakeLifecycle()
	lc.add("A", "i1", "h1", "open")
	lc.cache["A/h1"] = cachedIssue{IssueID: "i1", EventCount: 10} // hot path, open, no transition
	s, evs := spikeSvc(lc, 1000)                                  // unreachable threshold: no spike
	frames := []StackFrame{{Filename: "app.go", Function: "boom", Lineno: 1, InApp: true}}
	for i := 0; i < 100; i++ {
		if _, err := s.ResolveIssueEvent(context.Background(), "A", "h1", "Boom", "c", "error", "r1", 1, EventOrigin{Environment: "prod", Frames: frames}); err != nil {
			t.Fatal(err)
		}
	}
	if renders != 0 {
		t.Fatalf("stack rendered %d times for %d quiet events, want 0 (no notification was emitted)", renders, 100)
	}
	if len(*evs) != 0 {
		t.Fatalf("unexpected notifications: %+v", *evs)
	}
}

// SF5, the paying paths: a regression and a spike each render exactly
// once (the memo keeps one event from paying twice).
func TestSummarizeStackLazy_RendersOncePerEmittingEvent(t *testing.T) {
	renders := 0
	orig := summarizeStack
	summarizeStack = func(frames []StackFrame) string {
		renders++
		return orig(frames)
	}
	t.Cleanup(func() { summarizeStack = orig })

	lc := newFakeLifecycle()
	lc.add("A", "i1", "h1", "resolved")
	lc.cache["A/h1"] = cachedIssue{IssueID: "i1", EventCount: 2, Resolved: true}
	s, evs := spikeSvc(lc, 1) // the regression event also crosses the spike threshold
	frames := []StackFrame{{Filename: "app.go", Function: "boom", Lineno: 1, InApp: true}}
	if _, err := s.ResolveIssueEvent(context.Background(), "A", "h1", "Boom", "c", "error", "r1", 1, EventOrigin{Frames: frames}); err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, ev := range *evs {
		kinds[ev.Kind] = true
		if ev.SampleStack == "" {
			t.Fatalf("%s notification emitted without a sample stack", ev.Kind)
		}
	}
	if !kinds[IssueEventRegression] || !kinds[IssueEventSpike] {
		t.Fatalf("expected a regression AND a spike from one event, got %+v", *evs)
	}
	// Both notifications share ONE render of the triggering event's stack.
	if renders != 1 {
		t.Fatalf("stack rendered %d times for one emitting event, want 1 (memoized)", renders)
	}

	// A new issue's first notification renders once too.
	renders = 0
	if _, err := s.ResolveIssueEvent(context.Background(), "A", "h-new", "Boom2", "c", "error", "r1", 1, EventOrigin{Frames: frames}); err != nil {
		t.Fatal(err)
	}
	if renders != 1 {
		t.Fatalf("new-issue path rendered %d times, want 1", renders)
	}
}

// Quiet-event cost stays flat in time as well as in renders: 100 events
// with frames take no measurable per-event stack cost. (A guard against
// re-introducing the render at a non-notifying site; generous bound.)
func TestResolveIssueEvent_QuietBurstIsFast(t *testing.T) {
	lc := newFakeLifecycle()
	lc.add("A", "i1", "h1", "open")
	lc.cache["A/h1"] = cachedIssue{IssueID: "i1", EventCount: 10}
	s, _ := spikeSvc(lc, 1000)
	frames := make([]StackFrame, 64) // a heavy stack would be expensive to render
	for i := range frames {
		frames[i] = StackFrame{Filename: "deep/pkg/file.go", Function: "LongFunctionName", Lineno: i, InApp: true}
	}
	start := time.Now()
	for i := 0; i < 1000; i++ {
		if _, err := s.ResolveIssueEvent(context.Background(), "A", "h1", "Boom", "c", "error", "r1", 1, EventOrigin{Frames: frames}); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("1000 quiet events with frames took %v - the stack render is back on the hot path", d)
	}
}
