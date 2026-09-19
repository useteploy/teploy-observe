package replays

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/useteploy/teploy-observe/internal/netsafe"
)

// errNoRedirects makes the proxy client refuse every redirect.
var errNoRedirects = errors.New("replay asset proxy: redirects are not followed")

// AssetProxy is the replay asset proxy (audit F38's designed allowlist).
//
// Structured replay snapshots serialize img elements; the player renders them
// through THIS same-origin proxy instead of letting the operator's browser
// dial recorded-page origins directly. The design constraints, each enforced
// below:
//
//   - ALLOWLIST: only hosts the operator listed in OBSERVE_REPLAY_ASSET_HOSTS
//     are fetched (empty list = the proxy is disabled; every request 404s).
//     An allowlist is the only posture that keeps the proxy from becoming an
//     open fetcher, and it is per-destination, not per-URL.
//   - BOUNDED SIZE: responses are capped (default 5 MiB); a body that reaches
//     the cap is rejected, not truncated — a truncated image is corrupt data
//     presented as real.
//   - MIME CHECK: the content type is sniffed from the leading bytes
//     (never trusted from the Content-Type header) and must be a raster
//     image. SVG is deliberately excluded — it is script-bearing markup, and
//     "just an image" is the entire point of this surface.
//   - SAME-SITE CACHING: responses carry `Cache-Control: private` (browser
//     may cache; shared proxies may not) plus a content-hash ETag with 304
//     handling, so one replay seek does not re-fetch the asset per keyframe.
//   - NO REDIRECT FOLLOWING: a 3xx is a rejection. Following redirects would
//     let an allowlisted host bounce the fetch to any destination, which is
//     the allowlist defeated in one hop.
//   - EGRESS CONFINED: the outbound client is netsafe's — private, loopback,
//     link-local (cloud metadata), and CGNAT destinations are refused at
//     DIAL time, so a hostname that resolves public-now-private-later
//     (DNS rebinding) is still blocked.
//
// Legacy raw-HTML snapshots are unaffected: they render with the injected
// deny-all CSP (best-effort, as before) and never load proxied assets.
type AssetProxy struct {
	hosts    map[string]struct{}
	maxBytes int64
	client   *http.Client
	logger   *slog.Logger
}

// DefaultAssetMaxBytes caps one proxied asset.
const DefaultAssetMaxBytes = 5 << 20

// allowedAssetMIMEs is what http.DetectContentType is permitted to report.
func allowedAssetMIME(ct string) bool {
	switch ct {
	case "image/png", "image/jpeg", "image/gif", "image/webp",
		"image/bmp", "image/x-icon", "image/vnd.microsoft.icon":
		return true
	}
	return false
}

// NewAssetProxy builds the proxy. hosts is the lowercase destination-host
// allowlist (port-insensitive: the host name must match exactly); an empty
// set DISABLES the proxy.
func NewAssetProxy(hosts []string, maxBytes int64, timeout time.Duration, logger *slog.Logger) *AssetProxy {
	return NewAssetProxyWithAllow(hosts, maxBytes, timeout, nil, logger)
}

// NewAssetProxyWithAllow additionally permits the given private networks at
// dial time (netsafe.Allow semantics — the same operator escape hatch the
// webhook senders have). Link-local (cloud metadata) stays unconditionally
// blocked. Production wiring passes nil; tests use it to point the proxy at
// a loopback fixture while still exercising the real dial-time check.
func NewAssetProxyWithAllow(hosts []string, maxBytes int64, timeout time.Duration, allow netsafe.Allow, logger *slog.Logger) *AssetProxy {
	set := make(map[string]struct{}, len(hosts))
	for _, h := range hosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if h != "" {
			set[h] = struct{}{}
		}
	}
	if maxBytes <= 0 {
		maxBytes = DefaultAssetMaxBytes
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	client := netsafe.ClientWithAllow(timeout, allow)
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return errNoRedirects
	}
	return &AssetProxy{hosts: set, maxBytes: maxBytes, client: client, logger: logger}
}

// Enabled reports whether any host is allowlisted (the player uses this to
// decide whether to rewrite snapshot srcs at all).
func (p *AssetProxy) Enabled() bool { return p != nil && len(p.hosts) > 0 }

