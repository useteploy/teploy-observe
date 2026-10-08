package detectors

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
)

// Engine projects detections into versioned performance issue snapshots.
// One process serializes read/update/write; event times remain min/max and
// a separate monotonic persistence version selects the current snapshot.
type Engine struct {
	db        *nucleus.Client
	logger    *slog.Logger
	detectors []Detector
	mu        sync.Mutex
}

// New returns an Engine wired with the default four-detector suite. Callers
// can build a different suite via NewWithDetectors.
func New(db *nucleus.Client) *Engine {
	return NewWithDetectors(db, DefaultDetectors())
}

func NewWithDetectors(db *nucleus.Client, detectors []Detector) *Engine {
	return &Engine{db: db, detectors: detectors, logger: slog.Default()}
}

// WithLogger threads a custom logger so per-engine warnings show up under
// the same handler context as the rest of the ingest path.
func (e *Engine) WithLogger(logger *slog.Logger) *Engine {
	if logger == nil {
		return e
	}
	e.logger = logger
	return e
}

// DefaultDetectors returns the suite that ships out of the box.
func DefaultDetectors() []Detector {
	return []Detector{
		NewNPlusOneDB(),
		NewSlowDBQuery(),
		NewConsecutiveDB(),
		NewSlowHTTPCall(),
	}
}

// RunAll runs every detector over the batch and returns the union of issues.
// Pure — no side effects, no DB access. Used by Persist and by tests.
func (e *Engine) RunAll(spans []Span) []Issue {
	var out []Issue
	for _, d := range e.detectors {
		out = append(out, d.Detect(spans)...)
	}
	return out
}

// Persist runs every detector and writes any findings into performance_issues.
// Legacy direct path (tests, pre-outbox callers): accumulation semantics, no
// per-intent dedupe markers. The outbox worker uses WriteIssues, which is
// idempotent under at-least-once delivery.
func (e *Engine) Persist(ctx context.Context, siteID string, spans []Span) {
	_ = e.WriteIssues(ctx, "", siteID, e.RunAll(spans))
}

// WriteIssues persists pre-computed findings into performance_issues and
// returns the first failure so the outbox worker can retry the intent.
//
// Contributions count individual detections, aggregated once per fingerprint
// within each intent. Completion markers make healthy retries idempotent.
// A crash after SQL commit but before marker commit can still overcount on
// retry; the documented write-then-mark residual is retained.
func (e *Engine) WriteIssues(ctx context.Context, intentID, siteID string, issues []Issue) error {
	if len(issues) == 0 {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	sql := e.db.SQL()
	kv := e.db.KV()
	for _, iss := range aggregateIssues(issues) {
		doneKey := "outbox:perf:" + intentID + ":" + iss.Fingerprint
		if intentID != "" {
			b, err := kv.Get(ctx, doneKey)
			if err != nil {
				return fmt.Errorf("perf marker read: %w", err)
			}
			if string(b) == "1" {
				continue
			}
		}
		previous, err := nucleus.Query[issueSnapshot](ctx, sql,
			`SELECT count, first_seen, last_seen, version, severity, trace_id, title, description FROM performance_issues WHERE site_id = $1 AND fingerprint = $2 ORDER BY version DESC LIMIT 1`, siteID, iss.Fingerprint)
		if err != nil {
			return fmt.Errorf("perf snapshot read: %w", err)
		}
		if len(previous) > 0 && previous[0].LastSeen > iss.LastSeen {
			iss.TraceID, iss.Title, iss.Description = previous[0].TraceID, previous[0].Title, previous[0].Description
		}
		count, firstSeen, lastSeen, version, severity := nextSnapshot(iss, previous, time.Now().UnixNano())
		_, err = sql.Exec(ctx,
			`INSERT INTO performance_issues (
				issue_id, tenant_id, site_id, trace_id,
				detector_name, fingerprint, title, description,
				severity, count, first_seen, last_seen, version
			) VALUES ($1,'default',$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
			genID(), siteID, iss.TraceID, iss.DetectorName, iss.Fingerprint, iss.Title, iss.Description,
			severity, strconv.FormatInt(count, 10), dbutil.IntParam(firstSeen), dbutil.IntParam(lastSeen), dbutil.IntParam(version))
		if err != nil {
			return fmt.Errorf("perf snapshot write: %w", err)
		}
		if intentID != "" {
			if err := kv.Set(ctx, doneKey, []byte("1")); err != nil {
				return fmt.Errorf("perf marker write: %w", err)
			}
		}
	}
	return nil
}

// genID is a small wrapper around crypto/rand for opaque issue IDs. Hex-
// encoded so it survives string-typed PK columns without escaping concerns.
func genID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure on a healthy host is exotic; fall back to a
		// timestamp-only ID rather than panicking inside the ingest path.
		return "perf-" + strconv.FormatInt(int64(b[0]), 16)
	}
	return "perf-" + hex.EncodeToString(b[:])
}

// Compile-time check that Engine satisfies the runtime "log on bad sql" path
// without a Persist call (helps callers stub the engine in tests).
var _ = fmt.Sprintf

type issueSnapshot struct {
	TraceID     string `db:"trace_id"`
	Title       string `db:"title"`
	Description string `db:"description"`
	Count       int64  `db:"count"`
	FirstSeen   int64  `db:"first_seen"`
	LastSeen    int64  `db:"last_seen"`
	Version     int64  `db:"version"`
	Severity    string `db:"severity"`
}

func severityRank(s string) int {
	switch s {
	case "fatal":
		return 3
	case "error":
		return 2
	case "warning":
		return 1
	}
	return 0
}
func aggregateIssues(issues []Issue) []Issue {
	groups := map[string]Issue{}
	for _, iss := range issues {
		if iss.Occurrences <= 0 {
			iss.Occurrences = 1
		}
		prev, ok := groups[iss.Fingerprint]
		if !ok {
			groups[iss.Fingerprint] = iss
			continue
		}
		count := prev.Occurrences + iss.Occurrences
		first, last := prev.FirstSeen, prev.LastSeen
		if iss.FirstSeen < first {
			first = iss.FirstSeen
		}
		if iss.LastSeen > last {
			last = iss.LastSeen
		}
		sev := prev.Severity
		if severityRank(iss.Severity) > severityRank(sev) {
			sev = iss.Severity
		}
		if iss.LastSeen > prev.LastSeen || (iss.LastSeen == prev.LastSeen && iss.TraceID < prev.TraceID) {
			prev = iss
		}
		prev.Occurrences, prev.FirstSeen, prev.LastSeen, prev.Severity = count, first, last, sev
		groups[iss.Fingerprint] = prev
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]Issue, 0, len(keys))
	for _, k := range keys {
		out = append(out, groups[k])
	}
	return out
}
func nextSnapshot(iss Issue, previous []issueSnapshot, now int64) (count, first, last, version int64, severity string) {
	count, first, last, version, severity = iss.Occurrences, iss.FirstSeen, iss.LastSeen, now, iss.Severity
	for _, p := range previous {
		count += p.Count
		if p.FirstSeen < first {
			first = p.FirstSeen
		}
		if p.LastSeen > last {
			last = p.LastSeen
		}
		if p.Version >= version {
			version = p.Version + 1
		}
		if severityRank(p.Severity) > severityRank(severity) {
			severity = p.Severity
		}
	}
	return
}
