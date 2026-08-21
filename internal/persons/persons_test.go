package persons

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

// TestListPersons_AggregatesByDistinctID pins the C2 contract: given
// synthetic events with three distinct_ids (one anonymous), ListPersons
// returns one Person per identified id, sorted by last_seen DESC,
// with correct first_seen / last_seen / event_count / session_count.
func TestListPersons_AggregatesByDistinctID(t *testing.T) {
	dsn := nucleustest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("nucleus not reachable at %s — skipping", dsn)
	}
	defer db.Close()

	site := "test_persons_" + personsToken()
	// Future window so we can't collide with seeded data.
	base := time.Date(2099, 8, 1, 0, 0, 0, 0, time.UTC).UnixMilli()

	plant := func(distinctID, sessionID, country, browser string, offsetMs int64) {
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
				'', '', '', $5, $6, '', '', '',
				0, 0, 'null', $7, '')`,
			personsRandID(), site, sessionID,
			strconv.FormatInt(base+offsetMs, 10),
			country, browser, distinctID,
		)
		if err != nil {
			t.Fatalf("plant event: %v", err)
		}
	}

	// userA: 3 events across 2 sessions, last_seen = base+3000
	plant("userA", "sa1", "US", "Chrome", 1000)
	plant("userA", "sa1", "US", "Chrome", 1500)
	plant("userA", "sa2", "US", "Firefox", 3000)

	// userB: 1 event, last_seen = base+5000 (most recent)
	plant("userB", "sb1", "DE", "Safari", 5000)

	// anonymous: 2 events, must NOT appear in default list
	plant("", "anon1", "FR", "Chrome", 4000)
	plant("", "anon2", "FR", "Chrome", 4500)

	svc := NewService(db)

	// Default: anonymous excluded, sorted by last_seen DESC.
	rows, err := svc.ListPersons(ctx, site, base, base+10000, 50, 0, false)
	if err != nil {
		t.Fatalf("ListPersons: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (got %+v)", len(rows), rows)
	}
	if rows[0].DistinctID != "userB" {
		t.Errorf("rows[0] = %s, want userB (last_seen DESC)", rows[0].DistinctID)
	}
	if rows[1].DistinctID != "userA" {
		t.Errorf("rows[1] = %s, want userA", rows[1].DistinctID)
	}

	// userA aggregate sanity.
	a := rows[1]
	if a.EventCount != 3 {
		t.Errorf("userA event_count = %d, want 3", a.EventCount)
	}
	if a.SessionCount != 2 {
		t.Errorf("userA session_count = %d, want 2", a.SessionCount)
	}
	if a.FirstSeenMs != base+1000 {
		t.Errorf("userA first_seen_ms = %d, want %d", a.FirstSeenMs, base+1000)
	}
	if a.LastSeenMs != base+3000 {
		t.Errorf("userA last_seen_ms = %d, want %d", a.LastSeenMs, base+3000)
	}
	if a.TopCountry != "US" {
		t.Errorf("userA top_country = %q, want US", a.TopCountry)
	}

	// include_anonymous=true: anon row should appear.
	withAnon, err := svc.ListPersons(ctx, site, base, base+10000, 50, 0, true)
	if err != nil {
		t.Fatalf("ListPersons (anon): %v", err)
	}
	if len(withAnon) != 3 {
		t.Fatalf("with anon rows = %d, want 3", len(withAnon))
	}
	// Anonymous distinct_id appears as "" — confirm at least one row has empty id.
	sawAnon := false
	for _, r := range withAnon {
		if r.DistinctID == "" {
			sawAnon = true
			if r.EventCount != 2 {
				t.Errorf("anon event_count = %d, want 2", r.EventCount)
			}
		}
	}
	if !sawAnon {
		t.Errorf("expected anon row in include_anonymous=true result")
	}
}

// TestListPersons_TopCountryIsMostRecent pins that TopCountry/TopBrowser reflect
// the most-recent value (argMax over timestamp), not the lexically-largest one
// (the old MAX(col) bug). Values are chosen so most-recent != lexical max.
func TestListPersons_TopCountryIsMostRecent(t *testing.T) {
	dsn := nucleustest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("nucleus not reachable at %s — skipping", dsn)
	}
	defer db.Close()

	site := "test_persons_mr_" + personsToken()
	base := time.Date(2099, 9, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	plant := func(country, browser string, offsetMs int64) {
		_, err := db.SQL().Exec(ctx,
			`INSERT INTO events (event_id, tenant_id, site_id, session_id, visit_id,
				timestamp, event_type, country, browser, distinct_id)
			 VALUES ($1, 'default', $2, 's1', 's1', $3, 'pageview', $4, $5, 'u1')`,
			personsRandID(), site, strconv.FormatInt(base+offsetMs, 10), country, browser)
		if err != nil {
			t.Fatalf("plant: %v", err)
		}
	}
	// Earlier = lexically larger; later = lexically smaller. MAX() would pick the
	// earlier (ZZ / Safari); argMax-over-timestamp must pick the later (AA / Chrome).
	plant("ZZ", "Safari", 1000)
	plant("AA", "Chrome", 5000)

	rows, err := NewService(db).ListPersons(ctx, site, base, base+10000, 50, 0, false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 person, got %d", len(rows))
	}
	if rows[0].TopCountry != "AA" {
		t.Fatalf("TopCountry = %q, want AA (most recent, not lexical max ZZ)", rows[0].TopCountry)
	}
	if rows[0].TopBrowser != "Chrome" {
		t.Fatalf("TopBrowser = %q, want Chrome (most recent)", rows[0].TopBrowser)
	}
}

// TestListPersons_PaginationAndCount pins limit/offset behaviour and
// the CountPersons helper.
func TestListPersons_PaginationAndCount(t *testing.T) {
	dsn := nucleustest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("nucleus not reachable")
	}
	defer db.Close()

	site := "test_persons_pg_" + personsToken()
	base := time.Date(2099, 8, 5, 0, 0, 0, 0, time.UTC).UnixMilli()

	for i := 0; i < 5; i++ {
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
				0, 0, 'null', $5, '')`,
			personsRandID(), site,
			"s_"+strconv.Itoa(i),
			strconv.FormatInt(base+int64(i*1000), 10),
			"u_"+strconv.Itoa(i),
		)
		if err != nil {
			t.Fatalf("plant: %v", err)
		}
	}

	svc := NewService(db)

	total, err := svc.CountPersons(ctx, site, base, base+10000, false)
	if err != nil {
		t.Fatalf("CountPersons: %v", err)
	}
	if total != 5 {
		t.Errorf("total = %d, want 5", total)
	}

	page1, err := svc.ListPersons(ctx, site, base, base+10000, 2, 0, false)
	if err != nil {
		t.Fatalf("ListPersons p1: %v", err)
	}
	if len(page1) != 2 {
		t.Errorf("page1 len = %d, want 2", len(page1))
	}

	page2, err := svc.ListPersons(ctx, site, base, base+10000, 2, 2, false)
	if err != nil {
		t.Fatalf("ListPersons p2: %v", err)
	}
	if len(page2) != 2 {
		t.Errorf("page2 len = %d, want 2", len(page2))
	}
	if page1[0].DistinctID == page2[0].DistinctID {
		t.Errorf("offset not applied: p1[0] = p2[0] = %s", page1[0].DistinctID)
	}
}

