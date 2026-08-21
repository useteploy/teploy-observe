package errors

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
)

// TestIngestErrorEvent_InsertsAndIndexes is the contract test for the
// canonical write-through wrapper. Pre-refactor, INSERT and IndexError
// lived in two callers (handler + seed) and the seed forgot the index
// step — search returned 500 on fresh installs.
//
// The test ingests a synthetic error and then confirms two things:
//  1. the row landed in error_events (so /api/v1/issues sees it),
//  2. it's findable via the search service (so /api/v1/issues/search
//     doesn't 500 on the same data).
//
// The test connects directly to nucleus over the same DSN the dev stack
// uses, so it skips cleanly when nucleus isn't running.
func TestIngestErrorEvent_InsertsAndIndexes(t *testing.T) {
	dsn := nucleustest.DSN(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("nucleus not reachable at %s — skipping integration test", dsn)
	}
	defer db.Close()

	issueSvc := NewIssueService(db)
	searchSvc := NewSearchService(db)
	svc := NewService(db, issueSvc, searchSvc, nil)

	// A unique substring so we can search-find this exact row without
	// colliding with any other test data already in the dev DB.
	uniq := uniqueToken()
	input := ErrorInput{
		SiteID:     "default",
		ErrorType:  "TestIngestError",
		ErrorValue: "ingest-wrapper " + uniq,
		Mechanism:  "captured",
		Handled:    true,
		Level:      "error",
		URL:        "https://test.local/IngestErrorEvent",
		Browser:    "Chrome",
		OS:         "macOS",
		Device:     "desktop",
		StackTrace: []StackFrame{{Filename: "test.go", Function: "TestIngest", Lineno: 1, InApp: true}},
	}

	issueID, err := svc.IngestErrorEvent(ctx, input)
	if err != nil {
		t.Fatalf("IngestErrorEvent: %v", err)
	}
	if issueID == "" {
		t.Fatalf("IngestErrorEvent returned empty issue_id")
	}

	// Sanity-call IndexError directly — surfaces vendored-SDK issues
	// that the wrapper silently swallows. If this fails, it's a Nucleus
	// dogfood finding, not a wrapper bug.
	if err := searchSvc.IndexError(ctx, "default", "explicit-test-"+uniq, "TestIngestError", "explicit "+uniq); err != nil {
		t.Fatalf("IndexError direct call: %v", err)
	}

	// (1) row landed in error_events with the issue_id we got back.
	type countRow struct {
		Count int64 `db:"count"`
	}
	rows, err := nucleus.Query[countRow](ctx, db.SQL(),
		`SELECT COUNT(*) AS count FROM error_events WHERE issue_id = $1`, issueID)
	if err != nil {
		t.Fatalf("count error_events: %v", err)
	}
	if len(rows) == 0 || rows[0].Count == 0 {
		t.Fatalf("no error_events found for issue_id=%s", issueID)
	}

	// (2) FTS path resolves the same event by message substring.
	hits, err := searchSvc.Search(ctx, "default", uniq, 10)
	if err != nil {
		t.Fatalf("FTS search after ingest: %v", err)
	}
	if len(hits) == 0 {
		t.Fatalf("FTS search %q returned no hits — IndexError did not run", uniq)
	}
}

// TestSearchIssues_LiveAPIDoesNotErrorOnMissingFTS pins the system-level
// contract: GET /api/v1/issues/search must not return 500 even if the FTS
// index has zero documents (or contains data inconsistent with
// error_events). Hits the live admin API.
func TestSearchIssues_LiveAPIDoesNotErrorOnMissingFTS(t *testing.T) {
	base := os.Getenv("OBSERVE_URL")
	if base == "" {
		base = "http://127.0.0.1:3000"
	}
	if !ping(t, base) {
		t.Skipf("observe stack not reachable at %s", base)
	}
	tok := liveLogin(t, base)

	req, _ := http.NewRequest("GET",
		base+"/api/v1/issues/search?site_id=default&q=__definitely_no_match__"+uniqueToken(),
		nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/v1/issues/search: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (want 200), body = %s", resp.StatusCode, string(body))
	}
	var arr []map[string]any
	if err := json.Unmarshal(body, &arr); err != nil {
		t.Fatalf("body not a JSON array: %v (body=%s)", err, string(body))
	}
}

// --- shared helpers ---

func ping(t *testing.T, base string) bool {
	t.Helper()
	resp, err := http.Get(base + "/healthz")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func liveLogin(t *testing.T, base string) string {
	t.Helper()
	resp, err := http.Post(base+"/api/v1/auth/login", "application/json",
		strings.NewReader(`{"username":"admin","password":"observe"}`))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("login: %d %s", resp.StatusCode, string(body))
	}
	var out struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.Token == "" {
		t.Fatalf("login: empty token")
	}
	return out.Token
}

// uniqueToken returns a short hex string unique enough for FTS-collision
// avoidance within a single test run.
func uniqueToken() string {
	const alphabet = "0123456789abcdef"
	now := time.Now().UnixNano()
	out := make([]byte, 12)
	for i := range out {
		out[i] = alphabet[now&15]
		now >>= 4
	}
	return string(out)
}
