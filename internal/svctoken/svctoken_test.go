package svctoken

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
	"github.com/useteploy/teploy-observe/internal/schema"
)

func TestNormalizeScopesIsAClosedSet(t *testing.T) {
	for _, bad := range []string{"users:write", "sql:query", "telemetry:read", "sites:delete", "keys:publish", "admin", "*", ""} {
		if _, err := NormalizeScopes([]string{bad}); err == nil {
			t.Errorf("scope %q must be refused", bad)
		}
	}
	if _, err := NormalizeScopes(nil); err == nil {
		t.Error("an empty scope list must be refused: there is no default capability")
	}
	got, err := NormalizeScopes([]string{ScopeKeysMint, ScopeSitesCreate, ScopeKeysMint})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "keys:mint,sites:create" {
		t.Fatalf("scopes = %v, want sorted and deduplicated", got)
	}
}

func TestMatchRefusesRevokedAndUnknown(t *testing.T) {
	live := Token{ID: "a", hash: hashToken("obs_svc_live")}
	revoked := Token{ID: "b", hash: hashToken("obs_svc_gone"), RevokedAt: 1}
	rows := []Token{live, revoked}
	if _, ok := match(rows, "obs_svc_live"); !ok {
		t.Error("live token must match")
	}
	if _, ok := match(rows, "obs_svc_gone"); ok {
		t.Error("revoked token must not match")
	}
	if _, ok := match(rows, "obs_svc_other"); ok {
		t.Error("unknown token must not match")
	}
}

func TestNextUpdatedAtIsStrictlyMonotonic(t *testing.T) {
	if got := nextUpdatedAt(100, 100); got != 101 {
		t.Errorf("same millisecond: got %d", got)
	}
	if got := nextUpdatedAt(100, 50); got != 101 {
		t.Errorf("clock moved back: got %d", got)
	}
	if got := nextUpdatedAt(100, 200); got != 200 {
		t.Errorf("clock advanced: got %d", got)
	}
}

