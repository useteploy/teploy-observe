package auth

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"
)

// connect is the shared "skip if nucleus down" boilerplate for auth DB tests.
func connect(t *testing.T) (context.Context, *nucleus.Client, func()) {
	t.Helper()
	dsn := os.Getenv("OBSERVE_NUCLEUS_URL")
	if dsn == "" {
		dsn = "postgres://postgres@localhost:5432/postgres?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		cancel()
		t.Skipf("nucleus not reachable at %s — skipping", dsn)
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
