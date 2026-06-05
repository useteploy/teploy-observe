package ingest

import (
	"testing"
	"time"
)

// With a site cap set, the aggregate ceiling must bound the site's total rate
// across DISTINCT IPs — not just per (site,ip). A long interval keeps refills
// from interfering within the test.
func TestRateLimiter_SiteAggregateCeiling(t *testing.T) {
	rl := NewRateLimiter(1000, time.Hour, 1000)
	rl.SetSiteCap("siteA", 5) // 5 rps aggregate; burst = 2x = 10

	allowed := 0
	// 20 distinct IPs, one request each. Without an aggregate ceiling all 20
	// would pass (each gets its own composite bucket). With it, the site is
	// capped at its aggregate burst (10).
	for i := 0; i < 20; i++ {
		ip := "10.0.0." + itoa(i)
		if rl.Allow("siteA", ip) {
			allowed++
		}
	}
	if allowed > 10 {
		t.Fatalf("aggregate ceiling not enforced: %d allowed across 20 IPs, want <= 10", allowed)
	}
	if allowed == 0 {
		t.Fatal("expected some requests to pass under the burst")
	}
}

// Without a site cap, behavior is unchanged: distinct IPs are independent and
// all pass (per-IP isolation, no aggregate ceiling).
func TestRateLimiter_NoSiteCapIndependentPerIP(t *testing.T) {
	rl := NewRateLimiter(1000, time.Hour, 1000)
	for i := 0; i < 50; i++ {
		if !rl.Allow("siteB", "10.0.1."+itoa(i)) {
			t.Fatalf("request %d from a fresh IP should pass with no site cap", i)
		}
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [3]byte
	n := len(b)
	for i > 0 {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
	}
	return string(b[n:])
}
