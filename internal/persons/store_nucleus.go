package persons

import (
	"context"
	"fmt"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
)

// nucleusEvents reads persons from the events table. The SQL is the C2
// aggregate, moved verbatim from the original Service methods.
type nucleusEvents struct{ db *nucleus.Client }

func (n *nucleusEvents) list(ctx context.Context, siteID string, fromMs, toMs int64, limit, offset int, includeAnonymous bool) ([]Person, error) {
	from := dbutil.IntParam(fromMs)
	to := dbutil.IntParam(toMs)

	// Inner aggregate computes the per-distinct_id summary. argMax picks
	// the most recent country/browser per person.
	anonClause := "AND distinct_id != ''"
	if includeAnonymous {
		anonClause = ""
	}

	q := fmt.Sprintf(`SELECT distinct_id,
	        MIN(CAST(timestamp AS BIGINT)) AS first_seen_ms,
	        MAX(CAST(timestamp AS BIGINT)) AS last_seen_ms,
	        COUNT(*) AS event_count,
	        COUNT(DISTINCT session_id) AS session_count,
	        argMax(country, CAST(timestamp AS BIGINT)) AS top_country,
	        argMax(browser, CAST(timestamp AS BIGINT)) AS top_browser
	 FROM events
	 WHERE site_id = $1
	   AND timestamp >= $2
	   AND timestamp < $3
	   %s
	 GROUP BY distinct_id
	 ORDER BY last_seen_ms DESC
	 LIMIT %d OFFSET %d`, anonClause, limit, offset)

	rows, err := nucleus.Query[Person](ctx, n.db.SQL(), q, siteID, from, to)
	if err != nil {
		return nil, fmt.Errorf("list persons: %w", err)
	}
	return rows, nil
}

func (n *nucleusEvents) List(ctx context.Context, siteID string, fromMs, toMs int64, limit, offset int, includeAnonymous bool) ([]Person, error) {
	return n.list(ctx, siteID, fromMs, toMs, limit, offset, includeAnonymous)
}

func (n *nucleusEvents) ListAll(ctx context.Context, siteID string, fromMs, toMs int64, limit int, includeAnonymous bool) ([]Person, error) {
	return n.list(ctx, siteID, fromMs, toMs, limit, 0, includeAnonymous)
}

func (n *nucleusEvents) Count(ctx context.Context, siteID string, fromMs, toMs int64, includeAnonymous bool) (int64, error) {
	from := dbutil.IntParam(fromMs)
	to := dbutil.IntParam(toMs)

	anonClause := "AND distinct_id != ''"
	if includeAnonymous {
		anonClause = ""
	}

	type countRow struct {
		Total int64 `db:"total"`
	}
	q := fmt.Sprintf(`SELECT COUNT(DISTINCT distinct_id) AS total
	 FROM events
	 WHERE site_id = $1
	   AND timestamp >= $2
	   AND timestamp < $3
	   %s`, anonClause)

	rows, err := nucleus.Query[countRow](ctx, n.db.SQL(), q, siteID, from, to)
	if err != nil {
		return 0, fmt.Errorf("count persons: %w", err)
	}
	if len(rows) == 0 {
		return 0, nil
	}
	return rows[0].Total, nil
}

func (n *nucleusEvents) Aggregate(ctx context.Context, siteID, key string) (Person, bool, error) {
	// Aggregate over the full history of this distinct_id (no time
	// window — the detail view always shows the lifetime summary).
	aggQ := `SELECT distinct_id,
	        MIN(CAST(timestamp AS BIGINT)) AS first_seen_ms,
	        MAX(CAST(timestamp AS BIGINT)) AS last_seen_ms,
	        COUNT(*) AS event_count,
	        COUNT(DISTINCT session_id) AS session_count,
	        argMax(country, CAST(timestamp AS BIGINT)) AS top_country,
	        argMax(browser, CAST(timestamp AS BIGINT)) AS top_browser
	 FROM events
	 WHERE site_id = $1 AND distinct_id = $2
	 GROUP BY distinct_id`

	rows, err := nucleus.Query[Person](ctx, n.db.SQL(), aggQ, siteID, key)
	if err != nil {
		return Person{}, false, fmt.Errorf("person aggregate: %w", err)
	}
	if len(rows) == 0 {
		return Person{}, false, nil
	}
	return rows[0], true, nil
}

func (n *nucleusEvents) Timeline(ctx context.Context, siteID, key string) ([]PersonEvent, error) {
	tlQ := `SELECT event_id, event_type,
	        COALESCE(url, '') AS url,
	        COALESCE(pathname, '') AS pathname,
	        CAST(timestamp AS BIGINT) AS timestamp
	 FROM events
	 WHERE site_id = $1 AND distinct_id = $2
	 ORDER BY CAST(timestamp AS BIGINT) DESC
	 LIMIT 100`
	return nucleus.Query[PersonEvent](ctx, n.db.SQL(), tlQ, siteID, key)
}

func (n *nucleusEvents) Exists(ctx context.Context, siteID, key string) (bool, error) {
	type idRow struct {
		EventID string `db:"event_id"`
	}
	rows, err := nucleus.Query[idRow](ctx, n.db.SQL(),
		`SELECT event_id FROM events WHERE site_id = $1 AND distinct_id = $2 LIMIT 1`, siteID, key)
	if err != nil {
		return false, fmt.Errorf("person exists: %w", err)
	}
	return len(rows) > 0, nil
}

// nucleusIdentity is the 061 side-table store. All three tables are
// ReplacingMergeTree collapsed with argMax over version (precedent:
// internal/platform/maintenance.go); writes are strictly-monotonic new
// versions and nothing is ever DELETEd.
type nucleusIdentity struct{ db *nucleus.Client }

