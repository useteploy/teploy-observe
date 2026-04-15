package errors

import (
	"context"
	"fmt"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/teploy/observe/internal/dbutil"
)

// ReleaseHealth represents error health for a single release.
type ReleaseHealth struct {
	ReleaseTag string    `json:"release_tag"`
	ErrorCount int64     `json:"error_count"`
	IssueCount int64     `json:"issue_count"`
	FirstSeen  time.Time `json:"first_seen"`
	LastSeen   time.Time `json:"last_seen"`
}

type releaseHealthRow struct {
	ReleaseTag string `db:"release_tag"`
	ErrorCount string `db:"error_count"`
	IssueCount string `db:"issue_count"`
	FirstSeen  string `db:"first_seen"`
	LastSeen   string `db:"last_seen"`
}

func (r releaseHealthRow) toDomain() ReleaseHealth {
	return ReleaseHealth{
		ReleaseTag: r.ReleaseTag,
		ErrorCount: parseInt64(r.ErrorCount),
		IssueCount: parseInt64(r.IssueCount),
		FirstSeen:  parseEpochMillis(r.FirstSeen),
		LastSeen:   parseEpochMillis(r.LastSeen),
	}
}

// ReleaseHealthList returns error health metrics per release for a time range.
func (s *IssueService) ReleaseHealthList(ctx context.Context, siteID string, from, to time.Time) ([]ReleaseHealth, error) {
	fromMs := dbutil.IntParam(from.UnixMilli())
	toMs := dbutil.IntParam(to.UnixMilli())

	rows, err := nucleus.Query[releaseHealthRow](ctx, s.db.SQL(),
		fmt.Sprintf(`SELECT
			COALESCE(release_tag, 'unknown') AS release_tag,
			CAST(COUNT(*) AS TEXT) AS error_count,
			CAST(COUNT(DISTINCT group_hash) AS TEXT) AS issue_count,
			CAST(MIN(CAST(timestamp AS BIGINT)) AS TEXT) AS first_seen,
			CAST(MAX(CAST(timestamp AS BIGINT)) AS TEXT) AS last_seen
		 FROM error_events
		 WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3
		   AND release_tag != ''
		 GROUP BY release_tag
		 ORDER BY last_seen DESC
		 LIMIT 20`),
		siteID, fromMs, toMs,
	)
	if err != nil {
		return nil, err
	}
	out := make([]ReleaseHealth, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.toDomain())
	}
	return out, nil
}
