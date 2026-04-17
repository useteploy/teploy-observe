package aiquery

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"
)

// SchemaCard caches a prose description of the Observe schema that the
// LLM can reason over. The UI rebuild or a migration bump invalidates it.
type SchemaCard struct {
	db      *nucleus.Client
	mu      sync.Mutex
	cached  string
	builtAt time.Time
	ttl     time.Duration
}

func NewSchemaCard(db *nucleus.Client) *SchemaCard {
	return &SchemaCard{db: db, ttl: 5 * time.Minute}
}

// Get returns a (possibly cached) schema description.
func (c *SchemaCard) Get(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cached != "" && time.Since(c.builtAt) < c.ttl {
		return c.cached, nil
	}

	type row struct {
		Table  string `db:"table_name"`
		Column string `db:"column_name"`
		Type   string `db:"data_type"`
	}
	rows, err := nucleus.Query[row](ctx, c.db.SQL(),
		"SELECT table_name, column_name, data_type FROM information_schema.columns WHERE table_schema = 'public' ORDER BY table_name, ordinal_position")
	if err != nil {
		// Fallback to the static list if information_schema is unavailable.
		c.cached = fallbackCard
		c.builtAt = time.Now()
		return c.cached, nil
	}

	byTable := map[string][]string{}
	for _, r := range rows {
		byTable[r.Table] = append(byTable[r.Table], fmt.Sprintf("%s (%s)", r.Column, r.Type))
	}

	var names []string
	for t := range byTable {
		// Skip Nucleus system tables that clutter the prompt.
		if strings.HasPrefix(t, "pg_") || strings.HasPrefix(t, "information_schema") {
			continue
		}
		names = append(names, t)
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString("Tables:\n")
	for _, t := range names {
		b.WriteString("- " + t + ": ")
		b.WriteString(strings.Join(byTable[t], ", "))
		b.WriteByte('\n')
	}

	c.cached = b.String()
	c.builtAt = time.Now()
	return c.cached, nil
}

// Invalidate clears the cached card. Call after migrations.
func (c *SchemaCard) Invalidate() {
	c.mu.Lock()
	c.cached = ""
	c.mu.Unlock()
}

// fallbackCard is used when information_schema queries fail. Keep in sync
// with the real schema on a best-effort basis — the live card is the
// authoritative source.
const fallbackCard = `Tables:
- events: event_id, timestamp, site_id, session_id, event_type, url, pathname, referrer, browser, os, country, properties
- events_recent: same shape as events, last 24h
- error_events: error_id, site_id, timestamp, message, stack, url, browser
- issues: issue_id, site_id, fingerprint, first_seen, last_seen, status, count
- spans: trace_id, span_id, parent_span_id, service, operation, start_time, duration_ns
- logs: log_id, site_id, timestamp, level, message, service_name, trace_id
- sessions: session_id, site_id, started_at, duration_ms, page_count
- flags: flag_id, site_id, flag_key, enabled, rollout_pct
- alert_rules: rule_id, site_id, metric, threshold, comparator
- llm_traces: trace_id, model, provider, prompt_tokens, completion_tokens, cost_usd
`
