// Package queryguard implements O12 query admission: declared CPU/time/
// row budgets and per-site + global concurrency bounds for the expensive
// read paths, with labeled refusals (code + remedy) instead of unbounded
// scans, and a snapshot of the admission state for /healthz.
//
// v1 posture (deliberate): when a slot is not available the query is
// REFUSED with a remedy, not queued. The Snapshot therefore reports the
// queue as the set of in-flight queries; a visible waiting-room with
// positions is a follow-up once refusal telemetry shows operators need it.
package queryguard

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Refusal codes. Stable strings: they appear at /healthz counters, in
// handler responses, and in tests that pin the labeled-refusal contract.
const (
	CodeConcurrencyGlobal = "query_concurrency_global"
	CodeConcurrencySite   = "query_concurrency_site"
	CodeBudgetRows        = "query_budget_rows"
	CodeBudgetTime        = "query_budget_time"
)

// Budgets declares the per-query resource budgets for the expensive read
// paths. Every field has a declared default (DefaultBudgets) and is
// env-tunable at startup (LoadBudgetsFromEnv); the values are surfaced in
// the /healthz snapshot so an operator reading a refusal can see the
// ceiling that produced it.
type Budgets struct {
	// Timeout bounds one expensive query's total wall time, including
	// engine-side scan time (pgx cancellation propagates server-side —
	// verified by the explorer's OBS-017 note, which this package relies
	// on for the cancellation half of O12).
	Timeout time.Duration
	// MaxScanRows bounds how many raw event rows one query may pull into
	// the application before it refuses. Rows are the declared proxy for
	// memory: the streaming scan holds O(1) rows at a time, so total
	// allocation is bounded by entities visited, and work is bounded by
	// this cap.
	MaxScanRows int64
	// MaxWindow bounds the time range a funnel/retention query may
	// request (older start is clamped, matching retention's pinned
	// 186-day clamp). A range clamp bounds the scan independently of row
	// counts.
	MaxWindow time.Duration
}

// DefaultBudgets are the declared O12 defaults. Funnel/retention windows
// keep the pre-O04 retention clamp (186 days); the scan-row ceiling of one
// million rows is roughly a day of pageviews at 10k/min — far above real
// dashboard funnels, far below anything that can OOM a 512 MiB engine
// through O(1)-row streaming.
func DefaultBudgets() Budgets {
	return Budgets{
		Timeout:     30 * time.Second,
		MaxScanRows: 1_000_000,
		MaxWindow:   186 * 24 * time.Hour,
	}
}

// LoadBudgetsFromEnv reads the O12 budget env knobs over defaults:
//
//	OBSERVE_QUERY_TIMEOUT_MS       per-query wall-time budget (default 30000)
//	OBSERVE_QUERY_MAX_SCAN_ROWS    per-query scanned-row budget (default 1000000)
//	OBSERVE_QUERY_MAX_WINDOW_DAYS  funnel/retention window clamp (default 186)
//
// Unparsable or non-positive values keep the default and log a warning —
// a bad knob must not take the read path to zero or negative budgets.
func LoadBudgetsFromEnv(getenv func(string) string, logger *slog.Logger) Budgets {
	b := DefaultBudgets()
	if getenv == nil {
		return b
	}
	intKnob := func(name string, dst *int64) {
		raw := strings.TrimSpace(getenv(name))
		if raw == "" {
			return
		}
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n <= 0 {
			if logger != nil {
				logger.Warn("query budget knob is not a positive integer — using the default", "env", name, "raw", raw)
			}
			return
		}
		*dst = n
	}
	var ms int64
	intKnob("OBSERVE_QUERY_TIMEOUT_MS", &ms)
	if ms > 0 {
		b.Timeout = time.Duration(ms) * time.Millisecond
	}
	intKnob("OBSERVE_QUERY_MAX_SCAN_ROWS", &b.MaxScanRows)
	var days int64
	intKnob("OBSERVE_QUERY_MAX_WINDOW_DAYS", &days)
	if days > 0 {
		b.MaxWindow = time.Duration(days) * 24 * time.Hour
	}
	return b
}

