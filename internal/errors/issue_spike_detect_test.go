package errors

// EV1 issue-spike DETECTOR tests (distinct from the spike-protection
// tests in spike_test.go: protection drops events, detection alerts).
// The detector is pure in-memory with an injectable clock, so these are
// deterministic without a database.

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

type fakeSpikeClock struct{ t time.Time }

func (c *fakeSpikeClock) now() time.Time { return c.t }
func (c *fakeSpikeClock) advance(d time.Duration) {
	c.t = c.t.Add(d)
}

func newTestDetector(min int64, factor float64) (*issueSpikeDetector, *fakeSpikeClock) {
	clk := &fakeSpikeClock{t: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)}
	d := newIssueSpikeDetector(min, factor)
	d.now = clk.now
	return d, clk
}

// A burst that crosses N_min fires exactly once; every further event of
// the same burst stays silent inside the one-hour gate.
func TestIssueSpikeDetector_BurstFiresOncePerHour(t *testing.T) {
	d, _ := newTestDetector(5, 4)
	fired, window := int(0), int64(0)
	for i := 0; i < 50; i++ {
		if f, w := d.observe("s", "i"); f {
			fired++
		} else {
			window = w
		}
	}
	if fired != 1 {
		t.Fatalf("fires = %d, want exactly 1 for a 50-event burst (min 5)", fired)
	}
	if window != 50 {
		t.Fatalf("window count = %d, want 50 (the detector keeps counting after firing)", window)
	}
}

// Below N_min nothing fires, however long the trickle runs.
func TestIssueSpikeDetector_QuietNeverFires(t *testing.T) {
	d, _ := newTestDetector(5, 4)
	for i := 0; i < 4; i++ {
		if f, _ := d.observe("s", "i"); f {
			t.Fatal("fired below N_min")
		}
	}
}

// The gate is one HOUR, not one window: after 5 minutes (a fresh bucket)
// the same burst still cannot re-fire.
func TestIssueSpikeDetector_GateIsAnHourNotAWindow(t *testing.T) {
	d, clk := newTestDetector(5, 4)
	for i := 0; i < 10; i++ {
		d.observe("s", "i")
	}
	clk.advance(6 * time.Minute)
	if f, _ := d.observe("s", "i"); f {
		t.Fatal("re-fired 6 minutes after the first spike - the gate is one hour")
	}
	// One hour after the fire, a fresh burst may fire again.
	clk.advance(54*time.Minute + time.Second)
	for i := 0; i < 4; i++ {
		d.observe("s", "i") // refill without crossing yet
	}
	if f, _ := d.observe("s", "i"); !f {
		t.Fatal("did not re-fire an hour after the previous spike despite a fresh crossing")
	}
}

// A steady background (a warm trail) raises the threshold: max(N_min,
// k x trailing per-5-minute mean) must beat a jump that is 3x the mean.
func TestIssueSpikeDetector_TrailingMeanRaisesThreshold(t *testing.T) {
	d, clk := newTestDetector(5, 4)
	// Quiet background: 4 events per 5-minute window for an hour. Each
	// window stays under N_min = 5, so the warm-up itself never fires.
	for w := 0; w < 12; w++ {
		for i := 0; i < 4; i++ {
			if f, _ := d.observe("s", "i"); f {
				t.Fatalf("fired during warm-up window %d", w)
			}
		}
		clk.advance(5 * time.Minute)
	}
	// Trail mean = 4, so the threshold is max(5, 4x4) = 16: a 12-event
	// jump (3x the mean, well above N_min) must not fire.
	for i := 0; i < 15; i++ {
		if f, _ := d.observe("s", "i"); f {
			t.Fatalf("fired at %d window events below k x trailing mean", i+1)
		}
	}
	// The 16th event of the window - 4x the mean - fires.
	if f, _ := d.observe("s", "i"); !f {
		t.Fatal("did not fire at k x trailing mean")
	}
}

// min = 0 disables the detector (the env-off posture); nil never fires.
func TestIssueSpikeDetector_DisabledAndNil(t *testing.T) {
	d, _ := newTestDetector(0, 4)
	if f, _ := d.observe("s", "i"); f {
		t.Fatal("min=0 must disable the detector")
	}
	var nilD *issueSpikeDetector
	if f, _ := nilD.observe("s", "i"); f {
		t.Fatal("nil detector fired")
	}
}

// Per-issue isolation: one issue's burst must not fire another's.
func TestIssueSpikeDetector_PerIssueIsolation(t *testing.T) {
	d, _ := newTestDetector(5, 4)
	for i := 0; i < 5; i++ {
		d.observe("s", "a")
	}
	if f, _ := d.observe("s", "b"); f {
		t.Fatal("issue b fired on issue a's burst")
	}
}

