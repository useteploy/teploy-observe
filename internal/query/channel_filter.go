package query

import (
	"context"
	"fmt"
	"github.com/useteploy/teploy-observe/internal/cohorts"
	"github.com/useteploy/teploy-observe/internal/queryguard"
	"time"
)

func (s *StatsService) channelEventIDs(ctx context.Context, siteID, channel string, from, to time.Time) ([]string, error) {
	if err := s.validateWindow(from, to); err != nil {
		return nil, err
	}
	switch channel {
	case ChannelDirect, ChannelOrganic, ChannelSocial, ChannelReferral, ChannelEmail, ChannelPaid, ChannelAI:
	default:
		return nil, fmt.Errorf("unknown channel %q", channel)
	}
	qctx, finish, err := s.beginQuery(ctx, siteID)
	if err != nil {
		return nil, err
	}
	defer finish()
	type row struct {
		ID       string `db:"event_id"`
		Referrer string `db:"referrer"`
		Source   string `db:"utm_source"`
		Medium   string `db:"utm_medium"`
	}
	rows, err := boundedRangeQuery[row](qctx, s, `SELECT event_id, COALESCE(referrer,'') AS referrer, COALESCE(utm_source,'') AS utm_source, COALESCE(utm_medium,'') AS utm_medium FROM events WHERE site_id=$1 AND timestamp >= $2 AND timestamp < $3`, siteID, from.UnixMilli(), to.UnixMilli())
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for _, r := range rows {
		if err := qctx.Err(); err != nil {
			return nil, err
		}
		if ClassifyChannel(r.Referrer, r.Source, r.Medium) == channel {
			ids = append(ids, r.ID)
			if len(ids) > cohorts.MaxFilterMembers {
				return nil, queryguard.RowBudgetRefusal(s.guard, int64(cohorts.MaxFilterMembers))
			}
		}
	}
	return ids, nil
}
