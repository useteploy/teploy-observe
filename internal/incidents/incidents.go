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

// Create inserts a new incident and returns the persisted row.
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

// activeWhere reads every incident row matching the scope, dedups to the latest
// row per incident, then keeps only those still open (ended_at == 0).
//
// The open filter MUST run after dedup. incidents is a plain mergetree and
// Close() appends a second row with ended_at>0, so an open row and a closed row
// for the same incident coexist. Filtering ended_at='0' in SQL drops the closed
// (latest) row, and dedup then surfaces the stale open row — leaving a closed
// incident "active" forever, which also permanently suppresses OnTrigger
// auto-declare for that rule.
func (s *Service) activeWhere(ctx context.Context, whereFrag string, args ...any) ([]Incident, error) {
	latest, err := s.dedupLatest(ctx, whereFrag, args...)
	if err != nil {
		return nil, err
	}
	out := make([]Incident, 0, len(latest))
	for _, inc := range latest {
		if inc.EndedAt == 0 {
			out = append(out, inc)
		}
	}
	return out, nil
}

// InRange returns incidents that overlap [from, to] for the given site.
// Handles both closed incidents (ended_at > 0) and open ones
// (ended_at = 0 is treated as "ongoing through now").
//
// We read all rows for the site and filter in-process because Nucleus
// reports BIGINT columns as text over the wire, which defeats BIGINT
// comparisons. This is cheap as long as the incident count is bounded
// (~thousands per site).
func (s *Service) InRange(ctx context.Context, siteID string, from, to int64) ([]Incident, error) {
	query := `SELECT incident_id, site_id, title, description, severity, source, rule_id,
	                 started_at, ended_at, created_by, updated_at
	          FROM incidents WHERE site_id = $1 ORDER BY updated_at DESC`
	rows, err := nucleus.Query[Incident](ctx, s.db.SQL(), query, siteID)
	if err != nil {
		return nil, err
	}
	latest := dedupByID(rows)
	out := make([]Incident, 0, len(latest))
	for _, inc := range latest {
		if inc.StartedAt > to {
			continue
		}
		if inc.EndedAt != 0 && inc.EndedAt < from {
			continue
		}
		out = append(out, inc)
	}
	return out, nil
}

// Active returns all open incidents (ended_at = 0) for a site.
func (s *Service) Active(ctx context.Context, siteID string) ([]Incident, error) {
	return s.activeWhere(ctx, `site_id = $1`, siteID)
}

func (s *Service) dedupLatest(ctx context.Context, whereFrag string, args ...any) ([]Incident, error) {
	query := `SELECT incident_id, site_id, title, description, severity, source, rule_id,
	                 started_at, ended_at, created_by, updated_at
	          FROM incidents WHERE ` + whereFrag + ` ORDER BY updated_at DESC`
	rows, err := nucleus.Query[Incident](ctx, s.db.SQL(), query, args...)
	if err != nil {
		return nil, err
	}
	return dedupByID(rows), nil
}

func dedupByID(rows []Incident) []Incident {
	seen := make(map[string]struct{}, len(rows))
	out := make([]Incident, 0, len(rows))
	for _, r := range rows {
		if _, ok := seen[r.IncidentID]; ok {
			continue
		}
		seen[r.IncidentID] = struct{}{}
		out = append(out, r)
	}
	return out
}

func genID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
