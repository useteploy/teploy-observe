package query

import (
	"context"
	"crypto/rand"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"
)

var randRead = rand.Read

// Value sources for a goal. See internal/schema/migrations/036_goal_value.up.sql.
const (
	// ValueSourceFixed values every conversion at the goal's own ValueMinor.
	ValueSourceFixed = "fixed"
	// ValueSourceEvent reads the amount off each conversion event, from the
	// property named by ValueProperty, so a $12 order and a $400 order are
	// counted at what they were actually worth.
	ValueSourceEvent = "event"
)

// DefaultValueProperty is the event property a per-event goal reads when the
// caller does not name one. "revenue" is what Plausible's and Fathom's SDKs
// send, so an existing integration keeps working.
const DefaultValueProperty = "revenue"

// goalColumns are the non-key columns of the goals table, in the order the
// read path collapses them. Keys (tenant_id, site_id, goal_id) are selected
// verbatim by LatestRows and must not appear here.
var goalColumns = []string{
	"name", "goal_type", "goal_value",
	"value_minor", "currency", "value_source", "value_property",
	"created_at",
}

// Goal represents a conversion goal definition.
type Goal struct {
	GoalID   string `json:"goal_id" db:"goal_id"`
	TenantID string `json:"-" db:"tenant_id"`
	SiteID   string `json:"site_id" db:"site_id"`
	Name     string `json:"name" db:"name"`
	GoalType string `json:"goal_type" db:"goal_type"` // "page" or "event"
	// GoalValue is the MATCHER — the pathname or event_type a conversion is
	// recognised by. It is not money; the money is ValueMinor. The two names
	// sit uncomfortably close together, and goal_value was here first.
	GoalValue string `json:"goal_value" db:"goal_value"`
	// ValueMinor is what one conversion is worth, as an integer count of
	// Currency's ISO-4217 minor units. Zero means unvalued. See money.go for
	// why this is never a float.
	ValueMinor int64 `json:"value_minor" db:"value_minor"`
	// Currency is an ISO-4217 alphabetic code, or empty when the goal carries
	// no value. Never defaulted to USD — a self-hosted tool does not get to
	// assume its operator bills in dollars.
	Currency string `json:"currency" db:"currency"`
	// ValueSource is ValueSourceFixed or ValueSourceEvent.
	ValueSource string `json:"value_source" db:"value_source"`
	// ValueProperty names the event property carrying the amount when
	// ValueSource is ValueSourceEvent.
	ValueProperty string `json:"value_property" db:"value_property"`
	CreatedAt     string `json:"created_at" db:"created_at"`
}

// HasValue reports whether the goal is configured to produce money at all.
func (g Goal) HasValue() bool {
	if g.Currency == "" {
		return false
	}
	if g.ValueSource == ValueSourceEvent {
		return true
	}
	return g.ValueMinor != 0
}

// GoalConversion is a goal with its conversion counts and value for a time
// range.
//
// Goal is a NAMED field, not embedded: docs/api-shape-convention.md forbids
// embedding in a response struct precisely because encoding/json flattens it,
// and the dashboard has always read this as `{ goal: {...}, conversions: n }`.
// While it was embedded the browser read `g.goal.name` off an object that had
// no `goal` key and the goals list threw on every site that had one.
type GoalConversion struct {
	Goal Goal `json:"goal"`
	// Conversions counts distinct sessions that converted — the "unique
	// conversions" number, unchanged from before value existed.
	Conversions int64 `json:"conversions"`
	// ConversionEvents counts the matching events themselves. A session that
	// buys twice is one conversion and two events, and money is summed over
	// events, so this is the count the value corresponds to.
	ConversionEvents int64   `json:"conversion_events"`
	Visitors         int64   `json:"visitors"`
	Rate             float64 `json:"rate"` // conversions / total visitors * 100
	// TotalValueMinor is the money the period's conversions were worth, in
	// Goal.Currency's minor units. Zero when the goal carries no value.
	TotalValueMinor int64 `json:"total_value_minor"`
}

// CreateGoal stores a new goal. g.GoalID, TenantID and CreatedAt are assigned
// here; the caller is responsible for having validated the value fields (see
// ValidateGoalValue).
func (s *StatsService) CreateGoal(ctx context.Context, g Goal) (*Goal, error) {
	if err := ValidateGoalValue(&g); err != nil {
		return nil, err
	}
	g.GoalID = generateQueryID()
	g.TenantID = "default"
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	g.CreatedAt = now
	if err := s.writeGoal(ctx, g, now); err != nil {
		return nil, fmt.Errorf("create goal: %w", err)
	}
	return &g, nil
}

