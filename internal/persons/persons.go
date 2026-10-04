// Package persons aggregates events.distinct_id into a per-user view.
//
// C2 (Wave 4, 2026-05-10) — PostHog parity. The schema columns the SDK
// identify() contract needs already landed in migration 018; this
// package is the read layer that turns the column into a "Persons" UI.
//
// Design choices:
//   - Persons are an aggregate over events; the refresh is implicit on
//     every read. Materialization is a phase-2 concern (cohort_members
//     table + periodic refresh per the design doc); v1 keeps the contract
//     simple.
//   - C3 (migration 061) adds SIDE tables keyed by the same person_key:
//     person_properties (identify traits), person_aliases (explicit
//     merge, resolved at read time in Go) and person_tombstones (erasure).
//     See identity.go and docs/IDENTITY_MODEL_ADR.md. Events are never
//     rewritten.
//   - Anonymous rows (distinct_id = ”) are excluded by default. The
//     caller can opt them in with a flag — useful for ops who haven't
//     wired identify() yet and want to see traffic shape.
//   - All aggregates scan as native int64 in Go (per nucleus dogfood
//     finding #24). BIGINT comparisons wrap both sides in CAST AS
//     BIGINT (per finding #6).
package persons

import (
	"context"
	"sync"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"
)

// Service exposes person aggregation queries.
type Service struct {
	db *nucleus.Client
	ev EventSource
	st IdentityStore
	mu sync.Mutex // serializes the read-modify-write paths (single-process boundary)
}

// NewService constructs a persons.Service backed by the shared nucleus client.
func NewService(db *nucleus.Client) *Service {
	return &Service{db: db, ev: &nucleusEvents{db: db}, st: &nucleusIdentity{db: db}}
}

// NewServiceFromParts builds a Service over explicit stores (tests and the
// in-memory implementation in memory.go).
func NewServiceFromParts(ev EventSource, st IdentityStore) *Service {
	return &Service{ev: ev, st: st}
}

// Person is the aggregate returned by ListPersons. Counts are int64 so
// callers can format them without a parse step. *Ms timestamps are
// epoch-milliseconds to match every other Observe API.
type Person struct {
	DistinctID   string `json:"distinct_id"   db:"distinct_id"`
	FirstSeenMs  int64  `json:"first_seen_ms" db:"first_seen_ms"`
	LastSeenMs   int64  `json:"last_seen_ms"  db:"last_seen_ms"`
	EventCount   int64  `json:"event_count"   db:"event_count"`
	SessionCount int64  `json:"session_count" db:"session_count"`
	TopCountry   string `json:"top_country"   db:"top_country"`
	TopBrowser   string `json:"top_browser"   db:"top_browser"`
}

// PersonEvent is a single timeline row for the detail view.
type PersonEvent struct {
	EventID   string `json:"event_id"   db:"event_id"`
	EventType string `json:"event_type" db:"event_type"`
	URL       string `json:"url"        db:"url"`
	Pathname  string `json:"pathname"   db:"pathname"`
	Timestamp int64  `json:"timestamp"  db:"timestamp"`
}

// PersonDetail bundles a person's aggregate row with its event timeline.
//
// C3: Properties holds the identify() traits (merged across the person's
// aliases, canonical wins), Aliases lists the person keys merged into this
// one, and CanonicalKey is the key the requested id resolved to.
type PersonDetail struct {
	Person       Person         `json:"person"`
	Timeline     []PersonEvent  `json:"timeline"`
	Properties   map[string]any `json:"properties"`
	Aliases      []string       `json:"aliases"`
	CanonicalKey string         `json:"canonical_key,omitempty"`
}

// ListPersons returns one row per distinct_id observed in the time
// window for siteID, ordered by last activity descending, WITHOUT alias
// resolution or erasure filtering (use ListResolved for the user-facing
// view). Anonymous (empty distinct_id) rows are excluded unless
// includeAnonymous=true — most operators want the identified-users view,
// but a fresh install without identify() wired needs the all-traffic view
// to verify the page is even rendering.
func (s *Service) ListPersons(ctx context.Context, siteID string, fromMs, toMs int64, limit, offset int, includeAnonymous bool) ([]Person, error) {
	if siteID == "" {
		return []Person{}, nil
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.ev.List(ctx, siteID, fromMs, toMs, limit, offset, includeAnonymous)
	if err != nil {
		return nil, err
	}
	if rows == nil {
		rows = []Person{}
	}
	return rows, nil
}

// CountPersons returns the total distinct_id count in the window (raw: no
// alias resolution or erasure filtering). Cheap helper used by the UI to
// render pagination totals without pulling a second page.
func (s *Service) CountPersons(ctx context.Context, siteID string, fromMs, toMs int64, includeAnonymous bool) (int64, error) {
	if siteID == "" {
		return 0, nil
	}
	return s.ev.Count(ctx, siteID, fromMs, toMs, includeAnonymous)
}

// DefaultWindow returns the default 30-day query window when the caller
// omits from / to. Centralised so the handler and tests agree on the
// same default.
func DefaultWindow() (int64, int64) {
	now := time.Now().UTC()
	return now.Add(-30 * 24 * time.Hour).UnixMilli(), now.UnixMilli()
}
