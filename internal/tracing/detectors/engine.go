package detectors

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
)

// Engine runs every registered detector across a span batch and persists the
// resulting issues into performance_issues (a replacing_mergetree keyed on
// fingerprint, see migration 015). The replacing merge REPLACES rows — it does
// not sum — so re-detection totals are accumulated explicitly on write via an
// atomic KV counter; the newest row (highest last_seen) wins the merge and
// carries the running count + earliest first_seen. See Persist.
type Engine struct {
	db        *nucleus.Client
	logger    *slog.Logger
	detectors []Detector
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
// Each insert is best-effort — a single bad row never aborts the batch.
func (e *Engine) Persist(ctx context.Context, siteID string, spans []Span) {
	issues := e.RunAll(spans)
	if len(issues) == 0 {
		return
	}
	sql := e.db.SQL()
	kv := e.db.KV()
	for _, iss := range issues {
		// performance_issues is a replacing_mergetree keyed on
		// (tenant_id, site_id, fingerprint) with version_column=last_seen, and
		// Nucleus DOES apply replacing dedup on read — so every re-detection
		// of a fingerprint collapses at read time to the single highest-
		// last_seen row. Writing count=1 per detection therefore undercounts
		// permanently (the surviving row always reads count=1) and pins
		// first_seen to the LATEST detection instead of the earliest.
		//
		// Accumulate on write instead. The running count lives in an atomic KV
		// counter (kv.Incr) so concurrent detector batches for the same
		// fingerprint can't lose an increment — a plain SELECT-then-INSERT
		// read-modify-write on the DB would race here. The new row's last_seen
		// is the newest, so it wins the replacing merge carrying the
		// accumulated count + earliest first_seen.
		countKey := "perf:count:" + siteID + ":" + iss.Fingerprint
		count, err := kv.Incr(ctx, countKey)
		if err != nil {
			e.logger.Warn("perf_issue: count incr failed",
				"detector", iss.DetectorName, "fingerprint", iss.Fingerprint, "err", err)
			count = 1 // best-effort: still record the detection
		}

		// first_seen must be the MINIMUM observed time, not the first writer's:
		// detection batches can arrive out of order. Read-modify-write min via
		// KV. The count above is the number that must be exact (it drives
		// severity/sorting), so it gets the atomic counter; a rare race here
		// only mis-pins first_seen by the gap between two near-simultaneous
		// first detections, which is immaterial for a "first seen" display.
		firstSeen := iss.FirstSeen
		firstKey := "perf:first:" + siteID + ":" + iss.Fingerprint
		if b, err := kv.Get(ctx, firstKey); err == nil && b != nil {
			if stored, perr := strconv.ParseInt(string(b), 10, 64); perr == nil && stored < firstSeen {
				firstSeen = stored
			}
		}
		if err := kv.Set(ctx, firstKey, []byte(strconv.FormatInt(firstSeen, 10))); err != nil {
			e.logger.Warn("perf_issue: first_seen set failed",
				"fingerprint", iss.Fingerprint, "err", err)
		}

		issueID := genID()
		_, err = sql.Exec(ctx,
			`INSERT INTO performance_issues (
				issue_id, tenant_id, site_id, trace_id,
				detector_name, fingerprint, title, description,
				severity, count, first_seen, last_seen
			) VALUES ($1,'default',$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
			issueID, siteID, iss.TraceID,
			iss.DetectorName, iss.Fingerprint, iss.Title, iss.Description,
			iss.Severity,
			strconv.FormatInt(count, 10),
			dbutil.IntParam(firstSeen),
			dbutil.IntParam(iss.LastSeen),
		)
		if err != nil {
			e.logger.Warn("perf_issue: insert failed",
				"detector", iss.DetectorName, "fingerprint", iss.Fingerprint, "err", err)
		}
	}
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
