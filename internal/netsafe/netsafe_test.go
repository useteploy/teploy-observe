package netsafe

import (
	"net"
	"testing"
)

func TestIsBlockedIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "::1", // loopback
		"169.254.169.254",       // cloud metadata (link-local)
		"10.0.0.5", "172.16.0.1", "192.168.1.1", // RFC1918
		"100.64.0.1", "100.100.100.100", // CGNAT / Tailscale
		"0.0.0.0",   // unspecified
		"fc00::1",   // ULA
		"fe80::1",   // link-local v6
		"224.0.0.1", // multicast
	}
	for _, s := range blocked {
		if !IsBlockedIP(net.ParseIP(s)) {
			t.Errorf("IsBlockedIP(%s) = false, want true (must be blocked)", s)
		}
	}
	allowed := []string{"1.1.1.1", "8.8.8.8", "93.184.216.34", "2606:2800:220:1::"}
	for _, s := range allowed {
		if IsBlockedIP(net.ParseIP(s)) {
			t.Errorf("IsBlockedIP(%s) = true, want false (public address)", s)
		}
	}
	if !IsBlockedIP(nil) {
		t.Error("IsBlockedIP(nil) must be true (fail closed on unparseable host)")
	}
}

func TestValidateURL(t *testing.T) {
	ok := []string{"https://example.com", "http://example.com:8080/x"}
	for _, u := range ok {
		if err := ValidateURL(u); err != nil {
			t.Errorf("ValidateURL(%s) = %v, want nil", u, err)
		}
	}
	bad := []string{
		"ftp://example.com",          // scheme
		"file:///etc/passwd",         // scheme
		"https://user:pw@example.com", // userinfo
		"https://",                    // no host
		"notaurl",                     // no host/scheme
	}
	for _, u := range bad {
		if err := ValidateURL(u); err == nil {
			t.Errorf("ValidateURL(%s) = nil, want error", u)
		}
	}
}
