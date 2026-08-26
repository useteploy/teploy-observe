// Package incidents tracks time-windowed events that should render as
// vertical markers across all charts (release cut, alert firing, manual
// "we had an outage" notes). Two sources: 'alert' (auto, from the
// platform/alerts package) and 'manual' (user clicks "Declare incident").
package incidents

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
)

const (
	SourceAlert  = "alert"
	SourceManual = "manual"
	SourceCron   = "cron"
)

type Incident struct {
	IncidentID  string `json:"incident_id" db:"incident_id"`
	SiteID      string `json:"site_id" db:"site_id"`
	Title       string `json:"title" db:"title"`
	Description string `json:"description" db:"description"`
	Severity    string `json:"severity" db:"severity"`
	Source      string `json:"source" db:"source"`
	RuleID      string `json:"rule_id" db:"rule_id"`
	StartedAt   int64  `json:"started_at" db:"started_at"`
	EndedAt     int64  `json:"ended_at" db:"ended_at"`
	CreatedBy   string `json:"created_by" db:"created_by"`
	UpdatedAt   int64  `json:"updated_at" db:"updated_at"`
}

type Service struct {
	db *nucleus.Client
}

func NewService(db *nucleus.Client) *Service {
	return &Service{db: db}
}

type CreateInput struct {
	SiteID      string `json:"site_id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Severity    string `json:"severity"`
	Source      string `json:"source"`
	RuleID      string `json:"rule_id"`
	StartedAt   int64  `json:"started_at"`
}

// MaxInRange bounds what InRange hands back. A marker overlay is a hint, not a
// listing: past a few hundred bands the chart cannot draw them distinguishably
// and the JSON is megabytes the browser has to parse on every range change.
// The newest incidents are kept.
const MaxInRange = 1000

// EnsureOpen returns the incident already open for in.RuleID, creating one only
// when there is none. It is the only correct way to auto-declare from a repeating
// detector.
//
// The second return reports whether a row was written. A lookup failure is
// returned as an error and NOTHING is created: the previous shape at the two
// call sites was `if active, _ := ActiveByRule(...); len(active) > 0 { skip }`,
// which reads a failed query as "nothing is open" and declares a duplicate. With
// a detector on a 45s tick that is one incident per tick for as long as the
// query keeps failing.
func (s *Service) EnsureOpen(ctx context.Context, in CreateInput, createdBy string) (Incident, bool, error) {
	if in.RuleID == "" {
		return Incident{}, false, fmt.Errorf("EnsureOpen requires a rule_id")
	}
	active, err := s.ActiveByRule(ctx, in.RuleID)
	if err != nil {
		return Incident{}, false, err
	}
	if len(active) > 0 {
		return active[0], false, nil
	}
	inc, err := s.Create(ctx, in, createdBy)
	if err != nil {
		return Incident{}, false, err
	}
	return inc, true, nil
}

// Create inserts a new incident and returns the persisted row.
//
// Callers driven by a repeating detector must use EnsureOpen instead — Create
// always writes a new incident.
func (s *Service) Create(ctx context.Context, in CreateInput, createdBy string) (Incident, error) {
	if in.SiteID == "" {
		in.SiteID = "default"
	}
	if in.Title == "" {
		return Incident{}, fmt.Errorf("title required")
	}
	if in.Severity == "" {
		in.Severity = "info"
	}
	if in.Source == "" {
		in.Source = SourceManual
	}
	if in.StartedAt == 0 {
		in.StartedAt = time.Now().UnixMilli()
	}
	id := genID()
	now := time.Now().UnixMilli()
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO incidents (incident_id, tenant_id, site_id, title, description, severity,
		 source, rule_id, started_at, ended_at, created_by, updated_at)
		 VALUES ($1, 'default', $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		id, in.SiteID, in.Title, in.Description, in.Severity, in.Source, in.RuleID,
		dbutil.IntParam(in.StartedAt), dbutil.IntParam(int64(0)), createdBy, dbutil.IntParam(now))
	if err != nil {
		return Incident{}, err
	}
	return Incident{
		IncidentID:  id,
		SiteID:      in.SiteID,
		Title:       in.Title,
		Description: in.Description,
		Severity:    in.Severity,
		Source:      in.Source,
		RuleID:      in.RuleID,
		StartedAt:   in.StartedAt,
		CreatedBy:   createdBy,
		UpdatedAt:   now,
	}, nil
}

// Close marks the incident as ended now. Finds the latest version of the
// row and writes a new version with ended_at set.
//
// `incidents` is a PLAIN mergetree with no version column, so this appends
// rather than updates and the reads dedup by updated_at (see activeWhere). The
// `ORDER BY updated_at DESC LIMIT 1` is load-bearing, not decoration: it is
// what keeps this INSERT...SELECT-from-the-same-table to ONE row per call.
// Verified on Nucleus v0.1.8 - the identical statement without the LIMIT goes
// 1, 2, 4, 8 on successive calls - and confirmed on the live instance, where
// `incidents` holds exactly 2 rows for each of 6,170 closed incidents and 1 for
// each of the 13 open ones. TestCloseWritesExactlyOneRow pins it.
func (s *Service) Close(ctx context.Context, incidentID string) error {
	now := dbutil.IntParam(time.Now().UnixMilli())
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO incidents (incident_id, tenant_id, site_id, title, description, severity,
		 source, rule_id, started_at, ended_at, created_by, updated_at)
		 SELECT incident_id, tenant_id, site_id, title, description, severity,
		        source, rule_id, started_at, $2, created_by, $2
		 FROM incidents WHERE incident_id = $1 ORDER BY updated_at DESC LIMIT 1`,
		incidentID, now)
	return err
}