// --- Idle eviction (SF3, review round 2) ---

// The failure being fixed: the tracked map was capped at 50,000 and never
// evicted, so once full, spikes SILENTLY STOPPED for every new issue.
// Now a full map first reclaims entries idle over an hour, and the
// reclaim is counted for /healthz.
func TestIssueSpikeDetector_FullMapEvictsIdleAndTracksNewIssue(t *testing.T) {
	d, clk := newTestDetector(3, 4)
	d.maxTracked = 3
	// Fill the map: a, b, c each seen once, then idle for 90 minutes.
	for _, id := range []string{"a", "b", "c"} {
		d.observe("s", id)
	}
	clk.advance(90 * time.Minute)
	// A NEW issue arrives while the map is full of idle entries.
	fired := 0
	for i := 0; i < 3; i++ {
		if f, _ := d.observe("s", "new"); f {
			fired++
		}
	}
	if fired != 1 {
		t.Fatalf("new issue fired %d times at min=3, want 1 - a full map of idle entries must not silence new issues", fired)
	}
	st := d.stats()
	if st.Evicted != 3 {
		t.Fatalf("evictions = %d, want 3 (a, b, c reclaimed)", st.Evicted)
	}
	if st.Tracked != 1 || st.MaxTrack != 3 {
		t.Fatalf("stats = %+v, want tracked=1 max=3", st)
	}
}

// A map full of ACTIVE issues is honestly at capacity: the new issue is
// not tracked (and cannot fire), no eviction is claimed, and the counter
// says so - silence with evidence, not silence by surprise.
func TestIssueSpikeDetector_FullMapOfActiveIssuesDoesNotFireNewOnes(t *testing.T) {
	d, clk := newTestDetector(3, 4)
	d.maxTracked = 2
	d.observe("s", "a")
	clk.advance(time.Minute)
	d.observe("s", "b")
	// Both entries are recent (active): nothing is reclaimable.
	for i := 0; i < 10; i++ {
		if f, _ := d.observe("s", "c"); f {
			t.Fatal("untracked issue fired past a full map of active entries")
		}
	}
	st := d.stats()
	if st.Evicted != 0 {
		t.Fatalf("evictions = %d, want 0 (nothing was idle)", st.Evicted)
	}
	if st.Tracked != 2 {
		t.Fatalf("tracked = %d, want 2", st.Tracked)
	}
	// Once a and b go idle, c is admitted and can fire.
	clk.advance(61 * time.Minute)
	fired := 0
	for i := 0; i < 3; i++ {
		if f, _ := d.observe("s", "c"); f {
			fired++
		}
	}
	if fired != 1 {
		t.Fatalf("c fired %d times after eviction made room, want 1", fired)
	}
	if st := d.stats(); st.Evicted != 2 {
		t.Fatalf("evictions = %d, want 2", st.Evicted)
	}
}

// Eviction keeps its promise: an issue evicted while idle starts CLEAN
// when it returns (no stale refire gate, no stale trail suppressing it).
func TestIssueSpikeDetector_EvictedIssueReturnsClean(t *testing.T) {
	d, clk := newTestDetector(3, 4)
	d.maxTracked = 2
	// a fires (consuming its one-per-hour gate), c fills the map.
	for i := 0; i < 3; i++ {
		d.observe("s", "a")
	}
	d.observe("s", "c")
	clk.advance(30 * time.Minute) // inside a's refire gate
	if f, _ := d.observe("s", "a"); f {
		t.Fatal("re-fired inside the one-hour gate")
	}
	// a and c are both idle past the hour now; b's arrival evicts them.
	clk.advance(61 * time.Minute)
	d.observe("s", "b")
	if got := d.stats().Evicted; got != 2 {
		t.Fatalf("evictions = %d, want 2 (a and c reclaimed)", got)
	}
	clk.advance(2 * time.Minute)
	// a returns: its old gate and trail are gone, so a fresh burst fires.
	fired := 0
	for i := 0; i < 3; i++ {
		if f, _ := d.observe("s", "a"); f {
			fired++
		}
	}
	if fired != 1 {
		t.Fatalf("returning issue fired %d times, want 1 (a clean re-entry)", fired)
	}
}

// The stats snapshot is nil-safe on both the detector and the service.
func TestIssueSpikeDetector_StatsNilSafety(t *testing.T) {
	var nilD *issueSpikeDetector
	if st := nilD.stats(); st.Tracked != 0 || st.Evicted != 0 {
		t.Fatalf("nil detector stats = %+v", st)
	}
	s := &IssueService{}
	if st := s.SpikeDetectorStats(); st.Tracked != 0 || st.Evicted != 0 {
		t.Fatalf("service without detector stats = %+v", st)
	}
}

