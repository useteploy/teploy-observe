package jobs

import (
	"context"
	"fmt"

	"github.com/neutron-build/neutron/go/nucleus"
)

type replayRetentionKey struct {
	Tenant string `db:"tenant_id"`
	Site   string `db:"site_id"`
	Replay string `db:"replay_id"`
}

// Browser replay timestamps can be relative, so payload lifetime follows the
// owning session. Unowned payloads are removed too: session and child creation
// commit atomically. Legacy site-less children survive while ANY matching
// tenant/replay owner is retained; no guessed site attribution is needed.
func (r *RetentionService) cleanupReplayPayloads(ctx context.Context, cutoff int64) error {
	var cursor replayRetentionKey
	first := true
	for {
		where := ""
		var args []any
		if !first {
			where = " WHERE tenant_id > $1 OR (tenant_id = $1 AND site_id > $2) OR (tenant_id = $1 AND site_id = $2 AND replay_id > $3)"
			args = []any{cursor.Tenant, cursor.Site, cursor.Replay}
		}
		rows, err := nucleus.Query[replayRetentionKey](ctx, r.db.SQL(), "SELECT DISTINCT tenant_id,site_id,replay_id FROM replay_events"+where+" ORDER BY tenant_id,site_id,replay_id LIMIT 100", args...)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		for _, key := range rows {
			ownerWhere := "tenant_id = $1 AND replay_id = $2 AND start_time >= $3"
			ownerArgs := []any{key.Tenant, key.Replay, cutoff}
			if key.Site != "" {
				ownerWhere += " AND site_id = $4"
				ownerArgs = append(ownerArgs, key.Site)
			}
			owners, err := nucleus.Query[boundaryRow](ctx, r.db.SQL(), "SELECT 1 AS c FROM replay_sessions WHERE "+ownerWhere+" LIMIT 1", ownerArgs...)
			if err != nil {
				return err
			}
			if len(owners) == 0 {
				_, err := r.cleanupWhere(ctx, "replay_events", "tenant_id = $1 AND site_id = $2 AND replay_id = $3", []any{key.Tenant, key.Site, key.Replay}, retentionKeys["replay_events"])
				if err != nil {
					return fmt.Errorf("replay payload cleanup: %w", err)
				}
			}
		}
		cursor = rows[len(rows)-1]
		first = false
	}
}
