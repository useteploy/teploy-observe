package main

import (
	"encoding/json"
	"testing"
)

// Regression pin for the stream-ticket "invalid token version" 401: the
// pinned neutronauth parses JWT claims with UseNumber, so the "tv" claim
// arrives as json.Number, and the former bare claims["tv"].(float64)
// assertion rejected every real login token. The live mint path is exercised
// end-to-end by e2e/tests/dashboard-replay-journey.spec.ts (the player
// opens a stream ticket); this pins the decoding itself.
func TestClaimTVAcceptsBothNumericRegimes(t *testing.T) {
	if v, ok := claimTV(float64(3)); !ok || v != 3 {
		t.Fatalf("float64 regime: got (%d, %v)", v, ok)
	}
	if v, ok := claimTV(json.Number("3")); !ok || v != 3 {
		t.Fatalf("json.Number regime: got (%d, %v)", v, ok)
	}
	if v, ok := claimTV(json.Number("0")); !ok || v != 0 {
		t.Fatalf("zero version: got (%d, %v)", v, ok)
	}
	for _, bad := range []any{nil, "3", json.Number("not-a-number"), true} {
		if _, ok := claimTV(bad); ok {
			t.Fatalf("non-numeric claim %v must not decode", bad)
		}
	}
}
