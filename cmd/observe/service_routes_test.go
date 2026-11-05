package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/neutron-build/neutron/go/neutronauth"
	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/audit"
	"github.com/useteploy/teploy-observe/internal/auth"
	"github.com/useteploy/teploy-observe/internal/ingest"
	"github.com/useteploy/teploy-observe/internal/nucleustest"
	"github.com/useteploy/teploy-observe/internal/platform"
	"github.com/useteploy/teploy-observe/internal/schema"
	"github.com/useteploy/teploy-observe/internal/share"
	"github.com/useteploy/teploy-observe/internal/sites"
	"github.com/useteploy/teploy-observe/internal/svctoken"
)

type captureAudit struct {
	mu     sync.Mutex
	events []audit.AuditEvent
	fail   bool
}

func (c *captureAudit) Record(_ context.Context, ev audit.AuditEvent) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail {
		return errors.New("audit store down")
	}
	c.events = append(c.events, ev)
	return nil
}

func (c *captureAudit) all() []audit.AuditEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]audit.AuditEvent(nil), c.events...)
}

type fakeAlerts struct{}

func (fakeAlerts) ListRules(context.Context, string) ([]platform.AlertRule, error) {
	return nil, nil
}
func (fakeAlerts) ListHistory(context.Context, string, int, int) ([]platform.AlertHistoryEntry, error) {
	return nil, nil
}

type svcFixture struct {
	mux     *http.ServeMux
	tokens  *svctoken.Store
	sites   *sites.SiteService
	authSvc *auth.AuthService
	hooks   *platform.WebhookService
	audit   *captureAudit
	manager string
}

