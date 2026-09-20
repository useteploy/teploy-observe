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
	// ValidateAPIKey fail-closes when the key's site has no sites row (audit
	// F09), so the site has to exist for the pre-revoke validation below to
	// legitimately pass.
	if _, err := db.SQL().Exec(ctx,
		`INSERT INTO sites (site_id, tenant_id, domain, name, created_at, session_salt)
		 VALUES ($1, 'default', '', 'revocation test', $2, $3)`,
		site, strconv.FormatInt(time.Now().UnixMilli(), 10), site+"-salt",
	); err != nil {
		t.Fatalf("plant site: %v", err)
	}
	plaintext, info, err := svc.CreateAPIKey(ctx, site, "revocation-test", nil)
	if err != nil {
		t.Fatalf("create api key: %v", err)
	}

	// Sanity: the fresh key validates and resolves to its site.
	validated, err := svc.ValidateAPIKey(ctx, plaintext)
	if err != nil || validated.SiteID != site {
		t.Fatalf("validate before revoke: site=%q err=%v (want site=%q nil)", validated.SiteID, err, site)
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

// TestAPIKeyScopes_R07 (round 4): key capabilities separate telemetry ingest
// from source-map publication. A telemetry-only key must validate without
// the publish capability; a publish key must validate with it; the
// normalized-scope gate rejects unknown capabilities.
func TestAPIKeyScopes_R07(t *testing.T) {
	ctx, db, done := connect(t)
	defer done()
	svc := testService(db)

	site := uniqueSite("r07scope")
	if _, err := db.SQL().Exec(ctx,
		`INSERT INTO sites (site_id, tenant_id, domain, name, created_at, session_salt)
		 VALUES ($1, 'default', '', 'scope test', $2, $3)`,
		site, strconv.FormatInt(time.Now().UnixMilli(), 10), site+"-salt",
	); err != nil {
		t.Fatalf("plant site: %v", err)
	}

	tele, _, err := svc.CreateAPIKey(ctx, site, "browser", nil)
	if err != nil {
		t.Fatalf("create telemetry key: %v", err)
	}
	validated, err := svc.ValidateAPIKey(ctx, tele)
	if err != nil {
		t.Fatalf("validate telemetry key: %v", err)
	}
	if !HasScope(validated.Scopes, ScopeTelemetry) {
		t.Fatalf("default key lacks telemetry scope: %q", validated.Scopes)
	}
	if HasScope(validated.Scopes, ScopePublish) {
		t.Fatalf("default key carries publish scope (must be explicit): %q", validated.Scopes)
	}

	pub, info, err := svc.CreateAPIKey(ctx, site, "ci", []string{ScopePublish})
	if err != nil {
		t.Fatalf("create publish key: %v", err)
	}
	if len(info.Scopes) != 1 || info.Scopes[0] != ScopePublish {
		t.Fatalf("publish key scopes: %v", info.Scopes)
	}
	validated, err = svc.ValidateAPIKey(ctx, pub)
	if err != nil {
		t.Fatalf("validate publish key: %v", err)
	}
	if !HasScope(validated.Scopes, ScopePublish) || HasScope(validated.Scopes, ScopeTelemetry) {
		t.Fatalf("publish key scopes wrong: %q", validated.Scopes)
	}

	if _, _, err := svc.CreateAPIKey(ctx, site, "typo", []string{"pubish"}); err == nil {
		t.Fatal("unknown scope accepted")
	}
}

// TestNormalizeScopes_R07: default, dedupe, rejection.
func TestNormalizeScopes_R07(t *testing.T) {
	got, err := NormalizeScopes(nil)
	if err != nil || len(got) != 1 || got[0] != ScopeTelemetry {
		t.Fatalf("default scopes: %v err=%v", got, err)
	}
	got, err = NormalizeScopes([]string{"publish", "publish", "telemetry"})
	if err != nil || len(got) != 2 {
		t.Fatalf("dedupe: %v err=%v", got, err)
	}
	if _, err := NormalizeScopes([]string{"admin"}); err == nil {
		t.Fatal("invented capability accepted")
	}
	if !HasScope("telemetry,publish", ScopePublish) || HasScope("telemetry", ScopePublish) {
		t.Fatal("HasScope broken")
	}
}