// spikeDetected drives the notifier on fire (the wiring to issue_spike).
func TestSpikeDetected_NotifiesOnce(t *testing.T) {
	s := &IssueService{spikeAlerts: newTestDetectorPtr(3, 4)}
	var got []IssueEvent
	s.SetNotifier(func(_ context.Context, ev IssueEvent) { got = append(got, ev) })
	stack := SummarizeStack([]StackFrame{{Filename: "app.go", Function: "boom", Lineno: 12, InApp: true}})
	lazy := func() string { return stack }
	for i := 0; i < 10; i++ {
		s.spikeDetected(context.Background(), "site", "iss", "gh", "Boom", "app.go", "error", "r1", "prod", lazy, int64(i+2))
	}
	if len(got) != 1 {
		t.Fatalf("issue_spike notifications = %d, want 1", len(got))
	}
	ev := got[0]
	if ev.Kind != IssueEventSpike || ev.IssueID != "iss" || ev.GroupHash != "gh" ||
		ev.Environment != "prod" || ev.SampleStack != stack || ev.WindowCount != 3 {
		t.Fatalf("event: %+v", ev)
	}
}

// The stack callback is LAZY: a burst that never fires must never pay for
// rendering one (SF5 - SummarizeStack was computed on every ingest event).
func TestSpikeDetected_DoesNotRenderStackWhenNotFiring(t *testing.T) {
	s := &IssueService{spikeAlerts: newTestDetectorPtr(1000, 4)}
	rendered := 0
	lazy := func() string { rendered++; return "app.go:1 boom" }
	for i := 0; i < 50; i++ {
		if s.spikeDetected(context.Background(), "site", "iss", "gh", "Boom", "app.go", "error", "r1", "prod", lazy, int64(i)) {
			t.Fatal("fired below the threshold")
		}
	}
	if rendered != 0 {
		t.Fatalf("stack rendered %d times without a firing, want 0", rendered)
	}
}

// A service with no detector (the &IssueService{} test posture) never
// fires and never panics.
func TestSpikeDetected_NoDetectorIsNoop(t *testing.T) {
	s := &IssueService{}
	s.SetNotifier(func(_ context.Context, ev IssueEvent) { t.Fatalf("unexpected %+v", ev) })
	if s.spikeDetected(context.Background(), "s", "i", "gh", "t", "c", "error", "r", "e", func() string { return "" }, 1) {
		t.Fatal("fired without a detector")
	}
}

func newTestDetectorPtr(min int64, factor float64) *issueSpikeDetector {
	d, _ := newTestDetector(min, factor)
	return d
}

// SummarizeStack: in-app frames preferred, scrubbed, whole-line 2 KB clip.
func TestSummarizeStack_ScrubsAndClips(t *testing.T) {
	frames := []StackFrame{
		{Filename: "webpack/bootstrap.js", Function: "module code", Lineno: 1},
		{Filename: "app/controller.ts", Function: "OrderController.create", Lineno: 42, InApp: true},
		{Filename: "app/util.ts", Function: "parseToken Authorization=Bearer abcdef123456", Lineno: 7, InApp: true},
	}
	got := SummarizeStack(frames)
	if got == "" {
		t.Fatal("empty summary")
	}
	// In-app frames only: the library frame must not lead.
	if !strings.Contains(got, "app/controller.ts:42 OrderController.create") {
		t.Fatalf("missing in-app frame: %q", got)
	}
	if strings.Contains(got, "bootstrap.js") {
		t.Fatalf("non-in-app frame leaked: %q", got)
	}
	if strings.Contains(got, "abcdef123456") {
		t.Fatalf("bearer token survived scrubbing: %q", got)
	}
	if !strings.Contains(got, Filtered) {
		t.Fatalf("scrubbed value marker absent: %q", got)
	}
}

// A stack whose whole lines alone exceed the budget is clipped at a line
// boundary and stays under 2 KB of valid UTF-8.
func TestSummarizeStack_TwoKBudget(t *testing.T) {
	var frames []StackFrame
	for i := 0; i < 12; i++ {
		frames = append(frames, StackFrame{
			Filename: "app/" + strings.Repeat("very-long-directory-name/", 8) + "file" + strconv.Itoa(i) + ".ts",
			Function: "namespacedService.methodCall.withArguments", Lineno: i, InApp: true,
		})
	}
	got := SummarizeStack(frames)
	if len(got) > 2000 {
		t.Fatalf("summary is %d bytes, want <= 2000", len(got))
	}
	if !strings.Contains(got, "[clipped]") {
		t.Fatalf("clip marker absent: %q", got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("clipped summary is not valid UTF-8")
	}
}
