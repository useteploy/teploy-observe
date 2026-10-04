// Package authguard wraps a telemetry-key validator with the pre-auth cost
// controls the Sentry-compatible and OTLP/gRPC receivers need: a short
// negative cache and a per-IP failed-attempt limiter.
//
// Why it exists: auth.AuthService.ValidateAPIKey runs two Nucleus queries per
// call and has no negative cache and no pre-auth limiter, so a client that
// sends random keys reaches the database at wire speed. Wrapping lives here
// (not in internal/auth) so the receivers that expose the key to anonymous
// callers carry the guard without touching the shared auth service.
//
// Properties:
//   - Successes are never cached: a valid key is validated against the store
//     every time, exactly as before (revocation takes effect immediately).
//   - A failure is remembered for NegativeTTL keyed by sha256(key), so the
//     same bad key costs one lookup per TTL. The cache is bounded.
//   - ErrAuthUnavailable (store outage) is neither cached nor counted: a
//     backend outage must not lock out good keys.
//   - Failures are counted per client IP in one-minute windows; past the
//     budget the IP is refused with ErrTooManyAttempts WITHOUT a lookup.
package authguard

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"time"

	"github.com/useteploy/teploy-observe/internal/auth"
)

// ErrTooManyAttempts is returned (without consulting the store) when a client
// IP has exhausted its failed-attempt budget for the current window.
var ErrTooManyAttempts = errors.New("authguard: too many failed authentication attempts")

// Defaults.
const (
	DefaultNegativeTTL       = 30 * time.Second
	DefaultNegativeMax       = 10000
	DefaultFailuresPerMinute = 60
	DefaultTrackedIPs        = 10000
	window                   = time.Minute
)

// Validator is the key check being wrapped (*auth.AuthService).
type Validator interface {
	ValidateAPIKey(ctx context.Context, key string) (auth.ValidatedKey, error)
}

// Config tunes a Guard; zero values take the defaults.
type Config struct {
	NegativeTTL       time.Duration
	NegativeMax       int
	FailuresPerMinute int // < 0 disables the IP limiter
	TrackedIPs        int
	Now               func() time.Time
}

type negEntry struct {
	err     error
	expires time.Time
}

type ipEntry struct {
	start time.Time
	fails int
}

// Stats are monotonic counters for /healthz.
type Stats struct {
	NegativeHits int64 `json:"negative_cache_hits"`
	IPBlocked    int64 `json:"ip_blocked"`
	StoreLookups int64 `json:"store_lookups"`
}

// Guard is a Validator with a negative cache and a per-IP failure limiter.
type Guard struct {
	inner Validator
	cfg   Config

	mu  sync.Mutex
	neg map[[32]byte]negEntry
	ips map[string]*ipEntry

	stats Stats
}

// New wraps inner.
func New(inner Validator, cfg Config) *Guard {
	if cfg.NegativeTTL <= 0 {
		cfg.NegativeTTL = DefaultNegativeTTL
	}
	if cfg.NegativeMax <= 0 {
		cfg.NegativeMax = DefaultNegativeMax
	}
	if cfg.FailuresPerMinute == 0 {
		cfg.FailuresPerMinute = DefaultFailuresPerMinute
	}
	if cfg.TrackedIPs <= 0 {
		cfg.TrackedIPs = DefaultTrackedIPs
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Guard{inner: inner, cfg: cfg, neg: map[[32]byte]negEntry{}, ips: map[string]*ipEntry{}}
}

// Stats returns a snapshot of the counters.
func (g *Guard) Stats() Stats {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.stats
}

// ValidateAPIKey satisfies Validator for callers with no client IP; only the
// negative cache applies.
func (g *Guard) ValidateAPIKey(ctx context.Context, key string) (auth.ValidatedKey, error) {
	return g.Validate(ctx, key, "")
}

// Validate checks key on behalf of clientIP ("" = unknown: no IP limiting).
func (g *Guard) Validate(ctx context.Context, key, clientIP string) (auth.ValidatedKey, error) {
	if g == nil {
		return auth.ValidatedKey{}, errors.New("authguard: nil guard")
	}
	now := g.cfg.Now()
	kh := sha256.Sum256([]byte(key))

	g.mu.Lock()
	if g.ipBlockedLocked(clientIP, now) {
		g.stats.IPBlocked++
		g.mu.Unlock()
		return auth.ValidatedKey{}, ErrTooManyAttempts
	}
	if e, ok := g.neg[kh]; ok {
		if now.Before(e.expires) {
			g.stats.NegativeHits++
			g.recordFailLocked(clientIP, now)
			g.mu.Unlock()
			return auth.ValidatedKey{}, e.err
		}
		delete(g.neg, kh)
	}
	g.stats.StoreLookups++
	g.mu.Unlock()

	v, err := g.inner.ValidateAPIKey(ctx, key)
	if err == nil {
		return v, nil
	}
	if errors.Is(err, auth.ErrAuthUnavailable) || ctx.Err() != nil {
		return auth.ValidatedKey{}, err
	}
	g.mu.Lock()
	if len(g.neg) >= g.cfg.NegativeMax {
		g.evictNegLocked(now)
	}
	g.neg[kh] = negEntry{err: err, expires: now.Add(g.cfg.NegativeTTL)}
	g.recordFailLocked(clientIP, now)
	g.mu.Unlock()
	return auth.ValidatedKey{}, err
}

func (g *Guard) evictNegLocked(now time.Time) {
	for k, e := range g.neg {
		if !now.Before(e.expires) {
			delete(g.neg, k)
		}
	}
	if len(g.neg) >= g.cfg.NegativeMax {
		// Everything is still live: drop an arbitrary half rather than grow.
		n := 0
		for k := range g.neg {
			delete(g.neg, k)
			if n++; n >= g.cfg.NegativeMax/2 {
				break
			}
		}
	}
}

func (g *Guard) ipBlockedLocked(ip string, now time.Time) bool {
	if ip == "" || g.cfg.FailuresPerMinute < 0 {
		return false
	}
	e := g.ips[ip]
	if e == nil || now.Sub(e.start) >= window {
		return false
	}
	return e.fails >= g.cfg.FailuresPerMinute
}

func (g *Guard) recordFailLocked(ip string, now time.Time) {
	if ip == "" || g.cfg.FailuresPerMinute < 0 {
		return
	}
	e := g.ips[ip]
	if e == nil || now.Sub(e.start) >= window {
		if e == nil && len(g.ips) >= g.cfg.TrackedIPs {
			for k, v := range g.ips {
				if now.Sub(v.start) >= window {
					delete(g.ips, k)
				}
			}
			if len(g.ips) >= g.cfg.TrackedIPs {
				// Table full of live offenders: forget everything rather
				// than let an address sweep grow memory.
				g.ips = map[string]*ipEntry{}
			}
		}
		e = &ipEntry{start: now}
		g.ips[ip] = e
	}
	e.fails++
}
