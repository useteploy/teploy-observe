package query

// C4 (Wave 2, 2026-05-10): cross-site board summary. Agency / MSP target —
// fan out per-site analytics queries in parallel and assemble one row per
// site for the /boards UI grid.
//
// Stays within internal/query for the per-site SQL but reaches into the
// sites package only for the SiteService interface (name + domain). All
// the heavy lifting is plain SQL against the shared events / error_events
// / replay_sessions / uptime_results tables.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
)

// SiteRow is one row in the board grid: aggregate stats for a single
// site over the requested window. All counts default to 0; LastActivityMs
// is 0 when the site has no events in the window (renders as "—" client
// side).
type SiteRow struct {
	SiteID         string  `json:"site_id"`
	SiteName       string  `json:"site_name"`
	Domain         string  `json:"domain"`
	Pageviews      int64   `json:"pageviews"`
	Visitors       int64   `json:"visitors"`
	Sessions       int64   `json:"sessions"`
	Errors         int64   `json:"errors"`
	UptimePct      float64 `json:"uptime_pct"`
	ReplayCount    int64   `json:"replay_count"`
	LastActivityMs int64   `json:"last_activity_ms"`
}

// SiteMeta is the minimum site-identity tuple BoardService needs.
// Keeping it as a small struct avoids importing the whole sites package
// into internal/query and lets tests plant fixtures without standing up
// a full SiteService.
type SiteMeta struct {
	SiteID string
	Name   string
	Domain string
}

// SiteLookup returns site metadata for a given site_id. The BoardService
// uses this to attach Name and Domain to each row without a hard
// dependency on internal/sites.
type SiteLookup func(ctx context.Context, siteID string) (SiteMeta, bool)

// BoardService computes cross-site aggregates for a list of site_ids.
// One instance is shared across requests; the per-call work is the
// fan-out itself, bounded by a semaphore.
type BoardService struct {
	db     *nucleus.Client
	lookup SiteLookup
}

// NewBoardService constructs a BoardService. lookup may be nil, in
// which case rows come back without SiteName / Domain populated (the
// IDs still flow through). Tests pass a stub lookup.
func NewBoardService(db *nucleus.Client, lookup SiteLookup) *BoardService {
	return &BoardService{db: db, lookup: lookup}
}

// fanoutLimit caps the per-call concurrency for site-level queries.
// 8 is a balance between throughput on a 50-site board and not blowing
// up the Nucleus connection pool when several boards refresh at once.
const fanoutLimit = 8

// BoardSummary fans out per-site aggregate queries across siteIDs and
// returns one SiteRow per site. Errors fetching a single site are
// logged into the row's UptimePct=-1 sentinel only when the failure is
// monitor-related; an analytics failure for one site does not abort
// the whole call (the row comes back with zeroed counts).
func (s *BoardService) BoardSummary(ctx context.Context, siteIDs []string, fromMs, toMs int64) ([]SiteRow, error) {
	if len(siteIDs) == 0 {
		return []SiteRow{}, nil
	}
	if fromMs >= toMs {
		return []SiteRow{}, nil
	}

	// Dedup site_ids defensively — a UI bug that double-adds a site
	// would otherwise issue two parallel fan-outs for the same id.
	seen := make(map[string]struct{}, len(siteIDs))
	dedup := make([]string, 0, len(siteIDs))
	for _, id := range siteIDs {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		dedup = append(dedup, id)
	}

	rows := make([]SiteRow, len(dedup))
	sem := make(chan struct{}, fanoutLimit)
	var wg sync.WaitGroup

	for i, id := range dedup {
		i, id := i, id
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			rows[i] = s.summarizeSite(ctx, id, fromMs, toMs)
		}()
	}
	wg.Wait()

	// Stable order: sites with traffic first (by pageviews desc), then
	// alphabetical. Keeps the grid usable on boards with mixed activity.
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Pageviews != rows[j].Pageviews {
			return rows[i].Pageviews > rows[j].Pageviews
		}
		return rows[i].SiteName < rows[j].SiteName
	})

	return rows, nil
}

