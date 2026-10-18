package platform

// EV1: the issue alert emitter - the bridge from the error ingest path to
// the durable signed outbox (notify_outbox.go).
//
// The hook it serves is fire-on-ingest: internal/errors calls its notifier
// inline between issue resolution and the error_events insert, and that
// notifier must never block or do I/O (notify.go's contract). This type
// honors it absolutely: Notify is a non-blocking channel send. A full
// channel drops the event and counts it (healthz issue_alerts.dropped) -
// an alert is a derived convenience, ingest is the product; a dropped
// alert is honest, a stalled ingest path is not.
//
// The drain goroutine does the slow half: after a short settle delay (the
// triggering event's own error_events row lands right after the notify
// call, so counts read a moment later are real), it re-reads the issue
// through the injected reader, rebuilds the payload with live numbers, and
// enqueues one durable intent per webhook target whose `kinds` filter
// opted into the kind. Retries, the HMAC signature, the delivery-id
// dedupe marker, maintenance-window suppression and dead letters all come
// from the existing outbox drain.
//
// Crash posture: anything still on the channel at shutdown is lost - the
// same declared budget as the in-memory integrations dispatcher. New and
// regression events remain re-derivable from issues.first_seen and
// first_regression_at; a spike hint is a hint. (Re-derivation is
// deliberately NOT done at startup for v1 - owner decision, design note
// 5.1 step 5.)

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/neutron-build/neutron/go/neutron"
)

// Emitter defaults. The queue must be far larger than a legitimate alert
// rate and far smaller than anything that could pressure the heap: a burst
// of new issues resolves to one channel send each.
const (
	DefaultIssueAlertQueue     = 1024
	DefaultIssueAlertSettleMs  = 2000
	issueAlertDeliveryDeadline = 15 * time.Second
)

// IssueAlertEvent is one issue lifecycle occurrence headed for the outbox,
// translated from internal/errors' IssueEvent by the wiring in cmd/observe
// (platform deliberately does not import internal/errors).
type IssueAlertEvent struct {
	// Kind is the outbox kind: issue_new, issue_regression or issue_spike.
	Kind string
	// WindowCount is the spike window's event count (issue_spike only).
	WindowCount int64

	IssueID     string
	SiteID      string
	Fingerprint string

	Title       string
	Culprit     string
	Level       string
	Release     string
	Environment string
	// SampleStack arrives pre-scrubbed and pre-clipped from the errors
	// package (post-scrub frames, file:line function, <= 2 KB); it is
	// clipped again here as defense in depth.
	SampleStack string
	At          time.Time
}

// IssueSnapshot is the re-read issue state the payload is built from. The
// live implementation is a GetIssue call wired in from cmd/observe.
type IssueSnapshot struct {
	IssueID            string
	SiteID             string
	GroupHash          string
	Title              string
	Culprit            string
	Level              string
	Status             string
	FirstSeen          time.Time
	LastSeen           time.Time
	EventCount         int64
	UserCount          int64
	RegressionCount    int64
	ReleaseTag         string // first-seen release
	FingerprintVersion int
}

// IssueAlertEmitter owns the bounded queue and the drain goroutine.
type IssueAlertEmitter struct {
	notifier *Notifier
	hooks    func(ctx context.Context, siteID string) ([]Webhook, error)
	read     func(ctx context.Context, siteID, issueID string) (*IssueSnapshot, error)
	baseURL  string
	logger   *slog.Logger

	queue  chan IssueAlertEvent
	settle time.Duration

	// dropped counts a full-queue drop; emitted counts events that reached
	// the drain (before per-hook routing).
	dropped atomic.Int64
	emitted atomic.Int64

	// seq mints the per-issue monotonic delivery_seq. In-memory: a restart
	// renumbers from 1. It orders notifications for a receiver, it is not
	// a durability claim.
	seqMu sync.Mutex
	seq   map[string]int64

	lifecycleMu sync.Mutex
	started     bool
	stopCtx     context.Context
	stopCancel  context.CancelFunc
	workerDone  chan struct{}
}

