package llm

// O14: the versioned model/cost catalog. The compiled-in table below stays
// as the seed source and the fallback; llm_model_prices (migration 054) is
// the operator-maintained, versioned truth: rows resolve by longest
// model-prefix match (provider-specific over provider-agnostic) among rows
// whose valid_from has arrived, newest effective row per prefix wins. A
// price change lands as a new valid_from row - history is never rewritten,
// so a cost computed last month stays reproducible.

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
)

// CatalogEntry is one effective catalog row (the collapsed view).
type CatalogEntry struct {
	Provider    string  `json:"provider"`
	ModelPrefix string  `json:"model_prefix"`
	InputPer1k  float64 `json:"input_per_1k"`
	OutputPer1k float64 `json:"output_per_1k"`
	Currency    string  `json:"currency"`
	ValidFrom   int64   `json:"valid_from"`
	Source      string  `json:"source"`
}

// builtinSeed is the compiled-in price table - the same numbers the old
// estimateCost hard-coded, now the catalog's seed (source 'builtin') and
// the no-database fallback. USD per 1K tokens.
type builtinPrice struct {
	prefix        string
	input, output float64
}

var builtinSeed = []builtinPrice{
	{"gpt-4o-mini", 0.00015, 0.0006},
	{"gpt-4o", 0.005, 0.015},
	{"gpt-4-turbo", 0.01, 0.03},
	{"gpt-4", 0.03, 0.06},
	{"gpt-3.5-turbo", 0.0005, 0.0015},
	{"o3-mini", 0.0011, 0.0044},
	{"o1-mini", 0.0011, 0.0044},
	{"o1", 0.015, 0.06},
	{"claude-3-5-sonnet", 0.003, 0.015},
	{"claude-3-5-haiku", 0.0008, 0.004},
	{"claude-3-opus", 0.015, 0.075},
	{"claude-3-sonnet", 0.003, 0.015},
	{"claude-3-haiku", 0.00025, 0.00125},
}

// catalogCache is the process-local catalog snapshot with a short TTL:
// ingest consults it per auto-cost call, and a fresh-but-cheap read beats
// both a per-call roundtrip and a stale-forever cache.
const catalogTTL = time.Minute

type catalogCache struct {
	mu        sync.Mutex
	entries   []CatalogEntry
	loadedAt  time.Time
	seedTried bool
}

func (c *catalogCache) fresh() bool {
	return time.Since(c.loadedAt) < catalogTTL
}

// pricesCollapse renders the argMax collapse over llm_model_prices - one
// logical row per (provider, model_prefix, valid_from), highest version
// wins, then the loader keeps only the NEWEST valid_from per key (the
// effective price).
func pricesCollapse(where string) string {
	if strings.TrimSpace(where) == "" {
		where = "1 = 1"
	}
	return `SELECT tenant_id, provider, model_prefix,
			argMax(input_per_1k, version) AS input_per_1k,
			argMax(output_per_1k, version) AS output_per_1k,
			argMax(currency, version) AS currency,
			argMax(valid_from, version) AS valid_from,
			argMax(source, version) AS source,
			MAX(version) AS version
		FROM llm_model_prices WHERE ` + where + `
		GROUP BY tenant_id, provider, model_prefix, valid_from`
}

