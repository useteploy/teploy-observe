package config

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestValidateRejectsBrokenNumericConfig is the audit F24 regression: values
// that used to panic at construction (negative/zero sizes and intervals) or
// silently mask a typo (malformed integers) fail validation instead.
func TestValidateRejectsBrokenNumericConfig(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Config)
		want string
	}{
		{"zero buffer", func(c *Config) { c.BufferSize = 0 }, "OBSERVE_BUFFER_SIZE"},
		{"negative buffer", func(c *Config) { c.BufferSize = -5 }, "OBSERVE_BUFFER_SIZE"},
		{"zero flush size", func(c *Config) { c.FlushSize = 0 }, "OBSERVE_FLUSH_SIZE"},
		{"flush above buffer", func(c *Config) { c.FlushSize = 10; c.BufferSize = 5 }, "OBSERVE_FLUSH_SIZE"},
		{"zero interval", func(c *Config) { c.FlushInterval = 0 }, "OBSERVE_FLUSH_INTERVAL_MS"},
		{"negative interval", func(c *Config) { c.FlushInterval = -time.Second }, "OBSERVE_FLUSH_INTERVAL_MS"},
		{"huge interval", func(c *Config) { c.FlushInterval = 10 * time.Minute }, "OBSERVE_FLUSH_INTERVAL_MS"},
		{"zero rate limit", func(c *Config) { c.RateLimit = 0 }, "OBSERVE_RATE_LIMIT"},
		{"zero retention", func(c *Config) { c.RawRetentionDays = 0 }, "OBSERVE_RAW_RETENTION_DAYS"},
		{"malformed integer", func(c *Config) {
			c.parseErr = fmt.Errorf("OBSERVE_BUFFER_SIZE must be an integer, got %q", "big")
		}, "must be an integer"},
	}
	for _, tc := range cases {
		c := Config{
			BufferSize: 1000, FlushSize: 100, FlushInterval: time.Second,
			RateLimit: 100, RawRetentionDays: 30, HourlyRetentionDays: 365,
			ErrorInboxRetentionDays: 14, ReplayBatchesRetentionDays: 14, DerivedOutboxRetentionDays: 7,
		}
		tc.mut(&c)
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Validate = %v, want error mentioning %q", tc.name, err, tc.want)
		}
	}
}

func TestValidateAcceptsDefaults(t *testing.T) {
	c := Config{
		BufferSize: 100_000, FlushSize: 500, FlushInterval: 2 * time.Second,
		RateLimit: 1000, RawRetentionDays: 30, HourlyRetentionDays: 365,
		ErrorInboxRetentionDays: 14, ReplayBatchesRetentionDays: 14, DerivedOutboxRetentionDays: 7,
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("default-shaped config must validate: %v", err)
	}
}
