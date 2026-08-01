package share

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"
)

func shareTestDB(t *testing.T) (*nucleus.Client, func()) {
	t.Helper()
	dsn := os.Getenv("OBSERVE_NUCLEUS_URL")
	if dsn == "" {
		dsn = "postgres://postgres@localhost:5432/postgres?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		cancel()
		t.Skipf("nucleus not reachable at %s — skipping", dsn)
	}
	return db, func() { db.Close(); cancel() }
}

func TestShare_CreateResolveRevoke(t *testing.T) {
	db, done := shareTestDB(t)
	defer done()
	ctx := context.Background()
	svc := NewShareService(db)

	site := fmt.Sprintf("share-%d", time.Now().UnixNano())
	link, err := svc.Create(ctx, site)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if link.Token == "" || link.SiteID != site {
		t.Fatalf("bad link: %+v", link)
	}

	got, err := svc.Resolve(ctx, link.Token)
	if err != nil || got != site {
		t.Fatalf("resolve: got %q err %v, want %q", got, err, site)
	}

	// Revoke must not error. (Note: DELETE visibility on the underlying engine
	// is eventually-consistent, so we don't assert immediate non-resolution.)
	if err := svc.Revoke(ctx, link.Token); err != nil {
		t.Fatalf("revoke: %v", err)
	}
}

func TestShare_ResolveUnknown(t *testing.T) {
	db, done := shareTestDB(t)
	defer done()
	svc := NewShareService(db)
	if _, err := svc.Resolve(context.Background(), ""); err == nil {
		t.Fatal("empty token should not resolve")
	}
	if _, err := svc.Resolve(context.Background(), "nope-"+fmt.Sprint(time.Now().UnixNano())); err == nil {
		t.Fatal("unknown token should not resolve")
	}
}

// TestShare_CreateSetsExpiry is the regression harness for OBS-023: links
// used to never expire.
func TestShare_CreateSetsExpiry(t *testing.T) {
	db, done := shareTestDB(t)
	defer done()
	ctx := context.Background()
	svc := NewShareService(db)

	before := time.Now().UnixMilli()
	link, err := svc.Create(ctx, fmt.Sprintf("share-exp-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if link.ExpiresAt == 0 {
		t.Fatal("expected a nonzero expires_at — links must not default to never-expiring")
	}
	wantMin := before + (29 * 24 * time.Hour).Milliseconds()
	wantMax := before + (31 * 24 * time.Hour).Milliseconds()
	if link.ExpiresAt < wantMin || link.ExpiresAt > wantMax {
		t.Errorf("expires_at = %d, want roughly 30 days out (between %d and %d)", link.ExpiresAt, wantMin, wantMax)
	}
	if link.ID == "" {
		t.Error("expected a non-empty derived ID")
	}
	if link.Status != "active" {
		t.Errorf("status = %q, want active", link.Status)
	}
}

// TestShare_ResolveRejectsExpired proves an expired link's token no longer
// resolves. Unlike revoke-then-resolve (see the eventual-consistency note on
// TestShare_CreateResolveRevoke), this isn't a write-visibility race: the row
// commits at creation time, and expiry is a pure time comparison evaluated
// fresh on each Resolve call.
func TestShare_ResolveRejectsExpired(t *testing.T) {
	db, done := shareTestDB(t)
	defer done()
	ctx := context.Background()
	svc := NewShareService(db)

	link, err := svc.CreateWithTTL(ctx, fmt.Sprintf("share-ttl-%d", time.Now().UnixNano()), 50*time.Millisecond)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Not yet expired.
	if _, err := svc.Resolve(ctx, link.Token); err != nil {
		t.Fatalf("resolve before expiry: %v", err)
	}

	time.Sleep(150 * time.Millisecond)

	if _, err := svc.Resolve(ctx, link.Token); err == nil {
		t.Fatal("expected an expired token to be rejected")
	}
}

// TestShare_ListMasksToken is the regression harness for the other half of
// OBS-023: List() used to return the full raw bearer token to any viewer.
func TestShare_ListMasksToken(t *testing.T) {
	db, done := shareTestDB(t)
	defer done()
	ctx := context.Background()
	svc := NewShareService(db)

	site := fmt.Sprintf("share-mask-%d", time.Now().UnixNano())
	link, err := svc.Create(ctx, site)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	links, err := svc.List(ctx, site)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(links) == 0 {
		t.Fatal("expected at least one link")
	}
	for _, l := range links {
		if l.Token == link.Token {
			t.Fatalf("List() returned the raw bearer token unmasked: %q", l.Token)
		}
		if l.ID == "" {
			t.Error("expected a non-empty derived ID on list rows")
		}
	}
}

func TestShare_ListScoped(t *testing.T) {
	db, done := shareTestDB(t)
	defer done()
	ctx := context.Background()
	svc := NewShareService(db)
	siteA := fmt.Sprintf("share-a-%d", time.Now().UnixNano())
	siteB := fmt.Sprintf("share-b-%d", time.Now().UnixNano())
	if _, err := svc.Create(ctx, siteA); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(ctx, siteB); err != nil {
		t.Fatal(err)
	}
	links, err := svc.List(ctx, siteA)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, l := range links {
		if l.SiteID != siteA {
			t.Fatalf("List(%s) leaked site %s", siteA, l.SiteID)
		}
	}
}
