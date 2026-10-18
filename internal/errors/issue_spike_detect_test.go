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

// spikeDetected drives the notifier on fire (the wiring to issue_spike).
func TestSpikeDetected_NotifiesOnce(t *testing.T) {
	s := &IssueService{spikeAlerts: newTestDetectorPtr(3, 4)}
	var got []IssueEvent
	s.SetNotifier(func(_ context.Context, ev IssueEvent) { got = append(got, ev) })
	stack := SummarizeStack([]StackFrame{{Filename: "app.go", Function: "boom", Lineno: 12, InApp: true}})
	for i := 0; i < 10; i++ {
		s.spikeDetected(context.Background(), "site", "iss", "gh", "Boom", "app.go", "error", "r1", "prod", stack, int64(i+2))
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

// A service with no detector (the &IssueService{} test posture) never
// fires and never panics.
func TestSpikeDetected_NoDetectorIsNoop(t *testing.T) {
	s := &IssueService{}
	s.SetNotifier(func(_ context.Context, ev IssueEvent) { t.Fatalf("unexpected %+v", ev) })
	if s.spikeDetected(context.Background(), "s", "i", "gh", "t", "c", "error", "r", "e", "", 1) {
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
