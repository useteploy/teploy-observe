package ingest

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// ipFromMiddleware runs RequestInfoMiddleware with the given trusted proxies
// and returns the client IP it derives for req.
func ipFromMiddleware(trustedCSV string, req *http.Request) (ip, ua string) {
	mw := RequestInfoMiddleware(ParseTrustedProxies(trustedCSV))
	mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip = ClientIPFromContext(r.Context())
		ua = UserAgentFromContext(r.Context())
	})).ServeHTTP(httptest.NewRecorder(), req)
	return
}

func TestRequestInfoMiddleware_ExtractsIP(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "192.168.1.100:12345"
	req.Header.Set("User-Agent", "TestBot/1.0")

	gotIP, gotUA := ipFromMiddleware("", req)
	if gotIP != "192.168.1.100" {
		t.Errorf("expected IP 192.168.1.100, got %s", gotIP)
	}
	if gotUA != "TestBot/1.0" {
		t.Errorf("expected UA TestBot/1.0, got %s", gotUA)
	}
}

// Untrusted peer: a spoofed X-Forwarded-For must be ignored and the real peer
// address used, so a client can't forge a fresh rate-limit identity.
func TestRequestInfoMiddleware_XFFIgnoredFromUntrustedPeer(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "203.0.113.9:5555"
	req.Header.Set("X-Forwarded-For", "10.0.0.1, 10.0.0.2")
	req.Header.Set("X-Real-Ip", "172.16.0.5")

	gotIP, _ := ipFromMiddleware("", req) // no trusted proxies
	if gotIP != "203.0.113.9" {
		t.Errorf("spoofed XFF should be ignored, expected peer 203.0.113.9, got %s", gotIP)
	}
}

// Trusted peer: XFF is honored, returning the rightmost untrusted hop.
func TestRequestInfoMiddleware_XFFFromTrustedProxy(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "10.0.0.2:5555" // the edge proxy
	// client -> trusted 10.0.0.3 -> trusted peer 10.0.0.2. Real client is 198.51.100.7.
	req.Header.Set("X-Forwarded-For", "198.51.100.7, 10.0.0.3")

	gotIP, _ := ipFromMiddleware("10.0.0.0/24", req)
	if gotIP != "198.51.100.7" {
		t.Errorf("expected real client 198.51.100.7 through trusted proxy, got %s", gotIP)
	}
}

func TestRequestInfoMiddleware_XRealIPFromTrustedProxy(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "10.0.0.2:5555"
	req.Header.Set("X-Real-Ip", "172.16.0.5")

	gotIP, _ := ipFromMiddleware("10.0.0.2", req)
	if gotIP != "172.16.0.5" {
		t.Errorf("expected X-Real-Ip 172.16.0.5 from trusted proxy, got %s", gotIP)
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
