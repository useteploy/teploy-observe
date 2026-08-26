package errors

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestGroupHash_InAppFrames(t *testing.T) {
	frames := []StackFrame{
		{Filename: "/app/src/utils/api.js", Function: "fetchData", InApp: true},
		{Filename: "/app/src/components/Dashboard.tsx", Function: "render", InApp: true},
		{Filename: "node_modules/preact/src/diff.js", Function: "diff", InApp: false},
	}
	h1 := GroupHash("TypeError", "Cannot read property 'id' of undefined", frames)
	h2 := GroupHash("TypeError", "Cannot read property 'name' of undefined", frames)
	// Same type + same in-app frames = same hash, regardless of message
	if h1 != h2 {
		t.Errorf("expected same hash for same frames, got %s vs %s", h1, h2)
	}
}

func TestGroupHash_NoInAppFrames(t *testing.T) {
	frames := []StackFrame{
		{Filename: "node_modules/lib.js", Function: "crash", InApp: false},
	}
	h1 := GroupHash("TypeError", "Cannot read property 'id' of undefined", frames)
	h2 := GroupHash("TypeError", "Cannot read property 'name' of undefined", frames)
	// Different messages = different hashes when no in-app frames
	if h1 == h2 {
		t.Errorf("expected different hashes for different messages, both got %s", h1)
	}
}

func TestGroupHash_NoFrames(t *testing.T) {
	h := GroupHash("ReferenceError", "x is not defined", nil)
	if h == "" {
		t.Error("expected non-empty hash")
	}
}

func TestGroupHash_Deterministic(t *testing.T) {
	frames := []StackFrame{
		{Filename: "/app/src/index.js", Function: "main", InApp: true},
	}
	h1 := GroupHash("Error", "test", frames)
	h2 := GroupHash("Error", "test", frames)
	if h1 != h2 {
		t.Errorf("hash not deterministic: %s vs %s", h1, h2)
	}
}

func TestParameterizeMessage(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{
			"Failed to fetch user 550e8400-e29b-41d4-a716-446655440000",
			"Failed to fetch user {uuid}",
		},
		{
			"Connection refused to 192.168.1.1:5432",
			"Connection refused to {ip}:5432",
		},
		{
			"Email sent to user@example.com failed",
			"Email sent to {email} failed",
		},
		{
			"Request to https://api.example.com/v1/users failed",
			"Request to {url} failed",
		},
		{
			"Record 1234567890 not found",
			"Record {num} not found",
		},
	}
	for _, tt := range tests {
		got := ParameterizeMessage(tt.input)
		if got != tt.expected {
			t.Errorf("ParameterizeMessage(%q)\n  got:  %q\n  want: %q", tt.input, got, tt.expected)
		}
	}
}

func TestIssueCulprit(t *testing.T) {
	frames := []StackFrame{
		{Filename: "node_modules/preact/diff.js", Function: "diff", InApp: false},
		{Filename: "/app/src/components/App.tsx", Function: "handleClick", InApp: true},
		{Filename: "/app/src/utils/api.js", Function: "post", InApp: true},
	}
	culprit := IssueCulprit(frames)
	if culprit != "handleClick in App.tsx" {
		t.Errorf("expected 'handleClick in App.tsx', got %q", culprit)
	}
}

func TestGroupHashRageClick_SameSelectorMerges(t *testing.T) {
	// Two rage clicks on the same selector + same page must collapse into
	// a single issue, even when the URL has different query strings.
	h1 := GroupHashRageClick("https://app.example.com/checkout?ref=hero", "button#submit")
	h2 := GroupHashRageClick("https://app.example.com/checkout?ref=email&utm=foo", "button#submit")
	if h1 != h2 {
		t.Errorf("rage clicks on same selector should merge, got %s vs %s", h1, h2)
	}
}

func TestGroupHashRageClick_DifferentSelectorSplits(t *testing.T) {
	h1 := GroupHashRageClick("https://app.example.com/checkout", "button#submit")
	h2 := GroupHashRageClick("https://app.example.com/checkout", "button#cancel")
	if h1 == h2 {
		t.Errorf("rage clicks on different selectors must not merge: %s", h1)
	}
}

func TestGroupHashRageClick_DifferentPageSplits(t *testing.T) {
	h1 := GroupHashRageClick("https://app.example.com/checkout", "button#submit")
	h2 := GroupHashRageClick("https://app.example.com/cart", "button#submit")
	if h1 == h2 {
		t.Errorf("rage clicks on different pages must not merge: %s", h1)
	}
}

func TestIssueTitle(t *testing.T) {
	title := IssueTitle("TypeError", "Cannot read property 'x' of null")
	if title != "TypeError: Cannot read property 'x' of null" {
		t.Errorf("unexpected title: %s", title)
	}

	title = IssueTitle("", "something broke")
	if title != "something broke" {
		t.Errorf("unexpected title: %s", title)
	}

	title = IssueTitle("Error", "")
	if title != "Error" {
		t.Errorf("unexpected title: %s", title)
	}
}

func TestTruncate(t *testing.T) {
	// ASCII input longer than max: exact byte-length truncation, "..." suffix.
	if out := truncate(strings.Repeat("a", 200), 120); len(out) != 120 || !strings.HasSuffix(out, "...") {
		t.Errorf("expected 120 bytes ending in '...', got %d bytes: %q", len(out), out)
	}

	// Multi-byte input: the cut must not split a rune, so the result stays
	// valid UTF-8 and within the byte budget. 100 runes of 'é' = 200 bytes,
	// so a naive s[:117] would land mid-rune.
	out := truncate(strings.Repeat("é", 100), 120)
	if !utf8.ValidString(out) {
		t.Errorf("expected valid UTF-8, got %q", out)
	}
	if len(out) > 120 {
		t.Errorf("expected at most 120 bytes, got %d", len(out))
	}

	// max < 3 must not panic on a negative slice index.
	if out := truncate("abcdef", 2); out != "" {
		t.Errorf("expected empty string for max < 3, got %q", out)
	}

	// Short input is returned unchanged.
	if out := truncate("short", 120); out != "short" {
		t.Errorf("expected unchanged input, got %q", out)
	}
}