// NewIssueAlertEmitter builds the emitter. hooks and read are the two
// seams production fills with WebhookService.List and an IssueService
// GetIssue adapter; nil hooks means nothing is routed, nil read means
// payloads carry only the triggering event's numbers.
func NewIssueAlertEmitter(notifier *Notifier, hooks func(ctx context.Context, siteID string) ([]Webhook, error), read func(ctx context.Context, siteID, issueID string) (*IssueSnapshot, error), baseURL string, logger *slog.Logger) *IssueAlertEmitter {
	if logger == nil {
		logger = slog.Default()
	}
	queue := DefaultIssueAlertQueue
	if v := envIssueAlertInt("OBSERVE_ISSUE_ALERT_QUEUE"); v > 0 {
		queue = v
	}
	settle := time.Duration(DefaultIssueAlertSettleMs) * time.Millisecond
	if v := envIssueAlertInt("OBSERVE_ISSUE_ALERT_SETTLE_MS"); v >= 0 {
		settle = time.Duration(v) * time.Millisecond
	}
	return &IssueAlertEmitter{
		notifier: notifier,
		hooks:    hooks,
		read:     read,
		baseURL:  strings.TrimRight(baseURL, "/"),
		logger:   logger,
		queue:    make(chan IssueAlertEvent, queue),
		settle:   settle,
		seq:      map[string]int64{},
	}
}

func envIssueAlertInt(name string) int {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return -1
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return -1
	}
	return n
}

// Notify admits one event. It NEVER blocks: a full queue drops the event,
// counts it, and returns. Safe to call from the ingest path.
func (e *IssueAlertEmitter) Notify(ev IssueAlertEvent) {
	if e == nil || e.notifier == nil || !IsIssueKind(ev.Kind) {
		return
	}
	select {
	case e.queue <- ev:
	default:
		// The drain is wedged or the burst exceeds the queue. Drop, count,
		// say so - never stall ingest on an alert.
		n := e.dropped.Add(1)
		if n == 1 || n%100 == 0 {
			e.logger.Error("issue alert queue overflow - event dropped, ingest unaffected",
				"kind", ev.Kind, "issue", ev.IssueID, "dropped_total", n)
		}
	}
}

// Start launches the drain goroutine. Idempotent.
func (e *IssueAlertEmitter) Start() {
	e.lifecycleMu.Lock()
	defer e.lifecycleMu.Unlock()
	if e.started {
		return
	}
	e.started = true
	e.stopCtx, e.stopCancel = context.WithCancel(context.Background())
	ctx := e.stopCtx
	e.workerDone = make(chan struct{})
	go func() {
		defer close(e.workerDone)
		e.run(ctx)
	}()
}

func (e *IssueAlertEmitter) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-e.queue:
			e.deliver(ctx, ev)
		}
	}
}

// Stop cancels the drain and waits for the in-flight event. Queued events
// are lost (the declared crash-parity budget above).
func (e *IssueAlertEmitter) Stop() { _ = e.StopContext(context.Background()) }

func (e *IssueAlertEmitter) StopContext(ctx context.Context) error {
	e.lifecycleMu.Lock()
	defer e.lifecycleMu.Unlock()
	if !e.started {
		return nil
	}
	e.stopCancel()
	select {
	case <-e.workerDone:
		e.started = false
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// LifecycleHook wires Start/Stop into the process lifecycle.
func (e *IssueAlertEmitter) LifecycleHook() neutron.LifecycleHook {
	return neutron.LifecycleHook{
		Name:    "issue-alert-emitter",
		OnStart: func(context.Context) error { e.Start(); return nil },
		OnStop:  e.StopContext,
	}
}

// IssueAlertStats is the /healthz "issue_alerts" block.
type IssueAlertStats struct {
	QueueDepth int64 `json:"queue_depth"`
	Emitted    int64 `json:"emitted"`
	Dropped    int64 `json:"dropped"`
}

// Stats snapshots the emitter counters.
func (e *IssueAlertEmitter) Stats() IssueAlertStats {
	if e == nil {
		return IssueAlertStats{}
	}
	return IssueAlertStats{
		QueueDepth: int64(len(e.queue)),
		Emitted:    e.emitted.Load(),
		Dropped:    e.dropped.Load(),
	}
}

// deliver settles, re-reads the issue, routes and enqueues. Errors are
// logged and swallowed: the emitter runs on its own goroutine and a
// failing read must not kill the drain.
func (e *IssueAlertEmitter) deliver(ctx context.Context, ev IssueAlertEvent) {
	if e.settle > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(e.settle):
		}
	}
	// Bound the whole delivery so a stuck hook listing or issue read
	// cannot wedge the single drain goroutine.
	dctx, cancel := context.WithTimeout(ctx, issueAlertDeliveryDeadline)
	defer cancel()

	e.emitted.Add(1)

	hooks := e.routeHooks(dctx, ev)
	if len(hooks) == 0 {
		return
	}
	snap := e.snapshot(dctx, ev)
	payload := e.buildPayload(ev, snap)

	for _, hook := range hooks {
		intent := NotificationIntent{
			Kind:       ev.Kind,
			RuleID:     "issue:" + ev.IssueID,
			SiteID:     ev.SiteID,
			WebhookID:  hook.WebhookID,
			TargetType: hook.WebhookType,
			TargetURL:  hook.URL,
			Secret:     hook.Secret,
			Payload:    payload,
		}
		if _, err := e.notifier.Enqueue(dctx, intent); err != nil {
			e.logger.Error("issue alert enqueue failed",
				"kind", ev.Kind, "issue", ev.IssueID, "webhook", hook.WebhookID, "err", err)
		}
	}
}