// loadCatalog reads the effective catalog: collapsed rows, filtered to
// valid_from <= now, and among rows sharing (provider, model_prefix) only
// the newest valid_from survives.
func (s *LLMService) loadCatalog(ctx context.Context, now time.Time) ([]CatalogEntry, error) {
	rows, err := nucleus.Query[CatalogEntry](ctx, s.db.SQL(),
		`SELECT provider, model_prefix, input_per_1k, output_per_1k, currency, valid_from, source
		 FROM (`+pricesCollapse("")+`)
		 WHERE valid_from <= $1
		 ORDER BY valid_from ASC`,
		dbutil.IntParam(now.UnixMilli()))
	if err != nil {
		return nil, fmt.Errorf("llm: catalog load: %w", err)
	}
	// Newest effective row per (provider, model_prefix) wins; ASC order +
	// map overwrite keeps the last (newest).
	effective := make(map[string]CatalogEntry, len(rows))
	for _, r := range rows {
		effective[r.Provider+"\x00"+r.ModelPrefix] = r
	}
	out := make([]CatalogEntry, 0, len(effective))
	for _, r := range effective {
		out = append(out, r)
	}
	// Longest prefix first, provider-specific before provider-agnostic at
	// equal length - the resolution order resolveCatalog walks.
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if len(a.ModelPrefix) != len(b.ModelPrefix) {
			return len(a.ModelPrefix) > len(b.ModelPrefix)
		}
		if a.Provider != b.Provider {
			return a.Provider > b.Provider // '' sorts last among equals
		}
		return a.ModelPrefix < b.ModelPrefix
	})
	return out, nil
}

// EnsureBuiltinCatalog seeds the compiled-in prices into llm_model_prices
// (source 'builtin', valid_from 0), one time per database: a seed row is
// inserted only when no row exists for that (provider ”, prefix, 0) key.
// Idempotent - concurrent callers converge on the replacing-mergetree key.
// Operator rows are never touched.
func (s *LLMService) EnsureBuiltinCatalog(ctx context.Context) error {
	s.cat.mu.Lock()
	defer s.cat.mu.Unlock()
	if s.cat.seedTried {
		return nil
	}
	now := time.Now().UTC().UnixMilli()
	for _, bp := range builtinSeed {
		existing, err := nucleus.Query[struct {
			Prefix string `db:"model_prefix"`
		}](ctx, s.db.SQL(),
			`SELECT model_prefix FROM (`+pricesCollapse("provider = '' AND model_prefix = $1 AND valid_from = 0")+`)`,
			bp.prefix)
		if err != nil {
			return fmt.Errorf("llm: catalog seed check: %w", err)
		}
		if len(existing) > 0 {
			continue
		}
		if _, err := s.db.SQL().Exec(ctx,
			`INSERT INTO llm_model_prices (
				tenant_id, provider, model_prefix, input_per_1k, output_per_1k,
				currency, valid_from, source, created_at, version
			) VALUES ('default', '', $1, $2, $3, 'usd', 0, 'builtin', $4, $4)`,
			bp.prefix, strconv.FormatFloat(bp.input, 'f', -1, 64),
			strconv.FormatFloat(bp.output, 'f', -1, 64), dbutil.IntParam(now),
		); err != nil {
			return fmt.Errorf("llm: catalog seed insert: %w", err)
		}
	}
	s.cat.seedTried = true
	return nil
}

// catalog returns the effective catalog through the TTL cache, seeding the
// builtins first. Falls back to the compiled-in table when the database
// cannot answer (estimation must not take ingest down with the store).
func (s *LLMService) catalog(ctx context.Context) []CatalogEntry {
	now := time.Now()
	s.cat.mu.Lock()
	if s.cat.fresh() {
		entries := s.cat.entries
		s.cat.mu.Unlock()
		return entries
	}
	s.cat.mu.Unlock()

	if err := s.EnsureBuiltinCatalog(ctx); err != nil {
		s.dbUnavailable(err)
	}
	entries, err := s.loadCatalog(ctx, now)
	if err != nil {
		// Store outage: serve the compiled-in fallback rather than failing
		// ingest; the failure is logged, and costs stay labeled estimated.
		s.dbUnavailable(err)
		return builtinFallback()
	}
	s.cat.mu.Lock()
	s.cat.entries = entries
	s.cat.loadedAt = now
	s.cat.mu.Unlock()
	return entries
}

func (s *LLMService) dbUnavailable(err error) {
	if s.logger != nil {
		s.logger.Warn("llm: catalog read failed; using the compiled-in fallback", "err", err)
	}
}

