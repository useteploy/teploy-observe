package auth

import (
	"context"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"
	"github.com/useteploy/teploy-observe/internal/schema"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
)

// schemaOnce applies the migrations once per test binary.
//
// These tests need `admin_users` and `api_keys`, which no test creates — so
// with a database present they all failed on "relation does not exist", and
// without one they all skipped. Either way the security coverage here (token
// revocation, the bootstrap admin race, password change) had never actually
// executed.
var schemaOnce sync.Once
var schemaErr error

// connect is the shared "skip if nucleus down" boilerplate for auth DB tests.
func connect(t *testing.T) (context.Context, *nucleus.Client, func()) {
	t.Helper()
	dsn := nucleustest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		cancel()
		t.Skipf("nucleus not reachable at %s — skipping", dsn)
	}

	// Build the same tables production runs on, rather than an ad-hoc subset
	// invented by whichever test ran first.
	schemaOnce.Do(func() { schemaErr = schema.Apply(ctx, db) })
	if schemaErr != nil {
		db.Close()
		cancel()
		t.Fatalf("apply schema: %v", schemaErr)
	}

	return ctx, db, func() {
		db.Close()
		cancel()
	}
}

func testService(db *nucleus.Client) *AuthService {
	return NewAuthService(db, "test-secret", slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func uniqueSite(prefix string) string {
	return prefix + "_" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

// TestAPIKeyRevocationPersists is the security regression for finding #4. The
// api_keys table is ORDER BY (tenant_id, key_hash); revoking via WHERE key_id (a
// non-ORDER-BY column) silently no-ops on a Nucleus mergetree, so the row stayed
// revoked='false' and the key KEPT AUTHENTICATING — an auth bypass. RevokeAPIKey
// must update by the ORDER-BY columns so the write actually lands.
func TestAPIKeyRevocationPersists(t *testing.T) {
	ctx, db, done := connect(t)
	defer done()
	svc := testService(db)

	site := uniqueSite("revtest")
	plaintext, info, err := svc.CreateAPIKey(ctx, site, "revocation-test")
	if err != nil {
		t.Fatalf("create api key: %v", err)
	}

	// Sanity: the fresh key validates and resolves to its site.
	gotSite, err := svc.ValidateAPIKey(ctx, plaintext)
	if err != nil || gotSite != site {
		t.Fatalf("validate before revoke: site=%q err=%v (want site=%q nil)", gotSite, err, site)
	}

	if err := svc.RevokeAPIKey(ctx, info.KeyID); err != nil {
		t.Fatalf("revoke api key: %v", err)
	}

	// The security-critical assertion: a revoked key MUST NOT authenticate.
	if _, err := svc.ValidateAPIKey(ctx, plaintext); err == nil {
		t.Fatal("revoked API key still authenticates — revocation regressed to a no-op")
	}
}

// TestRevokeUnknownKeyErrors ensures revoking a non-existent key id is a clean
// error, not a silent success (which would mask a caller bug).
func TestRevokeUnknownKeyErrors(t *testing.T) {
	ctx, db, done := connect(t)
	defer done()
	svc := testService(db)
	if err := svc.RevokeAPIKey(ctx, "does-not-exist-"+uniqueSite("x")); err == nil {
		t.Fatal("revoking an unknown key id should error, got nil")
	}
}
