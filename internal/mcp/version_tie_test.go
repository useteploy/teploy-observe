package mcp

import (
	"context"
	"strconv"
	"testing"
	"time"
)

// mcp_tokens is a plain MergeTree whose read collapses by argMax over
// updated_at (the incidents shape). Create and Revoke both stamp updated_at
// from the clock, so a same-millisecond create+revoke ties and the argMax can
// resolve the PRE-revoke row — a revoked token surfacing as live. This is the
// mcp instance of the version-tie defect recorded in AUDIT_OPEN at 70f6eff.

// TestSameMillisecondCreateRevokeStaysRevoked is the tight-loop regression:
// mint and revoke in immediate succession (no sleeps), the live flake's exact
// shape. After the monotonic fix, Revoke stamps max(now, prior updated_at+1)
// and the revoked row always wins the collapse.
func TestSameMillisecondCreateRevokeStaysRevoked(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		plain, tok, err := s.Create(ctx, "tie-bot", RoleEditor)
		if err != nil {
			t.Fatalf("iter %d create: %v", i, err)
		}
		if err := s.Revoke(ctx, tok.ID); err != nil {
			t.Fatalf("iter %d revoke: %v", i, err)
		}
		if _, ok := s.Verify(ctx, plain); ok {
			t.Fatalf("iter %d: a just-revoked token still verifies — the revoke row tied or lost the updated_at collapse", i)
		}
		list, err := s.List(ctx)
		if err != nil {
			t.Fatalf("iter %d list: %v", i, err)
		}
		for _, rec := range list {
			if rec.ID == tok.ID && !rec.Revoked() {
				t.Fatalf("iter %d: List reports a just-revoked token as live — the pre-revoke row won an updated_at tie", i)
			}
		}
	}
}

// TestRevokeBumpsPastAFutureUpdatedAt is the deterministic arm: the prior row
// is seeded directly with a FUTURE updated_at (the same protection covers a
// regressed/skewed clock). Before the fix, Revoke stamped updated_at=now
// below the seed and the token stayed live; after, max(now, prior+1) makes
// the revoked row win.
func TestRevokeBumpsPastAFutureUpdatedAt(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	plain, tok, err := s.Create(ctx, "future-bot", RoleViewer)
	if err != nil {
		t.Fatal(err)
	}

	future := strconv.FormatInt(time.Now().UTC().Add(time.Hour).UnixMilli(), 10)
	if _, err := s.db.SQL().Exec(ctx,
		`INSERT INTO mcp_tokens (token_id, tenant_id, name, hash, role, created_at, last_used_at, revoked_at, updated_at)
		 VALUES ($1, 'default', 'future-bot', $2, 'viewer', $3, 0, 0, $3)`,
		tok.ID, tok.Hash, future); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := s.Revoke(ctx, tok.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, ok := s.Verify(ctx, plain); ok {
		t.Fatal("Revoke wrote at or below the seeded future row — the token still verifies as live")
	}
	list, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range list {
		if rec.ID == tok.ID && !rec.Revoked() {
			t.Fatal("List reports the token live — the revoke lost to the seeded future row")
		}
	}
}
