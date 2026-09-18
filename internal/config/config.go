package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Addr string
	// IngestAddr, when set, starts a second listener that serves ONLY the
	// telemetry-write endpoints (see cmd/observe/ingest_listener.go). It is
	// the port you publish to the internet — dashboard, read API and admin
	// stay on Addr, which can then stay on localhost or a tailnet. Empty
	// (default) keeps the single-listener behaviour: everything on Addr.
	IngestAddr string
	// PublicURL is the externally reachable base URL of this instance
	// ("https://observe.example.com"), used for SSO metadata and generated
	// links. Empty falls back to the request's Host header, which a client
	// can spoof — set this whenever the instance is reachable by a name.
	PublicURL   string
	NucleusURL  string
	SiteID      string
	SessionSalt string

	// Ingestion buffer
	BufferSize    int
	FlushInterval time.Duration
	FlushSize     int

	// Retention
	RawRetentionDays    int
	HourlyRetentionDays int

	// Rate limiting
	RateLimit int
	// TrustedProxies is a comma-separated list of CIDRs/IPs whose
	// X-Forwarded-For / X-Real-Ip headers are trusted for client-IP
	// extraction. Empty (default) means trust none — use the peer address —
	// so a client can't spoof its IP to evade per-IP rate limiting.
	TrustedProxies string

	// Auth
	JWTSecret     string
	AuditKey      string
	AdminUser     string
	AdminPassword string

	// DemoMode locks the deployment to a read-only public demo state.
	// Writes on /api/v1/* (except auth/login and ingest) return 403.
	DemoMode bool

	// parseErr records malformed numeric env values so Validate can fail
	// loudly at startup instead of Load silently swapping in a default
	// that masks the operator's typo (audit F24).
	parseErr error
}

func Load() Config {
	c := Config{
		Addr:           envOr("OBSERVE_ADDR", ":3000"),
		IngestAddr:     envOr("OBSERVE_INGEST_ADDR", ""),
		PublicURL:      strings.TrimRight(envOr("OBSERVE_PUBLIC_URL", ""), "/"),
		NucleusURL:     envOr("OBSERVE_NUCLEUS_URL", "postgres://localhost:5432/observe"),
		SiteID:         envOr("OBSERVE_SITE_ID", "default"),
		SessionSalt:    envOr("OBSERVE_SESSION_SALT", ""),
		RateLimit:      1000,
		TrustedProxies: envOr("OBSERVE_TRUSTED_PROXIES", ""),
		JWTSecret:      envOr("OBSERVE_JWT_SECRET", ""),
		AuditKey:       envOr("OBSERVE_AUDIT_KEY", ""),
		AdminUser:      envOr("OBSERVE_ADMIN_USER", "admin"),
		AdminPassword:  envOr("OBSERVE_ADMIN_PASSWORD", ""),
		DemoMode:       envOr("OBSERVE_DEMO_MODE", "") == "true",
	}
	var err error
	set := func(key string, def int, dst *int) {
		if err != nil {
			return
		}
		var n int
		n, err = envIntStrict(key, def)
		if err == nil {
			*dst = n
		}
	}
	set("OBSERVE_BUFFER_SIZE", 100_000, &c.BufferSize)
	set("OBSERVE_FLUSH_SIZE", 500, &c.FlushSize)
	set("OBSERVE_RAW_RETENTION_DAYS", 30, &c.RawRetentionDays)
	set("OBSERVE_HOURLY_RETENTION_DAYS", 365, &c.HourlyRetentionDays)
	set("OBSERVE_RATE_LIMIT", 1000, &c.RateLimit)
	flushMs := 2000
	set("OBSERVE_FLUSH_INTERVAL_MS", 2000, &flushMs)
	if err == nil && (flushMs < 1 || flushMs > 60000) {
		// AUD-009: bound the raw milliseconds BEFORE multiplying into a
		// duration — a huge value can overflow int64 during multiplication
		// and wrap into something the post-hoc range check misreads.
		err = fmt.Errorf("OBSERVE_FLUSH_INTERVAL_MS must be in [1, 60000] milliseconds, got %d", flushMs)
	}
	if err == nil {
		c.FlushInterval = time.Duration(flushMs) * time.Millisecond
	} else {
		c.FlushInterval = 2000 * time.Millisecond
	}
	c.parseErr = err
	return c
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// envIntStrict parses an integer env var, failing on malformed values
// instead of silently substituting the default (audit F24: a typo'd value
// used to become a different, working-looking configuration).
func envIntStrict(key string, fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer, got %q", key, raw)
	}
	return n, nil
}

// Validate rejects configurations that would panic at construction or
// silently disable ingestion (audit F24). Run before any service starts.
func (c Config) Validate() error {
	if c.parseErr != nil {
		return c.parseErr
	}
	if c.BufferSize < 1 {
		return fmt.Errorf("OBSERVE_BUFFER_SIZE must be >= 1, got %d", c.BufferSize)
	}
	if c.FlushSize < 1 || c.FlushSize > c.BufferSize {
		return fmt.Errorf("OBSERVE_FLUSH_SIZE must be in [1, OBSERVE_BUFFER_SIZE=%d], got %d", c.BufferSize, c.FlushSize)
	}
	// Non-positive intervals panic time.NewTicker; unboundedly large ones
	// are certainly a unit mistake (ms vs s).
	if c.FlushInterval <= 0 || c.FlushInterval > time.Minute {
		return fmt.Errorf("OBSERVE_FLUSH_INTERVAL_MS must be in (0, 60000] milliseconds, got %d", c.FlushInterval.Milliseconds())
	}
	if c.RateLimit < 1 {
		return fmt.Errorf("OBSERVE_RATE_LIMIT must be >= 1, got %d", c.RateLimit)
	}
	if c.RawRetentionDays < 1 {
		return fmt.Errorf("OBSERVE_RAW_RETENTION_DAYS must be >= 1, got %d", c.RawRetentionDays)
	}
	if c.HourlyRetentionDays < 1 {
		return fmt.Errorf("OBSERVE_HOURLY_RETENTION_DAYS must be >= 1, got %d", c.HourlyRetentionDays)
	}
	// AUD-009: weak explicitly-configured secrets were accepted silently.
	// Unset still means "generate per process" (logged at startup); a value
	// an operator DID set must carry real entropy. Length floors, not
	// composition rules — these are operator-chosen machine secrets.
	if c.JWTSecret != "" && len(c.JWTSecret) < 16 {
		return fmt.Errorf("OBSERVE_JWT_SECRET must be at least 16 characters when set (unset generates a random per-process secret)")
	}
	if c.AuditKey != "" && len(c.AuditKey) < 32 {
		return fmt.Errorf("OBSERVE_AUDIT_KEY must be at least 32 characters when set (unset falls back with a startup warning)")
	}
	return nil
}
