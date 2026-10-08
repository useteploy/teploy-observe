package tracking

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/neutron-build/neutron/go/nucleus"
	"github.com/useteploy/teploy-observe/internal/nucleustest"
	"github.com/useteploy/teploy-observe/internal/schema"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
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
	user := os.Getenv("OBSERVE_ADMIN_USER")
	if user == "" {
		user = "admin"
	}
	pass := os.Getenv("OBSERVE_ADMIN_PASSWORD")
	if pass == "" {
		// Same default as e2e/tests/helpers.ts ("observe" alone is under
		// the server's 8-char password floor and can never be booted).
		pass = "observe-e2e-pass"
	}
	body, _ := json.Marshal(map[string]string{"username": user, "password": pass})
	resp, err := http.Post(base+"/api/v1/auth/login", "application/json", bytes.NewReader(body))
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

func TestRegisteredPixelsPersistClicks(t *testing.T) {
	ctx := context.Background()
	db, err := nucleus.Connect(ctx, nucleustest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := schema.Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	svc := NewLinkService(db)
	tenant := fmt.Sprintf("pixel-%d", time.Now().UnixNano())
	link, err := svc.CreateLink(ctx, tenant, "site", "test", "https://example.com")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /t/{slug}/pixel.gif", svc.PixelHandler())
	mux.HandleFunc("GET /t/pixel.gif", svc.PixelHandler())
	for _, path := range []string{"/t/" + link.Slug + "/pixel.gif?slug=unknown", "/t/pixel.gif?slug=" + link.Slug, "/t/pixel.gif", "/t/deadbeef/pixel.gif"} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 || w.Header().Get("Content-Type") != "image/gif" || !bytes.Equal(w.Body.Bytes(), transparentGIF) || w.Header().Get("Cache-Control") == "" {
			t.Fatalf("pixel %s: %d %v", path, w.Code, w.Header())
		}
	}
	links, err := svc.ListLinks(ctx, tenant, "site")
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 || links[0].ClickCount != "2" {
		t.Fatalf("clicks=%+v", links)
	}
}

func TestPixelMissingOrMalformedIdentityStillServesGIF(t *testing.T) {
	svc := NewLinkService(nil) // Invalid identity must not touch storage.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /t/{slug}/pixel.gif", svc.PixelHandler())
	mux.HandleFunc("GET /t/pixel.gif", svc.PixelHandler())
	for _, path := range []string{"/t/pixel.gif", "/t/pixel.gif?slug=invalid", "/t/not-hex!/pixel.gif?slug=12345678", "/t/pixel.gif?slug=0123456789"} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), transparentGIF) {
			t.Fatalf("pixel %s: %d %q", path, w.Code, w.Body.Bytes())
		}
	}
}
