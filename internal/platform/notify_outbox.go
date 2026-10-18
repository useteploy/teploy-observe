package platform

// O10: the notification outbox - the durable delivery half of alerting.
//
// A threshold crossing enqueues one intent per (notification, webhook
// target) in the SAME transaction as the evaluation edge that caused it
// (see alerts_engine.go). This file is the drain: delivery is a webhook
// POST carrying the row's id as X-Observe-Delivery (the R22 dedupe marker,
// stable across attempts), failures retry with exponential backoff under an
// attempt budget, the budget's end is a durable dead letter (kept with
// last_error, counted at /healthz, never auto-pruned), and an active
// maintenance window suppresses the delivery visibly instead of silently
// dropping it.
//
// Exactly-once posture (same as the replay ledger): the drain is
// at-least-once - a crash between a successful POST and the disposition
// mark re-POSTs the intent after restart - and the receiver dedupes on the
// delivery id for the exactly-once effect. Restart survival needs no
// in-memory state: pending intents live in the table, and Start's immediate
// pass resumes them.
//
// Boundary: single-process (the standing AUD-018 posture) - the drain
// serializes in-process; a multi-replica deployment needs a lease/CAS claim
// before this table carries external side effects. Mirrors internal/outbox.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/neutron-build/neutron/go/neutron"
	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
	"github.com/useteploy/teploy-observe/internal/incidents"
	"github.com/useteploy/teploy-observe/internal/integrations"
	"github.com/useteploy/teploy-observe/internal/netsafe"
)

// Notification intent kinds (the edge that caused them).
const (
	NotifyIncidentOpened    = "incident_opened"
	NotifyIncidentRecovered = "incident_recovered"
	NotifyIncidentRepeat    = "incident_repeat"

	// O10 tails: cron heartbeat incidents ride the same durable outbox
	// (they were EnsureOpen-only - the incident existed, nothing was
	// delivered).
	NotifyCronMissed    = "cron_missed"
	NotifyCronRecovered = "cron_recovered"

	// EV1 (066): issue-level alert kinds. Fired by the issue alert emitter
	// (issue_alerts.go) from internal/errors lifecycle events; delivered
	// with the same HMAC scheme, delivery-id dedupe, retries and dead
	// letters as every other outbox kind. OPT-IN per webhook via the
	// `kinds` filter (webhooks.go): a webhook that does not list them
	// receives none.
	NotifyIssueNew        = "issue_new"
	NotifyIssueRegression = "issue_regression"
	NotifyIssueSpike      = "issue_spike"
)

// Notifier defaults (mirroring the derived-outbox worker).
const (
	notifyDefaultMaxAttempts = 5
	notifyDefaultPollEvery   = 2 * time.Second
	notifyDefaultBatchSize   = 500
	notifyMaxLastErrorChars  = 1024
)

// IncidentRecorder appends a timeline event through the disposition transaction.
// Wired to incidents.Service.RecordEventTx; nil disables timeline recording.
type IncidentRecorder func(ctx context.Context, sql *nucleus.SQLModel, eventID, incidentID string, at int64, kind, actor, detail string) error

// Notifier owns the notification_outbox table: enqueue happens inside the
// engine's transactions (enqueueTx); the drain, dispositions and healthz
// counters live here.
type Notifier struct {
	db       *nucleus.Client
	logger   *slog.Logger
	client   *http.Client
	recorder IncidentRecorder

	maxAttempts int
	backoffBase time.Duration
	backoffCap  time.Duration
	pollEvery   time.Duration
	batchSize   int
	maintenance *MaintenanceService

	// processMu serializes the drain (the single-process claim).
	processMu sync.Mutex

	lifecycleMu sync.Mutex
	started     bool
	stopCtx     context.Context
	stopCancel  context.CancelFunc
	workerDone  chan struct{}

	failedMu       sync.Mutex
	failedAttempts map[string]*atomic.Int64

	// now is the clock; tests freeze it to make backoff and due-ness
	// deterministic. skipMark is a TEST-ONLY crash simulation: deliver the
	// POST, then return without writing the disposition - the window a
	// process kill opens between a successful send and its durable mark.
	now      func() time.Time
	skipMark bool
	// enqueueHook injects admission failures before selected target writes in tests.
	enqueueHook func(NotificationIntent) error
}

func NewNotifier(db *nucleus.Client, logger *slog.Logger, maintenance *MaintenanceService, recorder IncidentRecorder) *Notifier {
	if logger == nil {
		logger = slog.Default()
	}
	return &Notifier{
		db:             db,
		logger:         logger,
		client:         netsafe.ClientWithAllow(10*time.Second, webhookAllow()),
		recorder:       recorder,
		maintenance:    maintenance,
		maxAttempts:    notifyDefaultMaxAttempts,
		backoffBase:    time.Second,
		backoffCap:     5 * time.Minute,
		pollEvery:      notifyDefaultPollEvery,
		batchSize:      notifyDefaultBatchSize,
		failedAttempts: map[string]*atomic.Int64{},
		now:            time.Now,
	}
}

