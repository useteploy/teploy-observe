package config

import (
	"os"
	"strconv"
	"time"
)

type Config struct {
	Addr        string
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

	// Auth
	JWTSecret     string
	AdminUser     string
	AdminPassword string
}

func Load() Config {
	c := Config{
		Addr:                envOr("OBSERVE_ADDR", ":3000"),
		NucleusURL:          envOr("OBSERVE_NUCLEUS_URL", "postgres://localhost:5432/observe"),
		SiteID:              envOr("OBSERVE_SITE_ID", "default"),
		SessionSalt:         envOr("OBSERVE_SESSION_SALT", "observe-default-salt"),
		BufferSize:          envInt("OBSERVE_BUFFER_SIZE", 100_000),
		FlushInterval:       time.Duration(envInt("OBSERVE_FLUSH_INTERVAL_MS", 2000)) * time.Millisecond,
		FlushSize:           envInt("OBSERVE_FLUSH_SIZE", 500),
		RawRetentionDays:    envInt("OBSERVE_RAW_RETENTION_DAYS", 30),
		HourlyRetentionDays: envInt("OBSERVE_HOURLY_RETENTION_DAYS", 365),
		JWTSecret:           envOr("OBSERVE_JWT_SECRET", ""),
		AdminUser:           envOr("OBSERVE_ADMIN_USER", "admin"),
		AdminPassword:       envOr("OBSERVE_ADMIN_PASSWORD", "observe"),
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
