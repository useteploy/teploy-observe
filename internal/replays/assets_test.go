package replays

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy-observe/internal/netsafe"
)

// pngHeader is a minimal valid PNG (1x1) so DetectContentType reports
// image/png from real bytes.
var pngHeader = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
	0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
}

func testProxy(t *testing.T, hosts []string, maxBytes int64) *AssetProxy {
	t.Helper()
	allow, err := netsafe.ParseAllow("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	return NewAssetProxyWithAllow(hosts, maxBytes, 2*time.Second, allow,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func proxyGet(p *AssetProxy, rawURL string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "/api/v1/replay-assets?u="+url.QueryEscape(rawURL), nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

func startUpstream(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func upstreamHost(raw string) string {
	u, _ := url.Parse(raw)
	return u.Hostname()
}

// F38: an allowlisted raster image proxied with sniffed type, private
// caching, and content-hash ETag revalidation.
func TestAssetProxy_AllowlistedImageServed(t *testing.T) {
	up := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html") // lie: the sniffer must win
		w.Header().Set("Cache-Control", "public, max-age=999999")
		_, _ = w.Write(pngHeader)
	})
	p := testProxy(t, []string{upstreamHost(up.URL)}, 0)

	rec := proxyGet(p, up.URL+"/logo.png")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d body %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("content type must be sniffed, got %q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "private, max-age=86400" {
		t.Fatalf("upstream cache headers must not be forwarded; want private, got %q", cc)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("content-hash ETag required")
	}

	// Revalidation: If-None-Match on the same URL answers 304 with no body.
	req := httptest.NewRequest("GET", "/api/v1/replay-assets?u="+url.QueryEscape(up.URL+"/logo.png"), nil)
	req.Header.Set("If-None-Match", etag)
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusNotModified {
		t.Fatalf("etag revalidation must 304, got %d", rec2.Code)
	}
}

// F38: the allowlist is the boundary — a non-listed host is a 403 even when
// the proxy is otherwise enabled; an empty allowlist disables the route.
func TestAssetProxy_AllowlistEnforced(t *testing.T) {
	up := startUpstream(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(pngHeader) })
	p := testProxy(t, []string{"assets.example.com"}, 0)
	if rec := proxyGet(p, up.URL+"/x.png"); rec.Code != http.StatusForbidden {
		t.Fatalf("non-allowlisted host must 403, got %d", rec.Code)
	}
	disabled := testProxy(t, nil, 0)
	if rec := proxyGet(disabled, up.URL+"/x.png"); rec.Code != http.StatusNotFound {
		t.Fatalf("disabled proxy must 404, got %d", rec.Code)
	}
}

// F38: redirects are rejected, not followed — following them would defeat
// the allowlist in one hop.
func TestAssetProxy_RefusesRedirects(t *testing.T) {
	up := startUpstream(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(pngHeader) })
	redirector := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, up.URL+"/real.png", http.StatusFound)
	})
	p := testProxy(t, []string{upstreamHost(redirector.URL), upstreamHost(up.URL)}, 0)
	if rec := proxyGet(p, redirector.URL+"/bounce"); rec.Code != http.StatusBadGateway {
		t.Fatalf("redirect must be rejected, got %d", rec.Code)
	}
}

// F38: MIME gate on sniffed bytes; SVG (script-bearing markup) excluded.
func TestAssetProxy_MIMEAllowlist(t *testing.T) {
	up := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/svg" {
			_, _ = w.Write([]byte("<svg xmlns='http://www.w3.org/2000/svg'><script>alert(1)</script></svg>"))
			return
		}
		_, _ = w.Write([]byte("<html><body>not an image</body></html>"))
	})
	p := testProxy(t, []string{upstreamHost(up.URL)}, 0)
	if rec := proxyGet(p, up.URL+"/page"); rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("html must be refused, got %d", rec.Code)
	}
	if rec := proxyGet(p, up.URL+"/svg"); rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("svg must be refused (script-bearing), got %d", rec.Code)
	}
}

