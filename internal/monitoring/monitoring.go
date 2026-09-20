package monitoring

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
	"github.com/useteploy/teploy-observe/internal/netsafe"
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

	mu        sync.Mutex
	lastCheck map[string]time.Time // monitor_id -> last check time
}

// NewUptimeService creates a new UptimeService.
func NewUptimeService(db *nucleus.Client, logger *slog.Logger) *UptimeService {
	return &UptimeService{
		db:     db,
		logger: logger,
		// SSRF-safe: blocks dialing private/loopback/link-local/metadata IPs and
		// re-validates redirects, so an operator-supplied monitor URL can't be
		// pointed at 169.254.169.254, internal services, or the Tailscale mesh.
		client:    netsafe.Client(30 * time.Second),
		lastCheck: make(map[string]time.Time),
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

// nextMutationVersion returns a replacement-table version strictly greater
// than prior (R19, round 4): same-millisecond mutations and clock rollback
// must not produce tied or regressed versions — latest-row collapse would
// keep the stale row and undo the delete/update.
func nextMutationVersion(prior, nowMS int64) string {
	next := nextMutationVersionInt(prior, nowMS)
	return strconv.FormatInt(next, 10)
}

func nextMutationVersionInt(prior, nowMS int64) int64 {
	if prior+1 > nowMS {
		return prior + 1
	}
	return nowMS
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
	if m.Method != "GET" && m.Method != "HEAD" {
		return nil, fmt.Errorf("monitor method must be GET or HEAD")
	}
	if err := netsafe.ValidateURL(m.URL); err != nil {
		return nil, fmt.Errorf("monitor url: %w", err)
	}
	// R20 (round 4): numeric configuration is validated, not silently
	// rewritten into something else. Defaults apply only to zero values
	// (omitted fields); an explicit Enabled=false now stays false — the old
	// `if !m.Enabled { m.Enabled = true }` made a disabled monitor
	// impossible to create while reporting that it had been.
	if m.ExpectedStatus == 0 {
		m.ExpectedStatus = 200
	}
	if m.IntervalSecs == 0 {
		m.IntervalSecs = 60
	}
	if err := validateMonitor(m); err != nil {
		return nil, err
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

// ValidateMonitor is the exported boundary check (handlers map it to 400);
// CreateMonitor applies it again as defense in depth.
func ValidateMonitor(m Monitor) error { return validateMonitor(m) }

// validateMonitor enforces the numeric bounds R20 (round 4): an accepted
// configuration must behave the way it reads. Interval floor of 10s keeps the
// scheduler honest; ceiling of a day; expected status must be a real HTTP
// status code.
func validateMonitor(m Monitor) error {
	if strings.TrimSpace(m.SiteID) == "" || strings.TrimSpace(m.Name) == "" {
		return fmt.Errorf("monitoring: site_id and name are required")
	}
	if m.IntervalSecs < 10 || m.IntervalSecs > 86400 {
		return fmt.Errorf("monitoring: interval_secs must be between 10 and 86400")
	}
	if m.ExpectedStatus < 100 || m.ExpectedStatus > 599 {
		return fmt.Errorf("monitoring: expected_status must be a valid HTTP status code")
	}
	return nil
}

// ListMonitors returns all enabled monitors for a site.
func (s *UptimeService) ListMonitors(ctx context.Context, siteID string) ([]Monitor, error) {
	rows, err := nucleus.Query[Monitor](ctx, s.db.SQL(),
		`SELECT monitor_id, tenant_id, site_id, name, url, method,
			interval_secs, expected_status, enabled, created_at, version
		 FROM `+uptimeMonitorsLatest("site_id = $1")+`
		 WHERE enabled = 'true'
		 ORDER BY created_at DESC`, siteID)
	if err != nil {
		return nil, fmt.Errorf("monitoring: list monitors: %w", err)
	}
	if rows == nil {
		rows = []Monitor{}
	}
	return rows, nil
}

// DeleteMonitor disables a monitor by re-inserting with enabled='false' and a
// bumped version. Scoped by site_id so a caller cannot disable another site's
// monitor by guessing its id. R19 (round 4): the replacement's version is
// computed from the latest row's version (strictly greater), so a delete in
// the same millisecond as a prior mutation — or after clock rollback — can no
// longer produce a tied/regressed version whose collapse resurrects the live
// row.
func (s *UptimeService) DeleteMonitor(ctx context.Context, siteID, monitorID string) error {
	latest, err := nucleus.Query[struct {
		Version string `db:"version"`
	}](ctx, s.db.SQL(),
		"SELECT version FROM "+uptimeMonitorsLatest("monitor_id = $1 AND site_id = $2"),
		monitorID, siteID)
	if err != nil {
		return fmt.Errorf("monitoring: delete monitor lookup: %w", err)
	}
	if len(latest) == 0 {
		return fmt.Errorf("monitoring: monitor not found")
	}
	prior, _ := strconv.ParseInt(latest[0].Version, 10, 64)
	next := nextMutationVersion(prior, time.Now().UTC().UnixMilli())
	_, err = s.db.SQL().Exec(ctx,
		`INSERT INTO uptime_monitors (monitor_id, tenant_id, site_id, name, url, method,
			interval_secs, expected_status, enabled, created_at, version)
		 SELECT monitor_id, tenant_id, site_id, name, url, method,
			interval_secs, expected_status, 'false', created_at, $3
		 FROM `+uptimeMonitorsLatest("monitor_id = $1 AND site_id = $2"),
		monitorID, siteID, next,
	)
	if err != nil {
		return fmt.Errorf("monitoring: delete monitor: %w", err)
	}
	return nil
}

// CheckMonitor performs an HTTP request against the monitor's URL and records
// the result.
//
// R17 (round 4): the probe budget and the persistence budget are separate.
// The probe runs under the caller's (possibly expiring) context, but the
// result row — the whole point of a timeout — is written under a detached,
// bounded context so a target timeout cannot leave "no result" as its only
// trace. Cancellation of the SCHEDULER itself (parent canceled before the
// probe ran or during it) is not a target outage and records nothing.
func (s *UptimeService) CheckMonitor(parent context.Context, m Monitor) error {
	timeout := time.Duration(m.IntervalSecs) * time.Second
	if timeout <= 0 || timeout > 30*time.Second {
		timeout = 30 * time.Second
	}
	probeCtx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	started := time.Now()
	req, err := http.NewRequestWithContext(probeCtx, m.Method, m.URL, nil)
	code, message := 0, ""
	if err == nil {
		req.Header.Set("User-Agent", "Teploy-Observe-Uptime/1.0")
		var resp *http.Response
		resp, err = s.client.Do(req)
		if resp != nil {
			code = resp.StatusCode
			resp.Body.Close()
		}
	}
	if parent.Err() != nil {
		// Scheduler stop is not target failure — recording a down row for a
		// monitor the process is no longer responsible for would be noise.
		return parent.Err()
	}
	if err != nil {
		message = err.Error()
	}
	persistCtx, stop := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
	defer stop()
	return s.recordResult(persistCtx, m, code, time.Since(started).Milliseconds(),
		err == nil && code == m.ExpectedStatus, message)
}

func (s *UptimeService) recordResult(ctx context.Context, m Monitor, statusCode int, responseMs int64, isUp bool, errMsg string) error {
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
		// R17 (round 4): the persistence failure is returned so RunChecks
		// can surface it instead of reporting a healthy run.
		return fmt.Errorf("monitoring: record result: %w", err)
	}
	return nil
}

// RunChecks iterates all enabled monitors across all sites and checks the ones
// whose configured interval has elapsed. Previously every monitor was checked
// on every tick (so interval_secs was ignored) and checks ran serially (so one
// slow target stalled the rest). Eligible checks now run concurrently under a
// bounded worker pool, each with a per-check timeout.
//
// R18 (round 4): only the checks that can actually START this tick are
// marked (lastCheck) — admitted in oldest-last-check order up to the worker
// capacity. The old loop premarked every due monitor, then queued them
// behind a 10-slot semaphore under a run-level deadline; with more due
// monitors than slots the queued tail inherited an expired context YET
// counted as checked, systematically starving exactly the monitors that were
// already late. Unadmitted monitors stay due and are first in line next
// tick.
func (s *UptimeService) RunChecks(ctx context.Context) error {
	monitors, err := nucleus.Query[Monitor](ctx, s.db.SQL(),
		`SELECT monitor_id, tenant_id, site_id, name, url, method,
			interval_secs, expected_status, enabled, created_at, version
		 FROM `+uptimeMonitorsLatest("")+`
		 WHERE enabled = 'true'`)
	if err != nil {
		return fmt.Errorf("monitoring: run checks query: %w", err)
	}

	const maxConcurrent = 10
	now := time.Now()
	var due []Monitor
	s.mu.Lock()
	for _, m := range monitors {
		interval := time.Duration(m.IntervalSecs) * time.Second
		if interval <= 0 {
			interval = 60 * time.Second
		}
		if last, ok := s.lastCheck[m.MonitorID]; ok && now.Sub(last) < interval {
			continue
		}
		due = append(due, m)
	}
	// Oldest last-check first, id as the deterministic tie-break — the
	// monitors that have waited longest get the slots.
	sort.Slice(due, func(i, j int) bool {
		a, b := s.lastCheck[due[i].MonitorID], s.lastCheck[due[j].MonitorID]
		if a.Equal(b) {
			return due[i].MonitorID < due[j].MonitorID
		}
		return a.Before(b)
	})
	if len(due) > maxConcurrent {
		due = due[:maxConcurrent]
	}
	for _, m := range due {
		// Only these actually start; everything else remains due.
		s.lastCheck[m.MonitorID] = now
	}
	s.mu.Unlock()

	var wg sync.WaitGroup
	var errMu sync.Mutex
	var firstErr error
	for _, m := range due {
		wg.Add(1)
		go func(m Monitor) {
			defer wg.Done()
			if err := s.CheckMonitor(ctx, m); err != nil && ctx.Err() == nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				errMu.Unlock()
			}
		}(m)
	}
	wg.Wait()
	return firstErr
}

// ListResults returns recent check results for a given monitor. R21 (round
// 4): the caller-supplied limit is capped (with a stable total order), so an
// authenticated reader cannot request an unbounded scan.
func (s *UptimeService) ListResults(ctx context.Context, monitorID string, limit int) ([]MonitorResult, error) {
	limit = pageLimit(limit, 50, 500)
	rows, err := nucleus.Query[MonitorResult](ctx, s.db.SQL(),
		fmt.Sprintf(`SELECT result_id, tenant_id, monitor_id, site_id,
			timestamp, status_code, response_ms, is_up, error_message
		 FROM uptime_results WHERE monitor_id = $1
		 ORDER BY timestamp DESC, result_id DESC LIMIT %d`, limit),
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

// pageLimit applies the shared list-size policy (R21, round 4): default when
// omitted, hard ceiling when excessive.
func pageLimit(requested, fallback, maximum int) int {
	if requested <= 0 {
		return fallback
	}
	if requested > maximum {
		return maximum
	}
	return requested
}

// ---------------------------------------------------------------------------
// Cron heartbeat monitoring
// ---------------------------------------------------------------------------

// CronService handles cron heartbeat registration and checkin tracking.
type CronService struct {
	db     *nucleus.Client
	logger *slog.Logger

	// OnCheckin, if set, is invoked after a successful check-in so a caller can
	// resolve any open missed-cron incident for that monitor.
	OnCheckin func(ctx context.Context, cron CronMonitor)
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
	PingToken   string `json:"ping_token" db:"ping_token"`
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

// CreateCron registers a new cron monitor. R20 (round 4): the configuration
// is validated before the INSERT (bounded name/slug, bounded nonnegative
// grace, nonempty schedule) and an explicit Enabled=false stays false — the
// service no longer silently flips it.
func (s *CronService) CreateCron(ctx context.Context, c CronMonitor) (*CronMonitor, error) {
	c.CronID = genID()
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	c.CreatedAt = now
	c.Version = now
	if c.TenantID == "" {
		c.TenantID = "default"
	}
	if c.GracePeriod == 0 {
		c.GracePeriod = 300
	}
	if err := validateCron(c); err != nil {
		return nil, err
	}
	// Opaque high-entropy token: possession of it is what authorizes a heartbeat.
	c.PingToken = genID() + genID()

	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO cron_monitors (cron_id, tenant_id, site_id, name, slug, schedule,
			grace_period, enabled, ping_token, created_at, version)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		c.CronID, c.TenantID, c.SiteID, c.Name, c.Slug, c.Schedule,
		strconv.Itoa(c.GracePeriod), strconv.FormatBool(c.Enabled), c.PingToken, now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("monitoring: create cron: %w", err)
	}
	return &c, nil
}

// ValidateCron is the exported boundary check (handlers map it to 400);
// CreateCron applies it again as defense in depth.
func ValidateCron(c CronMonitor) error { return validateCron(c) }

// validateCron enforces the R20 (round 4) bounds: site/name required and
// bounded; slug bounded when supplied (the slug is a label now that the
// legacy slug check-in routes are retired — R05); schedule bounded (an empty
// schedule stays legal: it is the documented grace-only mode the missed-cron
// detector supports); grace nonnegative and bounded.
func validateCron(c CronMonitor) error {
	if strings.TrimSpace(c.SiteID) == "" {
		return fmt.Errorf("monitoring: site_id is required")
	}
	if strings.TrimSpace(c.Name) == "" || len(c.Name) > 128 {
		return fmt.Errorf("monitoring: name is required and at most 128 characters")
	}
	if len(c.Slug) > 128 {
		return fmt.Errorf("monitoring: slug is at most 128 characters")
	}
	if len(c.Schedule) > 64 {
		return fmt.Errorf("monitoring: schedule is at most 64 characters")
	}
	if c.GracePeriod < 0 || c.GracePeriod > 86400 {
		return fmt.Errorf("monitoring: grace_period must be between 0 and 86400 seconds")
	}
	return nil
}

// ListCrons returns all enabled cron monitors for a site.
func (s *CronService) ListCrons(ctx context.Context, siteID string) ([]CronMonitor, error) {
	rows, err := nucleus.Query[CronMonitor](ctx, s.db.SQL(),
		`SELECT cron_id, tenant_id, site_id, name, slug, schedule,
			grace_period, enabled, COALESCE(ping_token, '') AS ping_token, created_at, version
		 FROM `+cronMonitorsLatest("site_id = $1")+`
		 WHERE enabled = 'true'
		 ORDER BY created_at DESC`, siteID)
	if err != nil {
		return nil, fmt.Errorf("monitoring: list crons: %w", err)
	}
	if rows == nil {
		rows = []CronMonitor{}
	}
	return rows, nil
}

// DeleteCron disables a cron monitor by re-inserting with enabled='false' and a
// bumped version. Scoped by site_id so a caller cannot disable another site's
// cron by guessing its id.
func (s *CronService) DeleteCron(ctx context.Context, siteID, cronID string) error {
	latest, err := nucleus.Query[struct {
		Version string `db:"version"`
	}](ctx, s.db.SQL(),
		"SELECT version FROM "+cronMonitorsLatest("cron_id = $1 AND site_id = $2"),
		cronID, siteID)
	if err != nil {
		return fmt.Errorf("monitoring: delete cron lookup: %w", err)
	}
	if len(latest) == 0 {
		return fmt.Errorf("monitoring: cron not found")
	}
	prior, _ := strconv.ParseInt(latest[0].Version, 10, 64)
	next := nextMutationVersion(prior, time.Now().UTC().UnixMilli())
	_, err = s.db.SQL().Exec(ctx,
		`INSERT INTO cron_monitors (cron_id, tenant_id, site_id, name, slug, schedule,
			grace_period, enabled, ping_token, created_at, version)
		 SELECT cron_id, tenant_id, site_id, name, slug, schedule,
			grace_period, 'false', ping_token, created_at, $3
		 FROM `+cronMonitorsLatest("cron_id = $1 AND site_id = $2"),
		cronID, siteID, next,
	)
	if err != nil {
		return fmt.Errorf("monitoring: delete cron: %w", err)
	}
	return nil
}

// RecordCheckinByToken records a heartbeat for the cron identified by its opaque
// ping token. Possession of the token authorizes the heartbeat; an unknown token
// maps to ErrCronNotFound (404). This replaces the old guessable (site_id, slug)
// scheme that allowed cross-site check-in spoofing.
func (s *CronService) RecordCheckinByToken(ctx context.Context, token, status string, durationMs int64) error {
	if token == "" {
		return fmt.Errorf("%w: empty token", ErrCronNotFound)
	}
	crons, err := nucleus.Query[CronMonitor](ctx, s.db.SQL(),
		`SELECT cron_id, tenant_id, site_id, name, slug, schedule,
			grace_period, enabled, COALESCE(ping_token, '') AS ping_token, created_at, version
		 FROM `+cronMonitorsLatest("ping_token = $1")+`
		 WHERE enabled = 'true'`,
		token)
	if err != nil {
		return fmt.Errorf("monitoring: lookup cron by token: %w", err)
	}
	if len(crons) == 0 {
		return fmt.Errorf("%w: token", ErrCronNotFound)
	}
	return s.insertCheckin(ctx, crons[0], status, durationMs)
}

// RecordCheckin records a heartbeat checkin for a cron job identified by slug.
// Retained for internal/legacy callers only; the public slug routes are
// retired (R05, round 4). A monitor that carries a ping token can NEVER be
// checked in through this path — possession of the opaque token is the
// authorization for its heartbeats, and a slug that happens to be known or
// guessed must not bypass it.
func (s *CronService) RecordCheckin(ctx context.Context, siteID, slug, status string, durationMs int64) error {
	// Look up the cron monitor by slug
	crons, err := nucleus.Query[CronMonitor](ctx, s.db.SQL(),
		`SELECT cron_id, tenant_id, site_id, name, slug, schedule,
			grace_period, enabled, COALESCE(ping_token, '') AS ping_token, created_at, version
		 FROM `+cronMonitorsLatest("site_id = $1 AND slug = $2")+`
		 WHERE enabled = 'true'`,
		siteID, slug)
	if err != nil {
		// Real backend failure — surface it so the handler returns 5xx and the
		// client retries, rather than conflating it with "no such cron" (404).
		return fmt.Errorf("monitoring: lookup cron %q/%q: %w", siteID, slug, err)
	}
	if len(crons) == 0 {
		return fmt.Errorf("%w: %q/%q", ErrCronNotFound, siteID, slug)
	}
	if crons[0].PingToken != "" {
		return fmt.Errorf("%w: %q/%q requires its ping token", ErrCronNotFound, siteID, slug)
	}
	return s.insertCheckin(ctx, crons[0], status, durationMs)
}

func (s *CronService) insertCheckin(ctx context.Context, cron CronMonitor, status string, durationMs int64) error {
	checkinID := genID()
	now := time.Now().UTC().UnixMilli()
	nowStr := dbutil.IntParam(now)

	if status == "" {
		status = "ok"
	}

	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO cron_checkins (checkin_id, tenant_id, cron_id, site_id,
			timestamp, status, duration_ms)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		checkinID, cron.TenantID, cron.CronID, cron.SiteID,
		nowStr, status, dbutil.IntParam(durationMs),
	)
	if err != nil {
		return fmt.Errorf("monitoring: record checkin: %w", err)
	}
	if s.OnCheckin != nil {
		// Detached from the caller's context on purpose. This runs on the
		// check-in request's context, and the hook's job is to resolve the
		// monitor's open incident — a heartbeat client that hangs up (curl
		// -m, a killed cron) cancels that request mid-query, the close never
		// lands, and the incident stays open forever. That is the
		// `cron incident auto-resolve failed ... context canceled` in the log.
		// The deadline still bounds it so a stuck query cannot leak.
		hookCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), checkinHookTimeout)
		defer cancel()
		s.OnCheckin(hookCtx, cron)
	}
	return nil
}

// checkinHookTimeout bounds the post-check-in hook. Generous next to a healthy
// close (two bounded queries) and well short of anything an operator would wait
// on.
const checkinHookTimeout = 15 * time.Second

// CheckMissed returns the enabled cron monitors whose next run is overdue.
//
// A monitor is overdue when now is past `last check-in + its schedule's period +
// its grace period`. The period term is the whole point: this used to compare
// against the grace period alone, so a cron that legitimately runs hourly with a
// five-minute grace was "missed" for fifty-five minutes out of every hour. Its
// incident opened, the next hourly ping closed it, and the cycle repeated — one
// incident per cron run, forever. Ten monitors produced 12,398 incident rows
// that way on the live instance, which is what buried the analytics chart under
// overlapping markers.
//
// A schedule that cannot be read as a period contributes 0, which is exactly the
// old behaviour, so an unparseable or empty schedule still alerts on grace alone.
// A monitor that has never checked in is measured from its creation time.
func (s *CronService) CheckMissed(ctx context.Context) ([]CronMonitor, error) {
	crons, err := nucleus.Query[CronMonitor](ctx, s.db.SQL(),
		`SELECT cron_id, tenant_id, site_id, name, slug, schedule,
			grace_period, enabled, COALESCE(ping_token, '') AS ping_token, created_at, version
		 FROM `+cronMonitorsLatest("")+`
		 WHERE enabled = 'true'`)
	if err != nil {
		return nil, fmt.Errorf("monitoring: check missed query: %w", err)
	}

	nowMs := time.Now().UTC().UnixMilli()
	var missed []CronMonitor

	for _, c := range crons {
		last, err := s.lastCheckinMs(ctx, c.CronID)
		if err != nil {
			s.logger.Error("monitoring: check missed last check-in failed", "cron", c.CronID, "err", err)
			continue
		}
		if last == 0 {
			// Never checked in — measure from when the monitor was registered.
			last, _ = strconv.ParseInt(c.CreatedAt, 10, 64)
		}
		if last == 0 {
			continue
		}
		if nowMs > last+DueAfterMs(c.Schedule, c.GracePeriod) {
			missed = append(missed, c)
		}
	}

	return missed, nil
}

// DueAfterMs is how long after a check-in a monitor may stay silent before it
// counts as missed: its schedule's period plus its grace period. Exported so the
// detector's arithmetic is testable without a database.
func DueAfterMs(schedule string, graceSecs int) int64 {
	if graceSecs <= 0 {
		graceSecs = 300
	}
	period, _ := SchedulePeriod(schedule)
	return period.Milliseconds() + int64(graceSecs)*1000
}

// lastCheckinMs is the timestamp of the monitor's most recent check-in, or 0 if
// it has never checked in. A single-column aggregate — the one query shape that
// reliably streams on a large Nucleus table rather than materialising it.
func (s *CronService) lastCheckinMs(ctx context.Context, cronID string) (int64, error) {
	type lastRow struct {
		Last string `db:"last"`
	}
	rows, err := nucleus.Query[lastRow](ctx, s.db.SQL(),
		`SELECT CAST(COALESCE(MAX(timestamp), 0) AS TEXT) AS last FROM cron_checkins
		 WHERE cron_id = $1`, cronID)
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	last, _ := strconv.ParseInt(rows[0].Last, 10, 64)
	return last, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func genID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
