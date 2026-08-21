package ingest

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

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
