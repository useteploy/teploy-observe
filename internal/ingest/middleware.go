package ingest

import (
	"context"
	"net"
	"net/http"
	"strings"
)

type ctxKey int

const (
	keyClientIP ctxKey = iota
	keyUserAgent
)

// ParseTrustedProxies parses a comma-separated list of CIDRs or bare IPs into
// networks. Bare IPs become /32 (or /128) networks. Invalid entries are
// skipped. Used to decide whose X-Forwarded-For headers to trust.
func ParseTrustedProxies(csv string) []*net.IPNet {
	var out []*net.IPNet
	for _, part := range strings.Split(csv, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if _, ipnet, err := net.ParseCIDR(part); err == nil {
			out = append(out, ipnet)
			continue
		}
		if ip := net.ParseIP(part); ip != nil {
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			out = append(out, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
		}
	}
	return out
}

// RequestInfoMiddleware extracts client IP and User-Agent from the request and
// stores them in the context for downstream handlers. X-Forwarded-For /
// X-Real-Ip are honored only when the immediate peer is one of trusted; with no
// trusted proxies (the default) the peer address is always used, so a client
// cannot spoof its IP to obtain a fresh rate-limit bucket.
func RequestInfoMiddleware(trusted []*net.IPNet) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := extractClientIP(r, trusted)
			ua := r.Header.Get("User-Agent")
			ctx := context.WithValue(r.Context(), keyClientIP, ip)
			ctx = context.WithValue(ctx, keyUserAgent, ua)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func isTrusted(ip net.IP, trusted []*net.IPNet) bool {
	for _, n := range trusted {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func extractClientIP(r *http.Request, trusted []*net.IPNet) string {
	peerHost, _, _ := net.SplitHostPort(r.RemoteAddr)
	peer := net.ParseIP(peerHost)

	// Only consult forwarding headers if the direct peer is a trusted proxy.
	if peer != nil && isTrusted(peer, trusted) {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// Walk right-to-left, skipping trusted proxies; the first untrusted
			// hop is the real client. If every hop is trusted, fall back to the
			// leftmost entry.
			parts := strings.Split(xff, ",")
			for i := len(parts) - 1; i >= 0; i-- {
				cand := strings.TrimSpace(parts[i])
				ip := net.ParseIP(cand)
				if ip == nil {
					continue
				}
				if !isTrusted(ip, trusted) {
					return cand
				}
			}
			if first := strings.TrimSpace(parts[0]); first != "" {
				return first
			}
		}
		if xri := strings.TrimSpace(r.Header.Get("X-Real-Ip")); xri != "" {
			return xri
		}
	}
	return peerHost
}

// ClientIPFromContext returns the client IP stored by RequestInfoMiddleware.
func ClientIPFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(keyClientIP).(string); ok {
		return v
	}
	return ""
}

// UserAgentFromContext returns the User-Agent stored by RequestInfoMiddleware.
func UserAgentFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(keyUserAgent).(string); ok {
		return v
	}
	return ""
}
