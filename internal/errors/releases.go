package errors

import (
	"context"
	"fmt"
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
		   AND CAST(timestamp AS BIGINT) >= CAST($2 AS BIGINT)
		   AND CAST(timestamp AS BIGINT) < CAST($3 AS BIGINT)
		   AND release_tag != ''
		 GROUP BY release_tag
		 ORDER BY last_seen DESC
		 LIMIT 20`),
		siteID, fromMs, toMs,
	)
}