func newSvcFixture(t *testing.T) *svcFixture {
	t.Helper()
	ctx := context.Background()
	db, err := nucleus.Connect(ctx, nucleustest.DSN(t))
	if err != nil {
		t.Skipf("nucleus not reachable: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := schema.Apply(ctx, db); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	f := &svcFixture{
		mux:     http.NewServeMux(),
		tokens:  svctoken.NewStore(db),
		sites:   sites.NewSiteService(db),
		authSvc: auth.NewAuthService(db, strings.Repeat("s", 40), logger),
		hooks:   platform.NewWebhookService(db, logger),
		audit:   &captureAudit{},
		manager: "mgr-" + randHex(t),
	}
	t.Cleanup(f.hooks.Shutdown)
	(&serviceAPI{
		tokens: f.tokens, sites: f.sites, keys: f.authSvc, shares: share.NewShareService(db),
		webhooks: f.hooks, alerts: fakeAlerts{}, audit: f.audit,
	}).register(f.mux)
	return f
}

func randHex(t *testing.T) string {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func (f *svcFixture) token(t *testing.T, scopes ...string) string {
	t.Helper()
	plaintext, _, err := f.tokens.Create(context.Background(), "token-"+randHex(t), f.manager, "root", scopes)
	if err != nil {
		t.Fatal(err)
	}
	return plaintext
}

func (f *svcFixture) do(t *testing.T, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	return rec
}

func (f *svcFixture) ownedSite(t *testing.T) string {
	t.Helper()
	site, _, err := f.sites.EnsureManaged(context.Background(), f.manager, "app:"+randHex(t), "", "App")
	if err != nil {
		t.Fatal(err)
	}
	return site.SiteID
}

type svcRoute struct {
	method, path, scope, body string
	ok                        int
}

func svcRoutes(siteID string) []svcRoute {
	base := "/api/v1/service/sites/" + siteID
	return []svcRoute{
		{http.MethodPost, "/api/v1/service/sites", svctoken.ScopeSitesCreate, `{"external_ref":"scope-route-` + siteID + `"}`, http.StatusCreated},
		{http.MethodPost, base + "/keys", svctoken.ScopeKeysMint, `{"label":"app"}`, http.StatusCreated},
		{http.MethodPost, base + "/share", svctoken.ScopeShareMint, `{}`, http.StatusCreated},
		{http.MethodPost, base + "/webhooks", svctoken.ScopeWebhooksWrite, `{"name":"n","url":"https://example.com/hook"}`, http.StatusCreated},
		{http.MethodDelete, base + "/webhooks/missing", svctoken.ScopeWebhooksWrite, ``, http.StatusNotFound},
		{http.MethodGet, base + "/alerts/rules", svctoken.ScopeAlertsRead, ``, http.StatusOK},
		{http.MethodGet, base + "/alerts/history", svctoken.ScopeAlertsRead, ``, http.StatusOK},
	}
}

var allScopes = []string{
	svctoken.ScopeSitesCreate, svctoken.ScopeKeysMint, svctoken.ScopeShareMint,
	svctoken.ScopeWebhooksWrite, svctoken.ScopeAlertsRead,
}

// Every route, against a token holding each scope alone: only its own scope
// gets through.
func TestEachServiceRouteRequiresExactlyItsScope(t *testing.T) {
	f := newSvcFixture(t)
	site := f.ownedSite(t)
	tokens := map[string]string{}
	for _, s := range allScopes {
		tokens[s] = f.token(t, s)
	}
	for _, rt := range svcRoutes(site) {
		for scope, tok := range tokens {
			rec := f.do(t, rt.method, rt.path, tok, rt.body)
			if scope == rt.scope {
				if rec.Code != rt.ok {
					t.Errorf("%s %s with %s: status %d, want %d (%s)", rt.method, rt.path, scope, rec.Code, rt.ok, rec.Body)
				}
				continue
			}
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s %s with %s: status %d, want 403", rt.method, rt.path, scope, rec.Code)
			}
		}
	}
}

func TestServiceRoutesRefuseBadCredentials(t *testing.T) {
	f := newSvcFixture(t)
	site := f.ownedSite(t)
	plaintext, rec, err := f.tokens.Create(context.Background(), "revoked-"+randHex(t), f.manager, "root", allScopes)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.tokens.Revoke(context.Background(), rec.ID); err != nil {
		t.Fatal(err)
	}
	apiKey, _, err := f.authSvc.CreateAPIKey(context.Background(), site, "ingest", nil)
	if err != nil {
		t.Fatal(err)
	}
	jwt, err := f.authSvc.GenerateToken("u", "root", auth.RoleAdmin, 1)
	if err != nil {
		t.Fatal(err)
	}
	// The right token without the Bearer scheme is not a credential.
	live := f.token(t, svctoken.ScopeSitesCreate)
	noScheme := httptest.NewRequest(http.MethodPost, "/api/v1/service/sites", strings.NewReader(`{"external_ref":"x"}`))
	noScheme.Header.Set("Authorization", live)
	noSchemeRec := httptest.NewRecorder()
	f.mux.ServeHTTP(noSchemeRec, noScheme)
	if noSchemeRec.Code != http.StatusUnauthorized {
		t.Errorf("token with no scheme: status %d, want 401", noSchemeRec.Code)
	}

	for name, cred := range map[string]string{
		"none": "", "revoked": plaintext, "made up": svctoken.Prefix + "nope",
		"telemetry key": apiKey, "admin jwt": jwt,
	} {
		for _, rt := range svcRoutes(site) {
			if got := f.do(t, rt.method, rt.path, cred, rt.body).Code; got != http.StatusUnauthorized {
				t.Errorf("%s on %s %s: status %d, want 401", name, rt.method, rt.path, got)
			}
		}
	}
}

// A site the token's manager does not own looks exactly like no site at all,
// on every site-scoped route, including with every scope held.
func TestServiceRoutesAreLimitedToOwnSites(t *testing.T) {
	f := newSvcFixture(t)
	tok := f.token(t, allScopes...)
	operatorSite, err := f.sites.Create(context.Background(), "operator.example.com", "Operator")
	if err != nil {
		t.Fatal(err)
	}
	foreign, _, err := f.sites.EnsureManaged(context.Background(), "someone-else-"+randHex(t), "app:x", "", "X")
	if err != nil {
		t.Fatal(err)
	}
	for _, site := range []string{operatorSite.SiteID, foreign.SiteID, "default", "no-such-site"} {
		for _, rt := range svcRoutes(site) {
			if rt.path == "/api/v1/service/sites" {
				continue
			}
			rec := f.do(t, rt.method, rt.path, tok, rt.body)
			if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "site not found") {
				t.Errorf("%s %s: status %d (%s), want 404 site not found", rt.method, rt.path, rec.Code, rec.Body)
			}
		}
	}
	keys, err := f.authSvc.ListAPIKeys(context.Background(), operatorSite.SiteID)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 {
		t.Fatalf("a refused mint still created keys: %v", keys)
	}
}

