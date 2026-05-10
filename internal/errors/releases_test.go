package errors

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"
)

// TestReleaseHealth_ComputesCrashFreeAndAdoption pins the math from B2
// phase 1: crash-free % = (sessions - sessions-with-error) / sessions,
// adoption % = sessions_in_release / sessions_in_window.
//
// Plants synthetic sessions and error_events directly via the SQL API
// (not the ingest wrapper — we need exact control over release_tag and
// timestamps), then calls Health() and asserts the math.
func TestReleaseHealth_ComputesCrashFreeAndAdoption(t *testing.T) {
	dsn := os.Getenv("OBSERVE_NUCLEUS_URL")
	if dsn == "" {
		dsn = "postgres://postgres@localhost:5432/postgres?sslmode=disable"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("nucleus not reachable at %s — skipping", dsn)
	}
	defer db.Close()

	// Use a unique site_id so we don't collide with seeded data or other
	// test runs. Window is a future time range so existing rows can't
	// interfere.
	siteID := "test_release_health_" + uniqueToken()
	from := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	tsMid := from.Add(12 * time.Hour).UnixMilli()

	// Plant sessions:
	//   - release v1.0: 4 sessions (one will be crashed)
	//   - release v1.1: 6 sessions (two will be crashed)
	// Adoption: v1.0 = 40%, v1.1 = 60%.
	plantSession := func(release string, sessID string) {
		_, err := db.SQL().Exec(ctx,
			`INSERT INTO sessions (
				tenant_id, site_id, session_id, first_ts, last_ts,
				pageviews, events_count, entry_url, exit_url,
				referrer, browser, os, device, country, language,
				screen_width, screen_height,
				utm_source, utm_medium, utm_campaign,
				is_bounce, version, release_tag
			) VALUES ('default', $1, $2, $3, $4,
				1, 1, '/', '/', '', '', '', '', '', '',
				0, 0, '', '', '',
				'false', '1', $5)`,
			siteID, sessID,
			strconv.FormatInt(tsMid, 10), strconv.FormatInt(tsMid, 10), release)
		if err != nil {
			t.Fatalf("plant session: %v", err)
		}
	}

	v10Sessions := []string{"sess_v10_1", "sess_v10_2", "sess_v10_3", "sess_v10_4"}
	v11Sessions := []string{"sess_v11_1", "sess_v11_2", "sess_v11_3",
		"sess_v11_4", "sess_v11_5", "sess_v11_6"}
	for _, s := range v10Sessions {
		plantSession("v1.0", s)
	}
	for _, s := range v11Sessions {
		plantSession("v1.1", s)
	}

	// Plant errors: v1.0 has 1 crashed session; v1.1 has 2.
	plantError := func(release, sessID string) {
		_, err := db.SQL().Exec(ctx,
			`INSERT INTO error_events (
				error_id, tenant_id, site_id, session_id, replay_id,
				issue_id, group_hash, timestamp, error_type, error_value,
				mechanism, handled, level, release_tag, environment,
				url, browser, os, device,
				stack_trace, breadcrumbs, contexts, extra, distinct_id
			) VALUES ($1, 'default', $2, $3, '', $4, $4,
				$5, 'TestErr', 'boom', 'captured', 'false', 'error', $6,
				'', '', '', '', '', '', '', '', '', '')`,
			randomID(), siteID, sessID, randomID(),
			strconv.FormatInt(tsMid+1, 10), release)
		if err != nil {
			t.Fatalf("plant error: %v", err)
		}
	}
	plantError("v1.0", "sess_v10_1")
	plantError("v1.1", "sess_v11_1")
	plantError("v1.1", "sess_v11_2")

	svc := NewReleaseHealthService(db)
	stats, err := svc.Health(ctx, siteID, from.UnixMilli(), to.UnixMilli())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}

	byRelease := make(map[string]ReleaseStat, len(stats))
	for _, s := range stats {
		byRelease[s.ReleaseTag] = s
	}

	v10, ok := byRelease["v1.0"]
	if !ok {
		t.Fatalf("v1.0 missing from result; got %+v", stats)
	}
	if v10.Sessions != 4 {
		t.Errorf("v1.0 sessions = %d, want 4", v10.Sessions)
	}
	if v10.CrashedSessions != 1 {
		t.Errorf("v1.0 crashed = %d, want 1", v10.CrashedSessions)
	}
	if v10.CrashFreeSessionPct != 75.0 {
		t.Errorf("v1.0 crash-free = %v, want 75.0", v10.CrashFreeSessionPct)
	}
	if v10.AdoptionPct != 40.0 {
		t.Errorf("v1.0 adoption = %v, want 40.0", v10.AdoptionPct)
	}

	v11, ok := byRelease["v1.1"]
	if !ok {
		t.Fatalf("v1.1 missing from result; got %+v", stats)
	}
	if v11.Sessions != 6 {
		t.Errorf("v1.1 sessions = %d, want 6", v11.Sessions)
	}
	if v11.CrashedSessions != 2 {
		t.Errorf("v1.1 crashed = %d, want 2", v11.CrashedSessions)
	}
	wantCF := float64(4) / float64(6) * 100.0
	if !approx(v11.CrashFreeSessionPct, wantCF, 0.001) {
		t.Errorf("v1.1 crash-free = %v, want ~%v", v11.CrashFreeSessionPct, wantCF)
	}
	if v11.AdoptionPct != 60.0 {
		t.Errorf("v1.1 adoption = %v, want 60.0", v11.AdoptionPct)
	}
	if v11.ErrorRate != 2.0/6.0 {
		t.Errorf("v1.1 error_rate = %v, want %v", v11.ErrorRate, 2.0/6.0)
	}
}

// TestReleaseHealth_NoSessionsReturnsEmpty pins the contract that an
// empty window returns []ReleaseStat{}, never nil — so the JSON wire is
// "[]" not "null".
func TestReleaseHealth_NoSessionsReturnsEmpty(t *testing.T) {
	dsn := os.Getenv("OBSERVE_NUCLEUS_URL")
	if dsn == "" {
		dsn = "postgres://postgres@localhost:5432/postgres?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("nucleus not reachable")
	}
	defer db.Close()

	svc := NewReleaseHealthService(db)
	from := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	to := from + int64(24*time.Hour/time.Millisecond)
	stats, err := svc.Health(ctx, "site_does_not_exist_"+uniqueToken(), from, to)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if len(stats) != 0 {
		t.Fatalf("expected 0 stats for empty site, got %d: %+v", len(stats), stats)
	}
}

func approx(a, b, tol float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < tol
}

func randomID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
