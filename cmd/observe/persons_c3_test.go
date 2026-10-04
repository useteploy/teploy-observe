package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/neutron-build/neutron/go/neutron"

	"github.com/useteploy/teploy-observe/internal/auth"
	"github.com/useteploy/teploy-observe/internal/identity"
	"github.com/useteploy/teploy-observe/internal/ingest"
	"github.com/useteploy/teploy-observe/internal/persons"
)

// testRoleMW stands in for JWTAuthMiddleware: the role comes from a header.
func testRoleMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		role := r.Header.Get("X-Test-Role")
		if role == "" {
			neutron.WriteError(w, r, neutron.ErrUnauthorized("no role"))
			return
		}
		next.ServeHTTP(w, r.WithContext(auth.WithRole(r.Context(), role)))
	})
}

func testRequire(allowed ...string) neutron.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			role := auth.RoleFromContext(r.Context())
			for _, a := range allowed {
				if a == role {
					next.ServeHTTP(w, r)
					return
				}
			}
			neutron.WriteError(w, r, neutron.ErrForbidden("insufficient role"))
		})
	}
}

// testKeyMW stands in for apiKeyMW: the key-bound site comes from a header.
func testKeyMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		site := r.Header.Get("X-Test-Key-Site")
		if site == "" {
			neutron.WriteError(w, r, neutron.ErrUnauthorized("no key"))
			return
		}
		next.ServeHTTP(w, r.WithContext(ingest.WithSiteID(r.Context(), site)))
	})
}

const testSalt = "salt"

func newPersonsTestRouter(m *persons.Memory) http.Handler {
	app := neutron.New()
	r := app.Router()
	RegisterPersonsRoutes(r, PersonsRouteDeps{
		JWT:    testRoleMW,
		Editor: testRequire(auth.RoleAdmin, auth.RoleEditor),
		Admin:  testRequire(auth.RoleAdmin),
		Ingest: r.Group("/api/v1", testKeyMW),
		Svc:    m.Service(), GlobalSalt: testSalt,
		Actor: func(*http.Request) string { return "tester" },
	})
	return r
}

