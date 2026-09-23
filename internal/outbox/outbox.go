// Package outbox implements the derived-work outbox (O01 ADR section 5.7).
//
// Originating state changes (today: trace span batches) commit their
// derived-work intents in the SAME transaction as the state itself: Enqueue
// takes a transaction-scoped SQLModel and contributes one row to that
// transaction's atomic unit, so an intent can never orphan ahead of the
// spans it describes and spans can never commit without their intents. A
// worker drains due intents idempotently — handlers derive from the frozen
// payload, so at-least-once delivery (crash mid-derive, restart resume,
// duplicate drain) reproduces identical output.
//
// Failure disposition: a failing derive retries with exponential backoff and
// after the attempt budget the row stays as a dead letter — kept with
// last_error, excluded from selection, counted at Stats — never silently
// dropped. Non-idempotent side effects key their dedupe markers on the
// intent id (see detectors.WriteIssues; webhook deliveries will reuse R22's
// stable delivery id the same way).
//
// Boundary: single-process, like every standing serialization in this repo
// (AUD-018 posture). The worker serializes in-process; a multi-replica
// deployment needs a lease/CAS claim before externally-visible kinds ride
// this table. Cost note: the due scan collapses the whole table (argMax per
// intent id) — processed intents accumulate until slice 5's retention policy
// (ADR section 5.8) prunes them, same posture as replay_batches and
// error_inbox.
package outbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
)

// Intent kinds. rollup + detector are wired (trace ingest); heatmap and
// webhook are reserved for the next slices (see migration 046).
const (
	KindTraceRollup = "rollup"
	KindTraceDetect = "detector"
	KindHeatmap     = "heatmap"
	KindWebhook     = "webhook"
)

// Worker defaults.
const (
	defaultMaxAttempts = 5
	defaultPollEvery   = 2 * time.Second
	defaultBatchSize   = 500
	maxLastErrorChars  = 1024
)

// Handler derives one intent's output from its frozen payload. intentID is
// the durable identity of the intent: non-idempotent side effects (KV count
// accumulation, external sends) key their dedupe markers on it. Returning an
// error schedules a retry; the payload is never rewritten.
type Handler func(ctx context.Context, intentID, siteID string, payload []byte) error

// Store owns the derived_outbox table: enqueue (inside originator
// transactions) plus the drain worker and healthz counters.
type Store struct {
	db       *nucleus.Client
	logger   *slog.Logger
	handlers map[string]Handler

	maxAttempts int
	backoffBase time.Duration
	backoffCap  time.Duration
	pollEvery   time.Duration
	batchSize   int

	// processMu serializes intent processing (the single-process claim):
	// the worker loop, the seed path's synchronous ProcessIDs, and any
	// concurrent ProcessDue caller take turns rather than double-deriving.
	processMu sync.Mutex

	lifecycleMu sync.Mutex
	started     bool
	stopCtx     context.Context
	stopCancel  context.CancelFunc
	wg          sync.WaitGroup

	failedMu       sync.Mutex
	failedAttempts map[string]*atomic.Int64
}

// New builds a Store with production defaults. Tests tune the retry policy
// through the With* options.
func New(db *nucleus.Client, logger *slog.Logger) *Store {
	if logger == nil {
		logger = slog.Default()
	}
	return &Store{
		db:             db,
		logger:         logger,
		handlers:       map[string]Handler{},
		maxAttempts:    defaultMaxAttempts,
		backoffBase:    time.Second,
		backoffCap:     5 * time.Minute,
		pollEvery:      defaultPollEvery,
		batchSize:      defaultBatchSize,
		failedAttempts: map[string]*atomic.Int64{},
	}
}

// Register binds a handler to a kind. An intent whose kind has no handler
// fails processing (and eventually dead-letters) rather than vanishing.
func (s *Store) Register(kind string, h Handler) {
	s.handlers[kind] = h
}

// WithMaxAttempts sets the attempt budget before an intent dead-letters.
func (s *Store) WithMaxAttempts(n int) *Store { s.maxAttempts = n; return s }

// WithBackoffBase sets the first retry delay (doubling, capped). Zero makes
// retries immediately due (tests).
func (s *Store) WithBackoffBase(d time.Duration) *Store { s.backoffBase = d; return s }

// WithBackoffCap sets the retry-delay ceiling.
func (s *Store) WithBackoffCap(d time.Duration) *Store { s.backoffCap = d; return s }

// WithPollInterval sets the worker drain cadence.
func (s *Store) WithPollInterval(d time.Duration) *Store { s.pollEvery = d; return s }

