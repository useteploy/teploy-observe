package tracing

import (
	"testing"
	"time"
)

func newCacheOnlyQueryService() *QueryService {
	return &QueryService{servicesCache: map[string]servicesEntry{}}
}

func summary(name string) []ServiceSummary {
	return []ServiceSummary{{ServiceName: name, RequestCount: 1}}
}

// TestServicesCache_DistinctWindowsDoNotCollide pins the bug the bounds-keying
// fixed: the key used to be site + window *length*, so any 24h historical range
// primed the entry the rolling 24h live view then read, and the Traces page
// served January's rollup as if it were the last day's.
func TestServicesCache_DistinctWindowsDoNotCollide(t *testing.T) {
	q := newCacheOnlyQueryService()

	histFrom := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	histTo := histFrom.Add(24 * time.Hour)
	q.storeServices(servicesKey("site1", histFrom, histTo), summary("january"))

	liveTo := time.Now().UTC()
	liveFrom := liveTo.Add(-24 * time.Hour)
	if got, ok := q.cachedServices(servicesKey("site1", liveFrom, liveTo)); ok {
		t.Fatalf("live 24h window served the historical entry: %+v", got)
	}
}

// TestServicesCache_SameLengthDifferentBoundsDoNotCollide is the general form:
// equal-length windows anywhere on the timeline must stay separate.
func TestServicesCache_SameLengthDifferentBoundsDoNotCollide(t *testing.T) {
	q := newCacheOnlyQueryService()

	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	const window = time.Hour
	for i, off := range []time.Duration{0, window, 24 * time.Hour, 90 * 24 * time.Hour} {
		from := base.Add(off)
		q.storeServices(servicesKey("site1", from, from.Add(window)), summary(from.String()))

		// Every earlier window must still be readable under its own key, and
		// none of them may answer for any of the others.
		for j, prev := range []time.Duration{0, window, 24 * time.Hour, 90 * 24 * time.Hour} {
			if j > i {
				break
			}
			pf := base.Add(prev)
			got, ok := q.cachedServices(servicesKey("site1", pf, pf.Add(window)))
			if !ok {
				t.Fatalf("window at +%v evicted by a different window", prev)
			}
			if got[0].ServiceName != pf.String() {
				t.Fatalf("window at +%v served %q", prev, got[0].ServiceName)
			}
		}
	}
}

// TestServicesCache_SiteScoped keeps the original per-site isolation.
func TestServicesCache_SiteScoped(t *testing.T) {
	q := newCacheOnlyQueryService()
	from := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)

	q.storeServices(servicesKey("site1", from, to), summary("one"))
	if _, ok := q.cachedServices(servicesKey("site2", from, to)); ok {
		t.Fatal("site2 read site1's rollup")
	}
}

// TestServicesCache_RollingWindowStillHits is the other half of the fix: the
// dashboard's bounds move on every request, so if the key were the raw bounds
// the cache would never hit and the expensive RED scan would run every time.
// Requests inside the same TTL bucket must share an entry.
func TestServicesCache_RollingWindowStillHits(t *testing.T) {
	q := newCacheOnlyQueryService()

	// Two rolling-24h requests a second apart, placed mid-bucket so they fall
	// on the same side of a truncation boundary — the common case for the live
	// view, where the alternative is a guaranteed miss on every request.
	to1 := time.Now().UTC().Truncate(servicesCacheTTL).Add(2 * time.Second)
	to2 := to1.Add(time.Second)

	q.storeServices(servicesKey("site1", to1.Add(-24*time.Hour), to1), summary("live"))
	got, ok := q.cachedServices(servicesKey("site1", to2.Add(-24*time.Hour), to2))
	if !ok {
		t.Fatal("second rolling request missed; the cache is now useless for the live view")
	}
	if got[0].ServiceName != "live" {
		t.Fatalf("got %q, want \"live\"", got[0].ServiceName)
	}
}

// TestServicesCache_ExpiresAndSweeps checks the TTL still governs freshness and
// that stale entries are dropped, so bounds-keying cannot grow the map forever.
func TestServicesCache_ExpiresAndSweeps(t *testing.T) {
	q := newCacheOnlyQueryService()
	from := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	stale := servicesKey("site1", from, from.Add(time.Hour))

	q.storeServices(stale, summary("old"))
	q.servicesCache[stale] = servicesEntry{at: time.Now().Add(-2 * servicesCacheTTL), result: summary("old")}

	if _, ok := q.cachedServices(stale); ok {
		t.Fatal("entry older than the TTL was served")
	}

	fresh := from.Add(48 * time.Hour)
	q.storeServices(servicesKey("site1", fresh, fresh.Add(time.Hour)), summary("new"))
	if _, ok := q.servicesCache[stale]; ok {
		t.Fatal("expired entry survived the sweep")
	}
	if len(q.servicesCache) != 1 {
		t.Fatalf("cache holds %d entries, want 1", len(q.servicesCache))
	}
}
