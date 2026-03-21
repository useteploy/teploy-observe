package ingest

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequestInfoMiddleware_ExtractsIP(t *testing.T) {
	var gotIP, gotUA string

	handler := RequestInfoMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotIP = ClientIPFromContext(r.Context())
		gotUA = UserAgentFromContext(r.Context())
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "192.168.1.100:12345"
	req.Header.Set("User-Agent", "TestBot/1.0")

	handler.ServeHTTP(httptest.NewRecorder(), req)

	if gotIP != "192.168.1.100" {
		t.Errorf("expected IP 192.168.1.100, got %s", gotIP)
	}
	if gotUA != "TestBot/1.0" {
		t.Errorf("expected UA TestBot/1.0, got %s", gotUA)
	}
}

func TestRequestInfoMiddleware_XForwardedFor(t *testing.T) {
	var gotIP string

	handler := RequestInfoMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotIP = ClientIPFromContext(r.Context())
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Forwarded-For", "10.0.0.1, 10.0.0.2, 10.0.0.3")

	handler.ServeHTTP(httptest.NewRecorder(), req)

	if gotIP != "10.0.0.1" {
		t.Errorf("expected first X-Forwarded-For IP 10.0.0.1, got %s", gotIP)
	}
}

func TestRequestInfoMiddleware_XRealIP(t *testing.T) {
	var gotIP string

	handler := RequestInfoMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotIP = ClientIPFromContext(r.Context())
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Real-Ip", "172.16.0.5")

	handler.ServeHTTP(httptest.NewRecorder(), req)

	if gotIP != "172.16.0.5" {
		t.Errorf("expected X-Real-Ip 172.16.0.5, got %s", gotIP)
	}
}

func TestSiteIDContext(t *testing.T) {
	var gotSite string

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := WithSiteID(r.Context(), "my-site")
		gotSite = SiteIDFromContext(ctx)
	})

	req := httptest.NewRequest("GET", "/", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if gotSite != "my-site" {
		t.Errorf("expected site_id my-site, got %s", gotSite)
	}
}

func TestSiteIDContext_Missing(t *testing.T) {
	var gotSite string

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSite = SiteIDFromContext(r.Context())
	})

	req := httptest.NewRequest("GET", "/", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if gotSite != "" {
		t.Errorf("expected empty site_id, got %s", gotSite)
	}
}