// backoff returns the retry delay after the given failed attempt count.
func (s *Store) backoff(attempts int64) time.Duration {
	if s.backoffBase <= 0 || attempts < 1 {
		return 0
	}
	d := s.backoffBase
	for i := int64(1); i < attempts && d < s.backoffCap; i++ {
		d *= 2
	}
	if d > s.backoffCap {
		d = s.backoffCap
	}
	return d
}

// intentRow is a collapsed (latest-version) view of one intent.
type intentRow struct {
	ID            string `db:"id"`
	Kind          string `db:"kind"`
	SiteID        string `db:"site_id"`
	Payload       string `db:"payload"`
	CreatedAt     int64  `db:"created_at"`
	Attempts      int64  `db:"attempts"`
	NextAttemptAt int64  `db:"next_attempt_at"`
	ProcessedAt   int64  `db:"processed_at"`
	LastError     string `db:"last_error"`
	Version       int64  `db:"version"`
}

// collapseSelect renders the argMax collapse over derived_outbox — one
// logical row per intent id, highest version wins (the form verified
// against the live engine; see internal/query/replacing.go).
func collapseSelect(where string) string {
	if strings.TrimSpace(where) == "" {
		where = "1 = 1"
	}
	return `SELECT tenant_id, id,
			argMax(kind, version) AS kind,
			argMax(site_id, version) AS site_id,
			argMax(payload, version) AS payload,
			argMax(created_at, version) AS created_at,
			argMax(attempts, version) AS attempts,
			argMax(next_attempt_at, version) AS next_attempt_at,
			argMax(processed_at, version) AS processed_at,
			argMax(last_error, version) AS last_error,
			MAX(version) AS version
		FROM derived_outbox WHERE ` + where + `
		GROUP BY tenant_id, id`
}

// Enqueue writes one intent row through sql - which MUST be the same
// transaction-scoped SQLModel as the originating state change, so the intent
// commits or rolls back with it. payload must be JSON-marshalable (nil
// enqueues an empty payload); it is frozen verbatim and never rewritten.
// Returns the minted intent id.
func (s *Store) Enqueue(ctx context.Context, sql *nucleus.SQLModel, siteID, kind string, payload any) (string, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("outbox: marshal payload for kind %s: %w", kind, err)
	}
	id := genID()
	now := time.Now().UTC().UnixMilli()
	if _, err := sql.Exec(ctx,
		`INSERT INTO derived_outbox (
			tenant_id, id, kind, site_id, payload, created_at,
			attempts, next_attempt_at, processed_at, last_error, version
		) VALUES ('default', $1, $2, $3, $4, $5, 0, 0, 0, '', $5)`,
		id, kind, siteID, string(raw), dbutil.IntParam(now),
	); err != nil {
		return "", fmt.Errorf("outbox: enqueue %s intent: %w", kind, err)
	}
	return id, nil
}

// ProcessDue runs one drain pass over due intents (pending, under the
// attempt budget, retry time reached), looping until the table stops
// yielding. Per-intent failures are recorded on their rows; only a scan
// failure returns an error. The return value counts processed intents
// (successes and failures alike — each was worked on).
func (s *Store) ProcessDue(ctx context.Context) (int, error) {
	s.processMu.Lock()
	defer s.processMu.Unlock()
	total := 0
	for {
		rows, err := s.due(ctx)
		if err != nil {
			return total, fmt.Errorf("outbox: due scan: %w", err)
		}
		if len(rows) == 0 {
			return total, nil
		}
		for i := range rows {
			s.processOne(ctx, &rows[i])
			total++
		}
		if len(rows) < s.batchSize {
			return total, nil
		}
	}
}

// ProcessIDs processes specific intents regardless of due-ness (the seed
// path's synchronous derive). Intents already processed are re-derived —
// handlers are idempotent — which is also the duplicate-delivery probe the
// oracle uses.
func (s *Store) ProcessIDs(ctx context.Context, ids ...string) error {
	if len(ids) == 0 {
		return nil
	}
	s.processMu.Lock()
	defer s.processMu.Unlock()
	for _, id := range ids {
		row, err := s.byID(ctx, id)
		if err != nil {
			return fmt.Errorf("outbox: read intent %s: %w", id, err)
		}
		if row == nil {
			continue
		}
		s.processOne(ctx, row)
	}
	return nil
}

