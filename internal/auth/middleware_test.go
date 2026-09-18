package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/neutron-dev/neutron-go/neutronauth"

	"github.com/useteploy/teploy-observe/internal/ingest"
)

// TestJWTAuthMiddleware_RevokedTokenRejected is the regression for OBS-011: a
// token minted before a password change must stop working immediately, not
// after its 24-hour expiry.
func TestJWTAuthMiddleware_RevokedTokenRejected(t *testing.T) {
	ctx, db, done := connect(t)
	defer done()
	svc := testService(db)

	if _, err := db.SQL().Exec(ctx, "DELETE FROM principals"); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	db.KV().Delete(ctx, bootstrapClaimKey)
	defer func() {
		db.SQL().Exec(ctx, "DELETE FROM principals")
		db.KV().Delete(ctx, bootstrapClaimKey)
	}()

	username := uniqueSite("mwtest")
	if _, err := svc.EnsureAdmin(ctx, username, "correcthorsebatterystaple"); err != nil {
		t.Fatalf("EnsureAdmin: %v", err)
	}

	token, err := svc.Login(ctx, username, "correcthorsebatterystaple")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	user, err := svc.Principals().LocalByUsername(ctx, username)
	if err != nil {
		t.Fatalf("fetch seeded principal: %v", err)
	}

	mw := JWTAuthMiddleware(svc)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// The freshly issued token works.
	req := httptest.NewRequest("GET", "/api/v1/whatever", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("fresh token: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Changing the password bumps token_version — the old token must now be
	// rejected even though it hasn't expired.
	if err := svc.ChangePassword(ctx, user.ID, "correcthorsebatterystaple", "new-password-999"); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}

	req2 := httptest.NewRequest("GET", "/api/v1/whatever", nil)
	req2.Header.Set("Authorization", "Bearer "+token)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Errorf("revoked token: expected 401, got %d: %s", rec2.Code, rec2.Body.String())
	}

	// A freshly minted token (post-change) works again.
	newToken, err := svc.Login(ctx, username, "new-password-999")
	if err != nil {
		t.Fatalf("Login with new password: %v", err)
	}
	req3 := httptest.NewRequest("GET", "/api/v1/whatever", nil)
	req3.Header.Set("Authorization", "Bearer "+newToken)
	rec3 := httptest.NewRecorder()
	handler.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusOK {
		t.Errorf("new token after password change: expected 200, got %d: %s", rec3.Code, rec3.Body.String())
	}
}

// TestJWTAuthMiddleware_StreamTicketContract is the AUD-008 regression:
// normal access JWTs are rejected in query strings everywhere; short-lived
// stream tickets are accepted ONLY as ?ticket= on the route prefix they
// were minted for; tickets do not work as general bearer tokens.
func TestJWTAuthMiddleware_StreamTicketContract(t *testing.T) {
	ctx, db, done := connect(t)
	defer done()
	svc := testService(db)

	if _, err := db.SQL().Exec(ctx, "DELETE FROM principals"); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	db.KV().Delete(ctx, bootstrapClaimKey)
	defer func() {
		db.SQL().Exec(ctx, "DELETE FROM principals")
		db.KV().Delete(ctx, bootstrapClaimKey)
	}()

	username := uniqueSite("mwtest")
	if _, err := svc.EnsureAdmin(ctx, username, "correcthorsebatterystaple"); err != nil {
		t.Fatalf("EnsureAdmin: %v", err)
	}
	token, err := svc.Login(ctx, username, "correcthorsebatterystaple")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	user, err := svc.Principals().LocalByUsername(ctx, username)
	if err != nil {
		t.Fatalf("fetch seeded principal: %v", err)
	}
	ticket, err := svc.GenerateStreamTicket(neutronauth.Claims{
		"sub": user.ID, "username": username, "role": RoleAdmin,
	}, "/api/v1/logs/stream", user.TokenVersion)
	if err != nil {
		t.Fatalf("GenerateStreamTicket: %v", err)
	}

	mw := JWTAuthMiddleware(svc)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// 1. A normal access JWT in the query string is REJECTED, even on the
	// allowlisted stream routes - the AUD-008 fix proper.
	req := httptest.NewRequest("GET", "/api/v1/export?token="+token, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("normal JWT in query: expected 401, got %d: %s", rec.Code, rec.Body.String())
	}

	// 2. The same JWT works via the header.
	reqH := httptest.NewRequest("GET", "/api/v1/issues", nil)
	reqH.Header.Set("Authorization", "Bearer "+token)
	recH := httptest.NewRecorder()
	handler.ServeHTTP(recH, reqH)
	if recH.Code != http.StatusOK {
		t.Errorf("header token: expected 200, got %d", recH.Code)
	}

	// 3. A minted ticket works on its bound route, in ?ticket= only.
	reqT := httptest.NewRequest("GET", "/api/v1/logs/stream?site_id=x&ticket="+ticket, nil)
	recT := httptest.NewRecorder()
	handler.ServeHTTP(recT, reqT)
	if recT.Code != http.StatusOK {
		t.Errorf("ticket on bound route: expected 200, got %d: %s", recT.Code, recT.Body.String())
	}
	if got := recT.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("ticket response: expected Referrer-Policy: no-referrer, got %q", got)
	}

	// 4. The same ticket on a DIFFERENT allowlisted route is rejected -
	// audience binding is per route, not per mechanism.
	reqW := httptest.NewRequest("GET", "/api/v1/export?ticket="+ticket, nil)
	recW := httptest.NewRecorder()
	handler.ServeHTTP(recW, reqW)
	if recW.Code != http.StatusUnauthorized {
		t.Errorf("ticket on non-bound route: expected 401, got %d", recW.Code)
	}

	// 5. A ticket presented as an Authorization header is rejected: it is
	// not a general bearer token.
	reqB := httptest.NewRequest("GET", "/api/v1/issues", nil)
	reqB.Header.Set("Authorization", "Bearer "+ticket)
	recB := httptest.NewRecorder()
	handler.ServeHTTP(recB, reqB)
	if recB.Code != http.StatusUnauthorized {
		t.Errorf("ticket as bearer header: expected 401, got %d", recB.Code)
	}

	// 6. Revoking the principal's sessions retires its outstanding tickets
	// too: the ticket embeds token_version and the middleware checks it
	// unconditionally.
	if err := svc.RevokeSessions(ctx, user.ID); err != nil {
		t.Fatalf("RevokeSessions: %v", err)
	}
	reqR := httptest.NewRequest("GET", "/api/v1/logs/stream?ticket="+ticket, nil)
	recR := httptest.NewRecorder()
	handler.ServeHTTP(recR, reqR)
	if recR.Code != http.StatusUnauthorized {
		t.Errorf("ticket after session revocation: expected 401, got %d", recR.Code)
	}
}

// AUD-002 (round 2): the no-API-key grace path is gone. A keyless request
// is rejected before any database lookup, so an instance with zero keys
// refuses telemetry rather than trusting caller-selected sites.
func TestAPIKeyAuthMiddleware_RejectsKeylessRequests(t *testing.T) {
	svc := &AuthService{}
	mw := APIKeyAuthMiddleware(svc)
	called := false
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/events", nil)
	req.Header.Set("X-Observe-Site", "forged-site")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("keyless request must 401, got %d", rec.Code)
	}
	if called {
		t.Fatal("handler must not run for a keyless request")
	}
	if ctx := ingest.SiteIDFromContext(req.Context()); ctx != "" {
		t.Fatal("no site may be bound from caller headers without a validated key")
	}
}
