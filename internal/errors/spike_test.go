package errors

import (
	"context"
	"testing"
	"time"
)

func TestSpikeIssueCapSamplesOneInN(t *testing.T) {
	l := NewSpikeLimiter(10, 0, 5)
	kept := 0
	for i := 0; i < 110; i++ {
		if !l.spikeCheck("s", "h", nil) {
			kept++
		}
	}
	// 10 under the cap + 100 over at 1/5 = 20 kept.
	if kept != 30 {
		t.Fatalf("kept=%d want 30", kept)
	}
	if got := l.DroppedIssue.Load(); got != 80 {
		t.Fatalf("DroppedIssue=%d want 80", got)
	}
	if l.DroppedSite.Load() != 0 {
		t.Fatal("site counter must stay 0")
	}
}

func TestSpikeSiteCapLabeled(t *testing.T) {
	l := NewSpikeLimiter(0, 5, 1000)
	for i := 0; i < 20; i++ {
		l.spikeCheck("s", "h"+string(rune('a'+i)), nil)
	}
	if l.DroppedSite.Load() == 0 || l.DroppedIssue.Load() != 0 {
		t.Fatalf("site=%d issue=%d", l.DroppedSite.Load(), l.DroppedIssue.Load())
	}
}

func TestSpikeExemptNewIssueAndRegression(t *testing.T) {
	l := NewSpikeLimiter(1, 1, 1000)
	l.spikeCheck("s", "h", nil)
	l.spikeCheck("s", "h", nil)
	if l.spikeCheck("s", "h", func() bool { return true }) {
		t.Fatal("exempt event (new issue/regression) must never drop")
	}
	if !l.spikeCheck("s", "h", func() bool { return false }) {
		t.Fatal("non-exempt over-cap event should drop")
	}
}

func TestSpikeWindowResetsAndSitesIsolated(t *testing.T) {
	l := NewSpikeLimiter(2, 0, 1000)
	now := time.Unix(1000, 0)
	l.now = func() time.Time { return now }
	for i := 0; i < 5; i++ {
		l.spikeCheck("a", "h", nil)
	}
	if l.spikeCheck("b", "h", nil) {
		t.Fatal("other site must not be limited by site a")
	}
	now = now.Add(2 * time.Minute)
	if l.spikeCheck("a", "h", nil) {
		t.Fatal("new window must reset counters")
	}
}

func TestSpikeDisabledAndNil(t *testing.T) {
	var l *SpikeLimiter
	if l.spikeCheck("s", "h", nil) {
		t.Fatal("nil limiter must keep")
	}
	t.Setenv("OBSERVE_ERROR_SPIKE_DISABLE", "true")
	t.Setenv("OBSERVE_ERROR_SPIKE_ISSUE_PER_MIN", "1")
	d := NewSpikeLimiterFromEnv()
	for i := 0; i < 10; i++ {
		if d.spikeCheck("s", "h", nil) {
			t.Fatal("disabled limiter dropped")
		}
	}
}

func TestSpikeEnvDefaults(t *testing.T) {
	l := NewSpikeLimiterFromEnv()
	if l.issueCap != DefaultSpikeIssuePerMin || l.siteCap != DefaultSpikeSitePerMin || l.sampleN != DefaultSpikeSample {
		t.Fatalf("defaults wrong: %+v", l)
	}
	t.Setenv("OBSERVE_ERROR_SPIKE_SAMPLE", "bogus")
	if NewSpikeLimiterFromEnv().sampleN != DefaultSpikeSample {
		t.Fatal("bad env must fall back")
	}
}

func TestSpikeExemptFailsOpenWithoutDB(t *testing.T) {
	var s *IssueService
	if !s.spikeExempt(context.Background(), "s", "h") {
		t.Fatal("unknown must be exempt")
	}
}
