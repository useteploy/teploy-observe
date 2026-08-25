package errors

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
)

// ReleaseHealth represents error health for a single release.
type ReleaseHealth struct {
	ReleaseTag string    `json:"release_tag"`
	ErrorCount int64     `json:"error_count"`
	IssueCount int64     `json:"issue_count"`
	FirstSeen  time.Time `json:"first_seen"`
	LastSeen   time.Time `json:"last_seen"`
}

// ReleaseHealthList returns error health metrics per release for a time range.
func (s *IssueService) ReleaseHealthList(ctx context.Context, siteID string, from, to time.Time) ([]ReleaseHealth, error) {
	fromMs := dbutil.IntParam(from.UnixMilli())
	toMs := dbutil.IntParam(to.UnixMilli())

	return nucleus.Query[ReleaseHealth](ctx, s.db.SQL(),
		fmt.Sprintf(`SELECT
			COALESCE(release_tag, 'unknown') AS release_tag,
			COUNT(*) AS error_count,
			COUNT(DISTINCT group_hash) AS issue_count,
			MIN(CAST(timestamp AS BIGINT)) AS first_seen,
			MAX(CAST(timestamp AS BIGINT)) AS last_seen
		 FROM error_events
		 WHERE site_id = $1
		   AND timestamp >= $2
		   AND timestamp < $3
		   AND release_tag != ''
		 GROUP BY release_tag
		 ORDER BY last_seen DESC
		 LIMIT 20`),
		siteID, fromMs, toMs,
	)
}

// ReleaseStat is a per-release health card for the /releases UI.
//
// Crash-free % = sessions in window without any error event of severity
// >= "error" / total sessions in window, per release_tag.
//
// Adoption % = (new sessions in release_tag) / (total new sessions in
// the window). Sums to 100% across all releases that show up in the
// window. Useful for tracking rollout progress.
//
// Error rate = errors / sessions for the release. A release with one
// error and one session has rate 1.0; a release with zero errors and
// 1000 sessions has rate 0.0.
type ReleaseStat struct {
	ReleaseTag          string  `json:"release_tag"`
	Sessions            int64   `json:"sessions"`
	CrashedSessions     int64   `json:"crashed_sessions"`
	CrashFreeSessionPct float64 `json:"crash_free_session_pct"`
	AdoptionPct         float64 `json:"adoption_pct"`
	Errors              int64   `json:"errors"`
	ErrorRate           float64 `json:"error_rate"`
	FirstSeen           int64   `json:"first_seen_ms"`
	LastSeen            int64   `json:"last_seen_ms"`
}

// ReleaseHealthService computes per-release health from sessions +
// error_events for a given window. Pure read-side; no schema beyond
// what migration 019 lands.
type ReleaseHealthService struct {
	db *nucleus.Client
}

// NewReleaseHealthService constructs the service.
func NewReleaseHealthService(db *nucleus.Client) *ReleaseHealthService {
	return &ReleaseHealthService{db: db}
}

// Health returns one ReleaseStat per release_tag observed in the window.
// Releases with zero sessions are omitted (they'd divide by zero).
func (s *ReleaseHealthService) Health(ctx context.Context, siteID string, fromMs, toMs int64) ([]ReleaseStat, error) {
	if fromMs >= toMs {
		return []ReleaseStat{}, nil
	}

	type sessRow struct {
		ReleaseTag string `db:"release_tag"`
		Sessions   int64  `db:"sessions"`
		FirstSeen  int64  `db:"first_seen"`
		LastSeen   int64  `db:"last_seen"`
	}
	sessRows, err := nucleus.Query[sessRow](ctx, s.db.SQL(),
		// sessions is a ReplacingMergeTree that Nucleus does not collapse on
		// read, so the session rollup's re-computations would each be counted
		// as another session. Collapse to the latest version per session key
		// first — see internal/query/replacing.go.
		`SELECT release_tag,
			COUNT(*) AS sessions,
			MIN(CAST(first_ts AS BIGINT)) AS first_seen,
			MAX(CAST(last_ts AS BIGINT)) AS last_seen
		 FROM (SELECT tenant_id, site_id, session_id,
			argMax(release_tag, version) AS release_tag,
			argMax(first_ts, version) AS first_ts,
			argMax(last_ts, version) AS last_ts
		       FROM sessions
		       WHERE site_id = $1 AND first_ts >= $2 AND first_ts < $3
		       GROUP BY tenant_id, site_id, session_id) s
		 GROUP BY release_tag`,
		siteID, strconv.FormatInt(fromMs, 10), strconv.FormatInt(toMs, 10),
	)
	if err != nil {
		return nil, fmt.Errorf("release health: sessions: %w", err)
	}

	// Per-release error count + crashed-session count. A "crashed"
	// session is one with at least one error of level >= error.
	type errRow struct {
		ReleaseTag      string `db:"release_tag"`
		Errors          int64  `db:"errors"`
		CrashedSessions int64  `db:"crashed_sessions"`
	}
	errRows, err := nucleus.Query[errRow](ctx, s.db.SQL(),
		`SELECT release_tag,
			COUNT(*) AS errors,
			COUNT(DISTINCT session_id) AS crashed_sessions
		 FROM error_events
		 WHERE site_id = $1
		   AND timestamp >= $2
		   AND timestamp < $3
		   AND level IN ('error', 'fatal')
		   AND session_id != ''
		 GROUP BY release_tag`,
		siteID, strconv.FormatInt(fromMs, 10), strconv.FormatInt(toMs, 10),
	)
	if err != nil {
		return nil, fmt.Errorf("release health: errors: %w", err)
	}
	errByRelease := make(map[string]errRow, len(errRows))
	for _, e := range errRows {
		errByRelease[e.ReleaseTag] = e
	}

	var totalSessions int64
	for _, r := range sessRows {
		totalSessions += r.Sessions
	}

	out := make([]ReleaseStat, 0, len(sessRows))
	for _, r := range sessRows {
		if r.Sessions == 0 {
			continue
		}
		stat := ReleaseStat{
			ReleaseTag: r.ReleaseTag,
			Sessions:   r.Sessions,
			FirstSeen:  r.FirstSeen,
			LastSeen:   r.LastSeen,
		}
		if e, ok := errByRelease[r.ReleaseTag]; ok {
			stat.Errors = e.Errors
			stat.CrashedSessions = e.CrashedSessions
		}
		// crash-free % capped at 100 in case of duplicate inserts that
		// merge later (ReplacingMergeTree).
		if r.Sessions > 0 {
			free := r.Sessions - stat.CrashedSessions
			if free < 0 {
				free = 0
			}
			stat.CrashFreeSessionPct = float64(free) * 100.0 / float64(r.Sessions)
			stat.ErrorRate = float64(stat.Errors) / float64(r.Sessions)
		}
		if totalSessions > 0 {
			stat.AdoptionPct = float64(r.Sessions) * 100.0 / float64(totalSessions)
		}
		out = append(out, stat)
	}

	// Sort by sessions descending so the dominant release is on top.
	sort.Slice(out, func(i, j int) bool { return out[i].Sessions > out[j].Sessions })
	return out, nil
}