func TestCreateSiteIsIdempotentOverHTTP(t *testing.T) {
	f := newSvcFixture(t)
	tok := f.token(t, svctoken.ScopeSitesCreate)
	body := `{"external_ref":"app:` + randHex(t) + `"}`

	first := f.do(t, http.MethodPost, "/api/v1/service/sites", tok, body)
	if first.Code != http.StatusCreated {
		t.Fatalf("first: %d %s", first.Code, first.Body)
	}
	second := f.do(t, http.MethodPost, "/api/v1/service/sites", tok, body)
	if second.Code != http.StatusOK {
		t.Fatalf("second: %d %s", second.Code, second.Body)
	}
	var a, b struct {
		Site struct {
			SiteID      string `json:"site_id"`
			Domain      string `json:"domain"`
			ManagedBy   string `json:"managed_by"`
			ExternalRef string `json:"external_ref"`
		} `json:"site"`
		Created bool `json:"created"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(second.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	if a.Site.SiteID == "" || a.Site.SiteID != b.Site.SiteID || !a.Created || b.Created {
		t.Fatalf("not idempotent: %+v then %+v", a, b)
	}
	if a.Site.ManagedBy != f.manager || a.Site.Domain != "" {
		t.Fatalf("site = %+v, want manager from the token and an empty domain", a.Site)
	}
	if strings.Contains(first.Body.String(), "session_salt") {
		t.Fatal("the session salt must never leave the server")
	}
}

func TestCreateSiteRefusesCallerChosenManagerAndBadRefs(t *testing.T) {
	f := newSvcFixture(t)
	tok := f.token(t, svctoken.ScopeSitesCreate)
	for _, body := range []string{
		`{"external_ref":"a","managed_by":"someone-else"}`,
		`{"external_ref":""}`,
		`{}`,
		`{"external_ref":"a\nb"}`,
		`{"external_ref":"` + strings.Repeat("x", maxExternalRefLen+1) + `"}`,
		`not json`,
	} {
		if rec := f.do(t, http.MethodPost, "/api/v1/service/sites", tok, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%.60s: status %d, want 400", body, rec.Code)
		}
	}
}

// A minted key must be a telemetry key and nothing more: it ingests, it cannot
// publish, and the request has no way to ask for more.
func TestMintedKeyIsTelemetryOnly(t *testing.T) {
	f := newSvcFixture(t)
	site := f.ownedSite(t)
	plaintext, tokRec, err := f.tokens.Create(context.Background(), "mint-"+randHex(t), f.manager, "root", []string{svctoken.ScopeKeysMint})
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/service/sites/" + site + "/keys"

	for _, body := range []string{`{"label":"x","scopes":["publish"]}`, `{"label":"x","scopes":["telemetry"]}`, `{}`} {
		if rec := f.do(t, http.MethodPost, path, plaintext, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", body, rec.Code)
		}
	}

	rec := f.do(t, http.MethodPost, path, plaintext, `{"label":"my-app"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint: %d %s", rec.Code, rec.Body)
	}
	var resp createAPIKeyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(resp.Key, "obs_") || strings.HasPrefix(resp.Key, svctoken.Prefix) {
		t.Fatalf("key = %q", resp.Key)
	}
	if strings.Join(resp.Info.Scopes, ",") != auth.ScopeTelemetry {
		t.Fatalf("scopes = %v", resp.Info.Scopes)
	}
	wantBy := "service:" + tokRec.ID
	if resp.Info.CreatedBy != wantBy {
		t.Fatalf("created_by = %q, want %q", resp.Info.CreatedBy, wantBy)
	}

	validated, err := f.authSvc.ValidateAPIKey(context.Background(), resp.Key)
	if err != nil {
		t.Fatal(err)
	}
	if validated.SiteID != site || auth.HasScope(validated.Scopes, auth.ScopePublish) || !auth.HasScope(validated.Scopes, auth.ScopeTelemetry) {
		t.Fatalf("validated = %+v", validated)
	}

	// The middleware real ingest runs resolves it to this site.
	var sawSite string
	ingestReq := httptest.NewRequest(http.MethodPost, "/api/v1/events", nil)
	ingestReq.Header.Set("X-API-Key", resp.Key)
	auth.APIKeyAuthMiddleware(f.authSvc)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		sawSite = ingest.SiteIDFromContext(r.Context())
	})).ServeHTTP(httptest.NewRecorder(), ingestReq)
	if sawSite != site {
		t.Fatalf("ingest resolved site %q, want %q", sawSite, site)
	}

	listed, err := f.authSvc.ListAPIKeys(context.Background(), site)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].CreatedBy != wantBy {
		t.Fatalf("listed = %+v", listed)
	}
}

