package jobs

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"
)

// Column identities are application-owned schema, never request input. The
// timestamp is included separately in every predicate; versions sharing an
// identity are counted physically before deletion, rather than assuming keys
// are unique in MergeTree tables.
var retentionKeys = map[string][]string{
	"events": {"tenant_id", "site_id", "event_id"}, "events_recent": {"tenant_id", "site_id", "event_id"},
	"stats_hourly":             {"tenant_id", "site_id", "pathname", "event_type"},
	"sessions":                 {"tenant_id", "site_id", "session_id"},
	"error_events":             {"tenant_id", "site_id", "error_id"},
	"logs":                     {"tenant_id", "site_id", "log_id"},
	"llm_traces":               {"tenant_id", "site_id", "trace_id"},
	"spans":                    {"tenant_id", "site_id", "trace_id", "span_id"},
	"span_links":               {"tenant_id", "site_id", "trace_id", "span_id", "link_idx"},
	"experiment_metric_events": {"tenant_id", "site_id", "event_id"},
	"service_stats":            {"tenant_id", "site_id", "service_name", "operation_name"},
	"replay_sessions":          {"tenant_id", "site_id", "replay_id"},
	"replay_events":            {"tenant_id", "site_id", "replay_id", "event_id"},
	"alert_evaluations":        {"tenant_id", "site_id", "eval_id"},
	"metric_points":            {"tenant_id", "site_id", "metric_name", "service_name", "attributes", "value", "histogram"},
	"host_metrics":             {"tenant_id", "site_id", "metric_id"},
	"uptime_results":           {"tenant_id", "site_id", "result_id"},
	"performance_issues":       {"tenant_id", "site_id", "fingerprint"},
	"service_dependencies":     {"tenant_id", "site_id", "src_service", "dst_service"},
	"error_inbox":              {"tenant_id", "site_id", "producer_id", "event_id"},
	"replay_batches":           {"tenant_id", "site_id", "replay_id", "producer_id", "batch_id"},
	"derived_outbox":           {"tenant_id", "id"},
	"notification_outbox":      {"tenant_id", "id"},
}

type retentionKeyRow struct {
	K0 string `db:"k0"`
	K1 string `db:"k1"`
	K2 string `db:"k2"`
	K3 string `db:"k3"`
	K4 string `db:"k4"`
	K5 string `db:"k5"`
	K6 string `db:"k6"`
	K7 string `db:"k7"`
}

func (r retentionKeyRow) values() []string {
	return []string{r.K0, r.K1, r.K2, r.K3, r.K4, r.K5, r.K6, r.K7}
}

// cleanupWhere runs each select/count/delete on the same transaction snapshot.
// No DELETE LIMIT is used: the pinned engine does not implement that clause.
func (r *RetentionService) cleanupWhere(ctx context.Context, table, where string, args []any, keys []string) (int64, error) {
	if len(keys) == 0 || len(keys) > 8 {
		return 0, fmt.Errorf("no bounded retention identity for %s", table)
	}
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		affected, err := func() (int64, error) {
			tx, err := r.db.Begin(ctx)
			if err != nil {
				return 0, err
			}
			defer func() {
				cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				tx.Rollback(cleanup)
			}()
			n, err := deleteRetentionChunk(ctx, tx.SQL(), table, where, args, keys)
			if err != nil {
				return 0, err
			}
			if err := tx.Commit(ctx); err != nil {
				return 0, err
			}
			return n, nil
		}()
		if err != nil {
			return total, err
		}

		total += affected
		if affected == 0 {
			return total, nil
		}
	}
}

func deleteRetentionChunk(ctx context.Context, sql *nucleus.SQLModel, table, where string, args []any, keys []string) (int64, error) {
	cols := make([]string, len(keys))
	orders := make([]string, len(keys))
	for i, k := range keys {
		cols[i] = fmt.Sprintf("CAST(%s AS TEXT) AS k%d", k, i)
		orders[i] = fmt.Sprintf("k%d", i)
	}
	rows, err := nucleus.Query[retentionKeyRow](ctx, sql, fmt.Sprintf("SELECT DISTINCT %s FROM %s WHERE %s ORDER BY %s LIMIT 500", strings.Join(cols, ","), table, where, strings.Join(orders, ",")), args...)
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	for {
		predicate, bindings := retentionIdentityPredicate(keys, rows, args)
		predicate = "(" + where + ") AND (" + predicate + ")"
		// Count visible matches with a limit, rather than COUNT over a giant tie.
		// DELETE may expose more physical versions than SELECT on some engines.
		matches, err := nucleus.Query[boundaryRow](ctx, sql, fmt.Sprintf("SELECT 1 AS c FROM %s WHERE %s LIMIT %d", table, predicate, retentionChunkSize+1), bindings...)
		if err != nil {
			return 0, err
		}
		if len(matches) <= retentionChunkSize {
			n, err := sql.Exec(ctx, "DELETE FROM "+table+" WHERE "+predicate, bindings...)
			if err == nil && n > retentionChunkSize {
				return 0, fmt.Errorf("retention: %s DELETE exposed %d physical rows beyond visible-row budget; rolling back (bounded physical DELETE required)", table, n)
			}
			return n, err
		}
		if len(rows) == 1 {
			return 0, fmt.Errorf("retention: %s identity has more than %d physical copies; refusing unbounded delete (engine bounded physical DELETE required)", table, retentionChunkSize)
		}
		rows = rows[:len(rows)/2]
	}
}

func retentionIdentityPredicate(keys []string, rows []retentionKeyRow, initial []any) (string, []any) {
	args := append([]any(nil), initial...)
	groups := make([]string, len(rows))
	for i, row := range rows {
		terms := make([]string, len(keys))
		values := row.values()
		for j, key := range keys {
			args = append(args, values[j])
			terms[j] = fmt.Sprintf("CAST(%s AS TEXT) = $%d", key, len(args))
		}
		groups[i] = "(" + strings.Join(terms, " AND ") + ")"
	}
	return strings.Join(groups, " OR "), args
}
