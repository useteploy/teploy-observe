package errors

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

// Issue lifecycle events a notifier is told about.
const (
	// IssueEventNew is the first occurrence of a grouped error.
	IssueEventNew = "new_issue"
	// IssueEventRegression is a resolved (or snoozed) issue receiving a new
	// event and reopening.
	IssueEventRegression = "regression"
	// IssueEventSpike is a burst of events on one open issue (the detector
	// in spike.go; at most one per issue per hour). The OUTBOX kind this
	// maps onto is "issue_spike" - the internal name keeps the existing
	// pair's convention.
	IssueEventSpike = "issue_spike"
)

// IssueEvent describes one issue lifecycle transition worth telling a human
// about.
type IssueEvent struct {
	Kind       string
	SiteID     string
	IssueID    string
	Title      string
	Culprit    string
	Level      string
	Release    string
	EventCount int64
	At         time.Time
	// GroupHash is the issue's grouping fingerprint, so a receiver can
	// key notifications on the error identity rather than the issue id
	// (which changes on merge).
	GroupHash string
	// Environment is the TRIGGERING event's environment tag; it lives
	// only on error_events, so it is passed through from the event (EV1).
	Environment string
	// SampleStack is the triggering event's stack summarized to
	// file:line function lines, scrubbed, clipped to 2 KB (SummarizeStack).
	SampleStack string
	// WindowCount is the spike window's event count (IssueEventSpike only).
	WindowCount int64
}

// IssueNotifier receives issue lifecycle events. It is called inline on the
// ingest path, so an implementation MUST NOT block or do I/O: enqueue and
// return. This package deliberately knows nothing about who consumes the
// events (cmd/observe wires the integrations dispatcher in), and a panic in a
// notifier is recovered here so it can never fail ingest.
type IssueNotifier func(ctx context.Context, ev IssueEvent)

// SetNotifier installs (or, with nil, removes) the issue event notifier.
// Safe to call while ingest is running.
func (s *IssueService) SetNotifier(n IssueNotifier) {
	if n == nil {
		s.notifier.Store(nil)
		return
	}
	s.notifier.Store(&n)
}

func (s *IssueService) notify(ctx context.Context, ev IssueEvent) {
	p := s.notifier.Load()
	if p == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Error("errors: issue notifier panicked; event dropped, ingest unaffected",
				"kind", ev.Kind, "site", ev.SiteID, "issue", ev.IssueID, "panic", r)
		}
	}()
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	(*p)(ctx, ev)
}

// sampleStackMaxFrames caps how many frames a notification's sample stack
// carries: enough to localise a fault, not enough to mirror the payload.
const sampleStackMaxFrames = 10

// sampleStackScrub redacts the rendered lines. The buffer path scrubs frames
// at admission (ScrubInput), but IngestErrorEvent is also reachable from
// paths that do not (the demo seed, ops tooling), so the summary scrubs
// again - idempotent on already-scrubbed text, closed on the rest.
var sampleStackScrub = NewScrubberFromEnv()

// SummarizeStack renders a notification-safe stack summary from a slice of
// (post-scrub) frames: the top in-app frames as "file:line function" lines
// - locations and function names only, no local variables, breadcrumbs or
// request data - scrubbed and clipped to 2 KB (platform.IssueStackMaxBytes;
// kept at 2000 to also fit teploy-ship's DETAIL_STACK_MAX).
func SummarizeStack(frames []StackFrame) string {
	picked := filterInApp(frames)
	if len(picked) == 0 {
		picked = frames
	}
	if len(picked) > sampleStackMaxFrames {
		picked = picked[:sampleStackMaxFrames]
	}
	var b strings.Builder
	for _, f := range picked {
		loc := f.Filename
		if loc == "" {
			loc = "(unknown)"
		}
		if f.Lineno > 0 {
			loc += ":" + strconv.Itoa(f.Lineno)
		}
		line := loc
		if f.Function != "" {
			line += " " + f.Function
		}
		line = sampleStackScrub.scrubString(line)
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		// Byte budget: stop at whole lines rather than ship a partial frame.
		if b.Len()+len(line) > 2000 {
			b.WriteString("[clipped]")
			break
		}
		b.WriteString(line)
	}
	return b.String()
}