// A share link minted here is the machine read credential: it resolves to the
// token's own site and only that one.
func TestMintedShareTokenResolvesToOwnSite(t *testing.T) {
	f := newSvcFixture(t)
	site := f.ownedSite(t)
	tok := f.token(t, svctoken.ScopeShareMint)
	path := "/api/v1/service/sites/" + site + "/share"
	if rec := f.do(t, http.MethodPost, path, tok, `{"ttl_days":-1}`); rec.Code != http.StatusBadRequest {
		t.Errorf("negative ttl: %d", rec.Code)
	}
	rec := f.do(t, http.MethodPost, path, tok, `{"ttl_days":7}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint: %d %s", rec.Code, rec.Body)
	}
	var link struct {
		Token  string `json:"token"`
		SiteID string `json:"site_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &link); err != nil {
		t.Fatal(err)
	}
	if link.Token == "" || link.SiteID != site {
		t.Fatalf("link = %+v", link)
	}
}

func TestWebhookWritesStayOnOwnSite(t *testing.T) {
	f := newSvcFixture(t)
	site := f.ownedSite(t)
	other := f.ownedSite(t)
	tok := f.token(t, svctoken.ScopeWebhooksWrite)
	base := "/api/v1/service/sites/" + site + "/webhooks"

	// A destination that could only ever be blocked at delivery is refused up
	// front, with a message that says so, and creates nothing.
	for _, bad := range []string{"http://127.0.0.1:9/x", "http://localhost/x", "http://169.254.169.254/latest/meta-data/", "http://10.1.2.3/x"} {
		rec := f.do(t, http.MethodPost, base, tok, `{"url":"`+bad+`"}`)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "non-public") {
			t.Errorf("%s: status %d %s, want 400 naming the reason", bad, rec.Code, rec.Body)
		}
	}
	if hooks, err := f.hooks.List(context.Background(), site); err != nil || len(hooks) != 0 {
		t.Fatalf("a refused destination created a webhook: %v %v", hooks, err)
	}

	// Same create-time validation as the admin route. Private destinations are
	// refused later, at dial time (netsafe), which is the point of that guard.
	for _, bad := range []string{`{"url":"ftp://example.com/x"}`, `{"url":"https://user:pw@example.com/x"}`, `{"name":"no url"}`} {
		if rec := f.do(t, http.MethodPost, base, tok, bad); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", bad, rec.Code)
		}
	}
	rec := f.do(t, http.MethodPost, base, tok, `{"name":"n","url":"https://example.com/hook"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var hook struct {
		WebhookID string `json:"webhook_id"`
		Secret    string `json:"secret"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &hook); err != nil {
		t.Fatal(err)
	}
	if hook.WebhookID == "" || hook.Secret == "" {
		t.Fatalf("hook = %+v (the signing secret must be revealed once)", hook)
	}

	// The id is real but the path names another site of the same manager.
	wrong := "/api/v1/service/sites/" + other + "/webhooks/" + hook.WebhookID
	if rec := f.do(t, http.MethodDelete, wrong, tok, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("delete through another site: %d", rec.Code)
	}
	hooks, err := f.hooks.List(context.Background(), site)
	if err != nil || len(hooks) != 1 {
		t.Fatalf("webhook must survive: %v %v", hooks, err)
	}
	if rec := f.do(t, http.MethodDelete, base+"/"+hook.WebhookID, tok, ""); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if hooks, err := f.hooks.List(context.Background(), site); err != nil || len(hooks) != 0 {
		t.Fatalf("webhook must be gone: %v %v", hooks, err)
	}
}

