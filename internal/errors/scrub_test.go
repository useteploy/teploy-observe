package errors

import (
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy-observe/internal/ingest"
)

func newScrubBuf() *ErrorBuffer {
	return NewErrorBuffer(nil, 50000, 100, time.Hour, slog.New(slog.DiscardHandler))
}

func TestScrubKeysCaseInsensitiveRecursive(t *testing.T) {
	s := NewScrubber()
	in := ErrorInput{
		Extra: map[string]any{
			"Password": "hunter2",
			"nested": map[string]any{
				"API-Key": "k",
				"list": []any{
					map[string]any{"Set-Cookie": "a=b", "ok": "fine"},
					"plain",
				},
				"apiKey": 12345,
			},
			"author":  "keep me",
			"user_id": "u1",
		},
		Contexts: map[string]any{"request": map[string]any{"headers": map[string]any{
			"Authorization": "Bearer abcdefghij", "X-CSRF-Token": "t", "Accept": "json"}}},
		Breadcrumbs: []Breadcrumb{{Message: "login", Data: map[string]any{"ssn": "123-45-6789", "page": "/x"}}},
	}
	out := s.ScrubInput(in)
	b, _ := json.Marshal(out)
	js := string(b)
	for _, leak := range []string{"hunter2", "a=b", "abcdefghij", "123-45-6789", "12345"} {
		if strings.Contains(js, leak) {
			t.Fatalf("leaked %q in %s", leak, js)
		}
	}
	for _, keep := range []string{"keep me", "fine", "plain", "json", "/x", "u1"} {
		if !strings.Contains(js, keep) {
			t.Fatalf("over-scrubbed: lost %q in %s", keep, js)
		}
	}
	// Input must not be mutated.
	if in.Extra.(map[string]any)["Password"] != "hunter2" {
		t.Fatal("ScrubInput mutated the caller's map")
	}
}

func TestScrubValuePatterns(t *testing.T) {
	s := NewScrubber()
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTYifQ.SflKxwRJSMeKKF2QT4fwpM"
	in := ErrorInput{
		ErrorValue:  "bad token " + jwt + " and Authorization: Bearer abc.def-ghi_jkl123 end; card 4111 1111 1111 1111",
		URL:         "https://x.test/p?ok=1&api_key=SECRET1&key=SECRET2&Sig=zzz",
		Breadcrumbs: []Breadcrumb{{Message: "GET /a?token=SECRET3&q=hi", Data: "password=SECRET4 trailing"}},
	}
	out := s.ScrubInput(in)
	b, _ := json.Marshal(out)
	js := string(b)
	for _, leak := range []string{jwt, "abc.def-ghi_jkl123", "4111", "SECRET1", "SECRET2", "SECRET3", "SECRET4", "=zzz"} {
		if strings.Contains(js, leak) {
			t.Fatalf("leaked %q in %s", leak, js)
		}
	}
	if !strings.Contains(out.URL, "ok=1") || !strings.Contains(out.Breadcrumbs[0].Message, "q=hi") {
		t.Fatalf("benign query params lost: %q / %q", out.URL, out.Breadcrumbs[0].Message)
	}
	if !strings.Contains(out.ErrorValue, "Bearer "+Filtered) {
		t.Fatalf("bearer not filtered: %q", out.ErrorValue)
	}
}

func TestScrubLuhn(t *testing.T) {
	s := NewScrubber()
	positives := []string{"4111111111111111", "4111-1111-1111-1111", "5500 0000 0000 0004", "378282246310005", "6011111111111117"}
	for _, p := range positives {
		if got := s.scrubString("pay " + p + " now"); !strings.Contains(got, Filtered) || strings.Contains(got, p) {
			t.Errorf("Luhn positive %q not filtered: %q", p, got)
		}
	}
	negatives := []string{"4111111111111112", "1234567890123456", "1700000000000", "request id 123456789"}
	for _, n := range negatives {
		if got := s.scrubString(n); got != n {
			t.Errorf("negative %q altered: %q", n, got)
		}
	}
}

func TestScrubNonStringValuesUnderSecretKeys(t *testing.T) {
	s := NewScrubber()
	out := s.ScrubInput(ErrorInput{Extra: map[string]any{
		"token": 42, "credit_card": map[string]any{"n": 1}, "ok": 7, "flag": true, "nil": nil,
		"cards": []any{1, 2}, "session_id": true,
	}})
	m := out.Extra.(map[string]any)
	for _, k := range []string{"token", "credit_card", "session_id"} {
		if m[k] != Filtered {
			t.Errorf("%s = %v", k, m[k])
		}
	}
	if m["ok"] != float64(7) && m["ok"] != 7 || m["flag"] != true || m["nil"] != nil {
		t.Errorf("non-secret scalars changed: %v", m)
	}
}

func TestScrubExtraKeysAndDisable(t *testing.T) {
	t.Setenv("OBSERVE_SCRUB_KEYS", "internal_ref, Tenant-Code")
	s := NewScrubberFromEnv()
	out := s.ScrubInput(ErrorInput{Extra: map[string]any{"InternalRef": "x", "tenantCode": "y", "other": "z"}})
	m := out.Extra.(map[string]any)
	if m["InternalRef"] != Filtered || m["tenantCode"] != Filtered || m["other"] != "z" {
		t.Fatalf("extra keys: %v", m)
	}
	t.Setenv("OBSERVE_SCRUB_DISABLE", "true")
	d := NewScrubberFromEnv()
	if !d.Disabled() {
		t.Fatal("disable not honoured")
	}
	in := ErrorInput{Extra: map[string]any{"password": "p"}}
	if got := d.ScrubInput(in).Extra.(map[string]any)["password"]; got != "p" {
		t.Fatalf("disabled scrubber altered input: %v", got)
	}
}

