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
	Release    string `json:"release" db:"release_tag"`
	ErrorCount string `json:"error_count" db:"error_count"`
	IssueCount string `json:"issue_count" db:"issue_count"`
	FirstSeen  string `json:"first_seen" db:"first_seen"`
	LastSeen   string `json:"last_seen" db:"last_seen"`
}

// ReleaseHealthList returns error health metrics per release for a time range.
func (s *IssueService) ReleaseHealthList(ctx context.Context, siteID string, from, to time.Time) ([]ReleaseHealth, error) {
	fromMs := dbutil.IntParam(from.UnixMilli())
	toMs := dbutil.IntParam(to.UnixMilli())

	return nucleus.Query[ReleaseHealth](ctx, s.db.SQL(),
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
}