// Writes leave a started row and a result row naming the token; refusals of
// anything token-shaped leave a denied row; reads and anonymous noise leave nothing;
// no row holds a secret.
func TestServiceCallsAreAudited(t *testing.T) {
	f := newSvcFixture(t)
	site := f.ownedSite(t)
	plaintext, tok, err := f.tokens.Create(context.Background(), "audited-"+randHex(t), f.manager, "root",
		[]string{svctoken.ScopeKeysMint, svctoken.ScopeAlertsRead})
	if err != nil {
		t.Fatal(err)
	}

	mint := f.do(t, http.MethodPost, "/api/v1/service/sites/"+site+"/keys", plaintext, `{"label":"a"}`)
	if mint.Code != http.StatusCreated {
		t.Fatalf("mint: %d", mint.Code)
	}
	var resp createAPIKeyResponse
	if err := json.Unmarshal(mint.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	events := f.audit.all()
	if len(events) != 2 || events[0].Result != "started" || events[1].Result != audit.ResultSuccess {
		t.Fatalf("mint rows = %+v", events)
	}
	for _, ev := range events {
		if ev.Actor != tok.ID || ev.ActorType != audit.ActorService || ev.Action != "service.keys.mint" || ev.SiteID != site {
			t.Fatalf("row = %+v", ev)
		}
	}
	if !strings.Contains(events[1].Metadata, resp.Info.KeyID) {
		t.Fatalf("the result row must name the key: %s", events[1].Metadata)
	}

	if rec := f.do(t, http.MethodGet, "/api/v1/service/sites/"+site+"/alerts/rules", plaintext, ""); rec.Code != http.StatusOK {
		t.Fatalf("read: %d", rec.Code)
	}
	if len(f.audit.all()) != 2 {
		t.Fatal("a successful read must not write an audit row")
	}

	if rec := f.do(t, http.MethodPost, "/api/v1/service/sites", plaintext, `{}`); rec.Code != http.StatusForbidden {
		t.Fatalf("missing scope: %d", rec.Code)
	}
	if rec := f.do(t, http.MethodGet, "/api/v1/service/sites/nope/alerts/rules", plaintext, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign site read: %d", rec.Code)
	}
	forged := svctoken.Prefix + strings.Repeat("A", 43)
	if rec := f.do(t, http.MethodPost, "/api/v1/service/sites", forged, `{}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("well-formed bad token: %d", rec.Code)
	}
	events = f.audit.all()
	if len(events) != 5 {
		t.Fatalf("rows = %d, want three more denials: %+v", len(events), events)
	}
	for _, ev := range events[2:] {
		if ev.Result != audit.ResultDenied {
			t.Fatalf("row = %+v", ev)
		}
	}

	// Anonymous noise costs no rows, whatever the method.
	for _, cred := range []string{"", "not-a-token", svctoken.Prefix + "short"} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			path := "/api/v1/service/sites/" + site + "/alerts/rules"
			if method == http.MethodPost {
				path = "/api/v1/service/sites"
			}
			if rec := f.do(t, method, path, cred, `{}`); rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s %q: %d", method, cred, rec.Code)
			}
		}
	}
	if got := len(f.audit.all()); got != 5 {
		t.Fatalf("anonymous requests wrote %d audit rows", got-5)
	}

	raw, err := json.Marshal(f.audit.all())
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{plaintext, resp.Key, forged} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("audit trail holds a credential: %s", raw)
		}
	}
}

// A long User-Agent and path must not reach the trail whole: these rows are written
// for callers who failed to authenticate.
func TestAuditFieldsAreBounded(t *testing.T) {
	f := newSvcFixture(t)
	forged := svctoken.Prefix + strings.Repeat("A", 43)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/service/sites/"+strings.Repeat("s", 2000)+"/alerts/rules", nil)
	req.Header.Set("Authorization", "Bearer "+forged)
	req.Header.Set("User-Agent", strings.Repeat("u", 10000))
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d", rec.Code)
	}
	events := f.audit.all()
	if len(events) != 1 {
		t.Fatalf("rows = %d", len(events))
	}
	ev := events[0]
	if len(ev.UserAgent) > maxAuditFieldLen || len(ev.Target) > maxAuditFieldLen || len(ev.SiteID) > maxAuditFieldLen {
		t.Fatalf("unbounded row: ua=%d target=%d site=%d", len(ev.UserAgent), len(ev.Target), len(ev.SiteID))
	}
}

// What a token mints outlives it, so the audit row has to be able to find it.
func TestMintedThingsAreTraceable(t *testing.T) {
	f := newSvcFixture(t)
	site := f.ownedSite(t)
	tok := f.token(t, svctoken.ScopeShareMint, svctoken.ScopeWebhooksWrite, svctoken.ScopeSitesCreate)
	last := func(action string) audit.AuditEvent {
		t.Helper()
		events := f.audit.all()
		for i := len(events) - 1; i >= 0; i-- {
			if events[i].Action == action && events[i].Result == audit.ResultSuccess {
				return events[i]
			}
		}
		t.Fatalf("no success row for %s in %+v", action, events)
		return audit.AuditEvent{}
	}

	rec := f.do(t, http.MethodPost, "/api/v1/service/sites/"+site+"/share", tok, `{}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("share: %d", rec.Code)
	}
	var link struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &link); err != nil {
		t.Fatal(err)
	}
	meta := last("service.share.mint").Metadata
	if !strings.Contains(meta, link.ID) || strings.Contains(meta, link.Token) {
		t.Fatalf("share row must name the id and not the token: %s", meta)
	}

	const hookURL = "https://hooks.example.com/services/SECRETPATH"
	rec = f.do(t, http.MethodPost, "/api/v1/service/sites/"+site+"/webhooks", tok, `{"url":"`+hookURL+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("webhook: %d %s", rec.Code, rec.Body)
	}
	var hook struct {
		WebhookID string `json:"webhook_id"`
		Secret    string `json:"secret"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &hook); err != nil {
		t.Fatal(err)
	}
	meta = last("service.webhooks.write").Metadata
	if !strings.Contains(meta, hook.WebhookID) || !strings.Contains(meta, "hooks.example.com") {
		t.Fatalf("webhook row must name the id and host: %s", meta)
	}
	if strings.Contains(meta, "SECRETPATH") || strings.Contains(meta, hook.Secret) {
		t.Fatalf("webhook row holds a credential: %s", meta)
	}
}

// Every store failure on this surface is a 503 with Retry-After and no internals.
func TestStoreFailuresAreCleanRetryableAnswers(t *testing.T) {
	f := newSvcFixture(t)
	site := f.ownedSite(t)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	closed, err := nucleus.Connect(ctx, nucleustest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()

	assert503 := func(name string, rec *httptest.ResponseRecorder) {
		t.Helper()
		if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
			t.Errorf("%s: status %d, Retry-After %q (%s)", name, rec.Code, rec.Header().Get("Retry-After"), rec.Body)
		}
		for _, leak := range []string{"svctoken", "closed", "pool", "INSERT", "sql"} {
			if strings.Contains(rec.Body.String(), leak) {
				t.Errorf("%s: body leaks internals: %s", name, rec.Body)
			}
		}
	}

	tok := f.token(t, allScopes...)
	mux := http.NewServeMux()
	(&serviceAPI{
		tokens: f.tokens, sites: f.sites, keys: auth.NewAuthService(closed, strings.Repeat("s", 40), logger),
		shares: share.NewShareService(closed), webhooks: platform.NewWebhookService(closed, logger),
		alerts: fakeAlerts{}, audit: f.audit,
	}).register(mux)
	call := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	assert503("key mint", call(http.MethodPost, "/api/v1/service/sites/"+site+"/keys", `{"label":"a"}`))
	assert503("share mint", call(http.MethodPost, "/api/v1/service/sites/"+site+"/share", `{}`))
	assert503("webhook create", call(http.MethodPost, "/api/v1/service/sites/"+site+"/webhooks", `{"url":"https://example.com/x"}`))

	// A closed sites service fails ownership first, and createSite has none.
	siteMux := http.NewServeMux()
	(&serviceAPI{
		tokens: f.tokens, sites: sites.NewSiteService(closed), keys: f.authSvc,
		shares: share.NewShareService(closed), webhooks: f.hooks, alerts: fakeAlerts{}, audit: f.audit,
	}).register(siteMux)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/service/sites", strings.NewReader(`{"external_ref":"x"}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	siteMux.ServeHTTP(rec, req)
	assert503("site create", rec)

	admin := httptest.NewRequest(http.MethodPost, "/api/v1/service-tokens", strings.NewReader(`{"name":"n","scopes":["sites:create"]}`))
	admin = admin.WithContext(neutronauth.WithClaims(auth.WithRole(admin.Context(), auth.RoleAdmin), neutronauth.Claims{"username": "root"}))
	rec = httptest.NewRecorder()
	serviceTokensCreateHandler(svctoken.NewStore(closed))(rec, admin)
	assert503("token create", rec)
}

// Too many requests from one address stop before anything else runs.
func TestServiceRoutesAreRateLimited(t *testing.T) {
	f := newSvcFixture(t)
	mux := http.NewServeMux()
	(&serviceAPI{
		tokens: f.tokens, sites: f.sites, keys: f.authSvc, webhooks: f.hooks, alerts: fakeAlerts{}, audit: f.audit,
		limit: ipRateLimitMW(ingest.NewRateLimiter(2, time.Minute, 2)),
	}).register(mux)
	forged := svctoken.Prefix + strings.Repeat("A", 43)
	codes := []int{}
	for i := 0; i < 4; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/service/sites", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+forged)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		codes = append(codes, rec.Code)
	}
	if codes[0] != http.StatusUnauthorized || codes[3] != http.StatusTooManyRequests {
		t.Fatalf("codes = %v", codes)
	}
	if got := len(f.audit.all()); got > 2 {
		t.Fatalf("limited requests still wrote audit rows: %d", got)
	}
}