func engineStore(t *testing.T) *Store {
	t.Helper()
	dsn := nucleustest.DSN(t)
	db, err := nucleus.Connect(context.Background(), dsn)
	if err != nil {
		t.Skipf("nucleus not reachable: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := schema.Apply(context.Background(), db); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	return NewStore(db)
}

func TestStoreLifecycleAgainstEngine(t *testing.T) {
	ctx := context.Background()
	s := engineStore(t)

	plaintext, tok, err := s.Create(ctx, "dash-lifecycle", "", "root", []string{ScopeSitesCreate})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plaintext, Prefix) {
		t.Fatalf("token %q lacks the prefix", plaintext)
	}
	if tok.ManagedBy != "dash-lifecycle" {
		t.Fatalf("managed_by = %q, want the name as default", tok.ManagedBy)
	}

	got, err := s.Verify(ctx, plaintext)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.ID != tok.ID || !got.Has(ScopeSitesCreate) || got.Has(ScopeKeysMint) || got.CreatedBy != "root" {
		t.Fatalf("verified token = %+v", got)
	}
	if got.LastUsedAt == 0 {
		t.Error("verify must stamp last_used_at")
	}

	if _, err := s.Verify(ctx, Prefix+"not-a-real-secret"); !errors.Is(err, ErrInvalid) {
		t.Errorf("unknown token: err = %v", err)
	}
	wellFormed := Prefix + strings.Repeat("A", secretEncodedLen)
	if !ValidFormat(wellFormed) || !ValidFormat(plaintext) {
		t.Fatal("ValidFormat must accept the shape Create mints")
	}
	if _, err := s.Verify(ctx, wellFormed); !errors.Is(err, ErrInvalid) {
		t.Errorf("well-formed unknown token: err = %v", err)
	}
	if _, err := s.Verify(ctx, "obs_0123"); !errors.Is(err, ErrInvalid) {
		t.Errorf("telemetry key shape: err = %v", err)
	}

	// Nothing stored or listed carries the plaintext or the hash on the wire.
	list, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, l := range list {
		found = found || l.ID == tok.ID
	}
	if !found {
		t.Fatal("created token is not listed")
	}

	if err := s.Revoke(ctx, tok.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := s.Verify(ctx, plaintext); !errors.Is(err, ErrInvalid) {
		t.Fatalf("revoked token must stop verifying: err = %v", err)
	}
	if err := s.Revoke(ctx, tok.ID); err != nil {
		t.Errorf("revoking twice must be idempotent: %v", err)
	}
	if err := s.Revoke(ctx, "no-such-token"); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoking nothing: err = %v", err)
	}
}

func TestCreateRefusesMissingAdminAndBadInput(t *testing.T) {
	ctx := context.Background()
	s := engineStore(t)
	if _, _, err := s.Create(ctx, "x", "", "", []string{ScopeSitesCreate}); err == nil {
		t.Error("a token with no creating admin must be refused")
	}
	if _, _, err := s.Create(ctx, "  ", "", "root", []string{ScopeSitesCreate}); err == nil {
		t.Error("a blank name must be refused")
	}
	if _, _, err := s.Create(ctx, "x", "", "root", []string{"users:write"}); err == nil {
		t.Error("an unknown scope must be refused")
	}
}

func rowCount(t *testing.T, s *Store, id string) int {
	t.Helper()
	rows, err := nucleus.Query[struct {
		UpdatedAt int64 `db:"updated_at"`
	}](context.Background(), s.db.SQL(), "SELECT updated_at FROM service_tokens WHERE token_id = $1", id)
	if err != nil {
		t.Fatal(err)
	}
	return len(rows)
}

// Every last-used write is an append, so without pruning the table grows for as
// long as the token lives. Writing on every verify is the worst case.
func TestLastUsedWritesDoNotGrowTheTable(t *testing.T) {
	ctx := context.Background()
	s := engineStore(t)
	s.usedInterval = 0
	plaintext, tok, err := s.Create(ctx, "bounded", "", "root", []string{ScopeSitesCreate})
	if err != nil {
		t.Fatal(err)
	}
	var last int64
	for i := 0; i < 25; i++ {
		got, err := s.Verify(ctx, plaintext)
		if err != nil {
			t.Fatalf("verify %d: %v", i, err)
		}
		if got.ID != tok.ID || got.Name != "bounded" || got.CreatedBy != "root" || !got.Has(ScopeSitesCreate) {
			t.Fatalf("verify %d lost the record: %+v", i, got)
		}
		last = got.LastUsedAt
	}
	if n := rowCount(t, s, tok.ID); n > 2 {
		t.Fatalf("%d rows after 25 writes, want a small constant", n)
	}
	list, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range list {
		if l.ID == tok.ID && l.LastUsedAt != last {
			t.Fatalf("last_used_at = %d, want %d", l.LastUsedAt, last)
		}
	}

	// The prune must never take a revocation with it, including when a verify that
	// read before the revoke appends after it.
	stale, err := s.Verify(ctx, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke(ctx, tok.ID); err != nil {
		t.Fatal(err)
	}
	s.noteUsed(ctx, stale)
	if _, err := s.Verify(ctx, plaintext); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a stale last-used write undid a revocation: err = %v", err)
	}
}

func TestLastUsedIsThrottledInProcess(t *testing.T) {
	ctx := context.Background()
	s := engineStore(t)
	plaintext, tok, err := s.Create(ctx, "throttled", "", "root", []string{ScopeSitesCreate})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Verify(ctx, plaintext); err != nil {
				t.Errorf("verify: %v", err)
			}
		}()
	}
	wg.Wait()
	if n := rowCount(t, s, tok.ID); n > 2 {
		t.Fatalf("%d rows after one interval's verifies", n)
	}
}

func TestRevokeAll(t *testing.T) {
	ctx := context.Background()
	s := engineStore(t)
	plaintext, _, err := s.Create(ctx, "revoke-all", "", "root", []string{ScopeSitesCreate})
	if err != nil {
		t.Fatal(err)
	}
	n, err := s.RevokeAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n < 1 {
		t.Fatalf("revoked %d", n)
	}
	if _, err := s.Verify(ctx, plaintext); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v", err)
	}
	if n, err := s.RevokeAll(ctx); err != nil || n != 0 {
		t.Fatalf("second run revoked %d, err %v", n, err)
	}
}

func TestCreateSeparatesInputFromStoreFailure(t *testing.T) {
	ctx := context.Background()
	if _, _, err := NewStore(nil).Create(ctx, "", "", "root", []string{ScopeSitesCreate}); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("blank name: err = %v", err)
	}
	s := engineStore(t)
	dsn := nucleustest.DSN(t)
	closed, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	_ = s
	_, _, err = NewStore(closed).Create(ctx, "n", "", "root", []string{ScopeSitesCreate})
	if err == nil || errors.Is(err, ErrInvalidRequest) || !errors.Is(err, ErrUnavailable) {
		t.Errorf("closed store: err = %v, want ErrUnavailable", err)
	}
}
