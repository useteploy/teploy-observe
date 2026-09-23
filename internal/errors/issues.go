package errors

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/query"
)

// issues is a ReplacingMergeTree keyed on (tenant_id, site_id, issue_id) whose
// rows are rewritten on every error batch. Nucleus does not reliably collapse
// the superseded versions, so both halves of every access have to be explicit:
//
//   - a READ collapses with argMax over version (issuesLatest) — otherwise it
//     returns an arbitrary version, and a status set by UpdateStatus is invisible.
//   - a WRITE reads through that same collapse before it inserts, so exactly one
//     row is written. The previous shape was
//     `INSERT INTO issues SELECT ... FROM issues WHERE issue_id = $1`, which
//     inserts one row per row already present: measured on a scratch Nucleus,
//     the physical count for a single issue went 1, 2, 4, 8 … 4096 over twelve
//     bumps. Live, that reached 16,847,389 rows.
var issueCols = []string{
	"group_hash", "title", "culprit", "level", "status",
	"first_seen", "last_seen", "event_count", "user_count",
	"release_tag", "fingerprint_version", "first_regression_at",
	"regression_count", "snooze_until", "version",
}

// issuesLatest renders the collapsed derived table, aliased `issues` so the
// surrounding query reads unchanged. where is applied before the collapse, so
// pass only version-stable predicates (the key, or group_hash); status and
// last_seen change between versions and must be filtered outside.
func issuesLatest(where string) string {
	return query.LatestRows("issues", issueCols, where) + " AS issues"
}

const issueSelectCols = `issue_id, tenant_id, site_id, group_hash, title, culprit, level, status,
			first_seen, last_seen, event_count, user_count, release_tag,
			fingerprint_version, first_regression_at, regression_count, snooze_until, version`

// IssueService manages error grouping, issue creation, and the grouphash-to-issue KV cache.
type IssueService struct {
	db *nucleus.Client
}

func NewIssueService(db *nucleus.Client) *IssueService {
	return &IssueService{db: db}
}

// Issue represents a grouped error issue.
//
// Lifecycle semantics (O05 slice 1; full table in AUDIT_OPEN.md):
//
//   - Status is open | resolved | ignored. A NEW event on a resolved
//     issue reopens it (status open) and bumps the regression markers;
//     an ignored issue stays ignored; an open issue stays open.
//   - FirstRegressionAt / RegressionCount distinguish
//     resolved-then-reopened from continuous: zero count + zero time =
//     the issue has never been reopened by an event (or predates 047,
//     when regressions were not tracked — the honest backfill).
//   - SnoozeUntil (zero = none) marks a resolved issue that auto-hides
//     until a deadline. SnoozeActive is computed at read time: a snooze
//     past its deadline with no new events reads as plain resolved.
//   - FirstSeen/LastSeen are INGESTION-time (the pinned truth: the
//     error wire protocol carries no client event-time, O03 ADR D8),
//     clamped on every bump so an out-of-order apply (PENDING retry,
//     WAL replay) can widen the window but never regress it.
//   - UserCount/EventCount are computed on read from error_events.
//   - FingerprintVersion is the grouping derivation the issue was
//     created under; recorded once, never rewritten.
type Issue struct {
	IssueID            string    `json:"issue_id"`
	SiteID             string    `json:"site_id"`
	GroupHash          string    `json:"group_hash"`
	Title              string    `json:"title"`
	Culprit            string    `json:"culprit"`
	Level              string    `json:"level"`
	Status             string    `json:"status"`
	FirstSeen          time.Time `json:"first_seen"`
	LastSeen           time.Time `json:"last_seen"`
	EventCount         int64     `json:"event_count"`
	UserCount          int64     `json:"user_count"`
	ReleaseTag         string    `json:"release_tag"`
	FingerprintVersion int       `json:"fingerprint_version"`
	FirstRegressionAt  time.Time `json:"first_regression_at"`
	RegressionCount    int64     `json:"regression_count"`
	SnoozeUntil        time.Time `json:"snooze_until"`
	SnoozeActive       bool      `json:"snooze_active"`
	// Releases is the release-impact breakdown (per-release event
	// counts) for this issue, populated on detail reads only.
	Releases []IssueRelease `json:"releases,omitempty"`
}

// IssueRelease is one release's share of an issue's events.
type IssueRelease struct {
	ReleaseTag string `json:"release_tag" db:"release_tag"`
	EventCount int64  `json:"event_count" db:"event_count"`
}

