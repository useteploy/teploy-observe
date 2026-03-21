package geo

import "testing"

func TestLookup_Known(t *testing.T) {
	// Verify that well-known IPs resolve to non-empty country codes
	knownPublic := []string{"8.8.8.8", "1.1.1.1", "223.255.255.1"}
	for _, ip := range knownPublic {
		got := Lookup(ip)
		if got == "" {
			t.Errorf("Lookup(%q) = empty, want a country code", ip)
		}
		if len(got) != 2 {
			t.Errorf("Lookup(%q) = %q, want 2-letter code", ip, got)
		}
	}
}

func TestLookup_GoogleDNS(t *testing.T) {
	// 8.8.8.8 is Google — allocated to US
	got := Lookup("8.8.8.8")
	if got != "US" {
		t.Errorf("Lookup(8.8.8.8) = %q, want US", got)
	}
}

func TestLookup_Private(t *testing.T) {
	privates := []string{"10.0.0.1", "192.168.1.1", "172.16.0.1", "127.0.0.1"}
	for _, ip := range privates {
		got := Lookup(ip)
		if got != "" {
			t.Errorf("Lookup(%q) = %q, want empty for private IP", ip, got)
		}
	}
}

func TestLookup_Invalid(t *testing.T) {
	if got := Lookup("not-an-ip"); got != "" {
		t.Errorf("invalid IP returned %q", got)
	}
	if got := Lookup(""); got != "" {
		t.Errorf("empty IP returned %q", got)
	}
}

func TestLookup_IPv6(t *testing.T) {
	if got := Lookup("2001:4860:4860::8888"); got != "" {
		t.Errorf("IPv6 returned %q, want empty", got)
	}
}

func BenchmarkLookup(b *testing.B) {
	for i := 0; i < b.N; i++ {
		Lookup("8.8.8.8")
	}
}