func do(h http.Handler, method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func seedP(m *persons.Memory, site string, keys ...string) {
	for _, k := range keys {
		m.AddPerson(site, persons.Person{DistinctID: k, FirstSeenMs: 1, LastSeenMs: 2, EventCount: 1, SessionCount: 1})
	}
}

func TestPersonsAuthzMatrix(t *testing.T) {
	m := persons.NewMemory()
	seedP(m, "s1", "a", "b", "c", "d")
	h := newPersonsTestRouter(m)

	mergeBody := func(from string) string {
		return `{"site_id":"s1","from_key":"` + from + `","into_key":"d"}`
	}
	eraseBody := `{"site_id":"s1","person_key":"c"}`
	cases := []struct {
		name         string
		method, path string
		body         string
		role         string
		want         int
	}{
		{"read anon", "GET", "/api/v1/persons?site_id=s1", "", "", 401},
		{"read viewer", "GET", "/api/v1/persons?site_id=s1", "", "viewer", 200},
		{"detail viewer", "GET", "/api/v1/persons/a?site_id=s1", "", "viewer", 200},
		{"merge anon", "POST", "/api/v1/persons/merge", mergeBody("a"), "", 401},
		{"merge viewer", "POST", "/api/v1/persons/merge", mergeBody("a"), "viewer", 403},
		{"merge editor", "POST", "/api/v1/persons/merge", mergeBody("a"), "editor", 200},
		{"merge admin", "POST", "/api/v1/persons/merge", mergeBody("b"), "admin", 200},
		{"erase anon", "POST", "/api/v1/persons/erase", eraseBody, "", 401},
		{"erase viewer", "POST", "/api/v1/persons/erase", eraseBody, "viewer", 403},
		{"erase editor", "POST", "/api/v1/persons/erase", eraseBody, "editor", 403},
		{"erase admin", "POST", "/api/v1/persons/erase", eraseBody, "admin", 200},
	}
	for _, c := range cases {
		hdr := map[string]string{}
		if c.role != "" {
			hdr["X-Test-Role"] = c.role
		}
		if got := do(h, c.method, c.path, c.body, hdr).Code; got != c.want {
			t.Errorf("%s: got %d want %d", c.name, got, c.want)
		}
	}
	// A JWT role must not reach the telemetry-key route and vice versa.
	if got := do(h, "POST", "/api/v1/persons/properties", `{"distinct_id":"u","properties":{"a":1}}`, map[string]string{"X-Test-Role": "admin"}).Code; got != 401 {
		t.Errorf("properties without API key: %d", got)
	}
	if got := do(h, "POST", "/api/v1/persons/merge", mergeBody("a"), map[string]string{"X-Test-Key-Site": "s1"}).Code; got != 401 {
		t.Errorf("merge with only an API key: %d", got)
	}
}

func TestPersonsMergeErrorsAndIDOR(t *testing.T) {
	m := persons.NewMemory()
	seedP(m, "s1", "a", "b")
	seedP(m, "s2", "z")
	h := newPersonsTestRouter(m)
	ed := map[string]string{"X-Test-Role": "editor"}

	if got := do(h, "POST", "/api/v1/persons/merge", `{"site_id":"s1","from_key":"a","into_key":"a"}`, ed).Code; got != 400 {
		t.Errorf("self-merge: %d", got)
	}
	if got := do(h, "POST", "/api/v1/persons/merge", `{"site_id":"s1","from_key":"z","into_key":"b"}`, ed).Code; got != 404 {
		t.Errorf("cross-site key: %d", got)
	}
	if got := do(h, "POST", "/api/v1/persons/merge", `{"site_id":"s1","from_key":"a","into_key":"b"}`, ed).Code; got != 200 {
		t.Errorf("merge: %d", got)
	}
	if got := do(h, "POST", "/api/v1/persons/merge", `{"site_id":"s1","from_key":"b","into_key":"a"}`, ed).Code; got != 409 {
		t.Errorf("cycle: %d", got)
	}
	if got := do(h, "POST", "/api/v1/persons/merge", `not json`, ed).Code; got != 400 {
		t.Errorf("bad json: %d", got)
	}
	// s2 listing is unaffected by s1's alias.
	w := do(h, "GET", "/api/v1/persons?site_id=s2", "", map[string]string{"X-Test-Role": "viewer"})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"z"`) {
		t.Errorf("s2 list: %d %s", w.Code, w.Body.String())
	}
}

func TestPersonsPropertiesEndpoint(t *testing.T) {
	m := persons.NewMemory()
	h := newPersonsTestRouter(m)
	key := map[string]string{"X-Test-Key-Site": "s1"}
	hashed := identity.HashDistinctID("user-42", testSalt)

	w := do(h, "POST", "/api/v1/persons/properties", `{"distinct_id":"user-42","properties":{"plan":"pro"}}`, key)
	if w.Code != 200 || !strings.Contains(w.Body.String(), hashed) || strings.Contains(w.Body.String(), "user-42") {
		t.Fatalf("hashing/echo: %d %s", w.Code, w.Body.String())
	}
	if raw, found, _ := m.GetProps(context.Background(), "s1", hashed); !found || !strings.Contains(raw, "pro") {
		t.Fatalf("stored under hashed key expected: %q %v", raw, found)
	}
	// Body site disagreeing with the key's site is a cross-tenant write.
	if got := do(h, "POST", "/api/v1/persons/properties", `{"site_id":"s2","distinct_id":"u","properties":{"a":1}}`, key).Code; got != 403 {
		t.Errorf("site mismatch: %d", got)
	}
	if _, found, _ := m.GetProps(context.Background(), "s2", identity.HashDistinctID("u", testSalt)); found {
		t.Error("write landed in another site")
	}
	for name, body := range map[string]string{
		"nested":   `{"distinct_id":"u","properties":{"a":{"b":1}}}`,
		"badkey":   `{"distinct_id":"u","properties":{"a b":1}}`,
		"reserved": `{"distinct_id":"u","properties":{"user_id":"raw"}}`,
		"noid":     `{"properties":{"a":1}}`,
		"big":      `{"distinct_id":"u","properties":{"a":"` + strings.Repeat("x", 2000) + `"}}`,
	} {
		if got := do(h, "POST", "/api/v1/persons/properties", body, key).Code; got != 400 {
			t.Errorf("%s: %d", name, got)
		}
	}
	// Erased persons refuse writes.
	if got := do(h, "POST", "/api/v1/persons/erase", `{"site_id":"s1","person_key":"`+hashed+`"}`, map[string]string{"X-Test-Role": "admin"}).Code; got != 200 {
		t.Fatalf("erase: %d", got)
	}
	if got := do(h, "POST", "/api/v1/persons/properties", `{"distinct_id":"user-42","properties":{"a":1}}`, key).Code; got != 409 {
		t.Errorf("write to erased person: %d", got)
	}
}
