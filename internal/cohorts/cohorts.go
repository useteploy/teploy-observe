// Package cohorts implements behavioural cohorts on top of the events
// table.
//
// C2 (Wave 4, 2026-05-10) — PostHog parity. Two pieces:
//
//  1. A simple rule DSL ({op, rules: []Rule}) the UI builds and the
//     server stores as JSON in the cohorts table (migration 023).
//  2. An evaluator that returns the list of distinct_ids matching a
//     rule. Used by the query pipeline at request time to filter
//     analytics charts on a cohort.
//
// Performance posture (v1): cohorts are materialised on-demand. Every
// query that opts in to cohort_id re-evaluates the rule. This is fine
// at our scale (10k-100k events / day per site) and lets us ship the
// PostHog-parity surface without a refresh job. Phase 2 will introduce
// a cohort_members table + periodic refresh; the API stays the same.
//
// Nucleus-specific care:
//   - Aggregates scanned as native int64 (finding #24, not CAST AS TEXT).
//   - BIGINT comparisons wrapped in CAST AS BIGINT on both sides (#6).
//   - ReplacingMergeTree reads dedup in Go because read-time dedup is
//     unreliable in Nucleus today (#10).
package cohorts

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
)

// Service exposes cohort CRUD and evaluation.
type Service struct {
	db *nucleus.Client
}

// NewService constructs a cohorts.Service backed by the shared nucleus client.
func NewService(db *nucleus.Client) *Service {
	return &Service{db: db}
}

// Rule is a single condition in a cohort definition.
//
// Supported Type values for v1:
//   - "event"    — presence/count of an event in a window
//   - "property" — exact match on an event attribute or session field
//
// Property rules apply on the events table (one row per event); the
// distinct_id set returned is the set of users who emitted *any* event
// matching the property. That matches the PostHog "person property"
// semantic in the common case where users carry session-level
// attributes (country, browser, etc.) on each event.
type Rule struct {
	Type     string `json:"type"`
	Name     string `json:"name,omitempty"`     // event type for type=event
	Window   string `json:"window,omitempty"`   // e.g. "30d" / "7d" / "24h" — type=event
	MinCount int    `json:"min_count,omitempty"` // type=event; >=1 means "at least one"
	Key      string `json:"key,omitempty"`      // column for type=property
	Operator string `json:"operator,omitempty"` // "=" / "!=" — type=property
	Value    string `json:"value,omitempty"`    // type=property
}

// Definition is the full saved cohort rule. Op is "and" for v1; "or"
// + nesting are deferred to a phase-2 builder.
type Definition struct {
	Op    string `json:"op"`
	Rules []Rule `json:"rules"`
}

// Cohort is a saved cohort row.
type Cohort struct {
	CohortID    string `json:"cohort_id"    db:"cohort_id"`
	TenantID    string `json:"-"            db:"tenant_id"`
	SiteID      string `json:"site_id"      db:"site_id"`
	Name        string `json:"name"         db:"name"`
	Description string `json:"description"  db:"description"`
	Rule        string `json:"rule"         db:"rule"`
	MemberCount int64  `json:"member_count" db:"member_count"`
	CreatedAt   int64  `json:"created_at"   db:"created_at"`
	UpdatedAt   int64  `json:"updated_at"   db:"updated_at"`
}

// ParseDefinition unmarshals the stored JSON rule into a typed Definition.
// Empty / invalid JSON returns a zero-rule cohort (matches no one).
func ParseDefinition(rule string) (Definition, error) {
	rule = strings.TrimSpace(rule)
	if rule == "" {
		return Definition{Op: "and"}, nil
	}
	var def Definition
	if err := json.Unmarshal([]byte(rule), &def); err != nil {
		return Definition{}, fmt.Errorf("invalid cohort rule json: %w", err)
	}
	if def.Op == "" {
		def.Op = "and"
	}
	return def, nil
}