func builtinFallback() []CatalogEntry {
	out := make([]CatalogEntry, 0, len(builtinSeed))
	for _, bp := range builtinSeed {
		out = append(out, CatalogEntry{
			Provider: "", ModelPrefix: bp.prefix,
			InputPer1k: bp.input, OutputPer1k: bp.output,
			Currency: "usd", ValidFrom: 0, Source: "builtin",
		})
	}
	return out
}

// resolveCatalog picks the price for one model string: the longest
// matching model_prefix among provider-specific rows first, then among
// provider-agnostic (”) rows. Resolution never depends on input order.
// No match -> the conservative unknown-model default.
func resolveCatalog(entries []CatalogEntry, provider, model string) (float64, float64, bool) {
	resolvePass := func(wantProvider string) (float64, float64, bool) {
		bestLen := -1
		var in, out float64
		for _, e := range entries {
			if e.Provider != wantProvider {
				continue
			}
			if model == e.ModelPrefix || strings.HasPrefix(model, e.ModelPrefix) {
				if len(e.ModelPrefix) > bestLen {
					bestLen = len(e.ModelPrefix)
					in, out = e.InputPer1k, e.OutputPer1k
				}
			}
		}
		return in, out, bestLen >= 0
	}
	if in, out, ok := resolvePass(provider); ok && provider != "" {
		return in, out, true
	}
	if in, out, ok := resolvePass(""); ok {
		return in, out, true
	}
	return 0.001, 0.002, false // conservative default for unknown models
}

// SetPrice writes one catalog row (operator path): same (provider, prefix,
// valid_from) key at a strictly-greater version replaces - the argMax
// collapse keeps one logical row.
func (s *LLMService) SetPrice(ctx context.Context, e CatalogEntry) error {
	if e.ModelPrefix == "" {
		return fmt.Errorf("model_prefix is required")
	}
	if e.InputPer1k < 0 || e.OutputPer1k < 0 {
		return fmt.Errorf("prices must be >= 0")
	}
	if e.Currency == "" {
		e.Currency = "usd"
	}
	if e.Source == "" {
		e.Source = "operator"
	}
	now := time.Now().UTC().UnixMilli()

	// Version must beat any existing row on the same key (the version-tie
	// defect): read the collapsed max version, stamp greater.
	prior, err := nucleus.Query[struct {
		Version int64 `db:"version"`
	}](ctx, s.db.SQL(),
		`SELECT MAX(version) AS version FROM (`+pricesCollapse("provider = $1 AND model_prefix = $2 AND valid_from = $3")+`)`,
		e.Provider, e.ModelPrefix, dbutil.IntParam(e.ValidFrom))
	if err != nil {
		return fmt.Errorf("llm: set price version read: %w", err)
	}
	version := now
	if len(prior) > 0 && prior[0].Version+1 > version {
		version = prior[0].Version + 1
	}
	if _, err := s.db.SQL().Exec(ctx,
		`INSERT INTO llm_model_prices (
			tenant_id, provider, model_prefix, input_per_1k, output_per_1k,
			currency, valid_from, source, created_at, version
		) VALUES ('default', $1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		e.Provider, e.ModelPrefix,
		strconv.FormatFloat(e.InputPer1k, 'f', -1, 64),
		strconv.FormatFloat(e.OutputPer1k, 'f', -1, 64),
		e.Currency, dbutil.IntParam(e.ValidFrom), e.Source,
		dbutil.IntParam(now), dbutil.IntParam(version),
	); err != nil {
		return fmt.Errorf("llm: set price: %w", err)
	}
	// Drop the cache so the next read reflects the change immediately.
	s.cat.mu.Lock()
	s.cat.loadedAt = time.Time{}
	s.cat.mu.Unlock()
	return nil
}

// ListPrices returns the effective catalog (the collapsed, newest-
// effective view an operator reviews).
func (s *LLMService) ListPrices(ctx context.Context) ([]CatalogEntry, error) {
	return s.loadCatalog(ctx, time.Now())
}