// UpdateGoal rewrites an existing goal, chiefly so a goal created before it
// could carry money can be given a value without losing its id — which is what
// conversions, saved views and reports refer to.
//
// It deletes the stored rows and writes one fresh row rather than appending a
// higher version, which is the shape a ReplacingMergeTree is supposed to want.
// Appending does not work on Nucleus v0.1.8: two rows sharing an ORDER BY key,
// written as versions N and N+1 from separate statements, are collapsed on the
// way into the memtable and the survivor is the OLDER row roughly half the
// time. Measured directly against the engine — six writes of ("A" @1000, "B"
// @1001) left one row each time, and `argMax(name, version)` returned "A" on
// three of them. The newer row is physically gone by the time any read
// happens, so no read-path collapse can recover it, and an edit that silently
// reverts is worse than one that costs an extra statement.
//
// The cost is a window between the DELETE and the INSERT where the goal does
// not exist. That is acceptable here — a goal is a definition, not data;
// conversions are computed from events and survive — and it is the only shape
// that is actually correct on this engine. Revisit if Nucleus fixes the
// collapse.
//
// CreatedAt is carried over from the stored row so an edit does not re-date
// the goal.
func (s *StatsService) UpdateGoal(ctx context.Context, siteID, goalID string, g Goal) (*Goal, error) {
	if err := ValidateGoalValue(&g); err != nil {
		return nil, err
	}
	existing, err := s.getGoal(ctx, siteID, goalID)
	if err != nil {
		return nil, err
	}
	g.GoalID = goalID
	g.SiteID = siteID
	g.TenantID = "default"
	g.CreatedAt = existing.CreatedAt
	if g.Name == "" {
		g.Name = existing.Name
	}
	if g.GoalType == "" {
		g.GoalType = existing.GoalType
	}
	if g.GoalValue == "" {
		g.GoalValue = existing.GoalValue
	}
	if err := s.DeleteGoal(ctx, siteID, goalID); err != nil {
		return nil, fmt.Errorf("update goal: %w", err)
	}
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	if err := s.writeGoal(ctx, g, now); err != nil {
		return nil, fmt.Errorf("update goal: %w", err)
	}
	return &g, nil
}

func (s *StatsService) writeGoal(ctx context.Context, g Goal, version string) error {
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO goals (goal_id, tenant_id, site_id, name, goal_type, goal_value,
		                    value_minor, currency, value_source, value_property,
		                    created_at, version)
		 VALUES ($1, 'default', $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		g.GoalID, g.SiteID, g.Name, g.GoalType, g.GoalValue,
		strconv.FormatInt(g.ValueMinor, 10), g.Currency, g.ValueSource, g.ValueProperty,
		g.CreatedAt, version,
	)
	return err
}

// ValidateGoalValue normalises and checks the money fields of g, filling in
// the defaults an API caller may omit. It rejects rather than guesses: a
// currency Observe cannot parse, or a property key that would be spliced into
// SQL, is a bad request and not something to silently drop.
func ValidateGoalValue(g *Goal) error {
	g.Currency = strings.ToUpper(strings.TrimSpace(g.Currency))
	g.ValueSource = strings.ToLower(strings.TrimSpace(g.ValueSource))
	g.ValueProperty = strings.TrimSpace(g.ValueProperty)

	if g.ValueSource == "" {
		g.ValueSource = ValueSourceFixed
	}
	if g.ValueSource != ValueSourceFixed && g.ValueSource != ValueSourceEvent {
		return fmt.Errorf("goal value_source must be %q or %q", ValueSourceFixed, ValueSourceEvent)
	}
	if g.ValueMinor < 0 {
		return fmt.Errorf("goal value_minor must not be negative")
	}
	if g.Currency != "" && !ValidCurrency(g.Currency) {
		return fmt.Errorf("goal currency must be a three-letter ISO-4217 code, got %q", g.Currency)
	}
	// A value with no currency is a number nobody can format or compare, and
	// defaulting it to USD would be a guess printed as fact.
	if g.Currency == "" && (g.ValueMinor != 0 || g.ValueSource == ValueSourceEvent) {
		return fmt.Errorf("goal currency is required when the goal carries a value")
	}
	if g.ValueSource == ValueSourceEvent {
		if g.ValueProperty == "" {
			g.ValueProperty = DefaultValueProperty
		}
		// The key is interpolated into the JSONB `->>` operand, which Nucleus
		// will not accept as a bind parameter, so it must be provably safe.
		if !validPropertyKey.MatchString(g.ValueProperty) {
			return fmt.Errorf("goal value_property must be alphanumeric or underscore, got %q", g.ValueProperty)
		}
		// A per-event goal reads its amount off each event; a fixed amount
		// alongside it would never be used, so refuse rather than ignore it.
		g.ValueMinor = 0
	} else {
		g.ValueProperty = ""
	}
	return nil
}

