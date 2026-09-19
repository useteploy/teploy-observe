package replays

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
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

	// Content-hash ETag first: one replay seek must not re-fetch.
	etag := `"` + contentETag(u.String()) + `"`
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
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
		p.logger.Warn("replay asset proxy: fetch failed", "url", redactAssetURL(u), "err", err)
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

	// Bound the read BEFORE sniffing: the sniff buffer is part of the cap.
	limited := io.LimitReader(resp.Body, p.maxBytes+1)
	head := make([]byte, 512)
	n, _ := io.ReadFull(limited, head)
	head = head[:n]
	ct := http.DetectContentType(head)
	if !allowedAssetMIME(ct) {
		p.logger.Warn("replay asset proxy: content type not allowlisted", "url", redactAssetURL(u), "type", ct)
		http.Error(w, "asset type not allowed", http.StatusUnsupportedMediaType)
		return
	}
	rest, err := io.ReadAll(limited)
	if err != nil {
		http.Error(w, "asset read failed", http.StatusBadGateway)
		return
	}
	if int64(len(head)+len(rest)) > p.maxBytes {
		http.Error(w, "asset exceeds size cap", http.StatusRequestEntityTooLarge)
		return
	}

	// Same-site caching: private (browser-only), content-addressed, and a
	// hard cap so a churning origin cannot pin cache entries forever. The
	// upstream's own cache/control headers are never forwarded.
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Length", strconv.Itoa(len(head)+len(rest)))
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Set("ETag", etag)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(head)
	_, _ = w.Write(rest)
}

// contentETag derives the deterministic (URL-keyed) weak validator. Keyed on
// the URL, not the body: the body would require fetching before a 304 can be
// answered, which defeats the point. A changed asset at the same URL is
// served stale for at most the max-age window — same tradeoff as any
// content-addressed CDN edge.
func contentETag(u string) string {
	sum := sha256.Sum256([]byte("observe-replay-asset|" + u))
	return hex.EncodeToString(sum[:16])
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
