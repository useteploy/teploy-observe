package errors

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"
)

// IssueService manages error grouping, issue creation, and the grouphash-to-issue KV cache.
type IssueService struct {
	db *nucleus.Client
}

func NewIssueService(db *nucleus.Client) *IssueService {
	return &IssueService{db: db}
}

// Issue represents a grouped error issue. Numeric and timestamp fields are
// typed for API consumers; the DB row shape stays in issueRow.
type Issue struct {
	IssueID    string    `json:"issue_id"`
	SiteID     string    `json:"site_id"`
	GroupHash  string    `json:"group_hash"`
	Title      string    `json:"title"`
	Culprit    string    `json:"culprit"`
	Level      string    `json:"level"`
	Status     string    `json:"status"`
	FirstSeen  time.Time `json:"first_seen"`
	LastSeen   time.Time `json:"last_seen"`
	EventCount int64     `json:"event_count"`
	UserCount  int64     `json:"user_count"`
	ReleaseTag string    `json:"release_tag"`
}

type issueRow struct {
	IssueID    string `db:"issue_id"`
	TenantID   string `db:"tenant_id"`
	SiteID     string `db:"site_id"`
	GroupHash  string `db:"group_hash"`
	Title      string `db:"title"`
	Culprit    string `db:"culprit"`
	Level      string `db:"level"`
	Status     string `db:"status"`
	FirstSeen  string `db:"first_seen"`
	LastSeen   string `db:"last_seen"`
	EventCount string `db:"event_count"`
	UserCount  string `db:"user_count"`
	ReleaseTag string `db:"release_tag"`
	Version    string `db:"version"`
}

func parseEpochMillis(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if ms, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.UnixMilli(ms).UTC()
	}
	return time.Time{}
}

