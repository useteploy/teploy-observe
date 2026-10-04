package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/neutron-build/neutron/go/neutron"

	"github.com/useteploy/teploy-observe/internal/auth"
	"github.com/useteploy/teploy-observe/internal/identity"
	"github.com/useteploy/teploy-observe/internal/persons"
)

const propsPath = "/api/v1/persons/properties"

// The telemetry key is public: it must never read stored traits back, from
// the write response or any error path.
func TestPersonsPropertiesNeverReturnsStoredValues(t *testing.T) {
	m := persons.NewMemory()
	hashed := identity.HashDistinctID("victim", testSalt)
	seedP(m, "s1", hashed)
	if err := m.PutProps(context.Background(), "s1", hashed, `{"email":"secret@example.com","plan":"enterprise"}`); err != nil {
		t.Fatal(err)
	}
	h := newPersonsTestRouter(m)
	key := map[string]string{"X-Test-Key-Site": "s1"}

	cases := []string{
		`{"distinct_id":"victim","properties":{"x":1}}`,
		`{"distinct_id":"victim","properties":{}}`,
		`{"distinct_id":"victim","properties":{"email":null}}`,
		`{"distinct_id":"victim","properties":{"a b":1}}`,
		`{"distinct_id":"victim","properties":{"x":1},"replace":true}`,
	}
	for _, body := range cases {
		w := personsDo(h, "POST", propsPath, body, key)
		for _, leak := range []string{"secret@example.com", "enterprise"} {
			if strings.Contains(w.Body.String(), leak) {
				t.Errorf("%s leaked %q: %d %s", body, leak, w.Code, w.Body.String())
			}
		}
	}
	w := personsDo(h, "POST", propsPath, `{"distinct_id":"victim","properties":{"x":1}}`, key)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"keys_written":["x"]`) || strings.Contains(w.Body.String(), `"properties"`) {
		t.Fatalf("response shape: %d %s", w.Code, w.Body.String())
	}
	// The write still merged: the earlier traits survive.
	raw, _, _ := m.GetProps(context.Background(), "s1", hashed)
	if !strings.Contains(raw, "enterprise") || !strings.Contains(raw, `"x":1`) {
		t.Fatalf("merge lost data: %s", raw)
	}
}

func TestPersonsPropertiesRequiresExistingPerson(t *testing.T) {
	m := persons.NewMemory()
	seedP(m, "s2", identity.HashDistinctID("only-in-s2", testSalt))
	h := newPersonsTestRouter(m)
	key := map[string]string{"X-Test-Key-Site": "s1"}
	for _, id := range []string{"never-seen", "only-in-s2"} {
		w := personsDo(h, "POST", propsPath, `{"distinct_id":"`+id+`","properties":{"a":1}}`, key)
		if w.Code != 404 {
			t.Errorf("%s: %d %s", id, w.Code, w.Body.String())
		}
		if _, found, _ := m.GetProps(context.Background(), "s1", identity.HashDistinctID(id, testSalt)); found {
			t.Errorf("%s: row created for unknown person", id)
		}
	}
}

func TestPersonsPropertiesReplaceGating(t *testing.T) {
	m := persons.NewMemory()
	hashed := identity.HashDistinctID("u1", testSalt)
	seedP(m, "s1", hashed)
	_ = m.PutProps(context.Background(), "s1", hashed, `{"keep":"me"}`)
	h := newPersonsTestRouter(m)
	key := map[string]string{"X-Test-Key-Site": "s1"}

	// Telemetry key: replace is refused and nothing is wiped.
	if got := personsDo(h, "POST", propsPath, `{"distinct_id":"u1","properties":{"a":1},"replace":true}`, key).Code; got != 403 {
		t.Errorf("replace via telemetry key: %d", got)
	}
	if raw, _, _ := m.GetProps(context.Background(), "s1", hashed); !strings.Contains(raw, "keep") {
		t.Fatalf("telemetry replace wiped traits: %s", raw)
	}

	body := `{"site_id":"s1","person_key":"` + hashed + `","properties":{"only":"this"}}`
	rep := "/api/v1/persons/properties/replace"
	for _, c := range []struct {
		role string
		want int
	}{{"", 401}, {"viewer", 403}, {"editor", 200}, {"admin", 200}} {
		hdr := map[string]string{}
		if c.role != "" {
			hdr["X-Test-Role"] = c.role
		}
		if got := personsDo(h, "POST", rep, body, hdr).Code; got != c.want {
			t.Errorf("replace role %q: got %d want %d", c.role, got, c.want)
		}
	}
	// The telemetry key alone cannot reach the editor route.
	if got := personsDo(h, "POST", rep, body, key).Code; got != 401 {
		t.Errorf("replace with API key only: %d", got)
	}
	if raw, _, _ := m.GetProps(context.Background(), "s1", hashed); raw != `{"only":"this"}` {
		t.Fatalf("editor replace: %s", raw)
	}
	// Unknown person and cross-site key: 404, no row.
	for _, k := range []string{"nobody", "other-site-key"} {
		b := `{"site_id":"s1","person_key":"` + k + `","properties":{"a":1}}`
		if got := personsDo(h, "POST", rep, b, map[string]string{"X-Test-Role": "editor"}).Code; got != 404 {
			t.Errorf("replace unknown %s: %d", k, got)
		}
	}
}

func TestPersonsPropertiesRateLimitedPerPerson(t *testing.T) {
	m := persons.NewMemory()
	seedP(m, "s1", identity.HashDistinctID("busy", testSalt), identity.HashDistinctID("calm", testSalt))
	app := neutron.New()
	r := app.Router()
	RegisterPersonsRoutes(r, PersonsRouteDeps{
		JWT:    testRoleMW,
		Editor: testRequire(auth.RoleAdmin, auth.RoleEditor),
		Admin:  testRequire(auth.RoleAdmin),
		Ingest: r.Group("/api/v1", testKeyMW),
		Svc:    m.Service(), GlobalSalt: testSalt,
		PropLimit: persons.NewWriteLimiter(3, time.Minute),
	})
	key := map[string]string{"X-Test-Key-Site": "s1"}
	var h http.Handler = r
	for i := 0; i < 3; i++ {
		if got := personsDo(h, "POST", propsPath, `{"distinct_id":"busy","properties":{"a":1}}`, key).Code; got != 200 {
			t.Fatalf("write %d: %d", i, got)
		}
	}
	w := personsDo(h, "POST", propsPath, `{"distinct_id":"busy","properties":{"a":1}}`, key)
	if w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatalf("4th write: %d", w.Code)
	}
	if got := personsDo(h, "POST", propsPath, `{"distinct_id":"calm","properties":{"a":1}}`, key).Code; got != 200 {
		t.Errorf("other person must be unaffected: %d", got)
	}
}

// The merged document stays capped at MaxPropertyKeys across many writes.
func TestPersonsPropertiesTotalKeyCap(t *testing.T) {
	m := persons.NewMemory()
	seedP(m, "s1", identity.HashDistinctID("big", testSalt))
	app := neutron.New()
	r := app.Router()
	RegisterPersonsRoutes(r, PersonsRouteDeps{
		JWT:    testRoleMW,
		Editor: testRequire(auth.RoleAdmin),
		Admin:  testRequire(auth.RoleAdmin),
		Ingest: r.Group("/api/v1", testKeyMW),
		Svc:    m.Service(), GlobalSalt: testSalt,
		PropLimit: persons.NewWriteLimiter(1000, time.Minute),
	})
	var h http.Handler = r
	key := map[string]string{"X-Test-Key-Site": "s1"}
	var last int
	for i := 0; i < persons.MaxPropertyKeys+5; i++ {
		last = personsDo(h, "POST", propsPath, fmt.Sprintf(`{"distinct_id":"big","properties":{"k%d":1}}`, i), key).Code
		if i < persons.MaxPropertyKeys && last != 200 {
			t.Fatalf("write %d: %d", i, last)
		}
	}
	if last != 400 {
		t.Fatalf("write past the cap: %d, want 400", last)
	}
}
