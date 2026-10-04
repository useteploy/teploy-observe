package experiments

import (
	"strings"
	"testing"
)

func TestMetricEventID_Keyed(t *testing.T) {
	a := metricEventID("e", "s", "u", "primary", "k1")
	if a != metricEventID("e", "s", "u", "primary", "k1") {
		t.Error("same key must give the same id")
	}
	for name, other := range map[string]string{
		"key":    metricEventID("e", "s", "u", "primary", "k2"),
		"user":   metricEventID("e", "s", "u2", "primary", "k1"),
		"metric": metricEventID("e", "s", "u", "rev", "k1"),
		"site":   metricEventID("e", "s2", "u", "primary", "k1"),
		"exp":    metricEventID("e2", "s", "u", "primary", "k1"),
	} {
		if other == a {
			t.Errorf("id must differ when %s differs", name)
		}
	}
	if !strings.HasPrefix(a, "k_") {
		t.Errorf("id = %q", a)
	}
	if metricEventID("e", "s", "u", "primary", "") == metricEventID("e", "s", "u", "primary", "") {
		t.Error("unkeyed ids must be random")
	}
}

func TestEventKeyRE(t *testing.T) {
	for _, ok := range []string{"a", "order-123:v1", strings.Repeat("x", 64)} {
		if !eventKeyRE.MatchString(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", strings.Repeat("x", 65), "a b", "a'b", "a;b"} {
		if eventKeyRE.MatchString(bad) {
			t.Errorf("%q should be rejected", bad)
		}
	}
}
