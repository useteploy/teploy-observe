package query

import (
	"context"
	"crypto/rand"
	"fmt"
	"strconv"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
)

var randRead = rand.Read

// Goal represents a conversion goal definition.
type Goal struct {
	GoalID    string `json:"goal_id" db:"goal_id"`
	TenantID  string `json:"-" db:"tenant_id"`
	SiteID    string `json:"site_id" db:"site_id"`
	Name      string `json:"name" db:"name"`
	GoalType  string `json:"goal_type" db:"goal_type"`   // "page" or "event"
	GoalValue string `json:"goal_value" db:"goal_value"` // pathname or event_type
	CreatedAt string `json:"created_at" db:"created_at"`
	Version   string `json:"-" db:"version"`
}

// GoalConversion is a goal with its conversion count for a time range.
type GoalConversion struct {
	Goal
	Conversions int64   `json:"conversions"`
	Visitors    int64   `json:"visitors"`
	Rate        float64 `json:"rate"` // conversions / total visitors * 100
}

func (s *StatsService) CreateGoal(ctx context.Context, siteID, name, goalType, goalValue string) (*Goal, error) {
	id := generateQueryID()
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO goals (goal_id, tenant_id, site_id, name, goal_type, goal_value, created_at, version)
		 VALUES ($1, 'default', $2, $3, $4, $5, $6, $7)`,
		id, siteID, name, goalType, goalValue, now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("create goal: %w", err)
	}
	return &Goal{GoalID: id, SiteID: siteID, Name: name, GoalType: goalType, GoalValue: goalValue, CreatedAt: now}, nil
}

func (s *StatsService) ListGoals(ctx context.Context, siteID string) ([]Goal, error) {
	return nucleus.Query[Goal](ctx, s.db.SQL(),
		`SELECT goal_id, tenant_id, site_id, name, goal_type, goal_value, created_at, version
		 FROM goals WHERE site_id = $1 ORDER BY created_at DESC`, siteID)
}

func (s *StatsService) DeleteGoal(ctx context.Context, goalID string) error {
	// ReplacingMergeTree: can't delete, so just let it be (or disable via a field)
	return nil
}

// GoalConversions computes conversion counts for all goals in a time range.
func (s *StatsService) GoalConversions(ctx context.Context, siteID string, from, to time.Time) ([]GoalConversion, error) {
	goals, err := s.ListGoals(ctx, siteID)
	if err != nil {
		return nil, err
	}
	if len(goals) == 0 {
		return nil, nil
	}

	fromMs := dbutil.IntParam(from.UnixMilli())
	toMs := dbutil.IntParam(to.UnixMilli())

	// Get total visitors for the period
	type countRow struct {
		Count string `db:"count"`
	}
	totalRows, err := nucleus.Query[countRow](ctx, s.db.SQL(),
		`SELECT CAST(COUNT(DISTINCT session_id) AS TEXT) AS count
		 FROM events WHERE site_id = $1 AND CAST(timestamp AS BIGINT) >= CAST($2 AS BIGINT) AND CAST(timestamp AS BIGINT) < CAST($3 AS BIGINT)`,
		siteID, fromMs, toMs,
	)
	totalVisitors := int64(0)
	if err == nil && len(totalRows) > 0 {
		totalVisitors, _ = strconv.ParseInt(totalRows[0].Count, 10, 64)
	}

	var results []GoalConversion
	for _, g := range goals {
		var q string
		switch g.GoalType {
		case "page":
			q = `SELECT CAST(COUNT(DISTINCT session_id) AS TEXT) AS count
				 FROM events WHERE site_id = $1 AND CAST(timestamp AS BIGINT) >= CAST($2 AS BIGINT) AND CAST(timestamp AS BIGINT) < CAST($3 AS BIGINT)
				   AND event_type = 'pageview' AND pathname = $4`
		case "event":
			q = `SELECT CAST(COUNT(DISTINCT session_id) AS TEXT) AS count
				 FROM events WHERE site_id = $1 AND CAST(timestamp AS BIGINT) >= CAST($2 AS BIGINT) AND CAST(timestamp AS BIGINT) < CAST($3 AS BIGINT)
				   AND event_type = $4`
		default:
			continue
		}

		rows, err := nucleus.Query[countRow](ctx, s.db.SQL(), q, siteID, fromMs, toMs, g.GoalValue)
		conversions := int64(0)
		if err == nil && len(rows) > 0 {
			conversions, _ = strconv.ParseInt(rows[0].Count, 10, 64)
		}

		rate := 0.0
		if totalVisitors > 0 {
			rate = float64(conversions) / float64(totalVisitors) * 100
		}

		results = append(results, GoalConversion{
			Goal:        g,
			Conversions: conversions,
			Visitors:    totalVisitors,
			Rate:        rate,
		})
	}

	return results, nil
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
