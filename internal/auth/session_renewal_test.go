package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/neutron-build/neutron/go/neutronauth"
)

// mintWithIat signs claims with an explicit iat/exp, the way an older session
// token looks (neutronauth.GenerateToken always stamps iat = now).
func mintWithIat(t *testing.T, secret string, claims neutronauth.Claims, iat time.Time) string {
	t.Helper()
	enc := base64.RawURLEncoding
	owned := neutronauth.Claims{}
	for k, v := range claims {
		owned[k] = v
	}
	owned["iat"] = iat.Unix()
	owned["exp"] = iat.Add(SessionTTL).Unix()
	payload, err := json.Marshal(owned)
	if err != nil {
		t.Fatal(err)
	}
	input := enc.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + enc.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(input))
	return input + "." + enc.EncodeToString(mac.Sum(nil))
}

const testSecret = "test-secret-test-secret-test-secret!"

func TestJWTAuthMiddleware_SlidingRenewal(t *testing.T) {
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

	username := uniqueSite("renew")
	if _, err := svc.EnsureAdmin(ctx, username, "correcthorsebatterystaple"); err != nil {
		t.Fatalf("EnsureAdmin: %v", err)
	}
	fresh, err := svc.Login(ctx, username, "correcthorsebatterystaple")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	user, err := svc.Principals().LocalByUsername(ctx, username)
	if err != nil || user == nil {
		t.Fatalf("fetch seeded principal: %v", err)
	}

	handler := JWTAuthMiddleware(svc)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	do := func(req *http.Request) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	bearer := func(tok string) *http.Request {
		req := httptest.NewRequest("GET", "/api/v1/whatever", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		return req
	}

	// Lifetime is 30 days.
	claims, err := svc.ValidateToken(fresh)
	if err != nil {
		t.Fatalf("ValidateToken: %v", err)
	}
	iat, _ := claimAsInt64(claims["iat"])
	exp, _ := claimAsInt64(claims["exp"])
	if exp-iat != int64(SessionTTL/time.Second) {
		t.Errorf("token lifetime = %ds, want %ds", exp-iat, int64(SessionTTL/time.Second))
	}

	// 1. A fresh token is not renewed.
	rec := do(bearer(fresh))
	if rec.Code != http.StatusOK {
		t.Fatalf("fresh token: got %d", rec.Code)
	}
	if h := rec.Header().Get(RenewedTokenHeader); h != "" {
		t.Errorf("fresh token was renewed")
	}

	// 2. A token issued 25h ago is accepted and renewed with the current
	// role and token_version. Its own role claim is stale on purpose: the
	// renewal reads the principal row, not the presented token.
	old := mintWithIat(t, testSecret, neutronauth.Claims{
		"sub": user.ID, "username": username, "role": RoleViewer, "tv": user.TokenVersion,
	}, time.Now().Add(-25*time.Hour))
	rec = do(bearer(old))
	if rec.Code != http.StatusOK {
		t.Fatalf("old token: got %d: %s", rec.Code, rec.Body.String())
	}
	renewed := rec.Header().Get(RenewedTokenHeader)
	if renewed == "" {
		t.Fatalf("old token was not renewed")
	}
	rc, err := svc.ValidateToken(renewed)
	if err != nil {
		t.Fatalf("renewed token invalid: %v", err)
	}
	rtv, _ := claimAsInt64(rc["tv"])
	riat, _ := claimAsInt64(rc["iat"])
	if rc["sub"] != user.ID || rc["role"] != RoleAdmin || rtv != user.TokenVersion {
		t.Errorf("renewed claims = %v, want sub=%s role=admin tv=%d", rc, user.ID, user.TokenVersion)
	}
	if time.Since(time.Unix(riat, 0)) > time.Minute {
		t.Errorf("renewed iat not current: %d", riat)
	}
	// The renewed token works and is not itself renewed.
	rec = do(bearer(renewed))
	if rec.Code != http.StatusOK || rec.Header().Get(RenewedTokenHeader) != "" {
		t.Errorf("renewed token: code %d, header %q", rec.Code, rec.Header().Get(RenewedTokenHeader))
	}

	// 3. A stream ticket presented with an old iat is never renewed.
	ticket := mintWithIat(t, testSecret, neutronauth.Claims{
		"aud": StreamTicketAudience, "sub": user.ID, "username": username,
		"role": RoleAdmin, "tv": user.TokenVersion, "route": "/api/v1/logs/stream",
	}, time.Now().Add(-25*time.Hour))
	rec = do(httptest.NewRequest("GET", "/api/v1/logs/stream?ticket="+ticket, nil))
	if rec.Header().Get(RenewedTokenHeader) != "" {
		t.Errorf("stream ticket was renewed")
	}

	// 4. Revocation still wins: after a password change the old-iat token
	// is rejected and no renewal is issued.
	if err := svc.ChangePassword(ctx, user.ID, "correcthorsebatterystaple", "new-password-999"); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	rec = do(bearer(old))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("revoked old token: expected 401, got %d", rec.Code)
	}
	if rec.Header().Get(RenewedTokenHeader) != "" {
		t.Errorf("revoked token was renewed")
	}
	rec = do(bearer(renewed))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("revoked renewed token: expected 401, got %d", rec.Code)
	}
}

// API keys go through APIKeyAuthMiddleware, which has no session to slide.
func TestAPIKeyAuthMiddleware_NeverRenews(t *testing.T) {
	ctx, db, done := connect(t)
	defer done()
	svc := testService(db)

	site := uniqueSite("renewkey")
	if _, err := db.SQL().Exec(ctx,
		`INSERT INTO sites (site_id, tenant_id, domain, name, created_at, session_salt)
		 VALUES ($1, 'default', '', 'renew test', $2, $3)`,
		site, strconv.FormatInt(time.Now().UnixMilli(), 10), site+"-salt",
	); err != nil {
		t.Fatalf("plant site: %v", err)
	}
	key, _, err := svc.CreateAPIKey(ctx, site, "renew-test", nil)
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	handler := APIKeyAuthMiddleware(svc)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest("POST", "/api/v1/ingest", nil)
	req.Header.Set("X-API-Key", key)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("api key: got %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get(RenewedTokenHeader) != "" {
		t.Errorf("api key request carried a renewed token")
	}
}
