package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
	obsschema "github.com/useteploy/teploy-observe/internal/schema"
)

// match is the step that decides whether a presented secret authenticates.
// These two cases are the whole security contract of the store and they run
// without a database, so they run everywhere.
func TestMatchRefusesUnknownAndRevoked(t *testing.T) {
	good := TokenPrefix + "good"
	revoked := TokenPrefix + "revoked"
	rows := []Token{
		{ID: "a", Name: "live", Hash: hashToken(good), Role: RoleViewer},
		{ID: "b", Name: "dead", Hash: hashToken(revoked), Role: RoleEditor, RevokedAt: 1},
	}

	if _, ok := match(rows, TokenPrefix+"nope"); ok {
		t.Error("an unknown token authenticated")
	}
	// The revoked row still hashes correctly — without the Revoked() check it
	// would authenticate, which is the whole point of testing it.
	if hashToken(revoked) != rows[1].Hash {
		t.Fatal("premise broken: the revoked row should still hash correctly")
	}
	if _, ok := match(rows, revoked); ok {
		t.Error("a revoked token authenticated")
	}
	got, ok := match(rows, good)
	if !ok || got.ID != "a" {
		t.Fatalf("a live token failed to authenticate: %+v %v", got, ok)
	}
}

func TestTokenRoleModel(t *testing.T) {
	if !(Token{Role: RoleViewer}).ReadOnly() {
		t.Error("viewer must be read-only")
	}
	if (Token{Role: RoleEditor}).ReadOnly() {
		t.Error("editor must not be read-only")
	}
	// Least privilege: anything unrecognized — including "admin", which is not
	// mintable — normalizes to viewer rather than being honoured or rejected.
	for _, in := range []string{"", "admin", "root", "EDITOR ", "nonsense"} {
		got := normalizeRole(in)
		if in == "EDITOR " {
			if got != RoleEditor {
				t.Errorf("normalizeRole(%q) = %q, want editor", in, got)
			}
			continue
		}
		if got != RoleViewer {
			t.Errorf("normalizeRole(%q) = %q, want viewer", in, got)
		}
	}
}

func testStore(t *testing.T) *TokenStore {
	t.Helper()
	dsn := nucleustest.DSN(t)
	ctx := context.Background()
	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(db.Close)
	if err := obsschema.Apply(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return NewTokenStore(db)
}

func TestTokenLifecycle(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	plain, tok, err := s.Create(ctx, "ci-bot", RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plain, TokenPrefix) {
		t.Fatalf("token missing %s prefix: %s", TokenPrefix, plain)
	}
	if tok.Hash == plain || strings.Contains(tok.Hash, plain) {
		t.Fatal("plaintext must not be stored")
	}
	if tok.Role != RoleViewer || !tok.ReadOnly() {
		t.Fatalf("role not applied: %+v", tok)
	}

	got, ok := s.Verify(ctx, plain)
	if !ok || got.Name != "ci-bot" {
		t.Fatalf("verify failed: %+v %v", got, ok)
	}
	if got.Hash != "" {
		t.Fatal("Verify must not hand back the stored hash")
	}
	if _, ok := s.Verify(ctx, TokenPrefix+"wrong"); ok {
		t.Fatal("wrong token verified")
	}
	// A secret without the prefix is refused before any hashing happens.
	if _, ok := s.Verify(ctx, "not-an-observe-token"); ok {
		t.Fatal("prefixless token verified")
	}

	list, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, rec := range list {
		if rec.ID != tok.ID {
			continue
		}
		found = true
		if rec.Hash != "" {
			t.Error("List leaked the token hash")
		}
		if rec.Name != "ci-bot" || rec.Role != RoleViewer {
			t.Errorf("List returned %+v", rec)
		}
	}
	if !found {
		t.Fatal("created token missing from List")
	}

	if err := s.Revoke(ctx, tok.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Verify(ctx, plain); ok {
		t.Fatal("revoked token still verifies")
	}
	// The row survives revocation so the audit trail can still name the actor.
	list, err = s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found = false
	for _, rec := range list {
		if rec.ID == tok.ID {
			found = true
			if !rec.Revoked() {
				t.Error("revoked token is not marked revoked")
			}
		}
	}
	if !found {
		t.Fatal("revoking a token must not delete its record")
	}

	if err := s.Revoke(ctx, "does-not-exist"); err == nil {
		t.Error("revoking an unknown token should fail")
	}
}

// Every write is an append and the read side collapses with argMax; a revoke
// that did not collapse correctly would resurrect the pre-revoke row.
func TestTokenUpdatesCollapseToTheNewestRow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	plain, tok, err := s.Create(ctx, "collapse-check", RoleEditor)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Verify(ctx, plain); !ok {
		t.Fatal("fresh token does not verify")
	}
	if err := s.Revoke(ctx, tok.ID); err != nil {
		t.Fatal(err)
	}

	list, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, rec := range list {
		if rec.ID == tok.ID {
			seen++
			if !rec.Revoked() {
				t.Error("List surfaced the stale pre-revoke row")
			}
		}
	}
	if seen != 1 {
		t.Fatalf("token appears %d times in List, want exactly 1", seen)
	}
}

func TestCreateRequiresAName(t *testing.T) {
	s := testStore(t)
	if _, _, err := s.Create(context.Background(), "   ", RoleViewer); err == nil {
		t.Fatal("a nameless token should be refused")
	}
}
