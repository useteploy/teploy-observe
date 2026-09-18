package ingest

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/neutron-dev/neutron-go/neutron"
	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
)

func ingestTestDB(t *testing.T) (*nucleus.Client, func()) {
	t.Helper()
	dsn := nucleustest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		cancel()
		t.Skipf("nucleus not reachable at %s — skipping", dsn)
	}
	return db, func() { db.Close(); cancel() }
}

// TestHandler_RejectsCrossTenantSiteID is the regression for the CRITICAL
// cross-tenant write: when an API key binds the request to site A (context),
// a body site_id of site B must be REJECTED, not silently written under B.
func TestHandler_RejectsCrossTenantSiteID(t *testing.T) {
	db, done := ingestTestDB(t)
	defer done()
	buf := NewBuffer(db, 1000, 100, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := Handler(buf, "test-salt", nil)

	// A real (non-bot) UA is required or the handler short-circuits as bot.
	siteCtx := func(site string) context.Context {
		c := context.WithValue(context.Background(), keyUserAgent, "Mozilla/5.0 (Macintosh) AppleWebKit/537.36 Chrome/120 Safari/537.36")
		return WithSiteID(c, site)
	}

	// Key-bound context = siteA; body claims siteB → forbidden.
	_, err := h(siteCtx("siteA"), IngestInput{SiteID: "siteB", EventType: "pageview", URL: "https://x/p"})
	if err == nil {
		t.Fatal("cross-tenant write was accepted — body site_id overrode the API-key site")
	}
	var appErr *neutron.AppError
	if !errors.As(err, &appErr) || appErr.Status != 403 {
		t.Fatalf("want 403 Forbidden, got %v", err)
	}

	// Matching site (or empty body site) under the same key must pass.
	if _, err := h(siteCtx("siteA"),
		IngestInput{SiteID: "siteA", EventType: "pageview", URL: "https://x/p"}); err != nil {
		t.Fatalf("matching site_id should be accepted: %v", err)
	}
	if _, err := h(siteCtx("siteA"),
		IngestInput{SiteID: "", EventType: "pageview", URL: "https://x/p"}); err != nil {
		t.Fatalf("empty body site_id under a key should inherit the key site: %v", err)
	}
}

// AUD-030 containment (round 2): referrer sanitization strips userinfo and
// fails closed on malformed input instead of storing it verbatim.
func TestCleanReferrer_FailClosedAndStripsUserinfo(t *testing.T) {
	if got := cleanReferrer("https://user:secret@external.example/path?q=1#f", "self.example"); got != "https://external.example/path" {
		t.Fatalf("userinfo/query/fragment must be stripped, got %q", got)
	}
	if got := cleanReferrer("http://[::1:not-a-url", ""); got != "" {
		t.Fatalf("malformed referrer must be dropped (fail closed), got %q", got)
	}
	if got := cleanReferrer("%zz://broken", ""); got != "" {
		t.Fatalf("unparseable referrer must be dropped, got %q", got)
	}
	if got := cleanReferrer("https://self.example/page", "self.example"); got != "" {
		t.Fatalf("self-referral must be dropped, got %q", got)
	}
}

// AUD-015 (round 2): truncation never splits a multi-byte character.
func TestTruncateUTF8_NeverSplitsRunes(t *testing.T) {
	s := "日本語テキスト"
	for limit := 1; limit <= len(s); limit++ {
		got := truncateUTF8(s, limit)
		if !utf8.ValidString(got) {
			t.Fatalf("limit %d produced invalid UTF-8: %q", limit, got)
		}
	}
	if got := truncateUTF8("short", 100); got != "short" {
		t.Fatalf("under-limit string must pass through, got %q", got)
	}
}

// AUD-015: unserializable properties are rejected at admission rather
// than silently stored as {}.
func TestPrepareEvent_RejectsUnserializableProperties(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/events", nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh) TestAgent/1.0")
	var ctx context.Context
	RequestInfoMiddleware(ParseTrustedProxies(""))(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		ctx = r.Context()
	})).ServeHTTP(httptest.NewRecorder(), req)
	_, err := prepareEvent(ctx, IngestInput{
		SiteID:     "s1",
		Properties: map[string]any{"chan": make(chan int)},
	}, "salt", nil)
	if err == nil {
		t.Fatal("unserializable properties must be rejected")
	}
	var appErr *neutron.AppError
	if !errors.As(err, &appErr) || appErr.Status != http.StatusBadRequest {
		t.Fatalf("expected 400, got %v", err)
	}
}