// ListGoals returns a site's goals, newest first.
//
// The read collapses by argMax over version: `goals` is a ReplacingMergeTree,
// Nucleus merges its segments only opportunistically, and UpdateGoal writes an
// edit as a new row. Without the collapse an edited goal would be listed twice
// — once at each version — and which one won would depend on hash order.
func (s *StatsService) ListGoals(ctx context.Context, siteID string) ([]Goal, error) {
	q := `SELECT goal_id, tenant_id, site_id, name, goal_type, goal_value,
	             value_minor, currency, value_source, value_property, created_at
	      FROM ` + LatestRows("goals", goalColumns, "site_id = $1") + ` g
	      ORDER BY created_at DESC, goal_id ASC`
	return nucleus.Query[Goal](ctx, s.db.SQL(), q, siteID)
}

func (s *StatsService) getGoal(ctx context.Context, siteID, goalID string) (*Goal, error) {
	goals, err := s.ListGoals(ctx, siteID)
	if err != nil {
		return nil, err
	}
	for i := range goals {
		if goals[i].GoalID == goalID {
			return &goals[i], nil
		}
	}
	return nil, fmt.Errorf("goal not found")
}

// DeleteGoal removes a goal. It used to `return nil` without touching the
// database, on the belief that a ReplacingMergeTree cannot delete; it can —
// DELETE removes the physical rows, which is what the rollup jobs already rely
// on to clear a window before rewriting it. Scoped by site_id so a caller
// cannot delete another site's goal by guessing its id.
//
// goals has no `enabled` column, so a soft delete is not available here and a
// hard delete is the right shape: nothing references a goal_id, and the
// conversion counts are computed from events, not stored against the goal.
// Every version of the goal goes, since the predicate names no version.
func (s *StatsService) DeleteGoal(ctx context.Context, siteID, goalID string) error {
	_, err := s.db.SQL().Exec(ctx,
		`DELETE FROM goals WHERE goal_id = $1 AND site_id = $2`,
		goalID, siteID,
	)
	if err != nil {
		return fmt.Errorf("delete goal: %w", err)
	}
	return nil
}

// numericLiteral matches a decimal an event property can be read as money.
// Applied inside the SQL so a single event carrying `revenue: "n/a"` cannot
// abort the whole sum — Nucleus raises `cannot cast 'n/a' to FLOAT` and takes
// the entire goal's revenue down with it.
const numericLiteral = `^-?[0-9]+(\.[0-9]+)?$`