// due selects the next batch of due intents through the collapse. A dead
// letter (next_attempt_at = -1) is never selected regardless of any
// process's attempt budget — dead is a durable row state, not a property of
// the current worker's config.
func (s *Store) due(ctx context.Context) ([]intentRow, error) {
	return nucleus.Query[intentRow](ctx, s.db.SQL(),
		`SELECT id, kind, site_id, payload, created_at, attempts, next_attempt_at, processed_at, last_error, version
		 FROM (`+collapseSelect("")+`)
		 WHERE processed_at = 0
		   AND next_attempt_at >= 0 AND next_attempt_at <= $1
		   AND attempts < $2
		 ORDER BY created_at, id
		 LIMIT $3`,
		dbutil.IntParam(time.Now().UTC().UnixMilli()), s.maxAttempts, s.batchSize,
	)
}

// byID reads one collapsed intent; nil when the id is unknown.
func (s *Store) byID(ctx context.Context, id string) (*intentRow, error) {
	rows, err := nucleus.Query[intentRow](ctx, s.db.SQL(),
		`SELECT id, kind, site_id, payload, created_at, attempts, next_attempt_at, processed_at, last_error, version
		 FROM (`+collapseSelect("id = $1")+`)`,
		id,
	)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

// processOne invokes the kind's handler and records the disposition as a new
// row version (strictly-monotonic stamp: max(now, prior+1) — the version-tie
// convention). A missing handler is a wiring failure: recorded as an error,
// never a silent skip.
func (s *Store) processOne(ctx context.Context, row *intentRow) {
	h, ok := s.handlers[row.Kind]
	var err error
	if !ok {
		err = fmt.Errorf("no handler registered for kind %q", row.Kind)
	} else {
		err = invoke(ctx, h, row.ID, row.SiteID, []byte(row.Payload))
	}

	if err == nil {
		if derr := s.markProcessed(ctx, row); derr != nil {
			s.logger.Error("outbox: mark processed failed",
				"intent", row.ID, "kind", row.Kind, "err", derr)
		}
		return
	}

	attempts := row.Attempts + 1
	s.bumpFailed(row.Kind)
	dead := attempts >= int64(s.maxAttempts)
	next := int64(0)
	if dead {
		// Durable dead letter: the sentinel is below any clock value, so no
		// later process — even one configured with a larger budget — retries
		// it. The row keeps last_error as the operator's evidence.
		next = -1
	} else {
		next = time.Now().UTC().UnixMilli() + s.backoff(attempts).Milliseconds()
	}
	if derr := s.markAttempt(ctx, row, attempts, next, err); derr != nil {
		s.logger.Error("outbox: record failure failed",
			"intent", row.ID, "kind", row.Kind, "err", derr)
	}
	if dead {
		s.logger.Error("outbox: intent dead-lettered — derived work keeps failing",
			"intent", row.ID, "kind", row.Kind, "site", row.SiteID,
			"attempts", attempts, "err", err)
	}
}

// invoke runs a handler, converting a panic into a recorded failure (a
// panicking derive must not kill the worker).
func invoke(ctx context.Context, h Handler, intentID, siteID string, payload []byte) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("handler panicked: %v", r)
		}
	}()
	return h(ctx, intentID, siteID, payload)
}

// markProcessed writes the processed disposition.
func (s *Store) markProcessed(ctx context.Context, row *intentRow) error {
	now := time.Now().UTC().UnixMilli()
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO derived_outbox (
			tenant_id, id, kind, site_id, payload, created_at,
			attempts, next_attempt_at, processed_at, last_error, version
		) VALUES ('default', $1, $2, $3, $4, $5, $6, $7, $8, '', $9)`,
		row.ID, row.Kind, row.SiteID, row.Payload, dbutil.IntParam(row.CreatedAt),
		dbutil.IntParam(row.Attempts), dbutil.IntParam(row.NextAttemptAt),
		dbutil.IntParam(now), dbutil.IntParam(nextVersion(row.Version, now)),
	)
	return err
}

// markAttempt writes the failure disposition (attempts bumped, backoff
// scheduled, last_error kept — the dead letter's evidence).
func (s *Store) markAttempt(ctx context.Context, row *intentRow, attempts, nextAt int64, cause error) error {
	now := time.Now().UTC().UnixMilli()
	lastErr := cause.Error()
	if len(lastErr) > maxLastErrorChars {
		lastErr = lastErr[:maxLastErrorChars]
	}
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO derived_outbox (
			tenant_id, id, kind, site_id, payload, created_at,
			attempts, next_attempt_at, processed_at, last_error, version
		) VALUES ('default', $1, $2, $3, $4, $5, $6, $7, 0, $8, $9)`,
		row.ID, row.Kind, row.SiteID, row.Payload, dbutil.IntParam(row.CreatedAt),
		dbutil.IntParam(attempts), dbutil.IntParam(nextAt),
		lastErr, dbutil.IntParam(nextVersion(row.Version, now)),
	)
	return err
}