// issueScan is the typed scan row; the lifecycle timestamp columns are
// TEXT (” = never) and map through msTime rather than scanning
// time.Time directly.
type issueScan struct {
	IssueID            string    `db:"issue_id"`
	SiteID             string    `db:"site_id"`
	GroupHash          string    `db:"group_hash"`
	Title              string    `db:"title"`
	Culprit            string    `db:"culprit"`
	Level              string    `db:"level"`
	Status             string    `db:"status"`
	FirstSeen          time.Time `db:"first_seen"`
	LastSeen           time.Time `db:"last_seen"`
	EventCount         int64     `db:"event_count"`
	UserCount          int64     `db:"user_count"`
	ReleaseTag         string    `db:"release_tag"`
	FingerprintVersion int       `db:"fingerprint_version"`
	FirstRegressionAt  string    `db:"first_regression_at"`
	RegressionCount    int64     `db:"regression_count"`
	SnoozeUntil        string    `db:"snooze_until"`
}

// msTime parses a unix-ms-as-text column; ” and unparseable values are
// the zero time (the honest encoding of "never").
func msTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	ms, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

func (r issueScan) toIssue(now time.Time) Issue {
	issue := Issue{
		IssueID:            r.IssueID,
		SiteID:             r.SiteID,
		GroupHash:          r.GroupHash,
		Title:              r.Title,
		Culprit:            r.Culprit,
		Level:              r.Level,
		Status:             r.Status,
		FirstSeen:          r.FirstSeen,
		LastSeen:           r.LastSeen,
		EventCount:         r.EventCount,
		UserCount:          r.UserCount,
		ReleaseTag:         r.ReleaseTag,
		FingerprintVersion: r.FingerprintVersion,
		FirstRegressionAt:  msTime(r.FirstRegressionAt),
		RegressionCount:    r.RegressionCount,
		SnoozeUntil:        msTime(r.SnoozeUntil),
	}
	issue.SnoozeActive = issue.Status == "resolved" &&
		!issue.SnoozeUntil.IsZero() && now.Before(issue.SnoozeUntil)
	return issue
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

	// 3. New issue — create. The grouping derivation version is
	// recorded ON the issue and never rewritten (the migration policy in
	// grouping.go's FingerprintVersion doc).
	issueID := generateID()
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	tsStr := strconv.FormatInt(ts, 10)
	sql := s.db.SQL()
	_, err = sql.Exec(ctx,
		`INSERT INTO issues (issue_id, tenant_id, site_id, group_hash, title, culprit, level, status, first_seen, last_seen, event_count, user_count, release_tag, fingerprint_version, version)
		 VALUES ($1, 'default', $2, $3, $4, $5, $6, 'open', $7, $8, '1', '0', $9, $10, $11)`,
		issueID, siteID, groupHash, title, culprit, level, tsStr, tsStr, release,
		strconv.Itoa(FingerprintVersion), now,
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
	rows, err := nucleus.Query[issueScan](ctx, s.db.SQL(),
		`SELECT `+issueSelectCols+`
		 FROM `+issuesLatest("site_id = $1 AND group_hash = $2"),
		siteID, groupHash,
	)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	issue := rows[0].toIssue(time.Now().UTC())
	return &issue, nil
}

// bumpIssue applies ONE new event to an existing issue in a single
// INSERT..SELECT off the collapsed latest row. O05 lifecycle semantics:
//
//   - RESOLVED -> reopened: status 'open', regression_count +1,
//     first_regression_at recorded on the first regression only,
//     snooze_until cleared.
//   - IGNORED -> stays ignored (the operator said stop); no markers.
//   - OPEN -> stays open; no markers.
//   - first_seen clamps DOWN (LEAST) and last_seen clamps UP (GREATEST)
//     against the event's ingestion-time ms, so an out-of-order apply
//     (PENDING retry, WAL replay) can widen the seen window but never
//     regress it.
//
// The regression markers distinguish resolved-then-reopened from
// continuous in the timeline: an open issue with regression_count > 0
// has been through at least one resolve/reopen cycle.
//
// Strictly-monotonic version (the 70f6eff version-tie defect): two
// error-batch flushes inside one millisecond must not tie, or the
// collapse resolves arbitrary counters for the issue.
func (s *IssueService) bumpIssue(ctx context.Context, issueID, siteID string, lastSeen, newCount int64) error {
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	lastSeenStr := strconv.FormatInt(lastSeen, 10)
	newCountStr := strconv.FormatInt(newCount, 10)
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO issues (issue_id, tenant_id, site_id, group_hash, title, culprit, level, status,
			first_seen, last_seen, event_count, user_count, release_tag,
			fingerprint_version, first_regression_at, regression_count, snooze_until, version)
			SELECT issue_id, tenant_id, site_id, group_hash, title, culprit, level,
			CASE WHEN status = 'resolved' THEN 'open' ELSE status END AS status,
			CAST(LEAST(CAST(first_seen AS BIGINT), CAST($3 AS BIGINT)) AS TEXT) AS first_seen,
			CAST(GREATEST(CAST(last_seen AS BIGINT), CAST($3 AS BIGINT)) AS TEXT) AS last_seen,
			$4 AS event_count, user_count, release_tag, fingerprint_version,
			CASE WHEN status = 'resolved' AND first_regression_at = '' THEN $3 ELSE first_regression_at END AS first_regression_at,
			CASE WHEN status = 'resolved' THEN CAST(CAST(regression_count AS BIGINT) + 1 AS TEXT) ELSE regression_count END AS regression_count,
			CASE WHEN status = 'resolved' THEN '' ELSE snooze_until END AS snooze_until,
		        GREATEST(CAST($5 AS BIGINT), version + 1) AS version
		 FROM `+issuesLatest("issue_id = $1 AND site_id = $2"),
		issueID, siteID, lastSeenStr, newCountStr, now,
	)
	return err
}

// UpdateStatus changes an issue's status (open, resolved, ignored).
// snoozeUntilMs > 0 (unix ms) is legal with status=resolved only (the
// handler validates): the issue stays resolved but auto-hides until the
// deadline — new events during the window reopen it immediately
// (bumpIssue), and once the deadline passes with no events it reads as
// plain resolved (SnoozeActive is computed lazily at read time; there
// is no background timer to flip it). Any status change clears an
// earlier snooze. Regression markers are history and survive every
// status change, including manual reopen.
func (s *IssueService) UpdateStatus(ctx context.Context, issueID, siteID, status string, snoozeUntilMs int64) error {
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	untilStr := ""
	if snoozeUntilMs > 0 {
		untilStr = strconv.FormatInt(snoozeUntilMs, 10)
	}
	// Same monotonic stamp as bumpIssue: a status flip inside the same
	// millisecond as a bump must still win the collapse.
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO issues (issue_id, tenant_id, site_id, group_hash, title, culprit, level, status,
			first_seen, last_seen, event_count, user_count, release_tag,
			fingerprint_version, first_regression_at, regression_count, snooze_until, version)
		 SELECT issue_id, tenant_id, site_id, group_hash, title, culprit, level, $3 AS status,
			first_seen, last_seen, event_count, user_count, release_tag, fingerprint_version,
			first_regression_at, regression_count,
			CASE WHEN $3 = 'resolved' AND $5 <> '' THEN $5 ELSE '' END AS snooze_until,
		        GREATEST(CAST($4 AS BIGINT), version + 1) AS version
		 FROM `+issuesLatest("issue_id = $1 AND site_id = $2"),
		issueID, siteID, status, now, untilStr,
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
	// status and last_seen are rewritten by UpdateStatus / bumpIssue, so both
	// the filter and the sort have to run on the collapsed row: filtering
	// status inside the derived table would match a superseded version and
	// list an issue that has since been resolved.
	var q string
	var params []any
	if status != "" {
		q = fmt.Sprintf(`SELECT `+issueSelectCols+`
		 FROM `+issuesLatest("site_id = $1")+`
		 WHERE status = $2
		 ORDER BY last_seen DESC
		 LIMIT %d OFFSET %d`, limit, offset)
		params = []any{siteID, status}
	} else {
		q = fmt.Sprintf(`SELECT `+issueSelectCols+`
		 FROM `+issuesLatest("site_id = $1")+`
		 ORDER BY last_seen DESC
		 LIMIT %d OFFSET %d`, limit, offset)
		params = []any{siteID}
	}
	issues, err := nucleus.Query[issueScan](ctx, s.db.SQL(), q, params...)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	out := make([]Issue, 0, len(issues))
	for i := range issues {
		issue := issues[i].toIssue(now)
		// user_count and event_count are computed on read (the stored counters race
		// under concurrent flushes — read-modify-write into a ReplacingMergeTree
		// loses increments). COUNT over error_events is exact and concurrency-safe.
		// Bounded by the page limit, so the per-issue lookups stay cheap.
		issue.UserCount = s.affectedUsers(ctx, siteID, issue.IssueID)
		issue.EventCount = s.eventCount(ctx, siteID, issue.IssueID)
		out = append(out, issue)
	}
	return out, nil
}

// GetIssue returns a single issue by ID.
func (s *IssueService) GetIssue(ctx context.Context, issueID, siteID string) (*Issue, error) {
	rows, err := nucleus.Query[issueScan](ctx, s.db.SQL(),
		`SELECT `+issueSelectCols+`
		 FROM `+issuesLatest("issue_id = $1 AND site_id = $2"),
		issueID, siteID,
	)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	issue := rows[0].toIssue(time.Now().UTC())
	issue.UserCount = s.affectedUsers(ctx, siteID, issueID)
	issue.EventCount = s.eventCount(ctx, siteID, issueID)
	issue.Releases = s.issueReleases(ctx, siteID, issueID)
	return &issue, nil
}

// eventCount returns the exact number of events for an issue, computed on read
// from the append-only error_events table (the stored issues.event_count is
// racey). Best-effort: returns 0 on any error.
func (s *IssueService) eventCount(ctx context.Context, siteID, issueID string) int64 {
	rows, err := nucleus.Query[issueCountRow](ctx, s.db.SQL(),
		`SELECT COUNT(*) AS n FROM error_events WHERE site_id = $1 AND issue_id = $2`,
		siteID, issueID,
	)
	if err != nil || len(rows) == 0 {
		return 0
	}
	return rows[0].N
}

type issueCountRow struct {
	N int64 `db:"n"`
}

// affectedUsers computes the issue's affected-user count, on read.
// Storage choice (O05): on-read aggregation, not a stored versioned
// counter — the same rationale as eventCount (a read-modify-write
// counter into the ReplacingMergeTree loses increments under concurrent
// flushes; COUNT DISTINCT over append-only error_events is exact), and
// it costs one bounded query per listed issue.
//
// Counting unit: distinct_id (the hashed identify() id, O03 person
// vocabulary) — counted over events that carry one; anonymous rows are
// CASEd to NULL so the empty string never counts as a "user". Only when
// an issue has NO identified events at all does it fall back to the
// pre-O05 session proxy (distinct session_id), documented as an
// estimate of affected anonymous traffic, never conflated with
// identified persons. Best-effort: returns 0 on any error.
func (s *IssueService) affectedUsers(ctx context.Context, siteID, issueID string) int64 {
	rows, err := nucleus.Query[affectedRow](ctx, s.db.SQL(),
		`SELECT COUNT(DISTINCT CASE WHEN distinct_id <> '' THEN distinct_id ELSE NULL END) AS n_distinct,
		        COUNT(DISTINCT CASE WHEN distinct_id = '' AND session_id <> '' THEN session_id ELSE NULL END) AS n_sessions
		 FROM error_events
		 WHERE site_id = $1 AND issue_id = $2`,
		siteID, issueID,
	)
	if err != nil || len(rows) == 0 {
		return 0
	}
	if rows[0].NDistinct > 0 {
		return rows[0].NDistinct
	}
	return rows[0].NSessions
}

type affectedRow struct {
	NDistinct int64 `db:"n_distinct"`
	NSessions int64 `db:"n_sessions"`
}

// issueReleases returns the per-release event breakdown for an issue
// (release impact), on read. A release is attributed ONLY from the
// event's own release field (SDKs send `release`; absent = ” = unknown
// — never fabricated). Ordered by release_tag for a total order under
// the LIMIT. Best-effort: returns nil on error.
func (s *IssueService) issueReleases(ctx context.Context, siteID, issueID string) []IssueRelease {
	rows, err := nucleus.Query[IssueRelease](ctx, s.db.SQL(),
		`SELECT release_tag, COUNT(*) AS event_count
		 FROM error_events
		 WHERE site_id = $1 AND issue_id = $2 AND release_tag <> ''
		 GROUP BY release_tag
		 ORDER BY release_tag ASC
		 LIMIT 10`,
		siteID, issueID,
	)
	if err != nil {
		return nil
	}
	return rows
}

// ErrorEvent represents a stored error event.
type ErrorEvent struct {
	ErrorID     string    `json:"error_id"`
	SiteID      string    `json:"site_id"`
	SessionID   string    `json:"session_id"`
	ReplayID    string    `json:"replay_id"`
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
		fmt.Sprintf(`SELECT error_id, tenant_id, site_id, session_id,
			COALESCE(replay_id, '') AS replay_id,
			issue_id, group_hash,
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

// IssuesByReplay returns issues that have at least one error event linked
// to the given replay_id. Used by the sessions UI to show a "View errors
// in this session" cross-jump.
func (s *IssueService) IssuesByReplay(ctx context.Context, siteID, replayID string) ([]Issue, error) {
	if replayID == "" {
		return nil, nil
	}
	type idRow struct {
		IssueID string `db:"issue_id"`
	}
	rows, err := nucleus.Query[idRow](ctx, s.db.SQL(),
		`SELECT DISTINCT issue_id FROM error_events
		 WHERE site_id = $1 AND replay_id = $2`,
		siteID, replayID,
	)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	out := make([]Issue, 0, len(rows))
	for _, r := range rows {
		if r.IssueID == "" {
			continue
		}
		issue, err := s.GetIssue(ctx, r.IssueID, siteID)
		if err == nil && issue != nil {
			out = append(out, *issue)
		}
	}
	return out, nil
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
		 WHERE site_id = $1 AND timestamp >= $2
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
		d := today.AddDate(0, 0, -(days - 1 - i))
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