// summarizeSite issues the per-site SELECTs. Each query failure is
// swallowed so one bad site doesn't poison the whole board — the row
// just comes back with zeroed counts for the failing column.
func (s *BoardService) summarizeSite(ctx context.Context, siteID string, fromMs, toMs int64) SiteRow {
	row := SiteRow{SiteID: siteID}
	if s.lookup != nil {
		if meta, ok := s.lookup(ctx, siteID); ok {
			row.SiteName = meta.Name
			row.Domain = meta.Domain
		}
	}
	if row.SiteName == "" {
		row.SiteName = siteID
	}

	from := dbutil.IntParam(fromMs)
	to := dbutil.IntParam(toMs)

	// Pageviews + visitors + sessions + last activity from events.
	type evRow struct {
		Pageviews int64 `db:"pageviews"`
		Visitors  int64 `db:"visitors"`
		Sessions  int64 `db:"sessions"`
		LastTS    int64 `db:"last_ts"`
	}
	// COALESCE around an aggregate hits a Nucleus bug ("aggregate
	// function MAX outside of aggregate context", finding #15-family).
	// Scan the bare aggregate; an empty result means MAX returns 0
	// from nucleus's defaults — which is exactly what we want.
	if r, err := nucleus.Query[evRow](ctx, s.db.SQL(),
		`SELECT
			COUNT(*) AS pageviews,
			COUNT(DISTINCT session_id) AS visitors,
			COUNT(DISTINCT visit_id) AS sessions,
			MAX(CAST(timestamp AS BIGINT)) AS last_ts
		 FROM events
		 WHERE site_id = $1
		   AND CAST(timestamp AS BIGINT) >= CAST($2 AS BIGINT)
		   AND CAST(timestamp AS BIGINT) < CAST($3 AS BIGINT)
		   AND event_type = 'pageview'`,
		siteID, from, to,
	); err == nil && len(r) > 0 {
		row.Pageviews = r[0].Pageviews
		row.Visitors = r[0].Visitors
		row.Sessions = r[0].Sessions
		row.LastActivityMs = r[0].LastTS
	}

	// Error count — every error event in the window, not deduped to
	// issues. The board column is meant to surface error volume, not
	// distinct issues (that's what /errors is for).
	type errRow struct {
		Errors int64 `db:"errors"`
	}
	if r, err := nucleus.Query[errRow](ctx, s.db.SQL(),
		`SELECT COUNT(*) AS errors
		 FROM error_events
		 WHERE site_id = $1
		   AND CAST(timestamp AS BIGINT) >= CAST($2 AS BIGINT)
		   AND CAST(timestamp AS BIGINT) < CAST($3 AS BIGINT)`,
		siteID, from, to,
	); err == nil && len(r) > 0 {
		row.Errors = r[0].Errors
	}

	// Replay session count.
	type rpRow struct {
		ReplayCount int64 `db:"replay_count"`
	}
	if r, err := nucleus.Query[rpRow](ctx, s.db.SQL(),
		`SELECT COUNT(*) AS replay_count
		 FROM replay_sessions
		 WHERE site_id = $1
		   AND CAST(start_time AS BIGINT) >= CAST($2 AS BIGINT)
		   AND CAST(start_time AS BIGINT) < CAST($3 AS BIGINT)`,
		siteID, from, to,
	); err == nil && len(r) > 0 {
		row.ReplayCount = r[0].ReplayCount
	}

	// Uptime % across all monitors for the site in the window. is_up is
	// stored as TEXT 'true'/'false' (see monitoring/monitoring.go); count
	// the truthy ratio. Sites without any monitor results in the window
	// get UptimePct=0 — UI can render "—".
	type upRow struct {
		Total int64 `db:"total"`
		Up    int64 `db:"up_count"`
	}
	if r, err := nucleus.Query[upRow](ctx, s.db.SQL(),
		`SELECT
			COUNT(*) AS total,
			SUM(CASE WHEN is_up = 'true' THEN 1 ELSE 0 END) AS up_count
		 FROM uptime_results
		 WHERE site_id = $1
		   AND CAST(timestamp AS BIGINT) >= CAST($2 AS BIGINT)
		   AND CAST(timestamp AS BIGINT) < CAST($3 AS BIGINT)`,
		siteID, from, to,
	); err == nil && len(r) > 0 && r[0].Total > 0 {
		row.UptimePct = float64(r[0].Up) * 100.0 / float64(r[0].Total)
	}

	// Replays don't write to events, so a board with replay-only
	// activity should still show a non-zero LastActivityMs.
	if row.LastActivityMs == 0 {
		type lastRpRow struct {
			LastTS int64 `db:"last_ts"`
		}
		// Same COALESCE-around-aggregate avoidance as the events
		// query above (Nucleus finding #15 family).
		if r, err := nucleus.Query[lastRpRow](ctx, s.db.SQL(),
			`SELECT MAX(CAST(start_time AS BIGINT)) AS last_ts
			 FROM replay_sessions
			 WHERE site_id = $1
			   AND CAST(start_time AS BIGINT) >= CAST($2 AS BIGINT)
			   AND CAST(start_time AS BIGINT) < CAST($3 AS BIGINT)`,
			siteID, from, to,
		); err == nil && len(r) > 0 {
			row.LastActivityMs = r[0].LastTS
		}
	}

	return row
}