// ParseAssetHosts reads the comma-separated OBSERVE_REPLAY_ASSET_HOSTS form.
func ParseAssetHosts(raw string) []string {
	var out []string
	for _, h := range strings.Split(raw, ",") {
		h = strings.TrimSpace(strings.ToLower(h))
		if h != "" {
			out = append(out, h)
		}
	}
	return out
}

// ServeHTTP handles GET /api/v1/replay-assets?u=<encoded absolute URL>.
// Auth is the route's middleware (JWT header or the route's stream ticket —
// the player cannot put headers on <img> requests).
func (p *AssetProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if p == nil || !p.Enabled() {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	raw := r.URL.Query().Get("u")
	if raw == "" {
		http.Error(w, "missing u parameter", http.StatusBadRequest)
		return
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		http.Error(w, "u must be an absolute http(s) URL", http.StatusBadRequest)
		return
	}
	if u.User != nil || u.Fragment != "" || u.RawFragment != "" {
		http.Error(w, "u must not carry userinfo or a fragment", http.StatusBadRequest)
		return
	}
	host := strings.ToLower(u.Hostname())
	if _, ok := p.hosts[host]; !ok {
		// The allowlist is the security boundary; a miss is a 403, not a 404,
		// so a misconfigured player is distinguishable from a disabled proxy.
		http.Error(w, "asset host not allowlisted", http.StatusForbidden)
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, u.String(), nil)
	if err != nil {
		http.Error(w, "bad upstream request", http.StatusBadGateway)
		return
	}
	req.Header.Set("Accept", "image/*")
	resp, err := p.client.Do(req)
	if err != nil {
		// TO-026: never log the raw error — *url.Error embeds the full
		// request URL, signed query included; the redacted URL field next
		// to it did not redact the error. Host + failure class only.
		p.logger.Warn("replay asset proxy: fetch failed",
			"host", host, "class", assetFailureClass(err))
		http.Error(w, "asset fetch failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Includes every 3xx: redirects are NOT followed (see the type doc).
		p.logger.Warn("replay asset proxy: non-200 upstream", "url", redactAssetURL(u), "status", resp.StatusCode)
		http.Error(w, "asset unavailable", http.StatusBadGateway)
		return
	}

	// TO-027: one bounded read of the whole body, PRESERVING the read
	// error. The old head-then-rest reads discarded the first error; a
	// transport failure after a valid image prefix could surface a
	// truncated image as a 200 success.
	body, err := io.ReadAll(io.LimitReader(resp.Body, p.maxBytes+1))
	if err != nil {
		p.logger.Warn("replay asset proxy: body read failed",
			"host", host, "class", assetFailureClass(err))
		http.Error(w, "asset read failed", http.StatusBadGateway)
		return
	}
	if int64(len(body)) > p.maxBytes {
		http.Error(w, "asset exceeds size cap", http.StatusRequestEntityTooLarge)
		return
	}
	ct := http.DetectContentType(body)
	if !allowedAssetMIME(ct) {
		p.logger.Warn("replay asset proxy: content type not allowlisted", "url", redactAssetURL(u), "type", ct)
		http.Error(w, "asset type not allowed", http.StatusUnsupportedMediaType)
		return
	}

	// TO-025: the ETag is a digest of the FETCHED BODY, computed after the
	// bounded read — a URL-keyed validator returned 304s without ever
	// consulting the origin, so a changed asset at the same URL stayed
	// stale forever (revalidation after max-age just got another 304).
	// Revalidation costs the upstream fetch; that is the price of a
	// validator that can actually notice changes.
	etag := contentETag(body)
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Set("ETag", etag)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// contentETag derives the strong validator from the served body (TO-025).
func contentETag(body []byte) string {
	sum := sha256.Sum256(body)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

// assetFailureClass reduces a transport error to a coarse, URL-free
// category for logs (TO-026). A custom transport's own error text can
// still embed anything — which is exactly why the raw error is never
// logged on this path.
func assetFailureClass(err error) string {
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timeout"
	}
	return "transport_failure"
}

// redactAssetURL keeps the query out of logs — the allowlisted host is the
// useful fact; the query may carry anything the recorded page carried.
func redactAssetURL(u *url.URL) string {
	q := u.RawQuery
	u.RawQuery = ""
	out := u.String()
	u.RawQuery = q
	return out
}
