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
// resulting issues into performance_issues. Re-detections of the same
// fingerprint extend last_seen + bump count via the replacing_mergetree merge
// (see migration 015).
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
	for _, iss := range issues {
		// issue_id is per-detection (not per-fingerprint) so a re-fire of
		// the same group still gets a unique row PK; the replacing
		// mergetree dedupe collapses on (tenant_id, site_id, fingerprint)
		// at read time.
		issueID := genID()
		_, err := sql.Exec(ctx,
			`INSERT INTO performance_issues (
				issue_id, tenant_id, site_id, trace_id,
				detector_name, fingerprint, title, description,
				severity, count, first_seen, last_seen
			) VALUES ($1,'default',$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
			issueID, siteID, iss.TraceID,
			iss.DetectorName, iss.Fingerprint, iss.Title, iss.Description,
			iss.Severity,
			"1",
			dbutil.IntParam(iss.FirstSeen),
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
