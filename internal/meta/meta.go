// Package meta exposes self-observability endpoints — stats about Observe
// itself (ingest rate, table sizes, retention policies, buffer depth).
package meta

import (
	"context"
	"fmt"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/observe/internal/jobs"
)

// Snapshot is the wire shape of GET /api/v1/meta.
type Snapshot struct {
	GeneratedAt  time.Time     `json:"generated_at"`
	Version      string        `json:"version"`
	Uptime       string        `json:"uptime"`
	IngestRate   IngestMetrics `json:"ingest"`
	Tables       []TableSize   `json:"tables"`
	Retention    []Policy      `json:"retention"`
}

// IngestMetrics measures what's coming in over the last minute.
type IngestMetrics struct {
	EventsLast1m int64 `json:"events_last_1m"`
	EventsLast1h int64 `json:"events_last_1h"`
	ErrorsLast1m int64 `json:"errors_last_1m"`
	LogsLast1m   int64 `json:"logs_last_1m"`
	SpansLast1m  int64 `json:"spans_last_1m"`
}

// TableSize reports approximate row count per table.
type TableSize struct {
	Table string `json:"table"`
	Rows  int64  `json:"rows"`
}

// Policy mirrors jobs.RetentionPolicy for the API shape.
type Policy struct {
	Table string `json:"table"`
	Days  int    `json:"days"`
}

// Service builds snapshots on demand.
type Service struct {
	db        *nucleus.Client
	retention *jobs.RetentionService
	startedAt time.Time
	version   string
}

// New constructs the service. `version` is usually the build version string.
func New(db *nucleus.Client, retention *jobs.RetentionService, version string) *Service {
	return &Service{
		db:        db,
		retention: retention,
		startedAt: time.Now().UTC(),
		version:   version,
	}
}

// Snapshot gathers a fresh stats snapshot.
func (s *Service) Snapshot(ctx context.Context) (Snapshot, error) {
	now := time.Now().UTC()
	snap := Snapshot{
		GeneratedAt: now,
		Version:     s.version,
		Uptime:      now.Sub(s.startedAt).Round(time.Second).String(),
	}

	// Ingest rates — count rows with timestamp > cutoff.
	snap.IngestRate.EventsLast1m = s.countSince(ctx, "events", "timestamp", now.Add(-time.Minute))
	snap.IngestRate.EventsLast1h = s.countSince(ctx, "events", "timestamp", now.Add(-time.Hour))
	snap.IngestRate.ErrorsLast1m = s.countSince(ctx, "error_events", "timestamp", now.Add(-time.Minute))
	snap.IngestRate.LogsLast1m = s.countSince(ctx, "logs", "timestamp", now.Add(-time.Minute))
	snap.IngestRate.SpansLast1m = s.countSince(ctx, "spans", "start_time", now.Add(-time.Minute))

	// Table sizes — a small curated list; full counts would be noisy.
	for _, t := range []string{
		"events", "events_recent", "sessions", "error_events", "issues",
		"logs", "spans", "replay_sessions", "feature_flags",
	} {
		snap.Tables = append(snap.Tables, TableSize{
			Table: t,
			Rows:  s.countTotal(ctx, t),
		})
	}

	// Retention policies for transparency.
	if s.retention != nil {
		for _, p := range s.retention.Policies() {
			snap.Retention = append(snap.Retention, Policy{Table: p.Table, Days: p.Days})
		}
	}

	return snap, nil
}

type countRow struct {
	Count int64 `db:"count"`
}

func (s *Service) countSince(ctx context.Context, table, col string, since time.Time) int64 {
	query := fmt.Sprintf(
		`SELECT COUNT(*) AS count FROM %s WHERE CAST(%s AS BIGINT) >= CAST($1 AS BIGINT)`,
		table, col,
	)
	rows, err := nucleus.Query[countRow](ctx, s.db.SQL(), query, since.UnixMilli())
	if err != nil || len(rows) == 0 {
		return 0
	}
	return rows[0].Count
}

func (s *Service) countTotal(ctx context.Context, table string) int64 {
	rows, err := nucleus.Query[countRow](ctx, s.db.SQL(),
		fmt.Sprintf(`SELECT COUNT(*) AS count FROM %s`, table))
	if err != nil || len(rows) == 0 {
		return 0
	}
	return rows[0].Count
}
