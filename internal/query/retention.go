package query

import (
	"context"
	"fmt"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

)

// RetentionCohort represents one row of the retention grid.
type RetentionCohort struct {
	CohortDate string    `json:"cohort_date"` // e.g. "2026-03-15"
	CohortSize int       `json:"cohort_size"` // visitors who first appeared in this period
	Periods    []float64 `json:"periods"`     // retention % for period 0, 1, 2, ...
}

// sessionFirstLast is the minimal session data for retention.
type sessionFirstLast struct {
	SessionID string `db:"session_id"`
	FirstTS   string `db:"first_ts"`
}

// Retention computes cohort retention over the given time range.
// Cohorts are grouped by the day of their first visit.
// Each period is one day (or one week if the range > 30 days).
func (s *StatsService) Retention(ctx context.Context, siteID string, from, to time.Time, periodDays int) ([]RetentionCohort, error) {
	if periodDays <= 0 {
		periodDays = 1
		if to.Sub(from) > 30*24*time.Hour {
			periodDays = 7
		}
	}

	// Clamp the window so a single request can't trigger an unbounded events
	// scan / map build. 186 days covers the widest realistic retention grid.
	const maxRetentionWindow = 186 * 24 * time.Hour
	if to.Sub(from) > maxRetentionWindow {
		from = to.Add(-maxRetentionWindow)
	}

	fromMs := from.UnixMilli()
	toMs := to.UnixMilli()

	// Get all sessions with first_ts in the time range
	rows, err := nucleus.Query[sessionFirstLast](ctx, s.db.SQL(),
		`SELECT session_id, first_ts
		 FROM `+LatestRows("sessions", []string{"first_ts"},
			`site_id = $1 AND first_ts >= $2 AND first_ts < $3`)+` AS s`,
		siteID, fromMs, toMs,
	)
	if err != nil {
		return nil, fmt.Errorf("retention query sessions: %w", err)
	}

	if len(rows) == 0 {
		return nil, nil
	}

	// Get ALL events for these sessions to find return visits
	// (sessions may have events outside the original time range)
	type eventRow struct {
		SessionID string `db:"session_id"`
		Timestamp string `db:"timestamp"`
	}
	events, err := nucleus.Query[eventRow](ctx, s.db.SQL(),
		`SELECT session_id, CAST(timestamp AS TEXT) AS timestamp
		 FROM events
		 WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3
		 ORDER BY session_id, timestamp ASC`,
		siteID, fromMs, toMs,
	)
	if err != nil {
		return nil, fmt.Errorf("retention query events: %w", err)
	}

	// Build session → list of active days
	periodMs := int64(periodDays) * 86400000
	sessionDays := make(map[string]map[int64]bool) // session_id -> set of period buckets
	for _, e := range events {
		ts := parseInt64(e.Timestamp)
		bucket := (ts / periodMs) * periodMs
		if sessionDays[e.SessionID] == nil {
			sessionDays[e.SessionID] = make(map[int64]bool)
		}
		sessionDays[e.SessionID][bucket] = true
	}

	// Build cohorts: group sessions by their first_ts period bucket
	type cohortData struct {
		sessions []string
		bucket   int64
	}
	cohortMap := make(map[int64]*cohortData)
	for _, r := range rows {
		ts := parseInt64(r.FirstTS)
		bucket := (ts / periodMs) * periodMs
		cd, ok := cohortMap[bucket]
		if !ok {
			cd = &cohortData{bucket: bucket}
			cohortMap[bucket] = cd
		}
		cd.sessions = append(cd.sessions, r.SessionID)
	}

	// Sort cohorts by date
	var buckets []int64
	for b := range cohortMap {
		buckets = append(buckets, b)
	}
	sortInt64(buckets)

	// Compute retention for each cohort
	totalPeriods := int((to.UnixMilli()-from.UnixMilli())/periodMs) + 1
	if totalPeriods > 12 {
		totalPeriods = 12
	}

	var result []RetentionCohort
	for _, bucket := range buckets {
		cd := cohortMap[bucket]
		cohort := RetentionCohort{
			CohortDate: time.UnixMilli(bucket).UTC().Format("2006-01-02"),
			CohortSize: len(cd.sessions),
			Periods:    make([]float64, totalPeriods),
		}

		for p := 0; p < totalPeriods; p++ {
			targetBucket := bucket + int64(p)*periodMs
			active := 0
			for _, sid := range cd.sessions {
				if sessionDays[sid] != nil && sessionDays[sid][targetBucket] {
					active++
				}
			}
			if cohort.CohortSize > 0 {
				cohort.Periods[p] = float64(active) / float64(cohort.CohortSize) * 100
			}
		}

		result = append(result, cohort)
	}

	return result, nil
}

func parseInt64(s string) int64 {
	var n int64
	for _, c := range s {
		if c >= '0' && c <= '9' {
			n = n*10 + int64(c-'0')
		}
	}
	return n
}

func sortInt64(s []int64) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
