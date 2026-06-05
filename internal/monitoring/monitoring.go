package monitoring

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
)

// ErrCronNotFound is returned by RecordCheckin when no enabled cron monitor
// matches the (site, slug). Callers map this to 404; any other error is a
// backend failure and must map to 5xx so heartbeat clients retry instead of
// treating a transient outage as "cron deleted".
var ErrCronNotFound = errors.New("monitoring: cron not found")

// ---------------------------------------------------------------------------
// Uptime monitoring
// ---------------------------------------------------------------------------

// UptimeService handles HTTP uptime checks and result storage.
type UptimeService struct {
	db     *nucleus.Client
	logger *slog.Logger
	client *http.Client
}

// NewUptimeService creates a new UptimeService.
func NewUptimeService(db *nucleus.Client, logger *slog.Logger) *UptimeService {
	return &UptimeService{
		db:     db,
		logger: logger,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

// Monitor represents an uptime monitor configuration.
type Monitor struct {
	MonitorID      string `json:"monitor_id"`
	TenantID       string `json:"-" db:"tenant_id"`
	SiteID         string `json:"site_id" db:"site_id"`
	Name           string `json:"name"`
	URL            string `json:"url"`
	Method         string `json:"method"`
	IntervalSecs   int    `json:"interval_secs" db:"interval_secs"`
	ExpectedStatus int    `json:"expected_status" db:"expected_status"`
	Enabled        bool   `json:"enabled"`
	CreatedAt      string `json:"created_at" db:"created_at"`
	Version        string `json:"-" db:"version"`
}

// MonitorResult stores the outcome of a single uptime check.
type MonitorResult struct {
	ResultID     string `json:"result_id" db:"result_id"`
	TenantID     string `json:"-" db:"tenant_id"`
	MonitorID    string `json:"monitor_id" db:"monitor_id"`
	SiteID       string `json:"site_id" db:"site_id"`
	Timestamp    int64  `json:"timestamp"`
	StatusCode   int    `json:"status_code" db:"status_code"`
	ResponseMs   int64  `json:"response_ms" db:"response_ms"`
	IsUp         bool   `json:"is_up" db:"is_up"`
	ErrorMessage string `json:"error_message" db:"error_message"`
}

// CreateMonitor inserts a new uptime monitor.
func (s *UptimeService) CreateMonitor(ctx context.Context, m Monitor) (*Monitor, error) {
	m.MonitorID = genID()
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	m.CreatedAt = now
	m.Version = now
	if m.TenantID == "" {
		m.TenantID = "default"
	}
	if m.Method == "" {
		m.Method = "GET"
	}
	if !m.Enabled {
		m.Enabled = true
	}
	if m.ExpectedStatus == 0 {
		m.ExpectedStatus = 200
	}
	if m.IntervalSecs == 0 {
		m.IntervalSecs = 60
	}

	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO uptime_monitors (monitor_id, tenant_id, site_id, name, url, method,
			interval_secs, expected_status, enabled, created_at, version)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		m.MonitorID, m.TenantID, m.SiteID, m.Name, m.URL, m.Method,
		strconv.Itoa(m.IntervalSecs), strconv.Itoa(m.ExpectedStatus), strconv.FormatBool(m.Enabled), now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("monitoring: create monitor: %w", err)
	}
	return &m, nil
}

// ListMonitors returns all enabled monitors for a site.
func (s *UptimeService) ListMonitors(ctx context.Context, siteID string) ([]Monitor, error) {
	rows, err := nucleus.Query[Monitor](ctx, s.db.SQL(),
		`SELECT monitor_id, tenant_id, site_id, name, url, method,
			interval_secs, expected_status, enabled, created_at, version
		 FROM uptime_monitors WHERE site_id = $1 AND enabled = 'true'
		 ORDER BY created_at DESC`, siteID)
	if err != nil {
		return nil, fmt.Errorf("monitoring: list monitors: %w", err)
	}
	if rows == nil {
		rows = []Monitor{}
	}
	return rows, nil
}

// DeleteMonitor disables a monitor by re-inserting with enabled='false' and a bumped version.
func (s *UptimeService) DeleteMonitor(ctx context.Context, monitorID string) error {
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO uptime_monitors (monitor_id, tenant_id, site_id, name, url, method,
			interval_secs, expected_status, enabled, created_at, version)
		 SELECT monitor_id, tenant_id, site_id, name, url, method,
			interval_secs, expected_status, 'false', created_at, $2
		 FROM uptime_monitors WHERE monitor_id = $1`,
		monitorID, now,
	)
	if err != nil {
		return fmt.Errorf("monitoring: delete monitor: %w", err)
	}
	return nil
}

// CheckMonitor performs an HTTP request against the monitor's URL and records the result.
func (s *UptimeService) CheckMonitor(ctx context.Context, m Monitor) {
	start := time.Now()

	req, err := http.NewRequestWithContext(ctx, m.Method, m.URL, nil)
	if err != nil {
		s.recordResult(ctx, m, 0, 0, false, err.Error())
		return
	}
	req.Header.Set("User-Agent", "Teploy-Observe-Uptime/1.0")

	resp, err := s.client.Do(req)
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		s.recordResult(ctx, m, 0, elapsed, false, err.Error())
		return
	}
	resp.Body.Close()

	isUp := resp.StatusCode == m.ExpectedStatus

	s.recordResult(ctx, m, resp.StatusCode, elapsed, isUp, "")
}

func (s *UptimeService) recordResult(ctx context.Context, m Monitor, statusCode int, responseMs int64, isUp bool, errMsg string) {
	resultID := genID()
	now := time.Now().UTC().UnixMilli()
	nowStr := dbutil.IntParam(now)

	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO uptime_results (result_id, tenant_id, monitor_id, site_id,
			timestamp, status_code, response_ms, is_up, error_message)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		resultID, m.TenantID, m.MonitorID, m.SiteID,
		nowStr, strconv.Itoa(statusCode), dbutil.IntParam(responseMs), strconv.FormatBool(isUp), errMsg,
	)
	if err != nil {
		s.logger.Error("monitoring: record result failed", "monitor", m.MonitorID, "err", err)
	}
}

// RunChecks iterates all enabled monitors across all sites and checks each one.
func (s *UptimeService) RunChecks(ctx context.Context) error {
	monitors, err := nucleus.Query[Monitor](ctx, s.db.SQL(),
		`SELECT monitor_id, tenant_id, site_id, name, url, method,
			interval_secs, expected_status, enabled, created_at, version
		 FROM uptime_monitors WHERE enabled = 'true'`)
	if err != nil {
		return fmt.Errorf("monitoring: run checks query: %w", err)
	}

	for _, m := range monitors {
		s.CheckMonitor(ctx, m)
	}
	return nil
}

// ListResults returns recent check results for a given monitor.
func (s *UptimeService) ListResults(ctx context.Context, monitorID string, limit int) ([]MonitorResult, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := nucleus.Query[MonitorResult](ctx, s.db.SQL(),
		fmt.Sprintf(`SELECT result_id, tenant_id, monitor_id, site_id,
			timestamp, status_code, response_ms, is_up, error_message
		 FROM uptime_results WHERE monitor_id = $1
		 ORDER BY timestamp DESC LIMIT %d`, limit),
		monitorID,
	)
	if err != nil {
		return nil, fmt.Errorf("monitoring: list results: %w", err)
	}
	if rows == nil {
		rows = []MonitorResult{}
	}
	return rows, nil
}

// ---------------------------------------------------------------------------
// Cron heartbeat monitoring
// ---------------------------------------------------------------------------

// CronService handles cron heartbeat registration and checkin tracking.
type CronService struct {
	db     *nucleus.Client
	logger *slog.Logger
}

// NewCronService creates a new CronService.
func NewCronService(db *nucleus.Client, logger *slog.Logger) *CronService {
	return &CronService{db: db, logger: logger}
}

// CronMonitor represents a registered cron job to track.
type CronMonitor struct {
	CronID      string `json:"cron_id" db:"cron_id"`
	TenantID    string `json:"-" db:"tenant_id"`
	SiteID      string `json:"site_id" db:"site_id"`
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	Schedule    string `json:"schedule"`
	GracePeriod int    `json:"grace_period" db:"grace_period"`
	Enabled     bool   `json:"enabled"`
	CreatedAt   string `json:"created_at" db:"created_at"`
	Version     string `json:"-" db:"version"`
}

// CronCheckin records a single heartbeat for a cron job.
type CronCheckin struct {
	CheckinID  string `json:"checkin_id" db:"checkin_id"`
	TenantID   string `json:"-" db:"tenant_id"`
	CronID     string `json:"cron_id" db:"cron_id"`
	SiteID     string `json:"site_id" db:"site_id"`
	Timestamp  int64  `json:"timestamp"`
	Status     string `json:"status"`
	DurationMs int64  `json:"duration_ms" db:"duration_ms"`
}

// CreateCron registers a new cron monitor.
func (s *CronService) CreateCron(ctx context.Context, c CronMonitor) (*CronMonitor, error) {
	c.CronID = genID()
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	c.CreatedAt = now
	c.Version = now
	if c.TenantID == "" {
		c.TenantID = "default"
	}
	if !c.Enabled {
		c.Enabled = true
	}
	if c.GracePeriod == 0 {
		c.GracePeriod = 300
	}

	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO cron_monitors (cron_id, tenant_id, site_id, name, slug, schedule,
			grace_period, enabled, created_at, version)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		c.CronID, c.TenantID, c.SiteID, c.Name, c.Slug, c.Schedule,
		strconv.Itoa(c.GracePeriod), strconv.FormatBool(c.Enabled), now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("monitoring: create cron: %w", err)
	}
	return &c, nil
}

// ListCrons returns all enabled cron monitors for a site.
func (s *CronService) ListCrons(ctx context.Context, siteID string) ([]CronMonitor, error) {
	rows, err := nucleus.Query[CronMonitor](ctx, s.db.SQL(),
		`SELECT cron_id, tenant_id, site_id, name, slug, schedule,
			grace_period, enabled, created_at, version
		 FROM cron_monitors WHERE site_id = $1 AND enabled = 'true'
		 ORDER BY created_at DESC`, siteID)
	if err != nil {
		return nil, fmt.Errorf("monitoring: list crons: %w", err)
	}
	if rows == nil {
		rows = []CronMonitor{}
	}
	return rows, nil
}

// DeleteCron disables a cron monitor by re-inserting with enabled='false' and a bumped version.
func (s *CronService) DeleteCron(ctx context.Context, cronID string) error {
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO cron_monitors (cron_id, tenant_id, site_id, name, slug, schedule,
			grace_period, enabled, created_at, version)
		 SELECT cron_id, tenant_id, site_id, name, slug, schedule,
			grace_period, 'false', created_at, $2
		 FROM cron_monitors WHERE cron_id = $1`,
		cronID, now,
	)
	if err != nil {
		return fmt.Errorf("monitoring: delete cron: %w", err)
	}
	return nil
}

