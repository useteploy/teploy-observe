package replays

import (
	"encoding/json"
	"strings"
	"testing"
)

func batch(t *testing.T, events string) *IngestInput {
	t.Helper()
	var in IngestInput
	if err := json.Unmarshal([]byte(`{"events":`+events+`}`), &in); err != nil {
		t.Fatal(err)
	}
	return &in
}

func TestCaptureEventsAccepted(t *testing.T) {
	in := batch(t, `[
	 {"type":"console","timestamp":1,"data":{"level":"warn","message":"hello","truncated":false}},
	 {"type":"network","timestamp":2,"data":{"method":"GET","url":"https://a.test/x?token=abc#f","status":200,"duration_ms":12,"size":300,"kind":"fetch"}},
	 {"type":"network","timestamp":3,"data":{"method":"POST","url":"https://a.test/y","status":0,"duration_ms":5}},
	 {"type":"click","timestamp":4,"data":{"x":1}}]`)
	if err := validateCaptureEvents(in); err != nil {
		t.Fatal(err)
	}
	got := in.Events[1].Data.(map[string]any)["url"]
	if got != "https://a.test/x" {
		t.Fatalf("query/fragment not stripped: %v", got)
	}
}

func TestCaptureEventsRejected(t *testing.T) {
	big := strings.Repeat("a", maxConsoleMessageBytes+1)
	netOK := `"method":"GET","url":"https://a.test/","status":200,"duration_ms":1`
	bad := map[string]string{
		"unknown level":  `{"type":"console","timestamp":1,"data":{"level":"debug","message":"x"}}`,
		"oversize msg":   `{"type":"console","timestamp":1,"data":{"level":"log","message":"` + big + `"}}`,
		"extra key args": `{"type":"console","timestamp":1,"data":{"level":"log","message":"x","args":[1]}}`,
		"non-object":     `{"type":"console","timestamp":1,"data":"x"}`,
		"net headers":    `{"type":"network","timestamp":1,"data":{` + netOK + `,"headers":{"a":"b"}}}`,
		"net body":       `{"type":"network","timestamp":1,"data":{` + netOK + `,"body":"x"}}`,
		"net bad scheme": `{"type":"network","timestamp":1,"data":{"method":"GET","url":"javascript:alert(1)","status":200,"duration_ms":1}}`,
		"net long url":   `{"type":"network","timestamp":1,"data":{"method":"GET","url":"https://a.test/` + strings.Repeat("p", 3000) + `","status":200,"duration_ms":1}}`,
		"net bad method": `{"type":"network","timestamp":1,"data":{"method":"get\n","url":"https://a.test/","status":200,"duration_ms":1}}`,
		"net status":     `{"type":"network","timestamp":1,"data":{"method":"GET","url":"https://a.test/","status":1000,"duration_ms":1}}`,
		"net fractional": `{"type":"network","timestamp":1,"data":{"method":"GET","url":"https://a.test/","status":200,"duration_ms":1.5}}`,
		"net neg size":   `{"type":"network","timestamp":1,"data":{` + netOK + `,"size":-1}}`,
		"net bad kind":   `{"type":"network","timestamp":1,"data":{` + netOK + `,"kind":"ws"}}`,
		"net huge dur":   `{"type":"network","timestamp":1,"data":{"method":"GET","url":"https://a.test/","status":200,"duration_ms":99999999999}}`,
	}
	for name, ev := range bad {
		if err := validateCaptureEvents(batch(t, "["+ev+"]")); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestCaptureBatchCounts(t *testing.T) {
	c := `{"type":"console","timestamp":1,"data":{"level":"log","message":"x"}}`
	n := `{"type":"network","timestamp":1,"data":{"method":"GET","url":"https://a.test/","status":200,"duration_ms":1}}`
	if err := validateCaptureEvents(batch(t, "["+strings.Repeat(c+",", maxConsoleEventsPerBatch)+c+"]")); err == nil {
		t.Error("console flood accepted")
	}
	if err := validateCaptureEvents(batch(t, "["+strings.Repeat(n+",", maxNetworkEventsPerBatch)+n+"]")); err == nil {
		t.Error("network flood accepted")
	}
	if err := validateCaptureEvents(batch(t, "["+strings.Repeat(c+",", maxConsoleEventsPerBatch-1)+c+"]")); err != nil {
		t.Errorf("at-cap console batch rejected: %v", err)
	}
}

func TestScrubConsoleText(t *testing.T) {
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NSJ9.c2lnbmF0dXJl"
	cases := map[string]string{
		"password=hunter2 ok":                "hunter2",
		`{"token":"abcd1234"}`:               "abcd1234",
		"Authorization: Bearer abc123def456": "abc123def456",
		"jwt " + jwt:                         jwt,
		"key " + strings.Repeat("a1", 20):    strings.Repeat("a1", 20),
		"api_key: sk_live_12345":             "sk_live_12345",
	}
	for in, secret := range cases {
		if out := ScrubConsoleText(in); strings.Contains(out, secret) {
			t.Errorf("secret survived: %q -> %q", in, out)
		}
	}
	if out := ScrubConsoleText("plain message 42"); out != "plain message 42" {
		t.Errorf("benign text altered: %q", out)
	}
}

func TestServerRescrubsAndTruncates(t *testing.T) {
	in := batch(t, `[{"type":"console","timestamp":1,"data":{"level":"error","message":"password=hunter2 boom"}}]`)
	if err := validateCaptureEvents(in); err != nil {
		t.Fatal(err)
	}
	if m := in.Events[0].Data.(map[string]any)["message"].(string); strings.Contains(m, "hunter2") {
		t.Fatalf("not re-scrubbed: %q", m)
	}
}

func TestScrubNetworkURLOpaqueSegment(t *testing.T) {
	u, ok := scrubNetworkURL("https://user:pw@a.test/v1/reset/Abcdef0123456789Abcdef0123456789/x?a=1")
	if !ok || strings.Contains(u, "Abcdef") || strings.Contains(u, "pw") || strings.Contains(u, "?") {
		t.Fatalf("got %q ok=%v", u, ok)
	}
}