// Refusal is a labeled admission refusal. It is an error so it flows
// through the existing handler paths; Code/Remedy make it actionable
// instead of a bare 500.
type Refusal struct {
	Code    string
	Message string
	Remedy  string
	// Status is the HTTP status a handler should map this to (429 for
	// load-shedding refusals, 504 for the time budget).
	Status int
}

func (r *Refusal) Error() string {
	if r.Remedy == "" {
		return fmt.Sprintf("%s: %s", r.Code, r.Message)
	}
	return fmt.Sprintf("%s: %s (remedy: %s)", r.Code, r.Message, r.Remedy)
}

func refusal(code, message, remedy string, status int) *Refusal {
	return &Refusal{Code: code, Message: message, Remedy: remedy, Status: status}
}

// Limiter bounds query concurrency globally and per site. The zero value
// is not usable; construct with NewLimiter (main.go reads the slot counts
// from env, tests pass them directly).
type Limiter struct {
	globalSlots int
	siteSlots   int

	mu       sync.Mutex
	global   int
	sites    map[string]int
	refused  map[string]int64 // refusal counts by code
	acquired int64
}

// Default concurrency slots. Global 8 / site 4 keeps a single site's
// dashboard burst from saturating the engine while leaving headroom for
// ingest flushes on the same Nucleus instance (the small-install default
// where ingest and queries share one engine).
const (
	DefaultGlobalSlots = 8
	DefaultSiteSlots   = 4
)

// NewLimiter returns a limiter with the given slot counts. Non-positive
// values fall back to the declared defaults.
func NewLimiter(globalSlots, siteSlots int) *Limiter {
	if globalSlots <= 0 {
		globalSlots = DefaultGlobalSlots
	}
	if siteSlots <= 0 {
		siteSlots = DefaultSiteSlots
	}
	return &Limiter{
		globalSlots: globalSlots,
		siteSlots:   siteSlots,
		sites:       make(map[string]int),
		refused:     make(map[string]int64),
	}
}

// LoadSlotsFromEnv reads the concurrency knobs:
//
//	OBSERVE_QUERY_GLOBAL_CONCURRENCY  total in-flight expensive queries (default 8)
//	OBSERVE_QUERY_SITE_CONCURRENCY    in-flight per site (default 4)
func LoadSlotsFromEnv(getenv func(string) string, logger *slog.Logger) (global, site int) {
	global, site = DefaultGlobalSlots, DefaultSiteSlots
	if getenv == nil {
		return
	}
	knob := func(name string, dst *int) {
		raw := strings.TrimSpace(getenv(name))
		if raw == "" {
			return
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			if logger != nil {
				logger.Warn("query concurrency knob is not a positive integer — using the default", "env", name, "raw", raw)
			}
			return
		}
		*dst = n
	}
	knob("OBSERVE_QUERY_GLOBAL_CONCURRENCY", &global)
	knob("OBSERVE_QUERY_SITE_CONCURRENCY", &site)
	return
}

