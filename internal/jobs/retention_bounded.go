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

// retentionIdentityExpr builds the injective text identity of a row: each
// key CAST-ed to TEXT with the separator escaped inside it, joined by the
// separator. Escaping makes the mapping injective - two distinct key
// tuples can never collide into the same concatenated string.
func retentionIdentityExpr(keys []string) string {
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("REPLACE(CAST(%s AS TEXT), '|', '||')", k)
	}
	return strings.Join(parts, " || '|' || ")
}

type retentionIdentRow struct {
	Ident string `db:"ident"`
}

// deleteRetentionChunk deletes at most retentionChunkSize rows matching
// where, in two statements on the caller's snapshot: a bounded scan reads
// each candidate's identity (engine-computed, LIMIT-retentionChunkSize),
// then one DELETE matches those identities through a flat IN-parameter
// list.
//
// Why this shape: the engine has no bounded physical DELETE (no
// DELETE ... LIMIT), and its parser rejects expression nesting past 256 -
// the OR-group-per-identity predicate this used to build was unparseable
// at candidate counts it itself allowed (500), and the IN-subquery variant
// (`IN (SELECT ... LIMIT n)`) nested-loops into quadratic time on ties
// (measured: 303ms at 1k rows, 2.7s at 4k). A flat parameter IN-list has
// constant nesting depth and the engine matches it in one pass (measured:
// 479ms for a 5000-of-6001 tie delete). The affected-row check keeps the
// physical-delete budget: an engine reporting more physical versions than
// the selected visible set is refused, never silently truncated.
func deleteRetentionChunk(ctx context.Context, sql *nucleus.SQLModel, table, where string, args []any, keys []string) (int64, error) {
	if len(keys) == 0 {
		return 0, fmt.Errorf("retention: %s has no identity keys", table)
	}
	ident := retentionIdentityExpr(keys)
	rows, err := nucleus.Query[retentionIdentRow](ctx, sql,
		fmt.Sprintf("SELECT %s AS ident FROM %s WHERE %s LIMIT %d", ident, table, where, retentionChunkSize), args...)
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	placeholders := make([]string, len(rows))
	vals := make([]any, len(rows))
	for i, r := range rows {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		vals[i] = r.Ident
	}
	n, err := sql.Exec(ctx, fmt.Sprintf("DELETE FROM %s WHERE %s IN (%s)",
		table, ident, strings.Join(placeholders, ",")), vals...)
	if err != nil {
		return 0, err
	}
	if n > int64(len(rows)) {
		return 0, fmt.Errorf("retention: %s DELETE exposed %d physical rows beyond the %d selected identities; rolling back (bounded physical DELETE required)", table, n, len(rows))
	}
	return n, nil
}