func (n *Notifier) WithMaxAttempts(v int) *Notifier           { n.maxAttempts = v; return n }
func (n *Notifier) WithBackoffBase(d time.Duration) *Notifier { n.backoffBase = d; return n }
func (n *Notifier) WithBackoffCap(d time.Duration) *Notifier  { n.backoffCap = d; return n }
func (n *Notifier) WithPollInterval(d time.Duration) *Notifier {
	n.pollEvery = d
	return n
}

// backoff returns the retry delay after the given failed attempt count
// (doubling from the base, capped).
func (n *Notifier) backoff(attempts int64) time.Duration {
	if n.backoffBase <= 0 || attempts < 1 {
		return 0
	}
	d := n.backoffBase
	for i := int64(1); i < attempts && d < n.backoffCap; i++ {
		d *= 2
	}
	if d > n.backoffCap {
		d = n.backoffCap
	}
	return d
}

// notifyRow is a collapsed (latest-version) view of one notification intent.
type notifyRow struct {
	ID               string `db:"id"`
	Kind             string `db:"kind"`
	RuleID           string `db:"rule_id"`
	IncidentID       string `db:"incident_id"`
	SiteID           string `db:"site_id"`
	WebhookID        string `db:"webhook_id"`
	TargetType       string `db:"target_type"`
	TargetURL        string `db:"target_url"`
	Secret           string `db:"secret"`
	Payload          string `db:"payload"`
	CreatedAt        int64  `db:"created_at"`
	Attempts         int64  `db:"attempts"`
	NextAttemptAt    int64  `db:"next_attempt_at"`
	DeliveredAt      int64  `db:"delivered_at"`
	SuppressedAt     int64  `db:"suppressed_at"`
	SuppressedReason string `db:"suppressed_reason"`
	LastError        string `db:"last_error"`
	Version          int64  `db:"version"`
}

// notifyCollapseSelect renders the argMax collapse over notification_outbox
// - one logical row per intent id, highest version wins (the form verified
// against the live engine; see internal/query/replacing.go).
func notifyCollapseSelect(where string) string {
	if strings.TrimSpace(where) == "" {
		where = "1 = 1"
	}
	return `SELECT tenant_id, id,
			argMax(kind, version) AS kind,
			argMax(rule_id, version) AS rule_id,
			argMax(incident_id, version) AS incident_id,
			argMax(site_id, version) AS site_id,
			argMax(webhook_id, version) AS webhook_id,
			argMax(target_type, version) AS target_type,
			argMax(target_url, version) AS target_url,
			argMax(secret, version) AS secret,
			argMax(payload, version) AS payload,
			argMax(created_at, version) AS created_at,
			argMax(attempts, version) AS attempts,
			argMax(next_attempt_at, version) AS next_attempt_at,
			argMax(delivered_at, version) AS delivered_at,
			argMax(suppressed_at, version) AS suppressed_at,
			argMax(suppressed_reason, version) AS suppressed_reason,
			argMax(last_error, version) AS last_error,
			MAX(version) AS version
		FROM notification_outbox WHERE ` + where + `
		GROUP BY tenant_id, id`
}

// NotificationIntent is the frozen delivery unit the engine enqueues - one
// per (notification, webhook target). Everything the POST needs is frozen
// here so a retry reproduces the identical request.
type NotificationIntent struct {
	ID         string // optional stable logical identity (transaction callers only)
	Kind       string
	RuleID     string
	IncidentID string
	SiteID     string
	WebhookID  string
	TargetType string
	TargetURL  string
	Secret     string
	Payload    string
}

// enqueueTx writes one intent through sql - which MUST be the transaction-
// scoped SQLModel of the evaluation edge causing the notification, so the
// intent commits or rolls back with the state it describes. The minted id
// is the delivery id (X-Observe-Delivery).
func (n *Notifier) enqueueTx(ctx context.Context, sql *nucleus.SQLModel, in NotificationIntent) (string, error) {
	if n.enqueueHook != nil {
		if err := n.enqueueHook(in); err != nil {
			return "", err
		}
	}
	id := in.ID
	if id == "" {
		id = genNotificationID()
	}
	now := n.now().UTC().UnixMilli()
	if _, err := sql.Exec(ctx,
		`INSERT INTO notification_outbox (
			tenant_id, id, kind, rule_id, incident_id, site_id, webhook_id,
			target_type, target_url, secret, payload, created_at,
			attempts, next_attempt_at, delivered_at, suppressed_at, suppressed_reason, last_error, version
		) VALUES ('default', $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 0, 0, 0, 0, '', '', $11)`,
		id, in.Kind, in.RuleID, in.IncidentID, in.SiteID, in.WebhookID,
		in.TargetType, in.TargetURL, in.Secret, in.Payload, dbutil.IntParam(now),
	); err != nil {
		return "", fmt.Errorf("notification enqueue: %w", err)
	}
	return id, nil
}