func TestScrubDepthAndSizeBounded(t *testing.T) {
	s := NewScrubber()
	// Deep chain with a secret beyond the depth limit: fail-closed.
	var deep any = map[string]any{"leaf": "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTYifQ.sig12345"}
	for i := 0; i < 100; i++ {
		deep = map[string]any{"n": deep}
	}
	b, _ := json.Marshal(s.ScrubInput(ErrorInput{Extra: deep}))
	if strings.Contains(string(b), "eyJ") || !strings.Contains(string(b), truncated) {
		t.Fatalf("deep payload not truncated fail-closed: %.200s", b)
	}
	// Huge breadth: performance guard.
	big := make([]any, 200000)
	for i := range big {
		big[i] = map[string]any{"k": strings.Repeat("a", 50), "password": "p"}
	}
	start := time.Now()
	out := s.ScrubInput(ErrorInput{Extra: big, ErrorValue: strings.Repeat("x ", 100000)})
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("scrub of huge payload took %v", d)
	}
	if got := len(out.Extra.([]any)); got != len(big) {
		t.Fatalf("len changed %d", got)
	}
	if tail := out.Extra.([]any)[len(big)-1]; tail != truncated {
		t.Fatalf("beyond node budget must be %q, got %v", truncated, tail)
	}
}

// Golden: scrubbing at Push must not move any event to another group.
func TestScrubDoesNotChangeGrouping(t *testing.T) {
	frames := []StackFrame{{Filename: "https://a.test/app.js?token=SECRET", Function: "doIt", InApp: true}}
	cases := []ErrorInput{
		{ErrorType: "TypeError", ErrorValue: "x is not a function, token=abc12345", StackTrace: frames},
		{ErrorType: "Error", ErrorValue: "Bearer abcdefghijkl rejected for 4111111111111111"},
		{ErrorType: "RageClick", URL: "https://x.test/p?api_key=ZZZ#frag", Selector: "#buy"},
		{ErrorType: "E", ErrorValue: "v", Fingerprint: []string{"custom", "password=hunter2"}},
		{},
	}
	b := newScrubBuf()
	for i, in := range cases {
		in.SiteID = "s"
		want := in
		want.URL = ingest.CapturedURL(want.URL)
		if want.ErrorType == "" && want.ErrorValue == "" {
			want.ErrorType, want.ErrorValue = "Error", "Unknown error"
		}
		wantHash, _ := ComputeGroupHash(FingerprintVersion, want)
		if err := b.Push("s", in); err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		var stored ErrorInput
		if err := json.Unmarshal(b.events[len(b.events)-1].Body, &stored); err != nil {
			t.Fatal(err)
		}
		if stored.PreGroupHash != wantHash {
			t.Fatalf("case %d: hash %s != raw-derived %s", i, stored.PreGroupHash, wantHash)
		}
	}
	// The derivation itself stays pinned by o05_grouping_test.go goldens.
}

func TestPushDiscardsClientPreGroupHashAndScrubsBeforeWAL(t *testing.T) {
	b := newScrubBuf()
	in := ErrorInput{ErrorType: "E", ErrorValue: "v password=hunter2", PreGroupHash: "attacker-chosen",
		Extra: map[string]any{"token": "T0KEN"}}
	if err := b.Push("s", in); err != nil {
		t.Fatal(err)
	}
	body := string(b.events[0].Body)
	if strings.Contains(body, "attacker-chosen") || strings.Contains(body, "hunter2") || strings.Contains(body, "T0KEN") {
		t.Fatalf("frozen record leaks: %s", body)
	}
	// Disabled: still discards the client value.
	b2 := newScrubBuf()
	b2.SetScrubber(&Scrubber{disabled: true})
	if err := b2.Push("s", in); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b2.events[0].Body), "attacker-chosen") {
		t.Fatal("client PreGroupHash survived")
	}
}

// Inbox idempotency under scrubbing: retries hash equal, a real
// difference still conflicts.
func TestScrubKeepsInboxIdempotency(t *testing.T) {
	b := newScrubBuf()
	mk := func(val string) ErrorInput {
		return ErrorInput{EventID: "evt-12345678", ErrorType: "E", ErrorValue: val,
			Extra: map[string]any{"password": "p1", "n": 1}}
	}
	if err := b.Push("s", mk("boom")); err != nil {
		t.Fatal(err)
	}
	if err := b.Push("s", mk("boom")); !errors.Is(err, ErrAdmittedDuplicate) {
		t.Fatalf("retry want duplicate, got %v", err)
	}
	if err := b.Push("s", mk("different")); !errors.Is(err, ErrEventIDConflict) {
		t.Fatalf("changed payload want conflict, got %v", err)
	}
	// Differing only in a filtered secret dedupes (documented in scrub.go).
	r := mk("boom")
	r.Extra = map[string]any{"password": "OTHER", "n": 1}
	if err := b.Push("s", r); !errors.Is(err, ErrAdmittedDuplicate) {
		t.Fatalf("secret-only difference want duplicate, got %v", err)
	}
}