// RecordCheckin records a heartbeat checkin for a cron job identified by slug.
func (s *CronService) RecordCheckin(ctx context.Context, siteID, slug, status string, durationMs int64) error {
	// Look up the cron monitor by slug
	crons, err := nucleus.Query[CronMonitor](ctx, s.db.SQL(),
		`SELECT cron_id, tenant_id, site_id, name, slug, schedule,
			grace_period, enabled, created_at, version
		 FROM cron_monitors WHERE site_id = $1 AND slug = $2 AND enabled = 'true'`,
		siteID, slug)
	if err != nil {
		// Real backend failure — surface it so the handler returns 5xx and the
		// client retries, rather than conflating it with "no such cron" (404).
		return fmt.Errorf("monitoring: lookup cron %q/%q: %w", siteID, slug, err)
	}
	if len(crons) == 0 {
		return fmt.Errorf("%w: %q/%q", ErrCronNotFound, siteID, slug)
	}
	cron := crons[0]

	checkinID := genID()
	now := time.Now().UTC().UnixMilli()
	nowStr := dbutil.IntParam(now)

	if status == "" {
		status = "ok"
	}

	_, err = s.db.SQL().Exec(ctx,
		`INSERT INTO cron_checkins (checkin_id, tenant_id, cron_id, site_id,
			timestamp, status, duration_ms)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		checkinID, cron.TenantID, cron.CronID, cron.SiteID,
		nowStr, status, dbutil.IntParam(durationMs),
	)
	if err != nil {
		return fmt.Errorf("monitoring: record checkin: %w", err)
	}
	return nil
}

// CheckMissed finds cron monitors that have not checked in within their grace period
// and returns them.
func (s *CronService) CheckMissed(ctx context.Context) ([]CronMonitor, error) {
	crons, err := nucleus.Query[CronMonitor](ctx, s.db.SQL(),
		`SELECT cron_id, tenant_id, site_id, name, slug, schedule,
			grace_period, enabled, created_at, version
		 FROM cron_monitors WHERE enabled = 'true'`)
	if err != nil {
		return nil, fmt.Errorf("monitoring: check missed query: %w", err)
	}

	now := time.Now().UTC()
	var missed []CronMonitor

	for _, c := range crons {
		graceSecs := int64(c.GracePeriod)
		if graceSecs <= 0 {
			graceSecs = 300
		}
		cutoff := dbutil.IntParam(now.Add(-time.Duration(graceSecs) * time.Second).UnixMilli())

		type countRow struct {
			Count string `db:"count"`
		}
		rows, err := nucleus.Query[countRow](ctx, s.db.SQL(),
			`SELECT CAST(COUNT(*) AS TEXT) AS count FROM cron_checkins
			 WHERE cron_id = $1 AND timestamp >= $2`,
			c.CronID, cutoff)
		if err != nil {
			s.logger.Error("monitoring: check missed count failed", "cron", c.CronID, "err", err)
			continue
		}
		if len(rows) > 0 {
			cnt, _ := strconv.ParseInt(rows[0].Count, 10, 64)
			if cnt == 0 {
				missed = append(missed, c)
			}
		}
	}

	return missed, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func genID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