// EvaluateCohort returns the list of distinct_ids that match the given
// rule for siteID. Anonymous (distinct_id = '') is always excluded
// from cohort membership; a cohort of "anonymous users" is not a
// useful concept.
func (s *Service) EvaluateCohort(ctx context.Context, siteID string, def Definition) ([]string, error) {
	if siteID == "" {
		return []string{}, nil
	}
	if def.Op == "" {
		def.Op = "and"
	}
	if def.Op != "and" {
		// Defer or / nested to phase 2 explicitly so the API is
		// predictable. The UI never offers anything but AND for v1.
		return nil, fmt.Errorf("cohort op %q not supported (v1 supports AND only)", def.Op)
	}
	if len(def.Rules) == 0 {
		return []string{}, nil
	}

	// Evaluate each rule to a set of distinct_ids, then AND-intersect.
	// Doing this per-rule (vs one big SQL with N joins) keeps each
	// query simple — Nucleus's optimizer is young (finding #15 family)
	// and per-rule queries are easier to reason about.
	var acc map[string]struct{}
	for i, r := range def.Rules {
		ids, err := s.evalRule(ctx, siteID, r)
		if err != nil {
			return nil, fmt.Errorf("rule %d (%s): %w", i, r.Type, err)
		}
		set := make(map[string]struct{}, len(ids))
		for _, id := range ids {
			if id == "" {
				continue
			}
			set[id] = struct{}{}
		}
		if i == 0 {
			acc = set
			continue
		}
		// AND-intersect.
		for id := range acc {
			if _, ok := set[id]; !ok {
				delete(acc, id)
			}
		}
		if len(acc) == 0 {
			return []string{}, nil
		}
	}

	out := make([]string, 0, len(acc))
	for id := range acc {
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

func (s *Service) evalRule(ctx context.Context, siteID string, r Rule) ([]string, error) {
	switch r.Type {
	case "event":
		return s.evalEventRule(ctx, siteID, r)
	case "property":
		return s.evalPropertyRule(ctx, siteID, r)
	default:
		return nil, fmt.Errorf("unsupported rule type %q (supported: event, property)", r.Type)
	}
}

// evalEventRule returns the distinct_ids that fired event r.Name at
// least r.MinCount times within r.Window.
//
// Implementation note (Nucleus dogfood finding #28): the natural form
//
//	GROUP BY distinct_id HAVING COUNT(*) >= N
//
// returns ZERO ROWS even when matching groups exist. Workaround: pull
// (distinct_id, count) pairs without HAVING and filter in Go. Cohort
// evaluation is bounded by per-site distinct_id cardinality (bounded
// by the user count, not event count) so the row movement is small.
func (s *Service) evalEventRule(ctx context.Context, siteID string, r Rule) ([]string, error) {
	if r.Name == "" {
		return nil, fmt.Errorf("event rule requires name")
	}
	min := r.MinCount
	if min <= 0 {
		min = 1
	}
	windowMs := parseWindow(r.Window)
	fromMs := time.Now().UTC().Add(-windowMs).UnixMilli()
	from := dbutil.IntParam(fromMs)

	type countedRow struct {
		DistinctID string `db:"distinct_id"`
		Count      int64  `db:"c"`
	}
	q := `SELECT distinct_id, COUNT(*) AS c
	 FROM events
	 WHERE site_id = $1
	   AND event_type = $2
	   AND CAST(timestamp AS BIGINT) >= CAST($3 AS BIGINT)
	   AND distinct_id != ''
	 GROUP BY distinct_id`
	rows, err := nucleus.Query[countedRow](ctx, s.db.SQL(), q, siteID, r.Name, from)
	if err != nil {
		return nil, fmt.Errorf("event rule query: %w", err)
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		if row.Count >= int64(min) {
			out = append(out, row.DistinctID)
		}
	}
	return out, nil
}

// evalPropertyRule returns the distinct_ids whose events match a
// property filter. Allowed keys are the indexed event columns —
// arbitrary JSON properties are deferred to phase 2 because they need
// JSON path support that Nucleus doesn't expose cleanly yet.
func (s *Service) evalPropertyRule(ctx context.Context, siteID string, r Rule) ([]string, error) {
	if r.Key == "" {
		return nil, fmt.Errorf("property rule requires key")
	}
	if !isAllowedPropertyKey(r.Key) {
		return nil, fmt.Errorf("property key %q not allowed (allowed: %s)", r.Key, strings.Join(allowedPropertyKeys(), ", "))
	}
	op := r.Operator
	if op == "" {
		op = "="
	}
	if op != "=" && op != "!=" {
		return nil, fmt.Errorf("operator %q not supported (supported: =, !=)", op)
	}

	type idRow struct {
		DistinctID string `db:"distinct_id"`
	}
	// Direct column comparison — safe because isAllowedPropertyKey
	// constrains the column name to a hard-coded allow-list, so the
	// fmt.Sprintf can't be SQL-injected.
	q := fmt.Sprintf(`SELECT DISTINCT distinct_id
	 FROM events
	 WHERE site_id = $1
	   AND %s %s $2
	   AND distinct_id != ''`, r.Key, op)
	rows, err := nucleus.Query[idRow](ctx, s.db.SQL(), q, siteID, r.Value)
	if err != nil {
		return nil, fmt.Errorf("property rule query: %w", err)
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.DistinctID)
	}
	return out, nil
}

// allowedPropertyKeys returns the columns a property rule may filter
// on. Locked-down to defeat SQL injection through r.Key.
func allowedPropertyKeys() []string {
	return []string{
		"country", "region", "city",
		"browser", "os", "device", "language",
		"utm_source", "utm_medium", "utm_campaign",
		"pathname", "referrer",
	}
}

func isAllowedPropertyKey(key string) bool {
	for _, k := range allowedPropertyKeys() {
		if k == key {
			return true
		}
	}
	return false
}

// parseWindow turns "30d" / "7d" / "24h" into a Duration. Unknown /
// empty defaults to 30 days, which is the PostHog default cohort
// window.
func parseWindow(w string) time.Duration {
	w = strings.TrimSpace(strings.ToLower(w))
	if w == "" {
		return 30 * 24 * time.Hour
	}
	if len(w) < 2 {
		return 30 * 24 * time.Hour
	}
	n, err := strconv.Atoi(w[:len(w)-1])
	if err != nil || n <= 0 {
		return 30 * 24 * time.Hour
	}
	switch w[len(w)-1] {
	case 'd':
		return time.Duration(n) * 24 * time.Hour
	case 'h':
		return time.Duration(n) * time.Hour
	case 'm':
		return time.Duration(n) * time.Minute
	default:
		return 30 * 24 * time.Hour
	}
}

// ---------------------------------------------------------------------------
// CRUD
// ---------------------------------------------------------------------------

// Create inserts a new cohort row and writes its initial member_count
// from a fresh evaluation. Failures during the count evaluation are
// non-fatal: the cohort still saves with member_count=0 so the user
// can retry "Refresh" from the UI.
func (s *Service) Create(ctx context.Context, siteID, name, description string, def Definition) (*Cohort, error) {
	if siteID == "" || name == "" {
		return nil, fmt.Errorf("site_id and name required")
	}
	raw, err := json.Marshal(def)
	if err != nil {
		return nil, fmt.Errorf("marshal rule: %w", err)
	}
	id := genID()
	nowMs := time.Now().UTC().UnixMilli()

	// Evaluate up front so the saved row has a meaningful count.
	members, _ := s.EvaluateCohort(ctx, siteID, def)
	count := int64(len(members))

	_, err = s.db.SQL().Exec(ctx,
		`INSERT INTO cohorts (cohort_id, tenant_id, site_id, name, description, rule, member_count, created_at, updated_at)
		 VALUES ($1, 'default', $2, $3, $4, $5, $6, $7, $7)`,
		id, siteID, name, description, string(raw),
		dbutil.IntParam(count), dbutil.IntParam(nowMs),
	)
	if err != nil {
		return nil, fmt.Errorf("create cohort: %w", err)
	}
	return &Cohort{
		CohortID: id, TenantID: "default", SiteID: siteID,
		Name: name, Description: description, Rule: string(raw),
		MemberCount: count, CreatedAt: nowMs, UpdatedAt: nowMs,
	}, nil
}

// Get returns a single cohort. Read-time dedup in Go per finding #10:
// ReplacingMergeTree's PK dedup is unreliable in Nucleus today, so we
// pick the row with the highest updated_at when multiple appear.
func (s *Service) Get(ctx context.Context, siteID, cohortID string) (*Cohort, error) {
	rows, err := nucleus.Query[Cohort](ctx, s.db.SQL(),
		`SELECT cohort_id, tenant_id, site_id, name,
		        COALESCE(description, '') AS description,
		        rule, member_count, created_at, updated_at
		 FROM cohorts WHERE site_id = $1 AND cohort_id = $2`,
		siteID, cohortID)
	if err != nil {
		return nil, fmt.Errorf("get cohort: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	winner := rows[0]
	for _, r := range rows[1:] {
		if r.UpdatedAt > winner.UpdatedAt {
			winner = r
		}
	}
	// A tombstone (name = '') means the cohort was deleted via the
	// soft-delete path. Treat as not-found so the API returns 404.
	if winner.Name == "" {
		return nil, nil
	}
	return &winner, nil
}

// List returns every cohort for a site, newest first. Same Go-side
// dedup as Get (finding #10).
func (s *Service) List(ctx context.Context, siteID string) ([]Cohort, error) {
	rows, err := nucleus.Query[Cohort](ctx, s.db.SQL(),
		`SELECT cohort_id, tenant_id, site_id, name,
		        COALESCE(description, '') AS description,
		        rule, member_count, created_at, updated_at
		 FROM cohorts WHERE site_id = $1`, siteID)
	if err != nil {
		return nil, fmt.Errorf("list cohorts: %w", err)
	}
	latest := make(map[string]Cohort, len(rows))
	for _, r := range rows {
		prev, ok := latest[r.CohortID]
		if !ok || r.UpdatedAt > prev.UpdatedAt {
			latest[r.CohortID] = r
		}
	}
	out := make([]Cohort, 0, len(latest))
	for _, r := range latest {
		if r.Name == "" {
			continue // tombstone
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt > out[j].CreatedAt
	})
	return out, nil
}

// Update overwrites the cohort definition. updated_at bumps; the
// ReplacingMergeTree merge wins on max(updated_at) (with read-side
// dedup per finding #10 as belt-and-braces). Re-evaluates member_count.
func (s *Service) Update(ctx context.Context, siteID, cohortID, name, description string, def Definition) (*Cohort, error) {
	existing, err := s.Get(ctx, siteID, cohortID)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, fmt.Errorf("cohort not found")
	}
	if name == "" {
		name = existing.Name
	}
	raw, err := json.Marshal(def)
	if err != nil {
		return nil, fmt.Errorf("marshal rule: %w", err)
	}

	members, _ := s.EvaluateCohort(ctx, siteID, def)
	count := int64(len(members))
	nowMs := time.Now().UTC().UnixMilli()
	// Force monotonic updated_at so the read-side dedup picks this row
	// even when wall-clock didn't tick since the previous write.
	if nowMs <= existing.UpdatedAt {
		nowMs = existing.UpdatedAt + 1
	}

	_, err = s.db.SQL().Exec(ctx,
		`INSERT INTO cohorts (cohort_id, tenant_id, site_id, name, description, rule, member_count, created_at, updated_at)
		 VALUES ($1, 'default', $2, $3, $4, $5, $6, $7, $8)`,
		cohortID, siteID, name, description, string(raw),
		dbutil.IntParam(count), dbutil.IntParam(existing.CreatedAt), dbutil.IntParam(nowMs),
	)
	if err != nil {
		return nil, fmt.Errorf("update cohort: %w", err)
	}
	return &Cohort{
		CohortID: cohortID, TenantID: "default", SiteID: siteID,
		Name: name, Description: description, Rule: string(raw),
		MemberCount: count, CreatedAt: existing.CreatedAt, UpdatedAt: nowMs,
	}, nil
}

// Refresh re-evaluates the cohort and writes a new updated_at +
// member_count row. Used by the "Refresh" button in the UI when the
// user wants to recompute without changing the definition.
func (s *Service) Refresh(ctx context.Context, siteID, cohortID string) (*Cohort, error) {
	existing, err := s.Get(ctx, siteID, cohortID)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, fmt.Errorf("cohort not found")
	}
	def, err := ParseDefinition(existing.Rule)
	if err != nil {
		return nil, err
	}
	return s.Update(ctx, siteID, cohortID, existing.Name, existing.Description, def)
}

// Delete soft-deletes a cohort by writing a tombstone row (name='').
// Hard DELETE is unreliable across replacing_mergetree merges in
// Nucleus today (finding #10 family); a tombstone is the safe pattern.
//
// updated_at is forced strictly greater than the existing row so the
// Go-side dedup in List/Get picks the tombstone even when wall-clock
// hasn't ticked since the previous write (test-suite race).
func (s *Service) Delete(ctx context.Context, siteID, cohortID string) error {
	existing, err := s.Get(ctx, siteID, cohortID)
	if err != nil {
		return err
	}
	if existing == nil {
		return nil // idempotent
	}
	nowMs := time.Now().UTC().UnixMilli()
	if nowMs <= existing.UpdatedAt {
		nowMs = existing.UpdatedAt + 1
	}
	_, err = s.db.SQL().Exec(ctx,
		`INSERT INTO cohorts (cohort_id, tenant_id, site_id, name, description, rule, member_count, created_at, updated_at)
		 VALUES ($1, 'default', $2, '', '', '', '0', $3, $4)`,
		cohortID, siteID,
		dbutil.IntParam(existing.CreatedAt), dbutil.IntParam(nowMs),
	)
	if err != nil {
		return fmt.Errorf("delete cohort: %w", err)
	}
	return nil
}

// Members returns a paginated slice of cohort distinct_ids. Re-evaluates
// the rule each call (v1 has no cohort_members table — see package doc).
func (s *Service) Members(ctx context.Context, siteID, cohortID string, limit, offset int) ([]string, error) {
	c, err := s.Get(ctx, siteID, cohortID)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return []string{}, nil
	}
	def, err := ParseDefinition(c.Rule)
	if err != nil {
		return nil, err
	}
	ids, err := s.EvaluateCohort(ctx, siteID, def)
	if err != nil {
		return nil, err
	}
	if offset < 0 {
		offset = 0
	}
	if offset >= len(ids) {
		return []string{}, nil
	}
	end := offset + limit
	if limit <= 0 || end > len(ids) {
		end = len(ids)
	}
	return ids[offset:end], nil
}

// MembersForFilter returns the full set of distinct_ids for a cohort,
// used by the query pipeline to filter analytics charts. Returns an
// empty slice for an empty / missing cohort so callers can substitute
// an "impossible" filter without special-casing nil.
func (s *Service) MembersForFilter(ctx context.Context, siteID, cohortID string) ([]string, error) {
	c, err := s.Get(ctx, siteID, cohortID)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return []string{}, nil
	}
	def, err := ParseDefinition(c.Rule)
	if err != nil {
		return nil, err
	}
	return s.EvaluateCohort(ctx, siteID, def)
}

func genID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
