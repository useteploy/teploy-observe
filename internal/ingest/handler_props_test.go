package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// propsTestCtx returns a context carrying a real (non-bot) user agent so the
// handler doesn't short-circuit, plus no key-bound site so the body site_id is
// authoritative.
func propsTestCtx() context.Context {
	return context.WithValue(context.Background(), keyUserAgent,
		"Mozilla/5.0 (Macintosh) AppleWebKit/537.36 Chrome/120 Safari/537.36")
}

// ingestJSON decodes a raw payload the way the HTTP layer does, runs it
// through the real handler and returns the buffered event. The buffer is
// created with a nil db — nothing is flushed, so no Nucleus is needed.
func ingestJSON(t *testing.T, raw string) Event {
	t.Helper()
	var in IngestInput
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		t.Fatalf("decode: %v", err)
	}
	buf := NewBuffer(nil, 100, 1000, time.Hour, nil)
	h := Handler(buf, "test-salt", nil)
	if _, err := h(propsTestCtx(), in); err != nil {
		t.Fatalf("handler: %v", err)
	}
	buf.mu.Lock()
	defer buf.mu.Unlock()
	if len(buf.events) != 1 {
		t.Fatalf("expected 1 buffered event, got %d", len(buf.events))
	}
	return buf.events[0]
}

// TestIngest_LegacyFlatPropsAreCollected is the regression for the browser SDK
// spreading custom event props at the top level of the payload: the server read
// only a nested `properties` object, so every custom property was stored as {}.
// Payloads in that shape are already deployed (the npm SDK, and the from-posthog
// migration recipe), so the server collects unknown top-level keys instead.
func TestIngest_LegacyFlatPropsAreCollected(t *testing.T) {
	// Exactly what sdk/browser's track("signup", {plan, seats}) used to emit.
	e := ingestJSON(t, `{
		"site_id": "default",
		"event_type": "signup",
		"plan": "pro",
		"seats": 4,
		"distinct_id": "u_123",
		"release": "v1.4.2"
	}`)

	if got := propertiesJSON(e.Properties); got == "{}" {
		t.Fatalf("stored properties are %s — every custom prop was dropped", got)
	}
	if got := e.Properties["plan"]; got != "pro" {
		t.Fatalf("properties[plan] = %v, want \"pro\" (flat prop was dropped)", got)
	}
	if got, ok := e.Properties["seats"].(float64); !ok || got != 4 {
		t.Fatalf("properties[seats] = %v, want 4", e.Properties["seats"])
	}
	// Real top-level fields must NOT be duplicated into properties.
	for _, k := range []string{"site_id", "event_type", "distinct_id", "release"} {
		if _, ok := e.Properties[k]; ok {
			t.Fatalf("properties must not contain reserved key %q", k)
		}
	}
	if e.ReleaseTag != "v1.4.2" {
		t.Fatalf("release_tag = %q, want v1.4.2", e.ReleaseTag)
	}
	if e.DistinctID == "" {
		t.Fatal("distinct_id was not recorded")
	}
}