// ---------------------------------------------------------------------------
// Saved boards (CRUD)
// ---------------------------------------------------------------------------

// SavedBoard is a persisted board definition. Payload is the JSON shape
// described on the migration:
//
//	{ "site_ids": [...], "metrics": [...], "window": "24h" }
//
// Stored in the dedicated `boards` table (migration 021). The Payload
// field is opaque to the backend — UI builds and validates it.
type SavedBoard struct {
	BoardID   string `json:"board_id" db:"board_id"`
	TenantID  string `json:"-" db:"tenant_id"`
	Name      string `json:"name" db:"name"`
	Payload   string `json:"payload" db:"payload"`
	CreatedBy string `json:"created_by" db:"created_by"`
	CreatedAt string `json:"created_at" db:"created_at"`
	Version   string `json:"-" db:"version"`
}

// BoardPayload is the typed shape the API accepts and stores in the
// JSONB payload column. Kept loose on purpose so the UI can iterate
// quickly without a schema migration each time.
type BoardPayload struct {
	SiteIDs []string `json:"site_ids"`
	Metrics []string `json:"metrics"`
	Window  string   `json:"window"`
}

// CreateBoard inserts a board row. payload is marshaled as JSON; if the
// caller passed an empty BoardPayload, the column lands as `{}`.
func (s *BoardService) CreateBoard(ctx context.Context, name string, payload BoardPayload, createdBy string) (*SavedBoard, error) {
	if name == "" {
		return nil, fmt.Errorf("board name required")
	}
	id := genBoardID()
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)

	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal payload: %w", err)
	}

	_, err = s.db.SQL().Exec(ctx,
		`INSERT INTO boards (board_id, tenant_id, name, payload, created_by, created_at, version)
		 VALUES ($1, 'default', $2, $3, $4, $5, $6)`,
		id, name, string(raw), createdBy, now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("create board: %w", err)
	}
	return &SavedBoard{
		BoardID:   id,
		TenantID:  "default",
		Name:      name,
		Payload:   string(raw),
		CreatedBy: createdBy,
		CreatedAt: now,
	}, nil
}

// ListBoards returns all boards for the default tenant ordered by most
// recent first. ReplacingMergeTree dedup at read time isn't reliable
// in Nucleus (finding #10 + tombstone-write tests), so the List path
// dedups by board_id in Go and picks the row with the highest version
// number. A row whose latest version has empty name is a tombstone and
// is filtered out.
func (s *BoardService) ListBoards(ctx context.Context) ([]SavedBoard, error) {
	rows, err := nucleus.Query[SavedBoard](ctx, s.db.SQL(),
		`SELECT board_id, tenant_id, name, COALESCE(payload, '') AS payload,
			created_by, created_at, version
		 FROM boards
		 ORDER BY created_at DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("list boards: %w", err)
	}
	latest := make(map[string]SavedBoard, len(rows))
	for _, r := range rows {
		prev, ok := latest[r.BoardID]
		if !ok || versionOf(r.Version) > versionOf(prev.Version) {
			latest[r.BoardID] = r
		}
	}
	out := make([]SavedBoard, 0, len(latest))
	for _, r := range latest {
		if r.Name == "" {
			continue // tombstone
		}
		out = append(out, r)
	}
	// Stable order by created_at desc (most-recent-first matches the
	// UI sidebar pattern used by /dashboards).
	sort.Slice(out, func(i, j int) bool {
		return versionOf(out[i].CreatedAt) > versionOf(out[j].CreatedAt)
	})
	return out, nil
}

// DeleteBoard hard-deletes a board row. We tested both the
// tombstone-with-bumped-version pattern (used by saved_views et al.)
// and a plain DELETE; only DELETE actually drops the row from
// subsequent SELECTs in Nucleus today (ReplacingMergeTree read-time
// dedup is unreliable — finding #10 family). Hard DELETE is also
// fine here because boards have no audit / replay requirement.
func (s *BoardService) DeleteBoard(ctx context.Context, boardID string) error {
	_, err := s.db.SQL().Exec(ctx,
		`DELETE FROM boards WHERE board_id = $1`,
		boardID,
	)
	if err != nil {
		return fmt.Errorf("delete board: %w", err)
	}
	return nil
}

// versionOf parses a numeric millisecond version string. Garbage or
// empty values sort lowest so they lose the dedup tiebreaker, which is
// the safe default (a tombstone with version="" can't accidentally
// shadow a real row).
func versionOf(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

func genBoardID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
