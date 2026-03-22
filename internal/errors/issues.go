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

// Issue represents a grouped error issue.
type Issue struct {
	IssueID    string `json:"issue_id" db:"issue_id"`
	TenantID   string `json:"tenant_id" db:"tenant_id"`
	SiteID     string `json:"site_id" db:"site_id"`
	GroupHash  string `json:"group_hash" db:"group_hash"`
	Title      string `json:"title" db:"title"`
	Culprit    string `json:"culprit" db:"culprit"`
	Level      string `json:"level" db:"level"`
	Status     string `json:"status" db:"status"`
	FirstSeen  string `json:"first_seen" db:"first_seen"`
	LastSeen   string `json:"last_seen" db:"last_seen"`
	EventCount string `json:"event_count" db:"event_count"`
	UserCount  string `json:"user_count" db:"user_count"`
	ReleaseTag string `json:"release_tag" db:"release_tag"`
	Version    string `json:"-" db:"version"`
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
		ec, _ := strconv.ParseInt(existing.EventCount, 10, 64)
		newCount := ec + 1
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
	rows, err := nucleus.Query[Issue](ctx, s.db.SQL(),
		`SELECT issue_id, tenant_id, site_id, group_hash, title, culprit, level, status,
			first_seen, last_seen, event_count, user_count, release_tag, version
		 FROM issues
		 WHERE site_id = $1 AND group_hash = $2`,
		siteID, groupHash,
	)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return &rows[0], nil
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
func (s *IssueService) ListIssues(ctx context.Context, siteID, status string, limit int) ([]Issue, error) {
	if limit <= 0 {
		limit = 20
	}
	var q string
	var params []any
	if status != "" {
		q = fmt.Sprintf(`SELECT issue_id, tenant_id, site_id, group_hash, title, culprit, level, status,
			first_seen, last_seen, event_count, user_count, release_tag, version
		 FROM issues
		 WHERE site_id = $1 AND status = $2
		 ORDER BY last_seen DESC
		 LIMIT %d`, limit)
		params = []any{siteID, status}
	} else {
		q = fmt.Sprintf(`SELECT issue_id, tenant_id, site_id, group_hash, title, culprit, level, status,
			first_seen, last_seen, event_count, user_count, release_tag, version
		 FROM issues
		 WHERE site_id = $1
		 ORDER BY last_seen DESC
		 LIMIT %d`, limit)
		params = []any{siteID}
	}
	return nucleus.Query[Issue](ctx, s.db.SQL(), q, params...)
}

// GetIssue returns a single issue by ID.
func (s *IssueService) GetIssue(ctx context.Context, issueID, siteID string) (*Issue, error) {
	rows, err := nucleus.Query[Issue](ctx, s.db.SQL(),
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
	return &rows[0], nil
}

// ErrorEvent represents a stored error event.
type ErrorEvent struct {
	ErrorID     string `json:"error_id" db:"error_id"`
	TenantID    string `json:"-" db:"tenant_id"`
	SiteID      string `json:"site_id" db:"site_id"`
	SessionID   string `json:"session_id" db:"session_id"`
	IssueID     string `json:"issue_id" db:"issue_id"`
	GroupHash   string `json:"group_hash" db:"group_hash"`
	Timestamp   int64  `json:"timestamp" db:"timestamp"`
	ErrorType   string `json:"error_type" db:"error_type"`
	ErrorValue  string `json:"error_value" db:"error_value"`
	Mechanism   string `json:"mechanism" db:"mechanism"`
	Handled     string `json:"handled" db:"handled"`
	Level       string `json:"level" db:"level"`
	ReleaseTag  string `json:"release_tag" db:"release_tag"`
	Environment string `json:"environment" db:"environment"`
	URL         string `json:"url" db:"url"`
	Browser     string `json:"browser" db:"browser"`
	OS          string `json:"os" db:"os"`
	Device      string `json:"device" db:"device"`
	StackTrace  string `json:"stack_trace" db:"stack_trace"`
	Breadcrumbs string `json:"breadcrumbs" db:"breadcrumbs"`
	Contexts    string `json:"contexts" db:"contexts"`
	Extra       string `json:"extra" db:"extra"`
}

// LatestEvents returns the most recent error events for an issue.
func (s *IssueService) LatestEvents(ctx context.Context, issueID, siteID string, limit int) ([]ErrorEvent, error) {
	if limit <= 0 {
		limit = 10
	}
	return nucleus.Query[ErrorEvent](ctx, s.db.SQL(),
		fmt.Sprintf(`SELECT error_id, tenant_id, site_id, session_id, issue_id, group_hash,
			timestamp, error_type, error_value, mechanism,
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
}

func generateID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