type propsRow struct {
	Props   string `db:"props"`
	Version int64  `db:"version"`
}

func (n *nucleusIdentity) latestProps(ctx context.Context, siteID, key string) (propsRow, bool, error) {
	rows, err := nucleus.Query[propsRow](ctx, n.db.SQL(),
		`SELECT argMax(props, version) AS props, MAX(version) AS version
		 FROM person_properties
		 WHERE tenant_id = 'default' AND site_id = $1 AND person_key = $2
		 GROUP BY person_key`, siteID, key)
	if err != nil {
		return propsRow{}, false, fmt.Errorf("get person properties: %w", err)
	}
	if len(rows) == 0 {
		return propsRow{}, false, nil
	}
	return rows[0], true, nil
}

func (n *nucleusIdentity) GetProps(ctx context.Context, siteID, key string) (string, bool, error) {
	r, ok, err := n.latestProps(ctx, siteID, key)
	return r.Props, ok, err
}

func (n *nucleusIdentity) PutProps(ctx context.Context, siteID, key, propsJSON string) error {
	prev, _, err := n.latestProps(ctx, siteID, key)
	if err != nil {
		return err
	}
	now := time.Now().UTC().UnixMilli()
	v := now
	if prev.Version+1 > v {
		v = prev.Version + 1
	}
	_, err = n.db.SQL().Exec(ctx,
		`INSERT INTO person_properties (tenant_id, site_id, person_key, props, updated_at, version)
		 VALUES ('default', $1, $2, $3, $4, $5)`,
		siteID, key, propsJSON, dbutil.IntParam(now), dbutil.IntParam(v))
	if err != nil {
		return fmt.Errorf("put person properties: %w", err)
	}
	return nil
}

func (n *nucleusIdentity) ListAliases(ctx context.Context, siteID string, limit int) ([]AliasRow, error) {
	// active is filtered OUTSIDE the collapse: an un-merge is a newer
	// version with active='false' and must win over the superseded row.
	rows, err := nucleus.Query[AliasRow](ctx, n.db.SQL(),
		fmt.Sprintf(`SELECT alias_key, canonical_key FROM (
		   SELECT alias_key,
		          argMax(canonical_key, version) AS canonical_key,
		          argMax(active, version) AS active
		   FROM person_aliases
		   WHERE tenant_id = 'default' AND site_id = $1
		   GROUP BY alias_key)
		 WHERE active = 'true'
		 ORDER BY alias_key
		 LIMIT %d`, limit), siteID)
	if err != nil {
		return nil, fmt.Errorf("list person aliases: %w", err)
	}
	return rows, nil
}

func (n *nucleusIdentity) PutAlias(ctx context.Context, siteID, alias, canonical, actor string, active bool) error {
	type vRow struct {
		Version int64 `db:"version"`
	}
	prev, err := nucleus.Query[vRow](ctx, n.db.SQL(),
		`SELECT MAX(version) AS version FROM person_aliases
		 WHERE tenant_id = 'default' AND site_id = $1 AND alias_key = $2
		 GROUP BY alias_key`, siteID, alias)
	if err != nil {
		return fmt.Errorf("read alias version: %w", err)
	}
	now := time.Now().UTC().UnixMilli()
	v := now
	if len(prev) > 0 && prev[0].Version+1 > v {
		v = prev[0].Version + 1
	}
	act := "true"
	if !active {
		act = "false"
	}
	_, err = n.db.SQL().Exec(ctx,
		`INSERT INTO person_aliases (tenant_id, site_id, alias_key, canonical_key, active, created_by, created_at, version)
		 VALUES ('default', $1, $2, $3, $4, $5, $6, $7)`,
		siteID, alias, canonical, act, actor, dbutil.IntParam(now), dbutil.IntParam(v))
	if err != nil {
		return fmt.Errorf("put person alias: %w", err)
	}
	return nil
}

type keyRow struct {
	PersonKey string `db:"person_key"`
}

func (n *nucleusIdentity) ListTombstones(ctx context.Context, siteID string, limit int) ([]string, error) {
	rows, err := nucleus.Query[keyRow](ctx, n.db.SQL(),
		fmt.Sprintf(`SELECT person_key FROM person_tombstones
		 WHERE tenant_id = 'default' AND site_id = $1 LIMIT %d`, limit), siteID)
	if err != nil {
		return nil, fmt.Errorf("list person tombstones: %w", err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.PersonKey)
	}
	return out, nil
}

func (n *nucleusIdentity) IsTombstoned(ctx context.Context, siteID, key string) (bool, error) {
	rows, err := nucleus.Query[keyRow](ctx, n.db.SQL(),
		`SELECT person_key FROM person_tombstones
		 WHERE tenant_id = 'default' AND site_id = $1 AND person_key = $2 LIMIT 1`, siteID, key)
	if err != nil {
		return false, fmt.Errorf("check person tombstone: %w", err)
	}
	return len(rows) > 0, nil
}

func (n *nucleusIdentity) PutTombstone(ctx context.Context, siteID, key, actor string) error {
	now := time.Now().UTC().UnixMilli()
	_, err := n.db.SQL().Exec(ctx,
		`INSERT INTO person_tombstones (tenant_id, site_id, person_key, erased_by, erased_at, version)
		 VALUES ('default', $1, $2, $3, $4, $4)`,
		siteID, key, actor, dbutil.IntParam(now))
	if err != nil {
		return fmt.Errorf("put person tombstone: %w", err)
	}
	return nil
}
