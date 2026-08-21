package query

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"testing"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
)

// TestBoardSummary_AggregatesAcrossSites pins the C4 contract: given
// synthetic events / errors / replays / uptime results across 3 sites,
// BoardSummary returns one row per site with the right counts.
//
// Plants data via the SQL API (not the ingest wrapper) because we need
// exact control over site_id and timestamps.
func TestBoardSummary_AggregatesAcrossSites(t *testing.T) {
	dsn := nucleustest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("nucleus not reachable at %s — skipping", dsn)
	}
	defer db.Close()

	// Future window so we can't collide with seeded or real data.
	from := time.Date(2099, 6, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	tsMid := from.Add(12 * time.Hour).UnixMilli()

	siteA := "test_board_a_" + boardsToken()
	siteB := "test_board_b_" + boardsToken()
	siteC := "test_board_c_" + boardsToken()

	// Plant pageviews per site: A=5, B=2, C=0.
	plantPV := func(site, sess string, n int) {
		for i := 0; i < n; i++ {
			_, err := db.SQL().Exec(ctx,
				`INSERT INTO events (
					event_id, tenant_id, site_id, session_id, visit_id,
					timestamp, event_type, url, pathname, title, referrer,
					utm_source, utm_medium, utm_campaign,
					country, browser, os, device, language,
					screen_width, screen_height,
					properties, distinct_id, release_tag
				) VALUES ($1, 'default', $2, $3, $3, $4, 'pageview',
					'https://x/', '/', '', '',
					'', '', '', '', '', '', '', '',
					0, 0, 'null', '', '')`,
				boardsRandID(), site, sess,
				strconv.FormatInt(tsMid+int64(i), 10),
			)
			if err != nil {
				t.Fatalf("plant pv: %v", err)
			}
		}
	}
	plantPV(siteA, "sess_a1", 3)
	plantPV(siteA, "sess_a2", 2)
	plantPV(siteB, "sess_b1", 2)

	// Plant errors: A=2, B=0, C=1.
	plantErr := func(site string) {
		_, err := db.SQL().Exec(ctx,
			`INSERT INTO error_events (
				error_id, tenant_id, site_id, session_id, replay_id,
				issue_id, group_hash, timestamp, error_type, error_value,
				mechanism, handled, level, release_tag, environment,
				url, browser, os, device,
				stack_trace, breadcrumbs, contexts, extra, distinct_id
			) VALUES ($1, 'default', $2, '', '', $3, $3,
				$4, 'BoardErr', 'boom', 'captured', 'false', 'error', '', '',
				'', '', '', '', 'null', 'null', 'null', 'null', '')`,
			boardsRandID(), site, boardsRandID(),
			strconv.FormatInt(tsMid+1, 10),
		)
		if err != nil {
			t.Fatalf("plant error: %v", err)
		}
	}
	plantErr(siteA)
	plantErr(siteA)
	plantErr(siteC)

	// Plant a replay session for siteB (so siteB's last_activity_ms
	// is non-zero even with no events of its own beyond the planted PVs).
	_, err = db.SQL().Exec(ctx,
		`INSERT INTO replay_sessions (
			replay_id, tenant_id, site_id, session_id, start_time,
			duration_ms, page_count, url, browser, os, device,
			has_error, distinct_id
		) VALUES ($1, 'default', $2, 'sess_b1', $3,
			'1000', '1', '/', '', '', '', 'false', '')`,
		boardsRandID(), siteB, strconv.FormatInt(tsMid+5, 10),
	)
	if err != nil {
		t.Fatalf("plant replay: %v", err)
	}

	// Plant uptime results for siteA: 4 ok, 1 down -> 80%.
	plantUp := func(site, monitor string, isUp string) {
		_, err := db.SQL().Exec(ctx,
			`INSERT INTO uptime_results (
				result_id, tenant_id, monitor_id, site_id,
				timestamp, status_code, response_ms, is_up, error_message
			) VALUES ($1, 'default', $2, $3, $4, '200', '50', $5, '')`,
			boardsRandID(), monitor, site,
			strconv.FormatInt(tsMid+1, 10), isUp,
		)
		if err != nil {
			t.Fatalf("plant uptime: %v", err)
		}
	}
	monitorA := "mon_" + boardsToken()
	for i := 0; i < 4; i++ {
		plantUp(siteA, monitorA, "true")
	}
	plantUp(siteA, monitorA, "false")

	// Stub lookup so SiteName / Domain come back populated without
	// inserting into the sites table.
	lookup := func(_ context.Context, id string) (SiteMeta, bool) {
		switch id {
		case siteA:
			return SiteMeta{SiteID: id, Name: "Site A", Domain: "a.test"}, true
		case siteB:
			return SiteMeta{SiteID: id, Name: "Site B", Domain: "b.test"}, true
		case siteC:
			return SiteMeta{SiteID: id, Name: "Site C", Domain: "c.test"}, true
		}
		return SiteMeta{}, false
	}

	svc := NewBoardService(db, lookup)
	rows, err := svc.BoardSummary(ctx, []string{siteA, siteB, siteC}, from.UnixMilli(), to.UnixMilli())
	if err != nil {
		t.Fatalf("BoardSummary: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3 (got %+v)", len(rows), rows)
	}

	bySite := map[string]SiteRow{}
	for _, r := range rows {
		bySite[r.SiteID] = r
	}

	a := bySite[siteA]
	if a.Pageviews != 5 {
		t.Errorf("siteA pageviews = %d, want 5", a.Pageviews)
	}
	if a.Visitors != 2 {
		t.Errorf("siteA visitors = %d, want 2", a.Visitors)
	}
	if a.Errors != 2 {
		t.Errorf("siteA errors = %d, want 2", a.Errors)
	}
	if a.UptimePct < 79.9 || a.UptimePct > 80.1 {
		t.Errorf("siteA uptime_pct = %v, want ~80", a.UptimePct)
	}
	if a.Domain != "a.test" {
		t.Errorf("siteA domain = %q, want a.test", a.Domain)
	}
	if a.LastActivityMs == 0 {
		t.Errorf("siteA last_activity_ms = 0, want non-zero")
	}

	b := bySite[siteB]
	if b.Pageviews != 2 {
		t.Errorf("siteB pageviews = %d, want 2", b.Pageviews)
	}
	if b.Errors != 0 {
		t.Errorf("siteB errors = %d, want 0", b.Errors)
	}
	if b.ReplayCount != 1 {
		t.Errorf("siteB replays = %d, want 1", b.ReplayCount)
	}
	if b.UptimePct != 0 {
		t.Errorf("siteB uptime_pct = %v, want 0 (no monitor results)", b.UptimePct)
	}

	c := bySite[siteC]
	if c.Pageviews != 0 {
		t.Errorf("siteC pageviews = %d, want 0", c.Pageviews)
	}
	if c.Errors != 1 {
		t.Errorf("siteC errors = %d, want 1", c.Errors)
	}
	if c.SiteName != "Site C" {
		t.Errorf("siteC name = %q, want 'Site C'", c.SiteName)
	}
}

