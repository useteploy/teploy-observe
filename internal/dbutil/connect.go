package dbutil

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/neutron-build/neutron/go/nucleus"
)

// ParsePoolConfig contains legacy parser panics and never propagates parser
// errors that embed operator credentials. Validate before pool goroutines start.
func ParsePoolConfig(dsn string) (cfg *pgxpool.Config, err error) {
	defer func() {
		if recover() != nil {
			cfg = nil
			err = errors.New("invalid Nucleus connection configuration")
		}
	}()
	cfg, err = pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("invalid Nucleus connection configuration")
	}
	if cfg.MaxConns < 1 || cfg.MinConns < 0 || cfg.MinIdleConns < 0 || cfg.MinConns > cfg.MaxConns || cfg.MinIdleConns > cfg.MaxConns || cfg.HealthCheckPeriod <= 0 || cfg.MaxConnLifetime < 0 || cfg.MaxConnIdleTime < 0 || cfg.MaxConnLifetimeJitter < 0 {
		return nil, errors.New("invalid Nucleus pool bounds or duration")
	}
	// The pinned driver does not enforce this policy on non-SCRAM auth paths.
	// Refuse that unsupported guarantee before any credentials cross the wire.
	if cfg.ConnConfig.ChannelBinding == "require" {
		return nil, errors.New("channel_binding=require is unsupported by the pinned Nucleus driver")
	}
	return cfg, nil
}

// Connect uses the validated configuration and keeps credentials out of errors.
func Connect(ctx context.Context, dsn string) (*nucleus.Client, error) {
	cfg, err := ParsePoolConfig(dsn)
	if err != nil {
		return nil, err
	}
	db, err := nucleus.Connect(ctx, "", nucleus.WithPoolConfig(cfg))
	if err != nil {
		return nil, errors.New("Nucleus connection failed; verify address, credentials and engine readiness")
	}
	return db, nil
}