func parseInt64(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

func (r issueRow) toDomain() Issue {
	return Issue{
		IssueID: r.IssueID, SiteID: r.SiteID, GroupHash: r.GroupHash,
		Title: r.Title, Culprit: r.Culprit, Level: r.Level, Status: r.Status,
		FirstSeen: parseEpochMillis(r.FirstSeen), LastSeen: parseEpochMillis(r.LastSeen),
		EventCount: parseInt64(r.EventCount), UserCount: parseInt64(r.UserCount),
		ReleaseTag: r.ReleaseTag,
	}
}

// kvCacheKey returns the KV key for a grouphash-to-issue mapping.
func kvCacheKey(siteID, groupHash string) string {
	return fmt.Sprintf("gh2issue:%s:%s", siteID, groupHash)
}

// cachedIssue is the minimal data stored in KV for fast lookups.
type cachedIssue struct {
	IssueID    string `json:"id"`
	EventCount int64  `json:"ec"`
}

// ResolveIssue looks up or creates an issue for the given grouphash.
// Uses KV cache for O(1) hot-path lookups. Returns the issue_id.
func (s *IssueService) ResolveIssue(ctx context.Context, siteID, groupHash, title, culprit, level, release string, ts int64) (string, error) {
	kv := s.db.KV()
	cacheKey := kvCacheKey(siteID, groupHash)

	// 1. Check KV cache
	data, err := kv.Get(ctx, cacheKey)
	if err == nil && data != nil {
		var ci cachedIssue
		if json.Unmarshal(data, &ci) == nil && ci.IssueID != "" {
			newCount := ci.EventCount + 1
			_ = s.bumpIssue(ctx, ci.IssueID, siteID, ts, newCount)
			ci.EventCount = newCount
			if raw, err := json.Marshal(ci); err == nil {
				_ = kv.Set(ctx, cacheKey, raw)
			}
			return ci.IssueID, nil
		}
	}

	// 2. Cache miss — check DB
	existing, err := s.findIssueByHash(ctx, siteID, groupHash)
	if err == nil && existing != nil {
		newCount := existing.EventCount + 1
		_ = s.bumpIssue(ctx, existing.IssueID, siteID, ts, newCount)
		ci := cachedIssue{IssueID: existing.IssueID, EventCount: newCount}
		if raw, err := json.Marshal(ci); err == nil {
			_ = kv.Set(ctx, cacheKey, raw)
		}
		return existing.IssueID, nil
	}

	// 3. New issue — create
	issueID := generateID()
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	tsStr := strconv.FormatInt(ts, 10)
	sql := s.db.SQL()
	_, err = sql.Exec(ctx,
		`INSERT INTO issues (issue_id, tenant_id, site_id, group_hash, title, culprit, level, status, first_seen, last_seen, event_count, user_count, release_tag, version)
		 VALUES ($1, 'default', $2, $3, $4, $5, $6, 'open', $7, $8, '1', '0', $9, $10)`,
		issueID, siteID, groupHash, title, culprit, level, tsStr, tsStr, release, now,
	)
	if err != nil {
		return "", fmt.Errorf("create issue: %w", err)
	}

	ci := cachedIssue{IssueID: issueID, EventCount: 1}
	if raw, err := json.Marshal(ci); err == nil {
		_ = kv.Set(ctx, cacheKey, raw)
	}

	return issueID, nil
}

func (s *IssueService) findIssueByHash(ctx context.Context, siteID, groupHash string) (*Issue, error) {
	rows, err := nucleus.Query[issueRow](ctx, s.db.SQL(),
		`SELECT issue_id, tenant_id, site_id, group_hash, title, culprit, level, status,
			first_seen, last_seen, event_count, user_count, release_tag, version
		 FROM issues
		 WHERE site_id = $1 AND group_hash = $2`,
		siteID, groupHash,
	)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	d := rows[0].toDomain()
	return &d, nil
}

func (s *IssueService) bumpIssue(ctx context.Context, issueID, siteID string, lastSeen, newCount int64) error {
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	lastSeenStr := strconv.FormatInt(lastSeen, 10)
	newCountStr := strconv.FormatInt(newCount, 10)
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO issues (issue_id, tenant_id, site_id, group_hash, title, culprit, level, status, first_seen, last_seen, event_count, user_count, release_tag, version)
		 SELECT issue_id, tenant_id, site_id, group_hash, title, culprit, level, status,
			first_seen, $3 AS last_seen, $4 AS event_count, user_count, release_tag, $5 AS version
		 FROM issues
		 WHERE issue_id = $1 AND site_id = $2`,
		issueID, siteID, lastSeenStr, newCountStr, now,
	)
	return err
}

// UpdateStatus changes an issue's status (open, resolved, ignored).
func (s *IssueService) UpdateStatus(ctx context.Context, issueID, siteID, status string) error {
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO issues (issue_id, tenant_id, site_id, group_hash, title, culprit, level, status, first_seen, last_seen, event_count, user_count, release_tag, version)
		 SELECT issue_id, tenant_id, site_id, group_hash, title, culprit, level, $3 AS status,
			first_seen, last_seen, event_count, user_count, release_tag, $4 AS version
		 FROM issues
		 WHERE issue_id = $1 AND site_id = $2`,
		issueID, siteID, status, now,
	)
	return err
}

// ListIssues returns issues for a site, ordered by last_seen descending.
func (s *IssueService) ListIssues(ctx context.Context, siteID, status string, limit, offset int) ([]Issue, error) {
	if limit <= 0 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	var q string
	var params []any
	if status != "" {
		q = fmt.Sprintf(`SELECT issue_id, tenant_id, site_id, group_hash, title, culprit, level, status,
			first_seen, last_seen, event_count, user_count, release_tag, version
		 FROM issues
		 WHERE site_id = $1 AND status = $2
		 ORDER BY last_seen DESC
		 LIMIT %d OFFSET %d`, limit, offset)
		params = []any{siteID, status}
	} else {
		q = fmt.Sprintf(`SELECT issue_id, tenant_id, site_id, group_hash, title, culprit, level, status,
			first_seen, last_seen, event_count, user_count, release_tag, version
		 FROM issues
		 WHERE site_id = $1
		 ORDER BY last_seen DESC
		 LIMIT %d OFFSET %d`, limit, offset)
		params = []any{siteID}
	}
	rows, err := nucleus.Query[issueRow](ctx, s.db.SQL(), q, params...)
	if err != nil {
		return nil, err
	}
	out := make([]Issue, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.toDomain())
	}
	return out, nil
}

// GetIssue returns a single issue by ID.
func (s *IssueService) GetIssue(ctx context.Context, issueID, siteID string) (*Issue, error) {
	rows, err := nucleus.Query[issueRow](ctx, s.db.SQL(),
		`SELECT issue_id, tenant_id, site_id, group_hash, title, culprit, level, status,
			first_seen, last_seen, event_count, user_count, release_tag, version
		 FROM issues
		 WHERE issue_id = $1 AND site_id = $2`,
		issueID, siteID,
	)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	d := rows[0].toDomain()
	return &d, nil
}

