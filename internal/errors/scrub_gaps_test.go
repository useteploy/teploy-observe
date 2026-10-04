package errors

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestScrubKeyFoldsFullwidthAndConfusables(t *testing.T) {
	s := NewScrubber()
	for _, k := range []string{
		"ＰＡＳＳＷＯＲＤ",      // fullwidth PASSWORD
		"pаssword",      // Cyrillic a
		"ａｐｉ＿ｋｅｙ",       // fullwidth api_key
		"pass​word",     // zero-width space
		"тoken",         // Cyrillic t
		"AuthorＩzation", // fullwidth I
		"ⅱⅱⅱ",           // roman numerals: must not panic, not sensitive
	} {
		got := s.sensitiveKey(k)
		want := !strings.HasPrefix(k, "ⅱ")
		if got != want {
			t.Errorf("sensitiveKey(%q)=%v want %v", k, got, want)
		}
	}
	out := s.ScrubInput(ErrorInput{Extra: map[string]any{"ＰＡＳＳＷＯＲＤ": "hunter2", "ok": "fine"}})
	b, _ := json.Marshal(out.Extra)
	if strings.Contains(string(b), "hunter2") || !strings.Contains(string(b), "fine") {
		t.Fatalf("extra: %s", b)
	}
}

func TestScrubFreeTextColonAndAuthForms(t *testing.T) {
	s := NewScrubber()
	cases := map[string]string{
		"login failed password: hunter2 for bob":    "hunter2",
		"password = hunter2":                        "hunter2",
		`{"x": 1} secret : 'two words'`:             "two words",
		"Authorization: Basic dXNlcjpwYXNzd29yZA==": "dXNlcjpwYXNzd29yZA",
		"sent Basic dXNlcjpwYXNzd29yZA== upstream":  "dXNlcjpwYXNzd29yZA",
		"ｐａｓｓｗｏｒｄ： hunter2":                         "hunter2",
		"Authorization=Bearer abcdef123456":         "abcdef123456",
	}
	for in, leak := range cases {
		out := s.scrubString(in)
		if strings.Contains(out, leak) {
			t.Errorf("%q leaked %q: %q", in, leak, out)
		}
	}
	// Benign text is untouched.
	for _, in := range []string{"Basic configuration is missing", "the user was not authorized", "ratio: 3 of 4"} {
		if out := s.scrubString(in); out != in {
			t.Errorf("benign %q changed to %q", in, out)
		}
	}
}

func TestScrubJSONInString(t *testing.T) {
	s := NewScrubber()
	in := `request failed: ` + `{"user":"bob","password":"hunter2","nested":{"api_key":"k-123","n":1}}`
	// Pure JSON string value.
	pure := `{"user":"bob","password":"hunter2","nested":{"api_key":"k-123","n":1},"list":[{"token":"t0"}]}`
	out := s.ScrubInput(ErrorInput{Extra: map[string]any{"body": pure}, ErrorValue: pure})
	b, _ := json.Marshal(out)
	for _, leak := range []string{"hunter2", "k-123", "t0\""} {
		if strings.Contains(string(b), leak) {
			t.Fatalf("leaked %q: %s", leak, b)
		}
	}
	if !strings.Contains(string(b), "bob") {
		t.Fatalf("benign content lost: %s", b)
	}
	// Prose around JSON still goes through the pattern rules.
	if strings.Contains(s.scrubString(`failed "password":"hunter2" body`), "hunter2") {
		t.Fatal("embedded JSON pair leaked")
	}
	_ = in

	// JSON nested in a JSON string is bounded and still scrubbed.
	inner, _ := json.Marshal(map[string]any{"password": "deep-secret"})
	mid, _ := json.Marshal(map[string]any{"inner": string(inner)})
	outer, _ := json.Marshal(map[string]any{"mid": string(mid)})
	got := s.scrubString(string(outer))
	if strings.Contains(got, "deep-secret") {
		t.Fatalf("nested JSON leaked: %s", got)
	}

	// Depth bomb: terminates, fail-closed.
	bomb := strings.Repeat("[", 5000) + strings.Repeat("]", 5000)
	_ = s.scrubString(bomb)
	// Malformed JSON falls through unchanged.
	if got := s.scrubString(`{"a": `); got != `{"a": ` {
		t.Fatalf("malformed changed: %q", got)
	}
}

func TestScrubIdentifierFieldsTokensOnlyAndGroupingStable(t *testing.T) {
	s := NewScrubber()
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.sig-abc123"
	in := ErrorInput{
		ErrorType:   "TypeError",
		ErrorValue:  "boom",
		ReleaseTag:  "v1.2.3+" + jwt,
		Selector:    "#login[data-token=abc123secret]",
		Fingerprint: []string{"custom", "key=" + jwt, "1234567890123456"},
		StackTrace:  []StackFrame{{Filename: "a.js", Function: "handler?token=zzz9", InApp: true}},
		Breadcrumbs: []Breadcrumb{{Category: "auth Bearer abcdefgh12345", Message: "m"}},
	}
	out := s.ScrubInput(in)
	b, _ := json.Marshal(out)
	for _, leak := range []string{jwt, "abc123secret", "zzz9", "abcdefgh12345"} {
		if strings.Contains(string(b), leak) {
			t.Fatalf("leaked %q: %s", leak, b)
		}
	}
	// Digit runs in fingerprints are identifiers, not cards: untouched.
	if out.Fingerprint[2] != "1234567890123456" || out.Fingerprint[0] != "custom" {
		t.Fatalf("fingerprint mangled: %v", out.Fingerprint)
	}
	if in.Fingerprint[1] != "key="+jwt || in.StackTrace[0].Function != "handler?token=zzz9" {
		t.Fatal("input mutated")
	}
	// Buffer path pins the hash from RAW input before scrubbing.
	buf := &ErrorBuffer{scrub: s}
	rec := buf.prepareRecord(in)
	want, err := ComputeGroupHash(FingerprintVersion, in)
	if err != nil || rec.PreGroupHash != want {
		t.Fatalf("grouping hash changed by scrubbing: %q vs %q (%v)", rec.PreGroupHash, want, err)
	}
}
