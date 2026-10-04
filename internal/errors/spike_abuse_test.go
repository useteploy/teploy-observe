package errors

import (
	"fmt"
	"testing"
	"time"
)

// An attacker varying the fingerprint on every event makes each event a
// "new issue"; the per-site new-issue cap must bound the admissions.
func TestSpikeAttackerVariesFingerprint(t *testing.T) {
	l := NewSpikeLimiter(1000, 10000, 10)
	l.newIssueCap = 50
	allNew := func() bool { return true }
	kept := 0
	for i := 0; i < 5000; i++ {
		if !l.spikeCheck("s", fmt.Sprintf("fp-%d", i), allNew) {
			kept++
		}
	}
	if kept != 50 {
		t.Fatalf("kept=%d want exactly the new-issue cap 50", kept)
	}
	if l.DroppedNewIssue.Load() != 4950 || l.AdmittedNewIssue.Load() != 50 {
		t.Fatalf("dropped=%d admitted=%d", l.DroppedNewIssue.Load(), l.AdmittedNewIssue.Load())
	}
	// A refused fingerprint stays refused: follow-up events cannot create it.
	if !l.spikeCheck("s", "fp-4999", allNew) {
		t.Fatal("denied fingerprint must stay dropped for the window")
	}
	// Another site is untouched.
	if l.spikeCheck("other", "x", allNew) {
		t.Fatal("other site must not be limited")
	}
	// Known (non-exempt) steady issues are never charged to the cap.
	if l.spikeCheck("s", "steady", func() bool { return false }) {
		t.Fatal("steady issue dropped")
	}
}

func TestSpikeNewIssueCapResetsAndEnv(t *testing.T) {
	l := NewSpikeLimiter(0, 0, 10)
	l.newIssueCap = 2
	now := time.Unix(5000, 0)
	l.now = func() time.Time { return now }
	yes := func() bool { return true }
	l.spikeCheck("s", "a", yes)
	l.spikeCheck("s", "b", yes)
	if !l.spikeCheck("s", "c", yes) {
		t.Fatal("third admission must drop")
	}
	now = now.Add(2 * time.Minute)
	if l.spikeCheck("s", "c", yes) {
		t.Fatal("new window must reset")
	}
	t.Setenv("OBSERVE_ERROR_SPIKE_NEW_ISSUE_CAP", "7")
	if NewSpikeLimiterFromEnv().newIssueCap != 7 {
		t.Fatal("env not read")
	}
}

// One flood issue must not consume the site budget meant for steady issues.
func TestSpikeSiteCapFairness(t *testing.T) {
	l := NewSpikeLimiter(0, 100, 1000)
	l.newIssueCap = 0
	steadyDropped := 0
	for round := 0; round < 5; round++ {
		for i := 0; i < 10; i++ {
			if l.spikeCheck("s", fmt.Sprintf("steady-%d", i), nil) {
				steadyDropped++
			}
		}
		for j := 0; j < 24; j++ {
			l.spikeCheck("s", "flood", nil)
		}
	}
	if steadyDropped != 0 {
		t.Fatalf("steady issues thinned (%d) while within fair share", steadyDropped)
	}
	if l.DroppedSite.Load() == 0 {
		t.Fatal("flood should have been thinned")
	}
}
