package tracking

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

// TestListLinks_EmptySiteReturns200 locks in the contract that a fresh-install
// site (no rows in `tracked_links`) returns HTTP 200 with `[]`, never 500.
//
// Before the fix, internal/tracking/links.go queried `links` while migration
// 005_features.up.sql:180 creates `tracked_links` — every read tripped a
// "table not found" error and the handler 500'd. This test pins the table
// name match to the schema.
//
// We hit the live admin API at OBSERVE_URL (default http://localhost:3000).
// If the stack isn't running, the test skips rather than fails — matches
// the existing e2e pattern of "stack must be up" tests.
func TestListLinks_EmptySiteReturns200(t *testing.T) {
	base := os.Getenv("OBSERVE_URL")
	if base == "" {
		// Pin to IPv4 so we don't accidentally hit a co-tenant on ::1.
		base = "http://127.0.0.1:3000"
	}

	if !stackUp(t, base) {
		t.Skipf("observe stack not reachable at %s — skipping live API test", base)
	}

	token := login(t, base)

	// Use a guaranteed-empty site_id so we exercise the nil-slice path.
	const emptySite = "__links_empty__"
	req, err := http.NewRequest("GET", base+"/api/v1/links?site_id="+emptySite, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/v1/links: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (want 200), body = %s", resp.StatusCode, string(body))
	}

	if strings.TrimSpace(string(body)) == "null" {
		t.Fatalf(`body is literal "null" — must be "[]"`)
	}

	var arr []map[string]any
	if err := json.Unmarshal(body, &arr); err != nil {
		t.Fatalf("body is not a JSON array: %v (body=%s)", err, string(body))
	}

	if len(arr) != 0 {
		t.Fatalf("empty site returned %d links — expected 0", len(arr))
	}
}

func stackUp(t *testing.T, base string) bool {
	t.Helper()
	resp, err := http.Get(base + "/healthz")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func login(t *testing.T, base string) string {
	t.Helper()
	resp, err := http.Post(base+"/api/v1/auth/login", "application/json",
		strings.NewReader(`{"username":"admin","password":"observe"}`))
	if err != nil {
		t.Fatalf("login post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("login: status %d, body %s", resp.StatusCode, string(body))
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("login decode: %v", err)
	}
	if out.Token == "" {
		t.Fatalf("login returned empty token")
	}
	return out.Token
}
