package config

import (
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
}

func Load() Config {
	c := Config{
		Addr:                envOr("OBSERVE_ADDR", ":3000"),
		IngestAddr:          envOr("OBSERVE_INGEST_ADDR", ""),
		PublicURL:           strings.TrimRight(envOr("OBSERVE_PUBLIC_URL", ""), "/"),
		NucleusURL:          envOr("OBSERVE_NUCLEUS_URL", "postgres://localhost:5432/observe"),
		SiteID:              envOr("OBSERVE_SITE_ID", "default"),
		SessionSalt:         envOr("OBSERVE_SESSION_SALT", ""),
		BufferSize:          envInt("OBSERVE_BUFFER_SIZE", 100_000),
		FlushInterval:       time.Duration(envInt("OBSERVE_FLUSH_INTERVAL_MS", 2000)) * time.Millisecond,
		FlushSize:           envInt("OBSERVE_FLUSH_SIZE", 500),
		RawRetentionDays:    envInt("OBSERVE_RAW_RETENTION_DAYS", 30),
		HourlyRetentionDays: envInt("OBSERVE_HOURLY_RETENTION_DAYS", 365),
		RateLimit:           envInt("OBSERVE_RATE_LIMIT", 1000),
		TrustedProxies:      envOr("OBSERVE_TRUSTED_PROXIES", ""),
		JWTSecret:           envOr("OBSERVE_JWT_SECRET", ""),
		AuditKey:            envOr("OBSERVE_AUDIT_KEY", ""),
		AdminUser:           envOr("OBSERVE_ADMIN_USER", "admin"),
		AdminPassword:       envOr("OBSERVE_ADMIN_PASSWORD", ""),
		DemoMode:            envOr("OBSERVE_DEMO_MODE", "") == "true",
	}
	return c
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}