// TestPersonDetail_TimelinePopulated pins that PersonDetail returns
// the aggregate plus a non-empty timeline ordered by timestamp DESC.
func TestPersonDetail_TimelinePopulated(t *testing.T) {
	dsn := nucleustest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("nucleus not reachable")
	}
	defer db.Close()

	site := "test_persons_tl_" + personsToken()
	uid := "user_detail_" + personsToken()
	base := time.Date(2099, 8, 10, 0, 0, 0, 0, time.UTC).UnixMilli()

	plant := func(eventType, path string, offsetMs int64) {
		_, err := db.SQL().Exec(ctx,
			`INSERT INTO events (
				event_id, tenant_id, site_id, session_id, visit_id,
				timestamp, event_type, url, pathname, title, referrer,
				utm_source, utm_medium, utm_campaign,
				country, browser, os, device, language,
				screen_width, screen_height,
				properties, distinct_id, release_tag
			) VALUES ($1, 'default', $2, 'sx', 'sx', $3, $4,
				$5, $6, '', '',
				'', '', '', '', '', '', '', '',
				0, 0, 'null', $7, '')`,
			personsRandID(), site,
			strconv.FormatInt(base+offsetMs, 10), eventType,
			"https://x"+path, path,
			uid,
		)
		if err != nil {
			t.Fatalf("plant: %v", err)
		}
	}
	plant("pageview", "/a", 1000)
	plant("pageview", "/b", 2000)
	plant("click", "/b", 2500)

	svc := NewService(db)
	det, err := svc.PersonDetail(ctx, site, uid)
	if err != nil {
		t.Fatalf("PersonDetail: %v", err)
	}
	if det.Person.DistinctID != uid {
		t.Errorf("distinct_id = %q, want %q", det.Person.DistinctID, uid)
	}
	if det.Person.EventCount != 3 {
		t.Errorf("event_count = %d, want 3", det.Person.EventCount)
	}
	if len(det.Timeline) != 3 {
		t.Fatalf("timeline len = %d, want 3", len(det.Timeline))
	}
	// DESC ordering: most recent (offset 2500) first.
	if det.Timeline[0].Timestamp != base+2500 {
		t.Errorf("timeline[0].timestamp = %d, want %d", det.Timeline[0].Timestamp, base+2500)
	}
}

func personsToken() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func personsRandID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
