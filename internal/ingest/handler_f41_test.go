package ingest

import (
	"testing"
)

// F41: explicit campaign fields are the primary attribution path — the
// analytics reads them, not the raw query.
func TestIngest_ExplicitUTMFieldsWin(t *testing.T) {
	e := ingestJSON(t, `{
		"site_id": "default",
		"event_type": "pageview",
		"url": "https://example.com/landing?utm_source=querysrc&utm_campaign=querycamp&email=me@x.io",
		"utm_source": "newsletter",
		"utm_medium": "email",
		"utm_campaign": "launch",
		"utm_term": "observability",
		"utm_content": "cta-a"
	}`)
	if e.UTMSource != "newsletter" || e.UTMMedium != "email" || e.UTMCampaign != "launch" ||
		e.UTMTerm != "observability" || e.UTMContent != "cta-a" {
		t.Fatalf("explicit fields must win: %+v", e)
	}
	// The stored URL carries none of the query — not the utm params, not
	// the email parameter the query also carried.
	if e.URL != "https://example.com/landing" {
		t.Fatalf("stored url must be sanitized, got %q", e.URL)
	}
}

// F41: partial explicit fields fall back per-field to the legacy query
// extraction, so a mixed producer loses nothing.
func TestIngest_UTMLegacyQueryFallback(t *testing.T) {
	e := ingestJSON(t, `{
		"site_id": "default",
		"url": "https://example.com/a?utm_source=src&utm_medium=pay&utm_campaign=cam&utm_term=term&utm_content=cont&token=hunter2",
		"utm_campaign": "override"
	}`)
	if e.UTMCampaign != "override" {
		t.Fatalf("explicit utm_campaign must win, got %q", e.UTMCampaign)
	}
	if e.UTMSource != "src" || e.UTMMedium != "pay" || e.UTMTerm != "term" || e.UTMContent != "cont" {
		t.Fatalf("unset fields must fall back to the query: %+v", e)
	}
	if e.URL != "https://example.com/a" {
		t.Fatalf("stored url must be sanitized (token gone), got %q", e.URL)
	}
}

// F41 server-side backstop: credentials and fragments never reach storage
// even when a producer sends them.
func TestIngest_URLSanitizationDropsUserinfoAndFragment(t *testing.T) {
	e := ingestJSON(t, `{
		"site_id": "default",
		"url": "https://user:secrets@example.com/x?utm_source=s#a/b/c"
	}`)
	if e.URL != "https://example.com/x" {
		t.Fatalf("userinfo/query/fragment must be stripped, got %q", e.URL)
	}
	if e.UTMSource != "s" {
		t.Fatalf("utm extraction still works from the incoming URL, got %q", e.UTMSource)
	}
	// Unparseable / non-http(s) URLs fail closed to empty.
	e = ingestJSON(t, `{"site_id": "default", "url": "javascript:alert(1)//example.com"}`)
	if e.URL != "" {
		t.Fatalf("non-http scheme must fail closed, got %q", e.URL)
	}
}