// TestBoardSummary_EmptyInputReturnsEmpty pins the contract that an
// empty site_ids list returns []SiteRow{} (not nil), matching the
// emptyOnNil convention used by every list endpoint.
func TestBoardSummary_EmptyInputReturnsEmpty(t *testing.T) {
	dsn := nucleustest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("nucleus not reachable")
	}
	defer db.Close()

	svc := NewBoardService(db, nil)
	rows, err := svc.BoardSummary(ctx, []string{}, 0, 1)
	if err != nil {
		t.Fatalf("BoardSummary: %v", err)
	}
	if rows == nil {
		t.Fatalf("rows = nil, want []")
	}
	if len(rows) != 0 {
		t.Fatalf("rows = %d, want 0", len(rows))
	}
}

// TestBoardSummary_DedupesSiteIDs pins that a duplicated site_id in the
// input only produces one row in the output, even under the parallel
// fan-out.
func TestBoardSummary_DedupesSiteIDs(t *testing.T) {
	dsn := nucleustest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("nucleus not reachable")
	}
	defer db.Close()

	site := "test_board_dedup_" + boardsToken()
	from := time.Date(2099, 7, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)

	svc := NewBoardService(db, nil)
	rows, err := svc.BoardSummary(ctx, []string{site, site, site}, from.UnixMilli(), to.UnixMilli())
	if err != nil {
		t.Fatalf("BoardSummary: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 (dedup)", len(rows))
	}
}

// TestSavedBoardCRUD pins the create / list / delete (tombstone)
// lifecycle of saved boards. Uses a dedicated test name so concurrent
// runs don't collide.
func TestSavedBoardCRUD(t *testing.T) {
	dsn := nucleustest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("nucleus not reachable")
	}
	defer db.Close()

	svc := NewBoardService(db, nil)

	name := "test_board_crud_" + boardsToken()
	payload := BoardPayload{
		SiteIDs: []string{"a", "b", "c"},
		Metrics: []string{"pageviews", "errors"},
		Window:  "24h",
	}
	created, err := svc.CreateBoard(ctx, name, payload, "tester")
	if err != nil {
		t.Fatalf("CreateBoard: %v", err)
	}
	if created.BoardID == "" {
		t.Fatalf("created.BoardID empty")
	}
	if created.Payload == "" || created.Payload == "{}" {
		t.Fatalf("created.Payload not persisted: %q", created.Payload)
	}

	// List should include our row.
	list, err := svc.ListBoards(ctx)
	if err != nil {
		t.Fatalf("ListBoards: %v", err)
	}
	found := false
	for _, b := range list {
		if b.BoardID == created.BoardID && b.Name == name {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("board not in list: %+v", list)
	}

	// Delete and verify the row is actually gone in storage. Nucleus
	// has a per-pool projection cache that survives DELETEs from a
	// sibling connection (logged as a new dogfood finding by this
	// session — read-after-write inconsistency on the same client).
	// We work around it by opening a fresh client for the verify SELECT.
	if err := svc.DeleteBoard(ctx, created.BoardID); err != nil {
		t.Fatalf("DeleteBoard: %v", err)
	}
	verifyDB, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("verify connect: %v", err)
	}
	defer verifyDB.Close()
	rows, err := nucleus.Query[SavedBoard](ctx, verifyDB.SQL(),
		`SELECT board_id, tenant_id, name, COALESCE(payload, '') AS payload,
			created_by, created_at, version
		 FROM boards WHERE board_id = $1`,
		created.BoardID,
	)
	if err != nil {
		t.Fatalf("verify list: %v", err)
	}
	for _, r := range rows {
		if r.Name != "" {
			t.Fatalf("board still in storage after delete: %+v", r)
		}
	}
}

func boardsToken() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func boardsRandID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