// nextVersion stamps a strictly-greater version than prior (the version-tie
// defect fix: two worker writes inside one millisecond must not tie, or the
// argMax collapse resolves the tie arbitrarily).
func nextVersion(prior, now int64) int64 {
	if now > prior+1 {
		return now
	}
	return prior + 1
}

// Start launches the background drain loop: an immediate pass (startup
// resume of the previous process's pending intents) then a fixed cadence.
// Idempotent; Stop cancels and waits for the in-flight intent.
func (s *Store) Start() {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.started {
		return
	}
	s.started = true
	s.stopCtx, s.stopCancel = context.WithCancel(context.Background())
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.run(s.stopCtx)
	}()
}

func (s *Store) run(ctx context.Context) {
	ticker := time.NewTicker(s.pollEvery)
	defer ticker.Stop()
	for {
		if _, err := s.ProcessDue(ctx); err != nil && ctx.Err() == nil {
			s.logger.Error("outbox: drain pass failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Stop cancels the worker and waits for the in-flight intent to finish.
// Pending intents survive the stop and resume on the next Start.
func (s *Store) Stop() {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if !s.started {
		return
	}
	s.started = false
	s.stopCancel()
	s.wg.Wait()
}

// KindStats is the per-kind healthz block (ADR section 5.10 shape).
type KindStats struct {
	Pending      int64 `json:"pending"`
	Processed    int64 `json:"processed"`
	Failed       int64 `json:"failed"`
	DeadLettered int64 `json:"dead_lettered"`
}

// StatsReport is the /healthz "outbox" block.
type StatsReport struct {
	Kinds map[string]KindStats `json:"by_kind"`
}

// Stats snapshots per-kind counters. pending/processed/dead_lettered are
// read from the table (restart-honest); failed counts failed handler
// attempts since this process started.
func (s *Store) Stats(ctx context.Context) StatsReport {
	report := StatsReport{Kinds: map[string]KindStats{}}
	rows, err := nucleus.Query[struct {
		Kind      string `db:"kind"`
		Processed string `db:"processed"`
		Dead      string `db:"dead"`
		Pending   string `db:"pending"`
	}](ctx, s.db.SQL(),
		`SELECT kind,
			CAST(SUM(CASE WHEN processed_at > 0 THEN 1 ELSE 0 END) AS TEXT) AS processed,
			CAST(SUM(CASE WHEN processed_at = 0 AND next_attempt_at < 0 THEN 1 ELSE 0 END) AS TEXT) AS dead,
			CAST(SUM(CASE WHEN processed_at = 0 AND next_attempt_at >= 0 THEN 1 ELSE 0 END) AS TEXT) AS pending
		 FROM (`+collapseSelect("")+`)
		 GROUP BY kind`,
	)
	if err != nil {
		s.logger.Error("outbox: stats scan failed", "err", err)
		return report
	}
	for _, r := range rows {
		var ks KindStats
		fmt.Sscanf(r.Processed, "%d", &ks.Processed)
		fmt.Sscanf(r.Dead, "%d", &ks.DeadLettered)
		fmt.Sscanf(r.Pending, "%d", &ks.Pending)
		if c := s.failedCount(r.Kind); c != nil {
			ks.Failed = c.Load()
		}
		report.Kinds[r.Kind] = ks
	}
	return report
}

func (s *Store) bumpFailed(kind string) {
	s.failedMu.Lock()
	defer s.failedMu.Unlock()
	c, ok := s.failedAttempts[kind]
	if !ok {
		c = &atomic.Int64{}
		s.failedAttempts[kind] = c
	}
	c.Add(1)
}

func (s *Store) failedCount(kind string) *atomic.Int64 {
	s.failedMu.Lock()
	defer s.failedMu.Unlock()
	return s.failedAttempts[kind]
}

// genID mints an intent id: crypto/rand hex, prefixed so an outbox id is
// recognizable in logs and dead letters.
func genID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure on a healthy host is exotic; a timestamp-based
		// id keeps the ingest transaction alive rather than panicking inside
		// it. Same fallback posture as the detectors' genID.
		return "obx-" + fmt.Sprintf("%x", time.Now().UTC().UnixNano())
	}
	return "obx-" + hex.EncodeToString(b[:])
}
