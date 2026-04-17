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

// LatestEvents returns the most recent error events for an issue.
func (s *IssueService) LatestEvents(ctx context.Context, issueID, siteID string, limit int) ([]ErrorEvent, error) {
	if limit <= 0 {
		limit = 10
	}
	return nucleus.Query[ErrorEvent](ctx, s.db.SQL(),
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
}

// DailyCount represents error volume for a single day (UTC).
type DailyCount struct {
	Day   string `json:"day" db:"day"`
	Count int64  `json:"count" db:"count"`
}

// DailyCounts returns error counts per UTC day for the last `days` days.
// Missing days are zero-filled so the client can render a continuous bar chart.
func (s *IssueService) DailyCounts(ctx context.Context, siteID string, days int) ([]DailyCount, error) {
	if days <= 0 || days > 90 {
		days = 14
	}
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	fromTime := today.AddDate(0, 0, -(days - 1))
	fromMs := strconv.FormatInt(fromTime.UnixMilli(), 10)

	type rawRow struct {
		Bucket int64 `db:"bucket"`
		Count  int64 `db:"count"`
	}
	rows, err := nucleus.Query[rawRow](ctx, s.db.SQL(),
		`SELECT (CAST(timestamp AS BIGINT) / 86400000) * 86400000 AS bucket,
		        COUNT(*) AS count
		 FROM error_events
		 WHERE site_id = $1 AND CAST(timestamp AS BIGINT) >= CAST($2 AS BIGINT)
		 GROUP BY (CAST(timestamp AS BIGINT) / 86400000) * 86400000
		 ORDER BY bucket ASC`,
		siteID, fromMs,
	)
	if err != nil {
		return nil, err
	}

	byDay := make(map[string]int64, len(rows))
	for _, r := range rows {
		t := time.UnixMilli(r.Bucket).UTC()
		byDay[t.Format("2006-01-02")] = r.Count
	}

	result := make([]DailyCount, 0, days)
	for i := 0; i < days; i++ {
		d := today.AddDate(0, 0, -(days-1-i))
		key := d.Format("2006-01-02")
		result = append(result, DailyCount{Day: key, Count: byDay[key]})
	}
	return result, nil
}

func generateID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