// ReleaseSparklinePoint is one daily sample of crash-free % for a release.
type ReleaseSparklinePoint struct {
	DayMs               int64   `json:"day_ms"`
	Sessions            int64   `json:"sessions"`
	CrashedSessions     int64   `json:"crashed_sessions"`
	CrashFreeSessionPct float64 `json:"crash_free_session_pct"`
}

// Sparkline returns a per-day series of crash-free % for one release
// over the last `days` days (inclusive of today, UTC). Used by the
// /releases UI to render a 14-day sparkline next to the stat card.
func (s *ReleaseHealthService) Sparkline(ctx context.Context, siteID, releaseTag string, days int) ([]ReleaseSparklinePoint, error) {
	if days <= 0 || days > 90 {
		days = 14
	}
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	fromTime := today.AddDate(0, 0, -(days - 1))
	fromMs := strconv.FormatInt(fromTime.UnixMilli(), 10)
	toMs := strconv.FormatInt(today.AddDate(0, 0, 1).UnixMilli(), 10)

	type sessRow struct {
		Bucket   int64 `db:"bucket"`
		Sessions int64 `db:"sessions"`
	}
	sessRows, err := nucleus.Query[sessRow](ctx, s.db.SQL(),
		`SELECT (CAST(first_ts AS BIGINT) / 86400000) * 86400000 AS bucket,
			COUNT(*) AS sessions
		 FROM (SELECT tenant_id, site_id, session_id,
			argMax(first_ts, version) AS first_ts
		       FROM sessions
		       WHERE site_id = $1 AND release_tag = $2
			 AND first_ts >= $3 AND first_ts < $4
		       GROUP BY tenant_id, site_id, session_id) s
		 GROUP BY (CAST(first_ts AS BIGINT) / 86400000) * 86400000`,
		siteID, releaseTag, fromMs, toMs,
	)
	if err != nil {
		return nil, fmt.Errorf("release sparkline sessions: %w", err)
	}

	type errRow struct {
		Bucket          int64 `db:"bucket"`
		CrashedSessions int64 `db:"crashed_sessions"`
	}
	errRows, err := nucleus.Query[errRow](ctx, s.db.SQL(),
		`SELECT (CAST(timestamp AS BIGINT) / 86400000) * 86400000 AS bucket,
			COUNT(DISTINCT session_id) AS crashed_sessions
		 FROM error_events
		 WHERE site_id = $1
		   AND release_tag = $2
		   AND level IN ('error', 'fatal')
		   AND session_id != ''
		   AND timestamp >= $3
		   AND timestamp < $4
		 GROUP BY (CAST(timestamp AS BIGINT) / 86400000) * 86400000`,
		siteID, releaseTag, fromMs, toMs,
	)
	if err != nil {
		return nil, fmt.Errorf("release sparkline errors: %w", err)
	}

	sessByDay := make(map[int64]int64, len(sessRows))
	for _, r := range sessRows {
		sessByDay[r.Bucket] = r.Sessions
	}
	errByDay := make(map[int64]int64, len(errRows))
	for _, r := range errRows {
		errByDay[r.Bucket] = r.CrashedSessions
	}

	out := make([]ReleaseSparklinePoint, 0, days)
	for i := 0; i < days; i++ {
		d := fromTime.AddDate(0, 0, i)
		key := d.UnixMilli()
		s := sessByDay[key]
		c := errByDay[key]
		if c > s {
			c = s
		}
		var pct float64
		if s > 0 {
			pct = float64(s-c) * 100.0 / float64(s)
		}
		out = append(out, ReleaseSparklinePoint{
			DayMs:               key,
			Sessions:            s,
			CrashedSessions:     c,
			CrashFreeSessionPct: pct,
		})
	}
	return out, nil
}