// Acquire admits one query for siteID or refuses it labeled. v1 refuses
// immediately when either bound is full (no queue wait); the caller must
// call the returned release exactly once on success.
func (l *Limiter) Acquire(ctx context.Context, siteID string) (func(), error) {
	l.mu.Lock()
	if l.global >= l.globalSlots {
		l.refused[CodeConcurrencyGlobal]++
		l.mu.Unlock()
		return nil, refusal(CodeConcurrencyGlobal,
			fmt.Sprintf("%d of %d global query slots in use", l.global, l.globalSlots),
			"retry when in-flight queries finish, or raise OBSERVE_QUERY_GLOBAL_CONCURRENCY",
			429)
	}
	if l.sites[siteID] >= l.siteSlots {
		l.refused[CodeConcurrencySite]++
		l.mu.Unlock()
		return nil, refusal(CodeConcurrencySite,
			fmt.Sprintf("site has %d of %d query slots in use", l.sites[siteID], l.siteSlots),
			"retry when this site's in-flight queries finish, or raise OBSERVE_QUERY_SITE_CONCURRENCY",
			429)
	}
	l.global++
	l.sites[siteID]++
	l.acquired++
	l.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			l.global--
			l.sites[siteID]--
			if l.sites[siteID] <= 0 {
				delete(l.sites, siteID)
			}
			l.mu.Unlock()
		})
	}, nil
}

// CountRefusal records a budget refusal (row/time) against the snapshot
// counters so all O12 refusal telemetry lives in one /healthz block.
func (l *Limiter) CountRefusal(code string) {
	l.mu.Lock()
	l.refused[code]++
	l.mu.Unlock()
}

// SiteSnapshot is the per-site leg of the /healthz view.
type SiteSnapshot struct {
	Running int `json:"running"`
	Limit   int `json:"limit"`
}

// Snapshot is the /healthz query-admission view: slot gauges, refusal
// counters by code, and the declared budgets that produced them.
type Snapshot struct {
	GlobalRunning int                     `json:"global_running"`
	GlobalLimit   int                     `json:"global_limit"`
	SiteLimit     int                     `json:"site_limit"`
	Sites         map[string]SiteSnapshot `json:"sites"`
	Refused       map[string]int64        `json:"refused_total"`
	AcquiredTotal int64                   `json:"acquired_total"`
	Budgets       map[string]any          `json:"budgets"`
}

func (l *Limiter) Snapshot(b Budgets) Snapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	sites := make(map[string]SiteSnapshot, len(l.sites))
	for site, n := range l.sites {
		sites[site] = SiteSnapshot{Running: n, Limit: l.siteSlots}
	}
	refused := make(map[string]int64, len(l.refused))
	for code, n := range l.refused {
		refused[code] = n
	}
	return Snapshot{
		GlobalRunning: l.global,
		GlobalLimit:   l.globalSlots,
		SiteLimit:     l.siteSlots,
		Sites:         sites,
		Refused:       refused,
		AcquiredTotal: l.acquired,
		Budgets: map[string]any{
			"timeout_ms":      b.Timeout.Milliseconds(),
			"max_scan_rows":   b.MaxScanRows,
			"max_window_days": int64(b.MaxWindow / (24 * time.Hour)),
		},
	}
}

// SiteIDs returns the sites with in-flight queries, sorted — used by
// tests and by operators diffing /healthz over time.
func (s Snapshot) SiteIDs() []string {
	ids := make([]string, 0, len(s.Sites))
	for id := range s.Sites {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// RowBudgetRefusal builds the labeled refusal for exceeding MaxScanRows
// and records it on the limiter (nil-safe: budget refusals are counted
// even when concurrency admission is disabled).
func RowBudgetRefusal(l *Limiter, max int64) *Refusal {
	if l != nil {
		l.CountRefusal(CodeBudgetRows)
	}
	return refusal(CodeBudgetRows,
		fmt.Sprintf("query scanned more than the declared maximum of %d rows", max),
		"narrow the time range or filters, or raise OBSERVE_QUERY_MAX_SCAN_ROWS",
		429)
}

// TimeBudgetRefusal builds the labeled refusal for the wall-time budget.
func TimeBudgetRefusal(l *Limiter, budget time.Duration) *Refusal {
	if l != nil {
		l.CountRefusal(CodeBudgetTime)
	}
	return refusal(CodeBudgetTime,
		fmt.Sprintf("query exceeded its %s wall-time budget", budget),
		"narrow the time range or filters, or raise OBSERVE_QUERY_TIMEOUT_MS",
		504)
}
