package integrations

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Issue event kinds the dispatcher accepts (mirrors internal/errors; kept as
// plain strings so this package does not import the errors package).
const (
	IssueKindNew        = "new_issue"
	IssueKindRegression = "regression"
)

// Dispatch bounds. Delivery of issue events is advisory and must never push
// back on error ingest, so everything here is bounded and lossy-with-a-counter
// rather than blocking.
const (
	// IssueNotifyQueueSize bounds events waiting for a worker. A full queue
	// drops the OLDEST event (loudly, counted) so the newest issue is the one
	// a human hears about.
	IssueNotifyQueueSize = 256
	// IssueNotifyWorkers is the number of concurrent delivery workers.
	IssueNotifyWorkers = 4
	// IssueNotifyCooldown is the minimum gap between notifications for the
	// SAME issue to the SAME integration, whatever the event kind. It is what
	// stops a flapping issue (resolve, regress, resolve, regress) from
	// spamming a channel.
	IssueNotifyCooldown = 15 * time.Minute
	// IssueNotifyDeliveryTimeout bounds one integration delivery end to end
	// (the HTTP client and SMTP dialer carry their own shorter timeouts; this
	// is the backstop that frees the worker regardless).
	IssueNotifyDeliveryTimeout = 30 * time.Second

	issueNotifyListTimeout   = 10 * time.Second
	issueNotifyCooldownCap   = 10000
	issueNotifyMaxTitleChars = 200
	issueNotifyMaxBodyChars  = 1000
)

// IssueEvent is an issue lifecycle event to deliver to a site's integrations.
type IssueEvent struct {
	Kind       string
	SiteID     string
	IssueID    string
	Title      string
	Culprit    string
	Level      string
	Release    string
	EventCount int64
}

// IssueDispatcherStats is the counter block for /healthz.
type IssueDispatcherStats struct {
	Enqueued        int64 `json:"enqueued"`
	Pending         int   `json:"pending"`
	Delivered       int64 `json:"delivered"`
	Failed          int64 `json:"failed"`
	TimedOut        int64 `json:"timed_out"`
	DroppedOverflow int64 `json:"dropped_overflow"`
	Suppressed      int64 `json:"suppressed_cooldown"`
	ListErrors      int64 `json:"list_errors"`
}

// IssueDispatcher delivers new-issue and regression events to a site's
// integrations on a bounded in-memory queue with a bounded worker set.
//
// Why in memory rather than the notification outbox: that outbox is a durable
// table keyed on alert rules and webhook targets, so reusing it would need a
// new intent kind, a schema change and edits to internal/platform. Issue
// events are advisory, the issue itself is already durable, and each attempt
// is still recorded in integration_deliveries. The trade-off is that events
// queued at a crash are lost; the counters make overflow visible.
type IssueDispatcher struct {
	list    func(ctx context.Context, siteID string) ([]Integration, error)
	deliver func(ctx context.Context, i Integration, p AlertPayload) error
	logger  *slog.Logger
	baseURL string
	now     func() time.Time
	// deliverTimeout bounds one delivery; tests shorten it.
	deliverTimeout time.Duration

	mu      sync.Mutex
	q       []IssueEvent
	stopped bool
	cool    map[string]time.Time
	sem     chan struct{}
	wg      sync.WaitGroup

	enqueued, delivered, failed, timedOut atomic.Int64
	dropped, suppressed, listErrors       atomic.Int64
}

// NewIssueDispatcher builds a dispatcher backed by this service. publicURL
// (may be empty) is the instance base URL used for the link in a payload.
func (s *IntegrationService) NewIssueDispatcher(publicURL string) *IssueDispatcher {
	return newIssueDispatcher(s.List, func(ctx context.Context, i Integration, p AlertPayload) error {
		return s.fireAndRecord(ctx, i, p, false, false)
	}, s.logger, publicURL)
}

func newIssueDispatcher(
	list func(ctx context.Context, siteID string) ([]Integration, error),
	deliver func(ctx context.Context, i Integration, p AlertPayload) error,
	logger *slog.Logger, publicURL string,
) *IssueDispatcher {
	if logger == nil {
		logger = slog.Default()
	}
	return &IssueDispatcher{
		list: list, deliver: deliver, logger: logger,
		baseURL:        strings.TrimRight(publicURL, "/"),
		now:            time.Now,
		deliverTimeout: IssueNotifyDeliveryTimeout,
		cool:           make(map[string]time.Time),
		sem:            make(chan struct{}, IssueNotifyWorkers),
	}
}

// Notify enqueues an event. It never blocks and never fails the caller.
func (d *IssueDispatcher) Notify(ev IssueEvent) {
	if ev.SiteID == "" || ev.IssueID == "" {
		return
	}
	d.mu.Lock()
	if d.stopped {
		d.mu.Unlock()
		return
	}
	if len(d.q) >= IssueNotifyQueueSize {
		old := d.q[0]
		d.q = d.q[1:]
		d.dropped.Add(1)
		d.logger.Error("issue notification queue overflow - dropped oldest queued event",
			"kind", old.Kind, "site", old.SiteID, "issue", old.IssueID)
	}
	d.q = append(d.q, ev)
	d.enqueued.Add(1)
	// The worker slot is taken (and wg incremented) under the lock so
	// Shutdown cannot miss a worker, and released under the lock by the
	// worker that finds the queue empty so an event is never stranded
	// between "queue empty" and "slot released".
	select {
	case d.sem <- struct{}{}:
		d.wg.Add(1)
		go d.worker()
	default:
		// Enough workers are already draining; one will pick this up.
	}
	d.mu.Unlock()
}

func (d *IssueDispatcher) worker() {
	defer d.wg.Done()
	for {
		d.mu.Lock()
		if len(d.q) == 0 {
			<-d.sem
			d.mu.Unlock()
			return
		}
		ev := d.q[0]
		d.q = d.q[1:]
		d.mu.Unlock()
		d.process(ev)
	}
}

func (d *IssueDispatcher) process(ev IssueEvent) {
	lctx, cancel := context.WithTimeout(context.Background(), issueNotifyListTimeout)
	intgs, err := d.list(lctx, ev.SiteID)
	cancel()
	if err != nil {
		d.listErrors.Add(1)
		d.logger.Warn("issue notification: listing integrations failed",
			"site", ev.SiteID, "issue", ev.IssueID, "err", err)
		return
	}
	payload := d.payload(ev)
	for _, intg := range intgs {
		if !d.claim(intg.IntegrationID, ev.IssueID) {
			d.suppressed.Add(1)
			continue
		}
		d.deliverOne(intg, payload)
	}
}

// claim applies the per-(integration, issue) cooldown. It claims at decision
// time, so a failing integration is also rate limited rather than retried on
// every flap.
func (d *IssueDispatcher) claim(integrationID, issueID string) bool {
	key := integrationID + "|" + issueID
	now := d.now()
	d.mu.Lock()
	defer d.mu.Unlock()
	if last, ok := d.cool[key]; ok && now.Sub(last) < IssueNotifyCooldown {
		return false
	}
	if len(d.cool) >= issueNotifyCooldownCap {
		for k, t := range d.cool {
			if now.Sub(t) >= IssueNotifyCooldown {
				delete(d.cool, k)
			}
		}
		if len(d.cool) >= issueNotifyCooldownCap {
			// Everything is still hot: forget the entries closest to expiry
			// rather than grow without bound. Worst case an old issue may
			// notify again early.
			for k := range d.cool {
				delete(d.cool, k)
				if len(d.cool) < issueNotifyCooldownCap/2 {
					break
				}
			}
		}
	}
	d.cool[key] = now
	return true
}

func (d *IssueDispatcher) deliverOne(i Integration, p AlertPayload) {
	ctx, cancel := context.WithTimeout(context.Background(), d.deliverTimeout)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("panic during delivery: %v", r)
			}
		}()
		done <- d.deliver(ctx, i, p)
	}()
	select {
	case err := <-done:
		if err != nil {
			// The error text is already redacted and recorded by the
			// delivery path; keep this line free of config and URLs.
			d.failed.Add(1)
			d.logger.Warn("issue notification delivery failed",
				"integration", i.IntegrationID, "type", i.IntType, "issue_event", p.RuleName)
			return
		}
		d.delivered.Add(1)
	case <-ctx.Done():
		d.timedOut.Add(1)
		d.logger.Warn("issue notification delivery timed out",
			"integration", i.IntegrationID, "type", i.IntType, "issue_event", p.RuleName)
	}
}