// Enqueue admits an independent intent. Incident transitions must use the
// originating transaction (EnsureCronMissed/RecoverCron or the alert engine).
func (n *Notifier) Enqueue(ctx context.Context, in NotificationIntent) (string, error) {
	return n.enqueueTx(ctx, n.db.SQL(), in)
}

// CronMonitorRef is the cron identity the notification payload carries
// (monitoring.CronMonitor narrowed to what delivery needs - platform
// cannot import internal/monitoring without a cycle through the engine).
type CronMonitorRef struct {
	CronID string
	SiteID string
	Name   string
	Slug   string
}

// BuildCronPayload freezes the payload for a cron heartbeat notification -
// the same NotificationPayload receivers already parse, with the fields a
// missed heartbeat can honestly fill (no metric value exists; the message
// carries the schedule context).
func BuildCronPayload(kind string, cron CronMonitorRef, incidentID string, graceSecs int, at time.Time) string {
	p := NotificationPayload{
		Kind: kind, IncidentID: incidentID,
		RuleID: "cron:" + cron.CronID, RuleName: cron.Name,
		Metric: "cron_heartbeat", SiteID: cron.SiteID, Severity: "warning",
		EvaluatedAt: at.UTC().Format(time.RFC3339),
	}
	switch kind {
	case NotifyCronMissed:
		p.Message = fmt.Sprintf("Cron missed: %q (slug %q) has not checked in within its %ds grace period (incident %s)",
			cron.Name, cron.Slug, graceSecs, incidentID)
	case NotifyCronRecovered:
		p.Message = fmt.Sprintf("Cron recovered: %q (slug %q) checked in again (incident %s closed)",
			cron.Name, cron.Slug, incidentID)
	}
	raw, _ := json.Marshal(p)
	return string(raw)
}

// LastIntentAt returns the newest enqueued intent time for a rule (0 when
// none). The engine's cooldown check uses it: a repeat notification of a
// still-firing rule waits for the cooldown since the LAST notification,
// opening included.
func (n *Notifier) LastIntentAt(ctx context.Context, ruleID string) (int64, error) {
	rows, err := nucleus.Query[struct {
		Last string `db:"last"`
	}](ctx, n.db.SQL(),
		`SELECT CAST(MAX(created_at) AS TEXT) AS last FROM (`+notifyCollapseSelect("rule_id = $1")+`)`, ruleID)
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 || rows[0].Last == "" {
		return 0, nil
	}
	var v int64
	fmt.Sscanf(rows[0].Last, "%d", &v)
	return v, nil
}

// DrainDue runs one drain pass over due intents (undelivered, unsuppressed,
// under the attempt budget, retry time reached), looping until the table
// stops yielding. Maintenance windows are resolved once per pass. Per-row
// failures are recorded on their rows; only a scan failure returns an error.
func (n *Notifier) DrainDue(ctx context.Context) (int, error) {
	if err := lockNotify(ctx, &n.processMu); err != nil {
		return 0, err
	}
	defer n.processMu.Unlock()

	now := n.now().UTC()
	var windows []MaintenanceWindow
	if n.maintenance != nil {
		var err error
		windows, err = n.maintenance.Active(ctx, now)
		if err != nil {
			return 0, fmt.Errorf("notification drain: maintenance lookup failed (delivering anyway would ignore the operator's window): %w", err)
		}
	}

	total := 0
	for {
		rows, err := n.due(ctx)
		if err != nil {
			return total, fmt.Errorf("notification drain: due scan: %w", err)
		}
		if len(rows) == 0 {
			return total, nil
		}
		for i := range rows {
			if err := ctx.Err(); err != nil {
				return total, err
			}
			if maintenanceMatch(windows, rows[i].SiteID) {
				if err := n.markSuppressed(ctx, &rows[i], now); err != nil {
					return total, err
				}
				total++
				continue
			}
			total++
			if err := n.processOne(ctx, &rows[i]); err != nil {
				return total, err
			}
		}
		if len(rows) < n.batchSize {
			return total, nil
		}
	}
}

// maintenanceMatch reports whether an active window covers the site
// (site_id ” covers every site).
func maintenanceMatch(windows []MaintenanceWindow, siteID string) bool {
	for _, w := range windows {
		if w.SiteID == "" || w.SiteID == siteID {
			return true
		}
	}
	return false
}

