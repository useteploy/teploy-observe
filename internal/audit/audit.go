// Package audit is observe's compliance/audit trail: an append-only,
// admin-only log of who did what, when, from where. It is the shared sink for
// access-audit events across the Teploy stack — observe's own admin mutations,
// the CLI, dash-initiated actions, and the Ship agent all record here (HashiCorp
// parity A4 + SOC2 evidence). Events are immutable: written synchronously,
// never updated, never deleted through the API.
package audit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"
	"github.com/useteploy/teploy-observe/internal/dbutil"
)

// AuditEvent is one immutable audit record. Timestamp is unix-millis (stored as
// BIGINT, matching the rest of observe).
type AuditEvent struct {
	AuditID   string `json:"id" db:"audit_id"`
	TenantID  string `json:"tenant_id" db:"tenant_id"`
	SiteID    string `json:"site_id" db:"site_id"`
	Timestamp int64  `json:"timestamp" db:"timestamp"`
	Actor     string `json:"actor" db:"actor"`           // username / api-key id / agent id ("" = system)
	ActorType string `json:"actor_type" db:"actor_type"` // user | apikey | agent | system
	Action    string `json:"action" db:"action"`         // dotted verb, e.g. auth.login, user.create, sql.run
	Target    string `json:"target" db:"target"`         // resource acted on (id/name), optional
	Result    string `json:"result" db:"result"`         // success | failure | denied
	SourceIP  string `json:"source_ip" db:"source_ip"`
	UserAgent string `json:"user_agent" db:"user_agent"`
	Metadata  string `json:"metadata" db:"metadata"` // JSON object of extra context
}

// Result constants.
const (
	ResultSuccess = "success"
	ResultFailure = "failure"
	ResultDenied  = "denied"
)

// Actor-type constants.
const (
	ActorUser   = "user"
	ActorAPIKey = "apikey"
	ActorAgent  = "agent"
	ActorSystem = "system"
)

const (
	defaultLimit = 200
	maxLimit     = 1000
)

// Service is the audit store. Construct one shared instance and hand it to
// every producer (like the other observe services).
type Service struct {
	db *nucleus.Client
}

// NewService wires the audit store to the shared Nucleus client.
func NewService(db *nucleus.Client) *Service {
	return &Service{db: db}
}

// Filter narrows an audit query. All fields are optional; zero-value = no
// constraint. From/To are unix-millis bounds (inclusive).
type Filter struct {
	SiteID string
	Actor  string
	Action string
	Result string
	From   int64
	To     int64
	Limit  int
}

var auditColumns = "audit_id, tenant_id, site_id, timestamp, actor, actor_type, action, target, result, source_ip, user_agent, metadata"

// Record writes one audit event synchronously (never via the lossy ingest
// buffer — an audit trail must be durable and immediate). Defaults are filled
// for id/timestamp/tenant/actor_type/result; Action is required.
func (s *Service) Record(ctx context.Context, ev AuditEvent) error {
	if strings.TrimSpace(ev.Action) == "" {
		return fmt.Errorf("audit: action is required")
	}
	if ev.AuditID == "" {
		ev.AuditID = genID()
	}
	if ev.Timestamp == 0 {
		ev.Timestamp = time.Now().UnixMilli()
	}
	if ev.TenantID == "" {
		ev.TenantID = "default"
	}
	if ev.SiteID == "" {
		ev.SiteID = "default"
	}
	if ev.ActorType == "" {
		ev.ActorType = ActorUser
	}
	if ev.Result == "" {
		ev.Result = ResultSuccess
	}
	if ev.Metadata == "" {
		ev.Metadata = "{}"
	}

	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO audit_events (`+auditColumns+`)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		ev.AuditID, ev.TenantID, ev.SiteID, dbutil.IntParam(ev.Timestamp),
		ev.Actor, ev.ActorType, ev.Action, ev.Target, ev.Result,
		ev.SourceIP, ev.UserAgent, ev.Metadata)
	return err
}

// List returns audit events matching the filter, newest first.
func (s *Service) List(ctx context.Context, f Filter) ([]AuditEvent, error) {
	query, args := buildListQuery(f)
	rows, err := nucleus.Query[AuditEvent](ctx, s.db.SQL(), query, args...)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// buildListQuery is the pure SQL builder (unit-tested without a DB). Time
// bounds use CAST(timestamp AS BIGINT) because Nucleus returns BIGINT as text
// over the wire, which would otherwise defeat range comparisons — and an audit
// log is unbounded, so filtering must happen in SQL, not in Go.
func buildListQuery(f Filter) (string, []any) {
	var conds []string
	var args []any
	add := func(frag string, val any) {
		args = append(args, val)
		conds = append(conds, fmt.Sprintf(frag, len(args)))
	}

	if f.SiteID != "" {
		add("site_id = $%d", f.SiteID)
	}
	if f.Actor != "" {
		add("actor = $%d", f.Actor)
	}
	if f.Action != "" {
		add("action = $%d", f.Action)
	}
	if f.Result != "" {
		add("result = $%d", f.Result)
	}
	if f.From > 0 {
		add("CAST(timestamp AS BIGINT) >= $%d", dbutil.IntParam(f.From))
	}
	if f.To > 0 {
		add("CAST(timestamp AS BIGINT) <= $%d", dbutil.IntParam(f.To))
	}

	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}

	limit := f.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}

	// limit is a validated int (clamped), inlined rather than parameterized —
	// some engines reject a bound param in LIMIT.
	query := "SELECT " + auditColumns + " FROM audit_events" + where +
		" ORDER BY CAST(timestamp AS BIGINT) DESC LIMIT " + fmt.Sprint(limit)
	return query, args
}

// MarshalMetadata is a convenience for producers: turn a map into the JSON
// string stored in Metadata. Returns "{}" on nil/empty or marshal failure so a
// metadata problem never blocks the audit write.
func MarshalMetadata(m map[string]any) string {
	if len(m) == 0 {
		return "{}"
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func genID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