// routeHooks returns the site's enabled webhooks that opted into this
// kind AND route the issue's mapped severity. Slack targets are welcome:
// the outbox renders them without the stack.
func (e *IssueAlertEmitter) routeHooks(ctx context.Context, ev IssueAlertEvent) []Webhook {
	if e.hooks == nil {
		return nil
	}
	hooks, err := e.hooks(ctx, ev.SiteID)
	if err != nil {
		e.logger.Error("issue alert webhook listing failed", "site", ev.SiteID, "err", err)
		return nil
	}
	severity := IssueSeverity(ev.Level)
	out := make([]Webhook, 0, len(hooks))
	for _, h := range hooks {
		if MatchesKind(h.Kinds, ev.Kind) && MatchesSeverity(h.Severities, severity) {
			out = append(out, h)
		}
	}
	return out
}

// snapshot re-reads the issue so event_count and last_seen are real. A
// failed read is not fatal: the payload falls back to the event's own
// numbers rather than losing the notification.
func (e *IssueAlertEmitter) snapshot(ctx context.Context, ev IssueAlertEvent) *IssueSnapshot {
	if e.read == nil {
		return nil
	}
	snap, err := e.read(ctx, ev.SiteID, ev.IssueID)
	if err != nil {
		e.logger.Warn("issue alert re-read failed - payload carries the triggering event's numbers",
			"issue", ev.IssueID, "err", err)
		return nil
	}
	return snap
}

// nextSeq mints the per-issue monotonic delivery sequence.
func (e *IssueAlertEmitter) nextSeq(issueID string) int64 {
	e.seqMu.Lock()
	defer e.seqMu.Unlock()
	e.seq[issueID]++
	if len(e.seq) > 65536 {
		// Bound the map (a site with endless one-off issues); renumbering
		// after eviction is the documented in-memory caveat.
		e.seq = map[string]int64{issueID: 1}
	}
	return e.seq[issueID]
}

// buildPayload freezes the JSON body once per event, shared by every
// routed target.
func (e *IssueAlertEmitter) buildPayload(ev IssueAlertEvent, snap *IssueSnapshot) string {
	p := IssueNotification{
		Kind:        ev.Kind,
		DeliverySeq: e.nextSeq(ev.IssueID),
		IssueID:     ev.IssueID,
		SiteID:      ev.SiteID,
		Service:     ev.SiteID,
		Fingerprint: ev.Fingerprint,
		Title:       ev.Title,
		Culprit:     ev.Culprit,
		Level:       ev.Level,
		Release:     ev.Release,
		Environment: ev.Environment,
		WindowCount: ev.WindowCount,
		SampleStack: ClipStack(ev.SampleStack),
	}
	if snap != nil {
		p.Fingerprint = firstNonEmpty(snap.GroupHash, p.Fingerprint)
		p.Title = firstNonEmpty(snap.Title, p.Title)
		p.Culprit = firstNonEmpty(snap.Culprit, p.Culprit)
		p.Level = firstNonEmpty(snap.Level, p.Level)
		p.EventCount = snap.EventCount
		p.UserCount = snap.UserCount
		p.RegressionCount = snap.RegressionCount
		p.FingerprintVersion = snap.FingerprintVersion
		p.FirstRelease = snap.ReleaseTag
		p.FirstSeen = snap.FirstSeen.UTC().Format(time.RFC3339)
		p.LastSeen = snap.LastSeen.UTC().Format(time.RFC3339)
	}
	if ev.Kind == NotifyIssueNew && p.EventCount == 0 {
		p.EventCount = 1
	}
	if ev.At.IsZero() {
		p.OccurredAt = time.Now().UTC().Format(time.RFC3339)
	} else {
		p.OccurredAt = ev.At.UTC().Format(time.RFC3339)
	}
	if e.baseURL != "" {
		p.IssueURL = e.baseURL + "/errors"
	}
	raw, _ := json.Marshal(p)
	return string(raw)
}

// IssueSeverity maps an error level onto the severity domain webhooks
// filter by (same mapping as the integrations dispatcher's severityFor).
func IssueSeverity(level string) string {
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

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
