package errors

import (
	"context"
	"log/slog"
	"time"
)

// Issue lifecycle events a notifier is told about.
const (
	// IssueEventNew is the first occurrence of a grouped error.
	IssueEventNew = "new_issue"
	// IssueEventRegression is a resolved (or snoozed) issue receiving a new
	// event and reopening.
	IssueEventRegression = "regression"
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
