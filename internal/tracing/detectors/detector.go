// Package detectors finds performance anti-patterns inside batches of OTLP
// spans (n+1 DB queries, slow DB queries, serially-executed sibling DB calls,
// slow outbound HTTP calls). Each detector is independent, runs over the same
// pre-flattened span batch, and emits zero or more Issue values that the
// engine persists into performance_issues.
//
// Detectors are pure: no DB access, no external services, no goroutines.
// Persistence is the engine's job. This keeps every detector trivially
// unit-testable from a hand-built span slice.
package detectors

import (
	"crypto/sha1"
	"encoding/hex"
	"strings"
)

// Span is the detector-side projection of an ingested span. Mirrors the
// fields detectors care about in tracing.flatSpan but kept separate so the
// detectors package has zero internal dependency on the tracing package's
// scan-row structs.
type Span struct {
	TraceID       string
	SpanID        string
	ParentSpanID  string
	ServiceName   string
	OperationName string
	SpanKind      string
	StartMs       int64
	EndMs         int64
	DurationMs    int64
	StatusCode    string
	// Attributes is the OTLP attribute map for this span. Detectors look at
	// db.statement, db.system, http.url, http.method, etc. Empty when no
	// attributes were attached.
	Attributes map[string]string
}

// Issue is one detector finding. The engine groups issues across batches by
// fingerprint into a single performance_issues row.
type Issue struct {
	TraceID      string
	DetectorName string
	Fingerprint  string
	Title        string
	Description  string
	Severity     string
	// FirstSeen / LastSeen are span-time millis. The engine treats them as
	// hints — a re-detection of the same fingerprint extends LastSeen and
	// preserves the existing FirstSeen.
	FirstSeen int64
	LastSeen  int64
}

// Detector inspects a batch of spans and returns any issues found. Detectors
// are stateless across calls; cross-batch grouping is the engine's job via
// the fingerprint replacing-mergetree dedupe.
type Detector interface {
	Name() string
	Detect(spans []Span) []Issue
}

// fingerprintSQL strips literal values from a SQL statement so logically
// identical queries (different ID values, different VARCHAR contents) produce
// the same fingerprint. Returns "" when input has no SELECT/INSERT/UPDATE/
// DELETE prefix so non-SQL statements don't accidentally collide.
//
// Implementation is deliberately simple — quoted strings → '?', numeric
// literals → '?', whitespace runs → single space. Does NOT try to be a SQL
// parser. The Sentry implementation has the same property.
func fingerprintSQL(stmt string) string {
	s := strings.TrimSpace(stmt)
	if s == "" {
		return ""
	}
	upper := strings.ToUpper(s)
	switch {
	case strings.HasPrefix(upper, "SELECT"),
		strings.HasPrefix(upper, "INSERT"),
		strings.HasPrefix(upper, "UPDATE"),
		strings.HasPrefix(upper, "DELETE"):
	default:
		return ""
	}

	var b strings.Builder
	b.Grow(len(s))
	i := 0
	prevSpace := false
	for i < len(s) {
		c := s[i]
		switch {
		case c == '\'':
			// Quoted string literal — replace with ?, skip until the
			// matching close quote.
			b.WriteByte('?')
			i++
			for i < len(s) && s[i] != '\'' {
				if s[i] == '\\' && i+1 < len(s) {
					i += 2
					continue
				}
				i++
			}
			if i < len(s) {
				i++
			}
			prevSpace = false
		case c >= '0' && c <= '9':
			// Numeric literal — collapse the whole digit run to a single ?.
			b.WriteByte('?')
			for i < len(s) && ((s[i] >= '0' && s[i] <= '9') || s[i] == '.') {
				i++
			}
			prevSpace = false
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
			i++
		default:
			b.WriteByte(c)
			prevSpace = false
			i++
		}
	}
	return strings.TrimSpace(b.String())
}

// hashFingerprint produces a stable 16-hex-char hash for a list of parts.
// SHA1-truncated is enough — collisions across detector outputs would just
// merge two issues, not corrupt anything.
func hashFingerprint(parts ...string) string {
	h := sha1.New()
	for i, p := range parts {
		if i > 0 {
			h.Write([]byte{0})
		}
		h.Write([]byte(p))
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum)[:16]
}

// isDBSpan reports whether a span looks like a database call. Falls back to
// span name prefix when db.system isn't set (the seeder uses naked names like
// "db.query users").
func isDBSpan(s Span) bool {
	if s.Attributes["db.system"] != "" {
		return true
	}
	if s.Attributes["db.statement"] != "" {
		return true
	}
	op := strings.ToLower(s.OperationName)
	return strings.HasPrefix(op, "db.") || strings.HasPrefix(op, "sql.")
}

// isHTTPClientSpan reports whether a span looks like an outbound HTTP call.
func isHTTPClientSpan(s Span) bool {
	if s.SpanKind != "client" {
		return false
	}
	if s.Attributes["http.url"] != "" || s.Attributes["http.method"] != "" {
		return true
	}
	op := strings.ToLower(s.OperationName)
	return strings.HasPrefix(op, "http.") || strings.HasPrefix(op, "http ")
}

// dbStatement returns the canonical SQL text for fingerprinting. Prefers
// db.statement, falls back to operation_name (the seeder leans on operation
// names for dependency labelling so it's a useful default).
func dbStatement(s Span) string {
	if v := s.Attributes["db.statement"]; v != "" {
		return v
	}
	return s.OperationName
}