// due selects the next batch of due intents through the collapse. A dead
// letter (next_attempt_at = -1) is never selected regardless of any
// process's attempt budget - dead is a durable row state.
func (n *Notifier) due(ctx context.Context) ([]notifyRow, error) {
	return nucleus.Query[notifyRow](ctx, n.db.SQL(),
		`SELECT id, kind, rule_id, incident_id, site_id, webhook_id, target_type, target_url, secret, payload,
			created_at, attempts, next_attempt_at, delivered_at, suppressed_at, suppressed_reason, last_error, version
		 FROM (`+notifyCollapseSelect("")+`)
		 WHERE delivered_at = 0 AND suppressed_at = 0
		   AND next_attempt_at >= 0 AND next_attempt_at <= $1
		   AND attempts < $2
		 ORDER BY created_at, id
		 LIMIT $3`,
		dbutil.IntParam(n.now().UTC().UnixMilli()), n.maxAttempts, n.batchSize,
	)
}

// processOne delivers one intent and records the disposition as a new row
// version (strictly-monotonic stamp, the version-tie convention).
func (n *Notifier) processOne(ctx context.Context, row *notifyRow) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	err := n.deliver(ctx, row)
	if err == nil {
		if n.skipMark {
			// TEST-ONLY crash simulation: the POST left, the disposition
			// mark did not. The row stays pending; the next drain re-POSTs
			// under the SAME delivery id.
			return nil
		}
		// A completed POST gets a bounded accounting context even if shutdown
		// canceled admission while the response arrived. Unsent rows stay pending.
		markCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		return n.markDelivered(markCtx, row)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}

	attempts := row.Attempts + 1
	n.bumpFailed(row.Kind)
	dead := attempts >= int64(n.maxAttempts)
	next := int64(0)
	if dead {
		// Durable dead letter: the sentinel is below any clock value, so no
		// later process - even one with a larger budget - retries it.
		next = -1
	} else {
		next = n.now().UTC().UnixMilli() + n.backoff(attempts).Milliseconds()
	}
	if derr := n.markAttempt(ctx, row, attempts, next, err); derr != nil {
		n.logger.Error("notification: record failure failed", "delivery_id", row.ID, "err", derr)
		return derr
	}
	if dead {
		n.logger.Error("notification: dead-lettered - delivery keeps failing",
			"delivery_id", row.ID, "kind", row.Kind, "site", row.SiteID,
			"attempts", attempts, "err", err)
	}
	return nil
}

// deliver performs the POST. The delivery id header is the dedupe marker;
// signed targets carry the same HMAC scheme as the in-memory webhook path
// (R22), so receivers verify both identically.
func (n *Notifier) deliver(ctx context.Context, row *notifyRow) error {
	if row.TargetType == "slack" {
		return n.postSlack(ctx, row)
	}
	return postSignedJSON(ctx, n.client, row.ID, row.TargetURL, row.Secret, []byte(row.Payload))
}

func (n *Notifier) postSlack(ctx context.Context, row *notifyRow) error {
	if IsIssueKind(row.Kind) {
		return n.postSlackIssue(ctx, row)
	}
	var p NotificationPayload
	if err := json.Unmarshal([]byte(row.Payload), &p); err != nil {
		return fmt.Errorf("slack notification: decode payload: %w", err)
	}
	text := fmt.Sprintf("*[%s] %s*\n%s\nSite: %s (incident %s)",
		p.Severity, p.RuleName, p.Message, p.SiteID, p.IncidentID)
	body, _ := json.Marshal(map[string]string{"text": text})
	return postSignedJSON(ctx, n.client, row.ID, row.TargetURL, "", body)
}

// postSlackIssue renders an issue_* intent for a Slack target. Slack renders
// chat text, not structured payloads, and chat scrolls: the sample stack is
// NEVER included - not truncated, not linked, omitted. The full signed
// payload is only ever POSTed to http-type targets.
func (n *Notifier) postSlackIssue(ctx context.Context, row *notifyRow) error {
	text, err := slackIssueText(row.Payload)
	if err != nil {
		return fmt.Errorf("slack issue notification: %w", err)
	}
	body, _ := json.Marshal(map[string]string{"text": text})
	return postSignedJSON(ctx, n.client, row.ID, row.TargetURL, "", body)
}

// Slack field caps for issue notifications, mirroring fireSlack's budget
// in internal/integrations: the title gets the room a headline needs,
// short context fields get a line each. Slack rejects chat messages past
// ~4 KB of blocks/text, which would dead-letter the intent, and an
// attacker-controlled title (the public ingest key lets anyone mint one)
// must never be the reason a delivery dies.
const (
	slackIssueTitleMax = 300
	slackIssueFieldMax = 100
)

