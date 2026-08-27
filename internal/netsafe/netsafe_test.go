package netsafe

import (
	"net"
	"testing"
	"time"
)

func TestIsBlockedIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "::1", // loopback
		"169.254.169.254",                       // cloud metadata (link-local)
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
		"ftp://example.com",           // scheme
		"file:///etc/passwd",          // scheme
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

// --- operator allowlist (Allow) ---------------------------------------------
//
// The guard is correct and, for a self-hosted fleet, total: every Teploy
// service lives on a tailnet (100.64.0.0/10), so an Observe alert aimed at a
// self-hosted Ship could never be delivered — blocked at dial time for being
// exactly what it was supposed to be. These tests pin the shape of the escape
// hatch so nobody later "fixes" it by widening the guard itself.

func TestParseAllowAcceptsTheFormsAnOperatorWillType(t *testing.T) {
	allow, err := ParseAllow("100.64.0.0/10, 10.0.0.0/8")
	if err != nil {
		t.Fatalf("comma-separated CIDRs: %v", err)
	}
	if len(allow) != 2 {
		t.Fatalf("want 2 networks, got %d", len(allow))
	}
	// A bare IP is the first thing someone naming one host reaches for.
	single, err := ParseAllow("100.76.150.126")
	if err != nil {
		t.Fatalf("bare IP: %v", err)
	}
	if !single.permits(net.ParseIP("100.76.150.126")) {
		t.Fatal("a bare IP must permit exactly itself")
	}
	if single.permits(net.ParseIP("100.76.150.127")) {
		t.Fatal("a bare IP must not permit its neighbour")
	}
	if _, err := ParseAllow(""); err != nil {
		t.Fatalf("empty is not an error: %v", err)
	}
	if _, err := ParseAllow("not-an-ip"); err == nil {
		t.Fatal("a malformed entry must be an error, not silently dropped")
	}
	if _, err := ParseAllow("10.0.0.0/99"); err == nil {
		t.Fatal("a malformed CIDR must be an error")
	}
}

func TestEmptyAllowIsExactlyTheOldBehaviour(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "10.1.2.3", "192.168.1.1", "169.254.169.254", "100.64.1.1", "0.0.0.0"} {
		if !IsBlockedIPWith(net.ParseIP(ip), nil) {
			t.Errorf("%s must stay blocked with no allowlist", ip)
		}
		if IsBlockedIPWith(net.ParseIP(ip), nil) != IsBlockedIP(net.ParseIP(ip)) {
			t.Errorf("%s: IsBlockedIP and IsBlockedIPWith(nil) must agree", ip)
		}
	}
	if IsBlockedIPWith(net.ParseIP("93.184.216.34"), nil) {
		t.Error("a public address must still be reachable")
	}
}

func TestAllowPermitsTheTailnetItNames(t *testing.T) {
	allow, err := ParseAllow("100.64.0.0/10")
	if err != nil {
		t.Fatal(err)
	}
	if IsBlockedIPWith(net.ParseIP("100.76.150.126"), allow) {
		t.Error("the declared tailnet must be reachable — this is the whole point")
	}
	// Declaring the tailnet says nothing about anywhere else.
	if !IsBlockedIPWith(net.ParseIP("10.1.2.3"), allow) {
		t.Error("RFC1918 was not declared and must stay blocked")
	}
	if !IsBlockedIPWith(net.ParseIP("127.0.0.1"), allow) {
		t.Error("loopback was not declared and must stay blocked")
	}
}

// The line that must not move. 169.254.169.254 is cloud instance metadata —
// credentials — and no configuration should be able to reach it. An operator
// who writes 0.0.0.0/0 gets a warning-shaped surprise, not the keys.
func TestLinkLocalIsNeverAllowlistable(t *testing.T) {
	for _, cidr := range []string{"169.254.0.0/16", "169.254.169.254/32", "0.0.0.0/0"} {
		allow, err := ParseAllow(cidr)
		if err != nil {
			t.Fatal(err)
		}
		if !IsBlockedIPWith(net.ParseIP("169.254.169.254"), allow) {
			t.Errorf("cloud metadata must stay blocked even when %q is declared", cidr)
		}
		if !IsBlockedIPWith(net.ParseIP("224.0.0.1"), allow) {
			t.Errorf("multicast is not a destination, even when %q is declared", cidr)
		}
		if !IsBlockedIPWith(net.ParseIP("0.0.0.0"), allow) {
			t.Errorf("the unspecified address is not a destination, even when %q is declared", cidr)
		}
	}
}

// Allowing by CIDR rather than by hostname is what preserves the guard's
// rebinding property: the decision is still made at dial time about an IP.
func TestAllowIsAddressesNotNames(t *testing.T) {
	if _, err := ParseAllow("ship.internal"); err == nil {
		t.Fatal("a hostname must be refused — allowing by name would hand back DNS rebinding")
	}
}

func TestClientWithAllowDialsWhatItWasTold(t *testing.T) {
	// A loopback listener stands in for "a private address the operator named".
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_, _ = conn.Write([]byte("HTTP/1.1 204 No Content\r\nContent-Length: 0\r\n\r\n"))
			conn.Close()
		}
	}()
	url := "http://" + listener.Addr().String() + "/"

	if _, err := Client(2 * time.Second).Get(url); err == nil {
		t.Fatal("the default client must refuse a private address")
	}
	allow, err := ParseAllow("127.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := ClientWithAllow(2*time.Second, allow).Get(url)
	if err != nil {
		t.Fatalf("a declared address must be reachable: %v", err)
	}
	resp.Body.Close()
}
