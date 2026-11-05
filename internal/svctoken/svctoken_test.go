package svctoken

import (
	"context"
	"errors"
	"strings"
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