// CloseByRule closes any open incident whose rule_id matches. Used by
// the alert service when a rule transitions from firing back to normal.
func (s *Service) CloseByRule(ctx context.Context, ruleID string) error {
	actives, err := s.ActiveByRule(ctx, ruleID)
	if err != nil {
		return err
	}
	for _, inc := range actives {
		if err := s.Close(ctx, inc.IncidentID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) ActiveByRule(ctx context.Context, ruleID string) ([]Incident, error) {
	return s.activeWhere(ctx, `rule_id = $1`, ruleID)
}

// activeWhere returns the incidents matching the scope that are still open.
//
// The open filter MUST run after the dedup, never inside it. incidents is a
// plain mergetree and Close() appends a second row with ended_at>0, so an open
// row and a closed row for the same incident coexist. A `WHERE ended_at = 0`
// against the raw table drops the closed (latest) row, and the dedup then
// surfaces the stale open one — leaving a closed incident "active" forever,
// which also permanently suppresses auto-declare for that rule.
//
// The collapse runs in the database (see latestSelect). It used to be done in
// Go over every row the scope matched, which on the live instance meant reading
// and hydrating thousands of rows on a 45s tick just to answer "is one of these
// open?"; slow enough that a check-in's request context could be cancelled out
// from under it before the answer came back.
func (s *Service) activeWhere(ctx context.Context, whereFrag string, args ...any) ([]Incident, error) {
	query := `SELECT incident_id, site_id, title, description, severity, source, rule_id,
	                 started_at, ended_at, created_by, updated_at
	          FROM (` + latestSelect(whereFrag) + `) AS latest
	          WHERE ended_at = 0
	          ORDER BY started_at DESC, incident_id ASC`
	rows, err := nucleus.Query[Incident](ctx, s.db.SQL(), query, args...)
	if err != nil {
		return nil, err
	}
	if rows == nil {
		rows = []Incident{}
	}
	return rows, nil
}

// latestSelect collapses `incidents` to one row per incident_id, keeping the
// values of the highest-updated_at version. incidents has no version column, so
// updated_at is the version: Create sets it to the insert time and Close sets it
// to the close time, both monotonic per incident.
//
// This is the same argMax-grouped-by-the-key shape internal/query.LatestSelect
// builds for the ReplacingMergeTree tables. `FINAL` parses but is silently
// ignored by Nucleus, and the engine's own merge does not honour a version
// column, so an explicit argMax is the only form that collapses.
//
// The ORDER BY that callers wrap this in must name the OUTPUT aliases — Nucleus
// resolves ORDER BY against the select list's output names only and silently
// ignores a term it cannot resolve — and must be total, because the GROUP BY
// emits its rows in hash order.
func latestSelect(whereFrag string) string {
	if whereFrag == "" {
		whereFrag = "1 = 1"
	}
	return `SELECT incident_id,
	               argMax(site_id, updated_at)     AS site_id,
	               argMax(title, updated_at)       AS title,
	               argMax(description, updated_at) AS description,
	               argMax(severity, updated_at)    AS severity,
	               argMax(source, updated_at)      AS source,
	               argMax(rule_id, updated_at)     AS rule_id,
	               argMax(started_at, updated_at)  AS started_at,
	               argMax(ended_at, updated_at)    AS ended_at,
	               argMax(created_by, updated_at)  AS created_by,
	               MAX(updated_at)                 AS updated_at
	        FROM incidents WHERE ` + whereFrag + `
	        GROUP BY incident_id`
}

// InRange returns incidents that overlap [from, to] for the given site, newest
// first, capped at MaxInRange. Handles both closed incidents (ended_at > 0) and
// open ones (ended_at = 0 is treated as "ongoing through now").
//
// The collapse to one row per incident happens in the database; the overlap
// filter stays in Go because Nucleus reports BIGINT columns as text over the
// wire, which defeats a BIGINT range comparison in SQL.
func (s *Service) InRange(ctx context.Context, siteID string, from, to int64) ([]Incident, error) {
	query := `SELECT incident_id, site_id, title, description, severity, source, rule_id,
	                 started_at, ended_at, created_by, updated_at
	          FROM (` + latestSelect("site_id = $1") + `) AS latest
	          ORDER BY started_at DESC, incident_id ASC`
	rows, err := nucleus.Query[Incident](ctx, s.db.SQL(), query, siteID)
	if err != nil {
		return nil, err
	}
	out := make([]Incident, 0, len(rows))
	for _, inc := range rows {
		if inc.StartedAt > to {
			continue
		}
		if inc.EndedAt != 0 && inc.EndedAt < from {
			continue
		}
		out = append(out, inc)
		if len(out) >= MaxInRange {
			break
		}
	}
	return out, nil
}

// Active returns all open incidents (ended_at = 0) for a site.
func (s *Service) Active(ctx context.Context, siteID string) ([]Incident, error) {
	return s.activeWhere(ctx, `site_id = $1`, siteID)
}

func genID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
