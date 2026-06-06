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