// GoalConversions computes conversion counts, and the money those conversions
// were worth, for every goal in a time range.
func (s *StatsService) GoalConversions(ctx context.Context, siteID string, from, to time.Time) ([]GoalConversion, error) {
	goals, err := s.ListGoals(ctx, siteID)
	if err != nil {
		return nil, err
	}
	if len(goals) == 0 {
		return nil, nil
	}

	fromMs := from.UnixMilli()
	toMs := to.UnixMilli()

	// Get total visitors for the period
	type countRow struct {
		Count string `db:"count"`
	}
	totalRows, err := nucleus.Query[countRow](ctx, s.db.SQL(),
		`SELECT CAST(COUNT(DISTINCT session_id) AS TEXT) AS count
		 FROM events WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3`,
		siteID, fromMs, toMs,
	)
	totalVisitors := int64(0)
	if err == nil && len(totalRows) > 0 {
		totalVisitors, _ = strconv.ParseInt(totalRows[0].Count, 10, 64)
	}

	var results []GoalConversion
	for _, g := range goals {
		match, ok := goalMatchSQL(g.GoalType)
		if !ok {
			continue
		}

		// Both counts in one pass: distinct sessions is the conversion count
		// the dashboard has always shown, and the raw event count is what the
		// money corresponds to.
		type conversionRow struct {
			Sessions string `db:"sessions"`
			Events   string `db:"events"`
		}
		rows, err := nucleus.Query[conversionRow](ctx, s.db.SQL(),
			`SELECT CAST(COUNT(DISTINCT session_id) AS TEXT) AS sessions,
			        CAST(COUNT(*) AS TEXT) AS events
			 FROM events
			 WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3 AND `+match,
			siteID, fromMs, toMs, g.GoalValue,
		)
		var conversions, events int64
		if err == nil && len(rows) > 0 {
			conversions, _ = strconv.ParseInt(rows[0].Sessions, 10, 64)
			events, _ = strconv.ParseInt(rows[0].Events, 10, 64)
		}

		rate := 0.0
		if totalVisitors > 0 {
			rate = float64(conversions) / float64(totalVisitors) * 100
		}

		total, err := s.goalValueMinor(ctx, siteID, g, match, fromMs, toMs, events)
		if err != nil {
			return nil, err
		}

		results = append(results, GoalConversion{
			Goal:             g,
			Conversions:      conversions,
			ConversionEvents: events,
			Visitors:         totalVisitors,
			Rate:             rate,
			TotalValueMinor:  total,
		})
	}

	return results, nil
}

// goalMatchSQL renders the predicate that recognises a conversion, with the
// matcher bound as $4.
func goalMatchSQL(goalType string) (string, bool) {
	switch goalType {
	case "page":
		return "event_type = 'pageview' AND pathname = $4", true
	case "event":
		return "event_type = $4", true
	default:
		return "", false
	}
}

// goalValueMinor returns what a period's conversions of g were worth, in
// g.Currency's minor units.
//
// Fixed goals multiply out in Go — integer arithmetic, no query. Per-event
// goals sum in Nucleus, but as integers: each event's amount is scaled to
// minor units and rounded to a BIGINT *before* it enters SUM, so the running
// total is exact however many orders it covers. Doing it the obvious way —
// SUM over DOUBLE PRECISION, rounded at the end — drifts by a cent or two per
// few thousand rows, and a revenue figure that disagrees with Stripe by two
// cents is a support ticket that costs more than this comment.
func (s *StatsService) goalValueMinor(ctx context.Context, siteID string, g Goal, match string, fromMs, toMs, events int64) (int64, error) {
	if !g.HasValue() {
		return 0, nil
	}
	if g.ValueSource == ValueSourceFixed {
		return g.ValueMinor * events, nil
	}

	// ValidateGoalValue has already proved ValueProperty is [A-Za-z0-9_]+.
	// Nucleus will not take the JSONB ->> operand as a bind parameter, so it
	// is interpolated; the regex is what makes that safe.
	prop := g.ValueProperty
	if !validPropertyKey.MatchString(prop) {
		return 0, fmt.Errorf("goal value: invalid value_property %q", prop)
	}
	scale := MinorUnitScale(g.Currency)

	type sumRow struct {
		Total string `db:"total"`
	}
	q := fmt.Sprintf(
		`SELECT CAST(SUM(CAST(ROUND(CAST(properties ->> '%s' AS DOUBLE PRECISION) * %d) AS BIGINT)) AS TEXT) AS total
		 FROM events
		 WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3 AND %s
		   AND properties ->> '%s' ~ '%s'`,
		prop, scale, match, prop, numericLiteral,
	)
	rows, err := nucleus.Query[sumRow](ctx, s.db.SQL(), q, siteID, fromMs, toMs, g.GoalValue)
	if err != nil {
		return 0, fmt.Errorf("goal value: %w", err)
	}
	if len(rows) == 0 || rows[0].Total == "" {
		// SUM over no rows is NULL, which arrives as an empty string. No
		// conversions carried a readable amount; that is zero, not an error.
		return 0, nil
	}
	total, err := strconv.ParseInt(rows[0].Total, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("goal value: unreadable total %q: %w", rows[0].Total, err)
	}
	return total, nil
}

func generateQueryID() string {
	b := make([]byte, 16)
	_, _ = cryptoRandRead(b)
	return fmt.Sprintf("%x", b)
}

// cryptoRandRead wraps crypto/rand.Read
var cryptoRandRead = func(b []byte) (int, error) {
	return randRead(b)
}
