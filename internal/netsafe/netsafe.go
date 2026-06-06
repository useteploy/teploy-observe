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

// IsBlockedIP reports whether ip is a non-public address that server-side
// fetchers must not reach: loopback, link-local (incl. 169.254.169.254 cloud
// metadata), private RFC1918/ULA, unspecified, multicast, or CGNAT/Tailscale
// 100.64.0.0/10.
func IsBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || ip.IsPrivate() {
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
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	dialer.Control = func(network, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		if IsBlockedIP(net.ParseIP(host)) {
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
