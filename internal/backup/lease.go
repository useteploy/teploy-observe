package backup

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Snapshot-lease boundary for dumps (F45's upstream half, consumed here).
//
// Nucleus's ACQUIRE SNAPSHOT LEASE pins one database-wide point-in-time
// moment for the SQL domain: while the lease is held, every other session's
// SQL mutation (DML and DDL through the executor's dispatch) waits at the
// engine's writer gate, and the holder transaction's reads keep one logical
// moment across every table.
//
// Scope, verified live against the 2026-09-18 engine build (and logged
// upstream in Teploy/_internal/UPSTREAM_BUGS.md): the gate does NOT cover
// the KV scalar functions — `SELECT KV_SET/SADD/ZADD/DEL(...)` from another
// session commit straight through a held lease, and the holder's KV reads
// are not snapshot-pinned. The KV srcmap section therefore gets its own
// convergence proof (two deep-equal consecutive namespace reads plus a
// closing relist) in kvsrcmap.go; the lease alone would silently mix
// moments there.
//
// The statement and SHOW/RELEASE are issued as raw SQL rather than through
// the SDK for the same reason deleteRelease issues KV_KEYS directly: this
// repo vendors its dependencies, and the vendored SDK predates the
// snapshot-lease surface. Re-vendoring currently pulls unrelated upstream
// drift that breaks the build.
//
// pgxRunner is what a dump reads through — the pool (historical behavior,
// independent per-statement moments) or one lease-holding transaction (all
// reads at one moment). Both *pgxpool.Pool and pgx.Tx satisfy it.

// pgxRunner is the subset of pgx pool/tx behavior the dump read path uses.
type pgxRunner interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// LeaseInfo is recorded in the manifest so an archive states honestly
// whether its tables and KV sections were captured at one leased moment or
// as independent reads (engines predating the lease, e.g. published v0.1.8).
type LeaseInfo struct {
	Held          bool  `json:"held"`
	TimeoutMillis int64 `json:"timeout_ms,omitempty"`
}

// leaseState tracks the holder-side window for the expiry check.
type leaseState struct {
	timeoutMillis int64
	deadline      time.Time
}

// defaultLeaseTimeoutMillis bounds how long a crashed dump process can block
// every writer on the instance. The lease is released at dump end (tx
// rollback); the timeout is only the crash bound — but it also bounds the
// consistency guarantee, so it must comfortably exceed a real dump. Ten
// minutes; override with OBSERVE_BACKUP_LEASE_TIMEOUT_MS.
const defaultLeaseTimeoutMillis = 600_000

func leaseTimeoutMillis() int64 {
	if raw := strings.TrimSpace(os.Getenv("OBSERVE_BACKUP_LEASE_TIMEOUT_MS")); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n >= 1000 {
			return n
		}
	}
	return defaultLeaseTimeoutMillis
}

// errLeaseHeld reports that another session (another backup, typically)
// holds the database-wide lease. Two concurrent backups must not both
// proceed: the second would silently fall back to leaseless mode.
var errLeaseHeld = fmt.Errorf("a snapshot lease is already held by another session — run one backup at a time")

// acquireDumpLease takes the snapshot lease on tx. Returns nil state (and a
// warning on errLog) when the engine does not support the statement — every
// published engine before the lease (v0.1.8) must still be backable, with
// the archive recording the downgrade. Any other failure is fatal: a backup
// that cannot tell which moment it read must not pretend to be one.
func acquireDumpLease(ctx context.Context, tx pgx.Tx, timeoutMillis int64, errLog io.Writer) (*leaseState, error) {
	_, err := tx.Exec(ctx, "ACQUIRE SNAPSHOT LEASE TIMEOUT "+strconv.FormatInt(timeoutMillis, 10))
	if err == nil {
		return &leaseState{timeoutMillis: timeoutMillis, deadline: time.Now().Add(time.Duration(timeoutMillis) * time.Millisecond)}, nil
	}
	if strings.Contains(err.Error(), "already held by another session") {
		return nil, errLeaseHeld
	}
	if isLeaseUnsupported(err) {
		fmt.Fprintf(errLog, "backup: engine does not support ACQUIRE SNAPSHOT LEASE — dumping without a consistent point-in-time boundary; the manifest records lease.held=false\n")
		return nil, nil
	}
	return nil, fmt.Errorf("acquire snapshot lease: %w", err)
}

// isLeaseUnsupported recognizes the parse/unsupported shapes engines
// predating the statement return for it. v0.1.8 answers "parse error: SQL
// parse error: ... found: ACQUIRE (SQLSTATE 42601)"; a leased engine that
// fails for another reason must not be downgraded to a leaseless dump
// silently.
func isLeaseUnsupported(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unsupported statement") ||
		strings.Contains(msg, "syntax error") ||
		strings.Contains(msg, "parse error") ||
		strings.Contains(msg, "parser error") ||
		strings.Contains(msg, "unknown statement") ||
		strings.Contains(msg, "unsupported command") ||
		strings.Contains(msg, "not supported")
}

// checkLeaseWindow verifies the dump's reads all happened inside the lease
// window. The engine releases blocked writers at expiry at the latest; past
// that point this process can no longer claim the archive is one moment, so
// the dump fails instead of shipping a mixed-moment archive that the
// manifest would label lease-consistent. The safety margin covers clock
// skew between this process and the engine.
func checkLeaseWindow(state *leaseState) error {
	if state == nil {
		return nil
	}
	if time.Now().Before(state.deadline.Add(-250 * time.Millisecond)) {
		return nil
	}
	return fmt.Errorf("snapshot lease expired mid-dump (timeout %d ms) — the archive may mix moments; raise OBSERVE_BACKUP_LEASE_TIMEOUT_MS and retry", state.timeoutMillis)
}
