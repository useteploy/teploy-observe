package sites

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"testing"

	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
	"github.com/useteploy/teploy-observe/internal/schema"
)

func managedTestService(t *testing.T) *SiteService {
	t.Helper()
	ctx := context.Background()
	db, err := nucleus.Connect(ctx, nucleustest.DSN(t))
	if err != nil {
		t.Skipf("nucleus not reachable: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := schema.Apply(ctx, db); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	return NewSiteService(db)
}

func uniqueRef(t *testing.T) string {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "app:" + hex.EncodeToString(b)
}

func TestEnsureManagedIsIdempotentPerManager(t *testing.T) {
	ctx := context.Background()
	s := managedTestService(t)
	ref := uniqueRef(t)

	first, created, err := s.EnsureManaged(ctx, "dash-a", ref, "", "App")
	if err != nil {
		t.Fatal(err)
	}
	if !created || first.ExternalRef != ref || first.ManagedBy != "dash-a" || first.Domain != "" {
		t.Fatalf("first create = %+v created=%v", first, created)
	}

	again, created, err := s.EnsureManaged(ctx, "dash-a", ref, "ignored.example.com", "Other")
	if err != nil {
		t.Fatal(err)
	}
	if created || again.SiteID != first.SiteID {
		t.Fatalf("same ref must return the same site: %+v created=%v", again, created)
	}
	if again.Name != "App" {
		t.Errorf("an idempotent replay must not rewrite the site, name = %q", again.Name)
	}

	other, created, err := s.EnsureManaged(ctx, "dash-b", ref, "", "App")
	if err != nil {
		t.Fatal(err)
	}
	if !created || other.SiteID == first.SiteID {
		t.Fatal("another manager's identical ref must not reach this site")
	}
}

func TestEnsureManagedConcurrentCallsConverge(t *testing.T) {
	ctx := context.Background()
	s := managedTestService(t)
	ref := uniqueRef(t)
	ids := make([]string, 8)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			site, _, err := s.EnsureManaged(ctx, "dash-race", ref, "", "App")
			if err != nil {
				t.Errorf("ensure: %v", err)
				return
			}
			ids[i] = site.SiteID
		}(i)
	}
	wg.Wait()
	for _, id := range ids[1:] {
		if id != ids[0] {
			t.Fatalf("concurrent creates produced different sites: %v", ids)
		}
	}
}

func TestIsManagedByAndList(t *testing.T) {
	ctx := context.Background()
	s := managedTestService(t)
	ref := uniqueRef(t)
	site, _, err := s.EnsureManaged(ctx, "dash-own", ref, "", "App")
	if err != nil {
		t.Fatal(err)
	}
	plain, err := s.Create(ctx, "plain.example.com", "Plain")
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		site, manager string
		want          bool
	}{
		{site.SiteID, "dash-own", true},
		{site.SiteID, "dash-other", false},
		{site.SiteID, "", false},
		{plain.SiteID, "dash-own", false},
		{plain.SiteID, "", false},
		{"no-such-site", "dash-own", false},
	} {
		got, err := s.IsManagedBy(ctx, tc.site, tc.manager)
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("IsManagedBy(%q, %q) = %v", tc.site, tc.manager, got)
		}
	}

	list, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, l := range list {
		if l.SiteID == site.SiteID {
			seen = l.ExternalRef == ref && l.ManagedBy == "dash-own"
		}
		if l.SiteID == plain.SiteID && (l.ExternalRef != "" || l.ManagedBy != "") {
			t.Error("an operator site must not report a manager")
		}
	}
	if !seen {
		t.Error("List must report the manager and ref of a managed site")
	}
}

// Deleting a managed site frees its ref and retires ownership. The replacement
// is a new site: resurrecting the old id would hand a deleted site's retained
// telemetry to whoever replayed the create.
func TestDeletedManagedSiteIsNotResurrected(t *testing.T) {
	ctx := context.Background()
	s := managedTestService(t)
	ref := uniqueRef(t)
	first, _, err := s.EnsureManaged(ctx, "dash-del", ref, "", "App")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, first.SiteID); err != nil {
		t.Fatal(err)
	}
	if owned, err := s.IsManagedBy(ctx, first.SiteID, "dash-del"); err != nil || owned {
		t.Fatalf("a deleted site must not be owned: owned=%v err=%v", owned, err)
	}
	next, created, err := s.EnsureManaged(ctx, "dash-del", ref, "", "App")
	if err != nil {
		t.Fatal(err)
	}
	if !created || next.SiteID == first.SiteID {
		t.Fatalf("expected a fresh site, got %+v created=%v", next, created)
	}
}

func TestAdoptHandsAnOperatorSiteToAManager(t *testing.T) {
	ctx := context.Background()
	s := managedTestService(t)
	plain, err := s.Create(ctx, "fylun.example.com", "Fylun")
	if err != nil {
		t.Fatal(err)
	}
	manager, ref := "dash-"+uniqueRef(t), uniqueRef(t)

	if owned, _ := s.IsManagedBy(ctx, plain.SiteID, manager); owned {
		t.Fatal("not owned before adoption")
	}
	site, adopted, err := s.Adopt(ctx, plain.SiteID, manager, ref)
	if err != nil {
		t.Fatal(err)
	}
	if !adopted || site.SiteID != plain.SiteID || site.ManagedBy != manager || site.ExternalRef != ref || site.Domain != "fylun.example.com" {
		t.Fatalf("adopted = %v site = %+v", adopted, site)
	}
	if owned, err := s.IsManagedBy(ctx, plain.SiteID, manager); err != nil || !owned {
		t.Fatalf("owned = %v, err = %v", owned, err)
	}
	// The idempotent create now finds this site instead of making another.
	found, created, err := s.EnsureManaged(ctx, manager, ref, "", "Other")
	if err != nil || created || found.SiteID != plain.SiteID {
		t.Fatalf("ensure after adopt: %+v created=%v err=%v", found, created, err)
	}

	again, adopted, err := s.Adopt(ctx, plain.SiteID, manager, ref)
	if err != nil || adopted || again.SiteID != plain.SiteID {
		t.Fatalf("retry: %+v adopted=%v err=%v", again, adopted, err)
	}

	for name, tc := range map[string]struct{ manager, ref string }{
		"another manager":      {"someone-else", ref},
		"same manager new ref": {manager, uniqueRef(t)},
	} {
		if _, _, err := s.Adopt(ctx, plain.SiteID, tc.manager, tc.ref); !errors.Is(err, ErrManagementConflict) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	other, err := s.Create(ctx, "other.example.com", "Other")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Adopt(ctx, other.SiteID, manager, ref); !errors.Is(err, ErrManagementConflict) {
		t.Errorf("one ref, two sites: err = %v", err)
	}
	if _, _, err := s.Adopt(ctx, "no-such-site", manager, uniqueRef(t)); !errors.Is(err, ErrSiteNotFound) {
		t.Errorf("missing site: err = %v", err)
	}
}