// F38: bounded size — a body reaching the cap is rejected, not truncated.
func TestAssetProxy_SizeCapRejectsNotTruncates(t *testing.T) {
	up := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(pngHeader)
		_, _ = w.Write(make([]byte, 64*1024))
	})
	p := testProxy(t, []string{upstreamHost(up.URL)}, 1024)
	rec := proxyGet(p, up.URL+"/big.png")
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over-cap asset must 413, got %d", rec.Code)
	}
}

// F38: egress confined at DIAL time — a private destination is refused even
// when its hostname is allowlisted (the strict production client passes no
// private-network allow).
func TestAssetProxy_PrivateEgressRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback listener: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	private := fmt.Sprintf("http://127.0.0.1:%d/x.png", port)
	p := NewAssetProxy([]string{"127.0.0.1"}, 0, 500*time.Millisecond,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec := proxyGet(p, private)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("private destination must be refused at dial time, got %d", rec.Code)
	}
}

// F38: malformed targets fail closed before any egress.
func TestAssetProxy_BadTargets(t *testing.T) {
	p := testProxy(t, []string{"anything.example.com"}, 0)
	for _, raw := range []string{
		"",
		"javascript:alert(1)",
		"/relative.png",
		"http://user:pass@anything.example.com/x.png",
		"http://anything.example.com/x.png#frag",
	} {
		if rec := proxyGet(p, raw); rec.Code != http.StatusBadRequest {
			t.Fatalf("target %q must 400, got %d", raw, rec.Code)
		}
	}
	if rec := proxyGet(p, "http://anything.example.com/x.png?utm_source=ok"); rec.Code == http.StatusBadRequest {
		t.Fatal("a plain query string is a legal target (the proxy relays it; logs redact it)")
	}
}

// TO-025: the validator is bound to the BODY, not the URL — a changed
// asset at the same URL must be re-served (200, new tag), not revalidated
// into staleness forever by a URL-keyed 304.
func TestAssetProxy_ChangedBodyAtSameURLIsNotStale(t *testing.T) {
	body := append([]byte(nil), pngHeader...)
	up := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	})
	p := testProxy(t, []string{upstreamHost(up.URL)}, 0)

	first := proxyGet(p, up.URL+"/logo.png")
	if first.Code != http.StatusOK {
		t.Fatalf("first fetch: %d", first.Code)
	}
	oldTag := first.Header().Get("ETag")

	// The origin changes the asset.
	body = append(body, 0x00, 0x01, 0x02)

	// A revalidation presenting the OLD tag must get the NEW body.
	req := httptest.NewRequest("GET", "/api/v1/replay-assets?u="+url.QueryEscape(up.URL+"/logo.png"), nil)
	req.Header.Set("If-None-Match", oldTag)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("a changed body must not 304 against the old tag, got %d", rec.Code)
	}
	if rec.Header().Get("ETag") == oldTag {
		t.Fatal("the validator must change with the body")
	}
	if rec.Body.Len() != len(body) {
		t.Fatalf("the new body must be served in full: %d of %d bytes", rec.Body.Len(), len(body))
	}
}

// TO-027: a read that fails after a valid image prefix must be a 502, not
// a truncated 200 "success".
func TestAssetProxy_TruncatedReadIsAnError(t *testing.T) {
	up := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngHeader)
		panic(http.ErrAbortHandler) // drop the connection mid-body
	})
	p := testProxy(t, []string{upstreamHost(up.URL)}, 0)
	rec := proxyGet(p, up.URL+"/logo.png")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("a truncated body must be a 502, got %d", rec.Code)
	}
}

// TO-026: the fetch-failure log carries host + failure class only — never
// the raw transport error (whose *url.Error embeds the full signed URL).
func TestAssetProxy_FetchFailureLogLeaksNothing(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	allow, err := netsafe.ParseAllow("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	p := NewAssetProxyWithAllow([]string{"upstream.invalid"}, 0, 500*time.Millisecond, allow, logger)

	// A URL with a secret-bearing query against an unresolvable host.
	rec := proxyGet(p, "https://upstream.invalid/x.png?sig=TO026_SECRET_TOKEN")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", rec.Code)
	}
	if strings.Contains(buf.String(), "TO026_SECRET_TOKEN") {
		t.Fatalf("log must not contain the query secret: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "transport_failure") {
		t.Fatalf("log should carry the failure class: %q", buf.String())
	}
}
