package ingest

import (
	"net/http"
	"sync"
	"time"
)

// RateLimiter implements a token bucket rate limiter keyed on a composite
// key. After auth middleware has set site_id on the request context, keys
// are "<site_id>|<ip>"; otherwise the bare IP. One noisy IP on site A
// therefore doesn't starve site B. Per-site caps override the default rate
// via SiteCap.
type RateLimiter struct {
	mu         sync.Mutex
	buckets    map[string]*bucket
	rate       int           // default tokens per interval
	interval   time.Duration // refill interval
	burst      int           // default max bucket size
	siteLimits map[string]int
}

type bucket struct {
	tokens   int
	lastFill time.Time
	cap      int // burst cap for this bucket
	rate     int // refill rate (tokens per interval) for this bucket
}

// NewRateLimiter creates a rate limiter.
// rate: default requests allowed per interval. burst: default max burst size.
func NewRateLimiter(rate int, interval time.Duration, burst int) *RateLimiter {
	rl := &RateLimiter{
		buckets:    make(map[string]*bucket),
		rate:       rate,
		interval:   interval,
		burst:      burst,
		siteLimits: make(map[string]int),
	}
	go func() {
		for range time.Tick(time.Minute) {
			rl.cleanup()
		}
	}()
	return rl
}

// SetSiteCap updates the rate limit for a specific site. 0 reverts to the
// default. Applies to new buckets; existing buckets take the new cap on
// next refill tick.
func (rl *RateLimiter) SetSiteCap(siteID string, ratePerSecond int) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	if ratePerSecond <= 0 {
		delete(rl.siteLimits, siteID)
		return
	}
	rl.siteLimits[siteID] = ratePerSecond
}

// Allow returns true if a request for (siteID, ip) is allowed. Either may be
// empty; at least one must identify the caller.
//
// When a per-site cap is configured (SetSiteCap), the request must pass BOTH a
// per-(site,ip) bucket (fairness — one noisy IP can't starve others) AND a
// site-aggregate bucket (a true ceiling on the site's total rps regardless of
// how many distinct IPs it spreads across). With no site cap set, only the
// composite bucket applies, so default behavior is unchanged.
func (rl *RateLimiter) Allow(siteID, ip string) bool {
	key := siteID
	if ip != "" {
		if key != "" {
			key += "|"
		}
		key += ip
	}
	if key == "" {
		return true // no identifier, can't bucket
	}

	rl.mu.Lock()
	defer rl.mu.Unlock()

	rate := rl.rate
	cap := rl.burst
	siteOverride, hasSiteCap := 0, false
	if siteID != "" {
		if override, ok := rl.siteLimits[siteID]; ok {
			// Site-specific override replaces both rate and burst; clamping
			// up to the global burst would defeat per-site isolation.
			rate = override
			cap = override * 2
			siteOverride, hasSiteCap = override, true
		}
	}

	// Refill both buckets first, then consume only if both have a token, so a
	// denial on one doesn't burn a token on the other.
	composite := rl.refillLocked(key, rate, cap)
	var aggregate *bucket
	if hasSiteCap {
		aggregate = rl.refillLocked("\x00site:"+siteID, siteOverride, siteOverride*2)
	}

	if composite.tokens <= 0 {
		return false
	}
	if aggregate != nil && aggregate.tokens <= 0 {
		return false
	}
	composite.tokens--
	if aggregate != nil {
		aggregate.tokens--
	}
	return true
}

// refillLocked fetches (or creates) the bucket for key, applies any retuned
// rate/cap, refills tokens for elapsed time, and returns it without consuming a
// token. Caller must hold rl.mu.
func (rl *RateLimiter) refillLocked(key string, rate, cap int) *bucket {
	b, ok := rl.buckets[key]
	if !ok {
		b = &bucket{tokens: cap, lastFill: time.Now(), cap: cap, rate: rate}
		rl.buckets[key] = b
		return b
	}
	if b.rate != rate || b.cap != cap {
		// site cap was retuned since last request; apply now
		b.rate = rate
		b.cap = cap
		if b.tokens > cap {
			b.tokens = cap
		}
	}
	elapsed := time.Since(b.lastFill)
	if refill := int(elapsed/rl.interval) * b.rate; refill > 0 {
		b.tokens += refill
		if b.tokens > b.cap {
			b.tokens = b.cap
		}
		b.lastFill = time.Now()
	}
	return b
}

func (rl *RateLimiter) cleanup() {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	cutoff := time.Now().Add(-5 * time.Minute)
	for key, b := range rl.buckets {
		if b.lastFill.Before(cutoff) {
			delete(rl.buckets, key)
		}
	}
}

// Middleware returns an HTTP middleware that rate limits by (site_id, ip).
// Must be mounted AFTER any auth middleware that populates site_id in the
// context.
func (rl *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := ClientIPFromContext(r.Context())
		if ip == "" {
			ip = r.RemoteAddr
		}
		siteID := SiteIDFromContext(r.Context())
		if !rl.Allow(siteID, ip) {
			http.Error(w, `{"error":"rate limited"}`, http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}
