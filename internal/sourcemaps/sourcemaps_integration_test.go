package sourcemaps

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
)

func testDB(t *testing.T) *nucleus.Client {
	t.Helper()
	dsn := nucleustest.DSN(t)
	db, err := nucleus.Connect(context.Background(), dsn)
	if err != nil {
		t.Skipf("nucleus not reachable at %s — skipping integration test", dsn)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestTrackRelease_ConcurrentDistinctReleasesAllSurvive is the regression for
// OBS-025: the old implementation stored releases as one JSON array in a
// single KV value and did a client-side read-modify-write, so concurrent
// uploads for different releases could silently lose one another.
func TestTrackRelease_ConcurrentDistinctReleasesAllSurvive(t *testing.T) {
	db := testDB(t)
	svc := NewSourceMapService(db)
	siteID := "concurrency-test-site"

	const n = 30
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = svc.TrackRelease(context.Background(), siteID, fmt.Sprintf("release-%d", i))
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("TrackRelease(release-%d): %v", i, err)
		}
	}

	releases, err := svc.ListReleases(context.Background(), siteID)
	if err != nil {
		t.Fatalf("ListReleases: %v", err)
	}
	if len(releases) != n {
		t.Fatalf("expected %d releases, got %d: %v", n, len(releases), releases)
	}
}

// TestTrackRelease_DuplicateIsIdempotent covers the "already tracked" path
// that used to be a manual linear scan.
func TestTrackRelease_DuplicateIsIdempotent(t *testing.T) {
	db := testDB(t)
	svc := NewSourceMapService(db)
	siteID := "idempotent-test-site"

	for i := 0; i < 3; i++ {
		if err := svc.TrackRelease(context.Background(), siteID, "v1"); err != nil {
			t.Fatalf("TrackRelease attempt %d: %v", i, err)
		}
	}
	releases, err := svc.ListReleases(context.Background(), siteID)
	if err != nil {
		t.Fatalf("ListReleases: %v", err)
	}
	if len(releases) != 1 || releases[0] != "v1" {
		t.Fatalf("expected exactly [v1], got %v", releases)
	}
}

// TestListReleases_EmptySiteReturnsEmptyNotError covers the legitimate
// "no releases yet" case, distinct from an error.
func TestListReleases_EmptySiteReturnsEmptyNotError(t *testing.T) {
	db := testDB(t)
	svc := NewSourceMapService(db)

	releases, err := svc.ListReleases(context.Background(), "never-uploaded-site")
	if err != nil {
		t.Fatalf("ListReleases: %v", err)
	}
	if len(releases) != 0 {
		t.Fatalf("expected no releases, got %v", releases)
	}
}
