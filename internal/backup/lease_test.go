package backup

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// F45: the unsupported-lease matcher must recognize what engines predating
// ACQUIRE SNAPSHOT LEASE actually return (v0.1.8: a parser error), and must
// NOT swallow a leased engine's real failures (conflict, runtime refusal).
func TestIsLeaseUnsupported(t *testing.T) {
	unsupported := []string{
		// v0.1.8, captured live 2026-09-18.
		`ERROR: parse error: SQL parse error: sql parser error: Expected: an SQL statement, found: ACQUIRE at Line: 1, Column: 1 (SQLSTATE 42601)`,
		"ERROR: syntax error at or near \"ACQUIRE\"",
		"unsupported statement type",
	}
	for _, msg := range unsupported {
		if !isLeaseUnsupported(errors.New(msg)) {
			t.Errorf("expected %q to be classified unsupported", msg)
		}
	}
	supported := []string{
		"a snapshot lease is already held by another session (4213 ms remaining)",
		"connection refused",
		"ACQUIRE SNAPSHOT LEASE requires an active transaction (BEGIN first)",
	}
	for _, msg := range supported {
		if isLeaseUnsupported(errors.New(msg)) {
			t.Errorf("expected %q to be classified as a real failure, not unsupported", msg)
		}
	}
}

// F45: an expired window must fail the dump rather than label a
// mixed-moment archive as lease-consistent.
func TestCheckLeaseWindowExpiry(t *testing.T) {
	if err := checkLeaseWindow(nil); err != nil {
		t.Fatalf("leaseless dump must not be window-checked: %v", err)
	}
	fresh := &leaseState{timeoutMillis: 60_000, deadline: time.Now().Add(60 * time.Second)}
	if err := checkLeaseWindow(fresh); err != nil {
		t.Fatalf("in-window dump failed: %v", err)
	}
	expired := &leaseState{timeoutMillis: 1_000, deadline: time.Now().Add(-time.Second)}
	err := checkLeaseWindow(expired)
	if err == nil || !strings.Contains(err.Error(), "expired mid-dump") {
		t.Fatalf("expected mid-dump expiry failure, got %v", err)
	}
	// Inside the safety margin counts as expired: the engine's clock is not
	// this process's clock.
	margin := &leaseState{timeoutMillis: 1_000, deadline: time.Now().Add(100 * time.Millisecond)}
	if err := checkLeaseWindow(margin); err == nil {
		t.Fatal("a dump finishing inside the clock-skew margin must fail closed")
	}
}

func TestLeaseTimeoutMillis(t *testing.T) {
	t.Setenv("OBSERVE_BACKUP_LEASE_TIMEOUT_MS", "")
	if got := leaseTimeoutMillis(); got != defaultLeaseTimeoutMillis {
		t.Fatalf("default timeout = %d, want %d", got, defaultLeaseTimeoutMillis)
	}
	t.Setenv("OBSERVE_BACKUP_LEASE_TIMEOUT_MS", "90000")
	if got := leaseTimeoutMillis(); got != 90_000 {
		t.Fatalf("env timeout = %d, want 90000", got)
	}
	// Below the floor: refuse silently-tiny windows that would guarantee
	// mid-dump expiry.
	t.Setenv("OBSERVE_BACKUP_LEASE_TIMEOUT_MS", "10")
	if got := leaseTimeoutMillis(); got != defaultLeaseTimeoutMillis {
		t.Fatalf("sub-floor timeout must fall back to the default, got %d", got)
	}
}