// Minting while the trail is down would be an unrecorded grant.
func TestWriteIsRefusedWhenAuditIsDown(t *testing.T) {
	f := newSvcFixture(t)
	site := f.ownedSite(t)
	tok := f.token(t, svctoken.ScopeKeysMint)
	f.audit.mu.Lock()
	f.audit.fail = true
	f.audit.mu.Unlock()
	rec := f.do(t, http.MethodPost, "/api/v1/service/sites/"+site+"/keys", tok, `{"label":"a"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", rec.Code)
	}
	keys, err := f.authSvc.ListAPIKeys(context.Background(), site)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 {
		t.Fatalf("a key was minted without an audit record: %+v", keys)
	}
}

func TestAdminTokenCreateRefusesGracePeriodAndBadInput(t *testing.T) {
	f := newSvcFixture(t)
	h := serviceTokensCreateHandler(f.tokens)

	// During first-run grace the middleware lets the request through with no
	// claims and no role. That must not mint a durable credential.
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/api/v1/service-tokens",
		strings.NewReader(`{"name":"n","scopes":["sites:create"]}`)))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("no admin: status %d, want 403", rec.Code)
	}
	// The same grace period must not let anyone read or revoke tokens either.
	graceList := httptest.NewRecorder()
	serviceTokensListHandler(f.tokens)(graceList, httptest.NewRequest(http.MethodGet, "/api/v1/service-tokens", nil))
	if graceList.Code != http.StatusForbidden {
		t.Fatalf("anonymous list: status %d, want 403", graceList.Code)
	}
	_, victim, err := f.tokens.Create(context.Background(), "victim-"+randHex(t), f.manager, "root", allScopes)
	if err != nil {
		t.Fatal(err)
	}
	graceRevoke := httptest.NewRequest(http.MethodDelete, "/api/v1/service-tokens/"+victim.ID, nil)
	graceRevoke.SetPathValue("token_id", victim.ID)
	graceRec := httptest.NewRecorder()
	serviceTokensRevokeHandler(f.tokens)(graceRec, graceRevoke)
	if graceRec.Code != http.StatusForbidden {
		t.Fatalf("anonymous revoke: status %d, want 403", graceRec.Code)
	}

	admin := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/service-tokens", strings.NewReader(body))
		ctx := neutronauth.WithClaims(auth.WithRole(req.Context(), auth.RoleAdmin), neutronauth.Claims{"username": "root"})
		rec := httptest.NewRecorder()
		h(rec, req.WithContext(ctx))
		return rec
	}
	for _, body := range []string{
		`{"name":"n"}`,
		`{"name":"n","scopes":["users:write"]}`,
		`{"name":"","scopes":["sites:create"]}`,
		`{"name":"n","scopes":["sites:create"],"role":"admin"}`,
	} {
		if rec := admin(body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", body, rec.Code)
		}
	}
	rec = admin(`{"name":"good-` + randHex(t) + `","scopes":["sites:create"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Token  string         `json:"token"`
		Record map[string]any `json:"record"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.Token, svctoken.Prefix) || out.Record["created_by"] != "root" {
		t.Fatalf("create response = %s", rec.Body)
	}
	if _, leaked := out.Record["hash"]; leaked {
		t.Fatal("the record must not carry the hash")
	}

	asAdmin := func(req *http.Request) *http.Request {
		return req.WithContext(neutronauth.WithClaims(auth.WithRole(req.Context(), auth.RoleAdmin), neutronauth.Claims{"username": "root"}))
	}
	list := httptest.NewRecorder()
	serviceTokensListHandler(f.tokens)(list, asAdmin(httptest.NewRequest(http.MethodGet, "/api/v1/service-tokens", nil)))
	if list.Code != http.StatusOK || strings.Contains(list.Body.String(), out.Token) || strings.Contains(list.Body.String(), `"hash"`) {
		t.Fatalf("list: %d %s", list.Code, list.Body)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/service-tokens/x", nil)
	req.SetPathValue("token_id", "no-such")
	missing := httptest.NewRecorder()
	serviceTokensRevokeHandler(f.tokens)(missing, asAdmin(req))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("revoke missing: %d", missing.Code)
	}
}

func TestAdoptSiteRoute(t *testing.T) {
	f := newSvcFixture(t)
	ctx := context.Background()
	plain, err := f.sites.Create(ctx, "fylun.example.com", "Fylun")
	if err != nil {
		t.Fatal(err)
	}
	h := adoptSiteHandler(f.sites, f.audit)
	call := func(site, body string, admin bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/sites/"+site+"/adopt", strings.NewReader(body))
		req.SetPathValue("site_id", site)
		if admin {
			req = req.WithContext(neutronauth.WithClaims(auth.WithRole(req.Context(), auth.RoleAdmin), neutronauth.Claims{"username": "root"}))
		}
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec
	}
	ref := "app:" + randHex(t)
	body := `{"managed_by":"` + f.manager + `","external_ref":"` + ref + `"}`

	// First-run grace reaches this handler with no identity at all.
	if rec := call(plain.SiteID, body, false); rec.Code != http.StatusForbidden {
		t.Fatalf("anonymous: %d", rec.Code)
	}
	for _, bad := range []string{`{"managed_by":"x"}`, `{"external_ref":"x"}`, `{"managed_by":"x","external_ref":"y","extra":1}`, `nope`} {
		if rec := call(plain.SiteID, bad, true); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", bad, rec.Code)
		}
	}
	if rec := call("no-such-site", body, true); rec.Code != http.StatusNotFound {
		t.Errorf("missing site: %d", rec.Code)
	}

	// A token cannot reach this site yet.
	tok := f.token(t, svctoken.ScopeKeysMint)
	keys := "/api/v1/service/sites/" + plain.SiteID + "/keys"
	if rec := f.do(t, http.MethodPost, keys, tok, `{"label":"a"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("before adoption: %d", rec.Code)
	}
	if rec := call(plain.SiteID, body, true); rec.Code != http.StatusCreated {
		t.Fatalf("adopt: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(t, http.MethodPost, keys, tok, `{"label":"a"}`); rec.Code != http.StatusCreated {
		t.Fatalf("after adoption: %d %s", rec.Code, rec.Body)
	}
	if rec := call(plain.SiteID, body, true); rec.Code != http.StatusOK {
		t.Errorf("retry: %d", rec.Code)
	}
	if rec := call(plain.SiteID, `{"managed_by":"someone-else","external_ref":"`+ref+`"}`, true); rec.Code != http.StatusConflict {
		t.Errorf("second manager: %d", rec.Code)
	}

	seen := false
	for _, ev := range f.audit.all() {
		if ev.Action == "sites.adopt" && ev.Result == audit.ResultSuccess && ev.Actor == "root" &&
			ev.SiteID == plain.SiteID && strings.Contains(ev.Metadata, f.manager) && strings.Contains(ev.Metadata, ref) {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("no sites.adopt row naming the manager and ref: %+v", f.audit.all())
	}

	f.audit.mu.Lock()
	f.audit.fail = true
	f.audit.mu.Unlock()
	other, err := f.sites.Create(ctx, "other.example.com", "Other")
	if err != nil {
		t.Fatal(err)
	}
	rec := call(other.SiteID, `{"managed_by":"m","external_ref":"r-`+randHex(t)+`"}`, true)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("audit down: %d", rec.Code)
	}
	if owned, err := f.sites.IsManagedBy(ctx, other.SiteID, "m"); err != nil || owned {
		t.Fatalf("adopted with the trail down: owned=%v err=%v", owned, err)
	}
}