// slackIssueText renders the chat line for an issue notification from its
// frozen payload. Deliberately excludes sample_stack.
//
// Every interpolated field runs through integrations.SlackEscape - the
// same neutraliser the integrations path uses (Slack's <...> control
// syntax covers <!channel> pings and disguised <https://evil|text> links,
// and the title/culprit/release/environment are attacker-influenced via
// the public browser ingest key) - and every field is capped so a 10 KB
// title cannot push the message past Slack's size limit into a
// dead letter.
func slackIssueText(payload string) (string, error) {
	var p IssueNotification
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return "", fmt.Errorf("decode payload: %w", err)
	}
	label := map[string]string{
		NotifyIssueNew:        "New issue",
		NotifyIssueRegression: "Regression",
		NotifyIssueSpike:      "Spike",
	}[p.Kind]
	if label == "" {
		label = p.Kind
	}
	esc := integrations.SlackEscape
	var b strings.Builder
	fmt.Fprintf(&b, "*[%s] %s: %s*",
		esc(strings.ToUpper(p.Level), slackIssueFieldMax),
		esc(label, slackIssueFieldMax),
		esc(p.Title, slackIssueTitleMax))
	if p.Culprit != "" {
		fmt.Fprintf(&b, "\nCulprit: %s", esc(p.Culprit, slackIssueTitleMax))
	}
	fmt.Fprintf(&b, "\nSite: %s (issue %s)", esc(p.SiteID, slackIssueFieldMax), esc(p.IssueID, slackIssueFieldMax))
	if p.Release != "" {
		fmt.Fprintf(&b, "\nRelease: %s", esc(p.Release, slackIssueFieldMax))
	}
	if p.Environment != "" {
		fmt.Fprintf(&b, "\nEnvironment: %s", esc(p.Environment, slackIssueFieldMax))
	}
	fmt.Fprintf(&b, "\nEvents: %d | Users: %d", p.EventCount, p.UserCount)
	if p.WindowCount > 0 {
		fmt.Fprintf(&b, " | Window events: %d", p.WindowCount)
	}
	return b.String(), nil
}

// recordDispositionTx persists the timeline through the same SQL transaction as
// its disposition. Failure rolls back the mark, making timeline repair retryable.
func (n *Notifier) recordDispositionTx(ctx context.Context, sql *nucleus.SQLModel, row *notifyRow, at int64, kind, detail string) error {
	if n.recorder == nil || row.IncidentID == "" {
		return nil
	}
	svc := incidents.NewService(n.db)
	known, err := svc.Get(ctx, row.IncidentID)
	if err != nil {
		return err
	}
	if known == nil {
		return fmt.Errorf("incident %s not found", row.IncidentID)
	}
	return n.recorder(ctx, sql, kind+"-"+row.ID, row.IncidentID, at, kind, "notifier", detail)
}