// TestIngest_PageviewFieldsStayTopLevel guards the other half: url/referrer/
// title/language/screen are fields the server genuinely reads, and must keep
// populating the event rather than being swept into properties.
func TestIngest_PageviewFieldsStayTopLevel(t *testing.T) {
	e := ingestJSON(t, `{
		"site_id": "default",
		"event_type": "pageview",
		"pathname": "/pricing?ref=hn",
		"url": "https://example.com/pricing?utm_source=hn",
		"referrer": "https://news.ycombinator.com/item?id=1",
		"title": "Pricing",
		"language": "en-US",
		"screen": "1920x1080"
	}`)

	if e.URL != "https://example.com/pricing?utm_source=hn" {
		t.Fatalf("url = %q", e.URL)
	}
	if e.Title != "Pricing" {
		t.Fatalf("title = %q", e.Title)
	}
	if e.Referrer != "https://news.ycombinator.com/item" {
		t.Fatalf("referrer = %q", e.Referrer)
	}
	if e.Language != "en-US" {
		t.Fatalf("language = %q", e.Language)
	}
	if e.ScreenWidth != 1920 || e.ScreenHeight != 1080 {
		t.Fatalf("screen = %dx%d", e.ScreenWidth, e.ScreenHeight)
	}
	if e.Hostname != "example.com" || e.Pathname != "/pricing" {
		t.Fatalf("hostname/pathname = %q/%q", e.Hostname, e.Pathname)
	}
	if e.UTMSource != "hn" {
		t.Fatalf("utm_source = %q", e.UTMSource)
	}
	// `pathname` is not a field the server reads — the server derives it from
	// the url — so a client-sent one is kept as a property rather than lost.
	if got := e.Properties["pathname"]; got != "/pricing?ref=hn" {
		t.Fatalf("properties[pathname] = %v", got)
	}
}

// TestIngest_NestedPropertiesStillWin covers the shape the embedded tracker
// (cmd/observe/tracker/observe.js) already sends, and the merge precedence.
func TestIngest_NestedPropertiesStillWin(t *testing.T) {
	e := ingestJSON(t, `{
		"site_id": "default",
		"event_type": "click",
		"properties": {"selector": "a#buy", "text": "Buy"},
		"extra": true
	}`)

	if e.Properties["selector"] != "a#buy" {
		t.Fatalf("nested properties lost: %v", e.Properties)
	}
	if e.Properties["extra"] != true {
		t.Fatalf("flat key not merged: %v", e.Properties)
	}

	// An explicit nested value wins over a same-named flat key.
	e = ingestJSON(t, `{
		"site_id": "default",
		"event_type": "click",
		"properties": {"plan": "nested"},
		"plan": "flat"
	}`)
	if e.Properties["plan"] != "nested" {
		t.Fatalf("properties[plan] = %v, want nested", e.Properties["plan"])
	}
}

// TestIngest_FlatPropsCapped keeps the max-50-properties rule on the lenient
// path: collecting flat keys must never push an otherwise-valid event over the
// limit and get the whole event rejected.
func TestIngest_FlatPropsCapped(t *testing.T) {
	body := map[string]any{"site_id": "default", "event_type": "spam"}
	for i := 0; i < 80; i++ {
		body[fmt.Sprintf("k%02d", i)] = i
	}
	raw, _ := json.Marshal(body)
	e := ingestJSON(t, string(raw))

	if len(e.Properties) != maxProperties {
		t.Fatalf("collected %d properties, want cap of %d", len(e.Properties), maxProperties)
	}
	// Deterministic: the cap keeps the lowest keys in sorted order.
	if _, ok := e.Properties["k00"]; !ok {
		t.Fatal("cap is not deterministic — k00 missing")
	}
	if _, ok := e.Properties["k79"]; ok {
		t.Fatal("cap is not deterministic — k79 should have been dropped")
	}
}

// TestIngest_ExplicitPropertiesOverLimitStillRejected keeps the pre-existing
// behaviour for a client that nests more than 50 properties itself.
func TestIngest_ExplicitPropertiesOverLimitStillRejected(t *testing.T) {
	props := map[string]any{}
	for i := 0; i < 51; i++ {
		props[fmt.Sprintf("k%02d", i)] = i
	}
	raw, _ := json.Marshal(map[string]any{
		"site_id": "default", "event_type": "spam", "properties": props,
	})
	var in IngestInput
	if err := json.Unmarshal(raw, &in); err != nil {
		t.Fatalf("decode: %v", err)
	}
	buf := NewBuffer(nil, 100, 1000, time.Hour, nil)
	if _, err := Handler(buf, "test-salt", nil)(propsTestCtx(), in); err == nil {
		t.Fatal("expected too-many-properties rejection")
	}
}