// payload renders an event as the AlertPayload every integration type
// already understands.
func (d *IssueDispatcher) payload(ev IssueEvent) AlertPayload {
	label := "New issue"
	if ev.Kind == IssueKindRegression {
		label = "Regression"
	}
	title := clean(ev.Title, issueNotifyMaxTitleChars)
	if title == "" {
		title = "Error"
	}
	var msg strings.Builder
	if ev.Kind == IssueKindRegression {
		msg.WriteString("A resolved issue received a new event and was reopened.")
	} else {
		msg.WriteString("First occurrence of a new error.")
	}
	if c := clean(ev.Culprit, issueNotifyMaxBodyChars); c != "" {
		msg.WriteString("\nCulprit: " + c)
	}
	if ev.Release != "" {
		msg.WriteString("\nRelease: " + clean(ev.Release, 100))
	}
	if ev.Level != "" {
		msg.WriteString("\nLevel: " + clean(ev.Level, 30))
	}
	link := ""
	if d.baseURL != "" {
		link = d.baseURL + "/errors"
	}
	return AlertPayload{
		Title:    label + ": " + title,
		Message:  msg.String(),
		Severity: severityFor(ev.Level),
		SiteID:   ev.SiteID,
		RuleName: ev.Kind,
		Metric:   "error_events",
		Value:    strconv.FormatInt(ev.EventCount, 10),
		URL:      link,
	}
}

func severityFor(level string) string {
	switch strings.ToLower(level) {
	case "fatal":
		return "critical"
	case "warning", "warn":
		return "warning"
	case "info", "debug":
		return "info"
	default:
		return "error"
	}
}

// clean makes attacker-influenced error text safe for headers and chat bodies:
// control characters (CR/LF would otherwise reach an SMTP Subject) become
// spaces, and the length is capped.
func clean(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > max {
		s = string(r[:max]) + "..."
	}
	return s
}

// Stats returns the dispatcher counters.
func (d *IssueDispatcher) Stats() IssueDispatcherStats {
	d.mu.Lock()
	pending := len(d.q)
	d.mu.Unlock()
	return IssueDispatcherStats{
		Enqueued:        d.enqueued.Load(),
		Pending:         pending,
		Delivered:       d.delivered.Load(),
		Failed:          d.failed.Load(),
		TimedOut:        d.timedOut.Load(),
		DroppedOverflow: d.dropped.Load(),
		Suppressed:      d.suppressed.Load(),
		ListErrors:      d.listErrors.Load(),
	}
}

// Shutdown stops admission and waits for in-flight and queued deliveries
// (each bounded by IssueNotifyDeliveryTimeout), or until ctx ends.
func (d *IssueDispatcher) Shutdown(ctx context.Context) error {
	d.mu.Lock()
	d.stopped = true
	d.mu.Unlock()
	done := make(chan struct{})
	go func() { d.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return errors.New("issue notification shutdown: " + ctx.Err().Error())
	}
}