// markDelivered writes the delivered disposition.
func (n *Notifier) markDelivered(ctx context.Context, row *notifyRow) error {
	tx, err := n.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	now := n.now().UTC().UnixMilli()
	_, err = tx.SQL().Exec(ctx,
		`INSERT INTO notification_outbox (
			tenant_id, id, kind, rule_id, incident_id, site_id, webhook_id,
			target_type, target_url, secret, payload, created_at,
			attempts, next_attempt_at, delivered_at, suppressed_at, suppressed_reason, last_error, version
		) VALUES ('default', $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, 0, '', '', $15)`,
		row.ID, row.Kind, row.RuleID, row.IncidentID, row.SiteID, row.WebhookID,
		row.TargetType, row.TargetURL, row.Secret, row.Payload, dbutil.IntParam(row.CreatedAt),
		dbutil.IntParam(row.Attempts), dbutil.IntParam(row.NextAttemptAt),
		dbutil.IntParam(now), dbutil.IntParam(notifyNextVersion(row.Version, now)),
	)
	if err != nil {
		return err
	}
	if err := n.recordDispositionTx(ctx, tx.SQL(), row, now, incidents.EventNotified, fmt.Sprintf("%s delivered (%s)", row.Kind, row.ID)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// markSuppressed records a maintenance-window suppression: no POST happened,
// no attempt was consumed, and the suppression stays inspectable.
func (n *Notifier) markSuppressed(ctx context.Context, row *notifyRow, at time.Time) error {
	tx, err := n.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	ms := at.UTC().UnixMilli()
	reason := "maintenance window active"
	_, err = tx.SQL().Exec(ctx,
		`INSERT INTO notification_outbox (
			tenant_id, id, kind, rule_id, incident_id, site_id, webhook_id,
			target_type, target_url, secret, payload, created_at,
			attempts, next_attempt_at, delivered_at, suppressed_at, suppressed_reason, last_error, version
		) VALUES ('default', $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, 0, $14, $15, $16, $17)`,
		row.ID, row.Kind, row.RuleID, row.IncidentID, row.SiteID, row.WebhookID,
		row.TargetType, row.TargetURL, row.Secret, row.Payload, dbutil.IntParam(row.CreatedAt),
		dbutil.IntParam(row.Attempts), dbutil.IntParam(row.NextAttemptAt),
		dbutil.IntParam(ms), reason, row.LastError,
		dbutil.IntParam(notifyNextVersion(row.Version, ms)),
	)
	if err != nil {
		n.logger.Error("notification: mark suppressed failed", "delivery_id", row.ID, "err", err)
		return err
	}
	if err := n.recordDispositionTx(ctx, tx.SQL(), row, ms, incidents.EventSuppressed, fmt.Sprintf("%s suppressed: %s (%s)", row.Kind, reason, row.ID)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// markAttempt writes the failure disposition (attempts bumped, backoff
// scheduled, last_error kept - the dead letter's evidence).
func (n *Notifier) markAttempt(ctx context.Context, row *notifyRow, attempts, nextAt int64, cause error) error {
	now := n.now().UTC().UnixMilli()
	lastErr := cause.Error()
	if len(lastErr) > notifyMaxLastErrorChars {
		lastErr = lastErr[:notifyMaxLastErrorChars]
	}
	_, err := n.db.SQL().Exec(ctx,
		`INSERT INTO notification_outbox (
			tenant_id, id, kind, rule_id, incident_id, site_id, webhook_id,
			target_type, target_url, secret, payload, created_at,
			attempts, next_attempt_at, delivered_at, suppressed_at, suppressed_reason, last_error, version
		) VALUES ('default', $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, 0, 0, '', $14, $15)`,
		row.ID, row.Kind, row.RuleID, row.IncidentID, row.SiteID, row.WebhookID,
		row.TargetType, row.TargetURL, row.Secret, row.Payload, dbutil.IntParam(row.CreatedAt),
		dbutil.IntParam(attempts), dbutil.IntParam(nextAt),
		lastErr, dbutil.IntParam(notifyNextVersion(row.Version, now)),
	)
	return err
}

// notifyNextVersion stamps a strictly-greater version than prior (the
// version-tie defect fix: two worker writes inside one millisecond must not
// tie, or the argMax collapse resolves the tie arbitrarily).
func notifyNextVersion(prior, now int64) int64 {
	if now > prior+1 {
		return now
	}
	return prior + 1
}

// Start launches the background drain loop: an immediate pass (startup
// resume of the previous process's pending notifications) then a fixed
// cadence. Idempotent; Stop cancels and waits for the in-flight delivery.
func (n *Notifier) Start() {
	n.lifecycleMu.Lock()
	defer n.lifecycleMu.Unlock()
	if n.started {
		return
	}
	n.started = true
	n.stopCtx, n.stopCancel = context.WithCancel(context.Background())
	n.workerDone = make(chan struct{})
	done := n.workerDone
	go func() {
		defer close(done)
		n.run(n.stopCtx)
	}()
}

func (n *Notifier) run(ctx context.Context) {
	ticker := time.NewTicker(n.pollEvery)
	defer ticker.Stop()
	for {
		if _, err := n.DrainDue(ctx); err != nil && ctx.Err() == nil {
			n.logger.Error("notification: drain pass failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Stop cancels the worker and waits for the in-flight delivery. Pending
// intents survive the stop and resume on the next Start.
func (n *Notifier) Stop() { _ = n.StopContext(context.Background()) }

// StopContext cancels admission and in-flight HTTP requests, and bounds waiting
// by the caller's deadline. A timed-out stop remains canceled/started until a
// later StopContext joins it, preventing an overlapping restart.
func (n *Notifier) StopContext(ctx context.Context) error {
	if err := lockNotify(ctx, &n.lifecycleMu); err != nil {
		return err
	}
	defer n.lifecycleMu.Unlock()
	if !n.started {
		return nil
	}
	n.stopCancel()
	done := n.workerDone
	select {
	case <-done:
		n.started = false
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func lockNotify(ctx context.Context, mu *sync.Mutex) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for !mu.TryLock() {
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	if err := ctx.Err(); err != nil {
		mu.Unlock()
		return err
	}
	return nil
}

// NotificationKindStats is the per-kind healthz block.
type NotificationKindStats struct {
	Pending      int64 `json:"pending"`
	Delivered    int64 `json:"delivered"`
	Suppressed   int64 `json:"suppressed"`
	DeadLettered int64 `json:"dead_lettered"`
	Failed       int64 `json:"failed"`
}

// NotificationStatsReport is the /healthz "notifications" block.
type NotificationStatsReport struct {
	ByKind map[string]NotificationKindStats `json:"by_kind"`
}

// Stats snapshots per-kind counters. pending/delivered/suppressed/
// dead_lettered are read from the table (restart-honest); failed counts
// failed delivery attempts since this process started.
func (n *Notifier) Stats(ctx context.Context) NotificationStatsReport {
	report := NotificationStatsReport{ByKind: map[string]NotificationKindStats{}}
	rows, err := nucleus.Query[struct {
		Kind       string `db:"kind"`
		Delivered  string `db:"delivered"`
		Suppressed string `db:"suppressed"`
		Dead       string `db:"dead"`
		Pending    string `db:"pending"`
	}](ctx, n.db.SQL(),
		`SELECT kind,
			CAST(SUM(CASE WHEN delivered_at > 0 THEN 1 ELSE 0 END) AS TEXT) AS delivered,
			CAST(SUM(CASE WHEN suppressed_at > 0 THEN 1 ELSE 0 END) AS TEXT) AS suppressed,
			CAST(SUM(CASE WHEN delivered_at = 0 AND suppressed_at = 0 AND next_attempt_at < 0 THEN 1 ELSE 0 END) AS TEXT) AS dead,
			CAST(SUM(CASE WHEN delivered_at = 0 AND suppressed_at = 0 AND next_attempt_at >= 0 THEN 1 ELSE 0 END) AS TEXT) AS pending
		 FROM (`+notifyCollapseSelect("")+`)
		 GROUP BY kind`,
	)
	if err != nil {
		n.logger.Error("notification: stats scan failed", "err", err)
		return report
	}
	for _, r := range rows {
		var ks NotificationKindStats
		fmt.Sscanf(r.Delivered, "%d", &ks.Delivered)
		fmt.Sscanf(r.Suppressed, "%d", &ks.Suppressed)
		fmt.Sscanf(r.Dead, "%d", &ks.DeadLettered)
		fmt.Sscanf(r.Pending, "%d", &ks.Pending)
		if c := n.failedCount(r.Kind); c != nil {
			ks.Failed = c.Load()
		}
		report.ByKind[r.Kind] = ks
	}
	return report
}

func (n *Notifier) bumpFailed(kind string) {
	n.failedMu.Lock()
	defer n.failedMu.Unlock()
	c, ok := n.failedAttempts[kind]
	if !ok {
		c = &atomic.Int64{}
		n.failedAttempts[kind] = c
	}
	c.Add(1)
}

func (n *Notifier) failedCount(kind string) *atomic.Int64 {
	n.failedMu.Lock()
	defer n.failedMu.Unlock()
	return n.failedAttempts[kind]
}

// NotificationPayload is the frozen JSON body of a notification intent.
// It is self-describing without any AI layer: what broke, where, how badly,
// the measured value against the threshold, and the incident to group by.
type NotificationPayload struct {
	Kind        string  `json:"kind"`
	IncidentID  string  `json:"incident_id"`
	RuleID      string  `json:"rule_id"`
	RuleName    string  `json:"rule_name"`
	Metric      string  `json:"metric"`
	Value       float64 `json:"value"`
	Threshold   string  `json:"threshold"`
	SiteID      string  `json:"site_id"`
	Severity    string  `json:"severity"`
	Samples     int64   `json:"samples"`
	StateFrom   string  `json:"state_from"`
	StateTo     string  `json:"state_to"`
	EvaluatedAt string  `json:"evaluated_at"`
	Message     string  `json:"message"`
}

// BuildNotificationPayload freezes the payload for one intent.
func BuildNotificationPayload(kind string, rule AlertRule, value float64, samples int64, from, to string, incidentID string, at time.Time) string {
	p := NotificationPayload{
		Kind: kind, IncidentID: incidentID, RuleID: rule.RuleID, RuleName: rule.Name,
		Metric: rule.Metric, Value: value,
		Threshold: strconv.FormatFloat(rule.Threshold, 'f', -1, 64),
		SiteID:    rule.SiteID, Severity: rule.Severity, Samples: samples,
		StateFrom: from, StateTo: to, EvaluatedAt: at.UTC().Format(time.RFC3339),
	}
	switch kind {
	case NotifyIncidentOpened:
		p.Message = fmt.Sprintf("%s is FIRING: %s = %.2f over the last %d min(s), threshold %s %s (severity %s)",
			rule.Name, rule.Metric, value, rule.WindowMinutes, rule.Operator, p.Threshold, rule.Severity)
	case NotifyIncidentRecovered:
		p.Message = fmt.Sprintf("%s RECOVERED: %s = %.2f is back within %s %s",
			rule.Name, rule.Metric, value, rule.Operator, p.Threshold)
	case NotifyIncidentRepeat:
		p.Message = fmt.Sprintf("%s is STILL FIRING: %s = %.2f, threshold %s %s (incident %s ongoing)",
			rule.Name, rule.Metric, value, rule.Operator, p.Threshold, incidentID)
	}
	raw, _ := json.Marshal(p)
	return string(raw)
}

// IssueNotification is the frozen JSON body of an issue_* notification
// intent (EV1, 066). Self-describing for a machine receiver: the grouping
// identity (fingerprint), where it happened (site/service/release/
// environment), how bad (counts, first/last seen), and a scrubbed sample
// stack clipped to issueStackMaxBytes so a receiver can show cause without
// a second read. Slack-type targets never receive the stack (postSlackIssue).
type IssueNotification struct {
	Kind        string `json:"kind"`
	DeliverySeq int64  `json:"delivery_seq"`
	IssueID     string `json:"issue_id"`
	SiteID      string `json:"site_id"`
	// Service names the failing component. There is no service tag on the
	// error wire yet, so v1 mirrors site_id; the field exists so a later
	// event tag can fill it without a payload breaking change.
	Service            string `json:"service"`
	Fingerprint        string `json:"fingerprint"`
	FingerprintVersion int    `json:"fingerprint_version"`
	Title              string `json:"title"`
	Culprit            string `json:"culprit"`
	Level              string `json:"level"`
	// Release is the TRIGGERING event's release; FirstRelease is the
	// issue's first-seen release (issues.release_tag) - the pair tells a
	// receiver whether the error followed a deploy.
	FirstRelease string `json:"first_release"`
	Release      string `json:"release"`
	Environment  string `json:"environment"`
	// EventCount/UserCount are re-read from the issue at emit time (real
	// numbers, not the cache's racey counter). WindowCount is the spike
	// window's event count (issue_spike only; 0 otherwise).
	EventCount  int64 `json:"event_count"`
	UserCount   int64 `json:"user_count"`
	WindowCount int64 `json:"window_count"`
	// RegressionCount is the issue's lifetime resolved-then-reopened count.
	RegressionCount int64  `json:"regression_count"`
	FirstSeen       string `json:"first_seen"`
	LastSeen        string `json:"last_seen"`
	// SampleStack is built from the POST-SCRUB frames of the triggering
	// event (scrubbing runs at admission, scrub.go): file:line function
	// lines only - no local variables, no breadcrumbs, no request data -
	// and clipped to IssueStackMaxBytes.
	SampleStack string `json:"sample_stack,omitempty"`
	IssueURL    string `json:"issue_url,omitempty"`
	OccurredAt  string `json:"occurred_at"`
}

// IssueStackMaxBytes caps sample_stack. 2 KB per the EV1 spec; 2000 rather
// than 2048 so it also fits under teploy-ship's DETAIL_STACK_MAX unchanged.
const IssueStackMaxBytes = 2000

// ClipStack bounds a sample stack to at most IssueStackMaxBytes
// (marker included), cutting on a rune boundary and dropping a
// partially-cut trailing line rather than shipping half a frame.
func ClipStack(s string) string {
	if len(s) <= IssueStackMaxBytes {
		return s
	}
	const marker = "\n[clipped]"
	cut := s[:IssueStackMaxBytes-len(marker)]
	// Drop a partial last line: keep everything before its final newline.
	if idx := strings.LastIndex(cut, "\n"); idx > 0 {
		cut = cut[:idx]
	}
	// Rune-boundary trim (a cut inside a multi-byte rune is invalid UTF-8).
	for len(cut) > 0 && !utf8.ValidString(cut[len(cut)-1:]) {
		cut = cut[:len(cut)-1]
	}
	return cut + marker
}

// postSignedJSON POSTs body to url with the stable delivery-id header and,
// when secret is set, the R22 HMAC signature scheme shared with the
// in-memory webhook path:
//
//	X-Observe-Delivery:  the logical delivery id (dedupe marker)
//	X-Observe-Timestamp: unix seconds
//	X-Observe-Signature: sha256=hex(HMAC-SHA256(secret, timestamp + "." + body))
func postSignedJSON(ctx context.Context, client *http.Client, deliveryID, url, secret string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Observe-Delivery", deliveryID)
	if secret != "" {
		ts := strconv.FormatInt(time.Now().UTC().Unix(), 10)
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(ts))
		mac.Write([]byte("."))
		mac.Write(body)
		req.Header.Set("X-Observe-Timestamp", ts)
		req.Header.Set("X-Observe-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("notification target returned %d", resp.StatusCode)
	}
	return nil
}

// genNotificationID mints a delivery id: crypto/rand hex, prefixed so a
// notification id is recognizable in logs and dead letters.
func genNotificationID() string {
	return "nfy-" + genID()
}

// LifecycleHook is the application hook, including incomplete join errors.
func (n *Notifier) LifecycleHook() neutron.LifecycleHook {
	return neutron.LifecycleHook{
		Name:    "notification-outbox",
		OnStart: func(context.Context) error { n.Start(); return nil },
		OnStop:  n.StopContext,
	}
}
