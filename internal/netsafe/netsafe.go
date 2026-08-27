// Package netsafe provides SSRF-resistant URL validation and an http.Client that
// refuses to connect to private/loopback/link-local/metadata addresses. The IP
// check runs at DIAL time (not just at resolve time), so a hostname that
// resolves to a public IP at validation and a private IP at request time
// (DNS rebinding) is still blocked.
package netsafe

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// ValidateURL enforces an http(s) scheme, a present host, and no userinfo.
// Call at config/create time to reject obviously bad destinations early.
func ValidateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("url scheme must be http or https")
	}
	if u.User != nil {
		return fmt.Errorf("url must not contain userinfo")
	}
	if u.Hostname() == "" {
		return fmt.Errorf("url must have a host")
	}
	return nil
}

// Allow is an operator-declared set of networks that MAY be dialled even though
// they are private.
//
// It exists because the guard below is correct and, for a self-hosted fleet,
// total: every Teploy service lives on a tailnet (100.64.0.0/10), so a webhook
// aimed at one could never be delivered — the destination was blocked at dial
// time for being exactly what it was supposed to be. Silently weakening the
// guard would have been the wrong fix; so would telling operators their own
// network is unreachable.
//
// Three properties keep this from becoming a hole:
//
//   - It is expressed as CIDRs, never hostnames. The guard's whole point is
//     that it runs at DIAL time so a name resolving public-then-private cannot
//     slip through; allowing by name would hand that back. An operator declares
//     addresses, and the dial-time check still decides.
//   - Link-local stays blocked unconditionally, allowlist or not. 169.254.169.254
//     is cloud instance metadata — credentials — and no configuration should be
//     able to reach it.
//   - Empty is the default, and an empty Allow behaves exactly as before.
type Allow []*net.IPNet

// ParseAllow reads a comma- or whitespace-separated CIDR list. An entry that is
// a bare IP is taken as a /32 (or /128), which is the form an operator naming
// one host will reach for first.
func ParseAllow(raw string) (Allow, error) {
	var out Allow
	for _, field := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' }) {
		entry := strings.TrimSpace(field)
		if entry == "" {
			continue
		}
		if !strings.Contains(entry, "/") {
			ip := net.ParseIP(entry)
			if ip == nil {
				return nil, fmt.Errorf("netsafe: %q is not an IP address or CIDR", entry)
			}
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			entry = fmt.Sprintf("%s/%d", ip.String(), bits)
		}
		_, network, err := net.ParseCIDR(entry)
		if err != nil {
			return nil, fmt.Errorf("netsafe: %q is not a valid CIDR: %w", field, err)
		}
		out = append(out, network)
	}
	return out, nil
}

func (a Allow) permits(ip net.IP) bool {
	for _, network := range a {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

// IsBlockedIP reports whether ip is a non-public address that server-side
// fetchers must not reach: loopback, link-local (incl. 169.254.169.254 cloud
// metadata), private RFC1918/ULA, unspecified, multicast, or CGNAT/Tailscale
// 100.64.0.0/10.
func IsBlockedIP(ip net.IP) bool {
	return IsBlockedIPWith(ip, nil)
}

// IsBlockedIPWith is IsBlockedIP with an operator allowlist. See Allow.
func IsBlockedIPWith(ip net.IP, allow Allow) bool {
	if ip == nil {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	// Never allowlistable, whatever the operator says: link-local covers cloud
	// instance metadata, and multicast/unspecified are not destinations.
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return true
	}
	if allow.permits(ip) {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() {
		return true
	}
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
		return true // 100.64.0.0/10 CGNAT / Tailscale
	}
	return false
}

// Client returns an http.Client that blocks connections to non-public addresses
// at dial time and re-validates every redirect hop.
func Client(timeout time.Duration) *http.Client {
	return ClientWithAllow(timeout, nil)
}

// ClientWithAllow is Client with an operator-declared allowlist (see Allow).
//
// A SEPARATE constructor rather than a package-level setting, on purpose: the
// allowance belongs to one caller. Observe fetches on several paths — webhook
// delivery, integrations, uptime monitoring — and only webhook delivery has an
// operator who has declared where their own services live. A global switch
// would have extended that trust to every fetcher, including ones whose URL
// comes from somewhere less deliberate.
func ClientWithAllow(timeout time.Duration, allow Allow) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	dialer.Control = func(network, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		if IsBlockedIPWith(net.ParseIP(host), allow) {
			return fmt.Errorf("netsafe: blocked connection to non-public address %s", host)
		}
		return nil
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{DialContext: dialer.DialContext},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("netsafe: too many redirects")
			}
			return ValidateURL(req.URL.String())
		},
	}
}

// DialControl exposes the dial-time guard for callers that build their own
// dialer (e.g. raw TCP checks). network is unused; address is "host:port".
func DialControl(_ context.Context, _, address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	if IsBlockedIP(net.ParseIP(host)) {
		return fmt.Errorf("netsafe: blocked connection to non-public address %s", host)
	}
	return nil
}