// ErrorEvent represents a stored error event.
type ErrorEvent struct {
	ErrorID     string    `json:"error_id"`
	SiteID      string    `json:"site_id"`
	SessionID   string    `json:"session_id"`
	IssueID     string    `json:"issue_id"`
	GroupHash   string    `json:"group_hash"`
	Timestamp   time.Time `json:"timestamp"`
	ErrorType   string    `json:"error_type"`
	ErrorValue  string    `json:"error_value"`
	Mechanism   string    `json:"mechanism"`
	Handled     bool      `json:"handled"`
	Level       string    `json:"level"`
	ReleaseTag  string    `json:"release_tag"`
	Environment string    `json:"environment"`
	URL         string    `json:"url"`
	Browser     string    `json:"browser"`
	OS          string    `json:"os"`
	Device      string    `json:"device"`
	StackTrace  string    `json:"stack_trace"`
	Breadcrumbs string    `json:"breadcrumbs"`
	Contexts    string    `json:"contexts"`
	Extra       string    `json:"extra"`
}

type errorEventRow struct {
	ErrorID     string `db:"error_id"`
	TenantID    string `db:"tenant_id"`
	SiteID      string `db:"site_id"`
	SessionID   string `db:"session_id"`
	IssueID     string `db:"issue_id"`
	GroupHash   string `db:"group_hash"`
	Timestamp   string `db:"timestamp"`
	ErrorType   string `db:"error_type"`
	ErrorValue  string `db:"error_value"`
	Mechanism   string `db:"mechanism"`
	Handled     string `db:"handled"`
	Level       string `db:"level"`
	ReleaseTag  string `db:"release_tag"`
	Environment string `db:"environment"`
	URL         string `db:"url"`
	Browser     string `db:"browser"`
	OS          string `db:"os"`
	Device      string `db:"device"`
	StackTrace  string `db:"stack_trace"`
	Breadcrumbs string `db:"breadcrumbs"`
	Contexts    string `db:"contexts"`
	Extra       string `db:"extra"`
}

func parseBool(s string) bool {
	switch s {
	case "true", "1", "TRUE", "True":
		return true
	}
	return false
}

func (r errorEventRow) toDomain() ErrorEvent {
	return ErrorEvent{
		ErrorID: r.ErrorID, SiteID: r.SiteID, SessionID: r.SessionID,
		IssueID: r.IssueID, GroupHash: r.GroupHash,
		Timestamp:  parseEpochMillis(r.Timestamp),
		ErrorType:  r.ErrorType,
		ErrorValue: r.ErrorValue,
		Mechanism:  r.Mechanism,
		Handled:    parseBool(r.Handled),
		Level:      r.Level, ReleaseTag: r.ReleaseTag, Environment: r.Environment,
		URL: r.URL, Browser: r.Browser, OS: r.OS, Device: r.Device,
		StackTrace: r.StackTrace, Breadcrumbs: r.Breadcrumbs,
		Contexts: r.Contexts, Extra: r.Extra,
	}
}

// LatestEvents returns the most recent error events for an issue.
func (s *IssueService) LatestEvents(ctx context.Context, issueID, siteID string, limit int) ([]ErrorEvent, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := nucleus.Query[errorEventRow](ctx, s.db.SQL(),
		fmt.Sprintf(`SELECT error_id, tenant_id, site_id, session_id, issue_id, group_hash,
			CAST(timestamp AS TEXT) AS timestamp,
			error_type, error_value, mechanism,
			COALESCE(handled, 'true') AS handled, level, release_tag, environment, url,
			browser, os, device,
			COALESCE(stack_trace, '') AS stack_trace,
			COALESCE(breadcrumbs, '') AS breadcrumbs,
			COALESCE(contexts, '') AS contexts,
			COALESCE(extra, '') AS extra
		 FROM error_events
		 WHERE issue_id = $1 AND site_id = $2
		 ORDER BY timestamp DESC
		 LIMIT %d`, limit),
		issueID, siteID,
	)
	if err != nil {
		return nil, err
	}
	out := make([]ErrorEvent, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.toDomain())
	}
	return out, nil
}

func generateID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
