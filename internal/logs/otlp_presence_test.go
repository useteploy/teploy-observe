package logs

import (
	"encoding/json"
	"testing"
)

// false, 0 and 0.0 set by the producer must render, not collapse to "".
func TestOTLPAny_JSONPreservesZeroValues(t *testing.T) {
	cases := map[string]string{
		`{"boolValue":false}`:  "false",
		`{"boolValue":true}`:   "true",
		`{"intValue":"0"}`:     "0",
		`{"doubleValue":0}`:    "0",
		`{"doubleValue":1.5}`:  "1.5",
		`{"stringValue":""}`:   "",
		`{"stringValue":"ok"}`: "ok",
		`{}`:                   "",
	}
	for in, want := range cases {
		var v otlpAny
		if err := json.Unmarshal([]byte(in), &v); err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if got := v.text(); got != want {
			t.Errorf("%s: got %q want %q", in, got, want)
		}
	}
}
