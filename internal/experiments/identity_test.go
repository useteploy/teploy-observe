package experiments

import (
	"context"
	"strings"
	"testing"

	"github.com/useteploy/teploy-observe/internal/identity"
)

func TestEventDistinctIDMatchesIngestDerivation(t *testing.T) {
	ctx := context.Background()
	svc := NewExperimentService(nil).WithPrivacy(
		func(_ context.Context, siteID string) (string, bool, bool) {
			switch siteID {
			case "site-salted":
				return "site-salt", false, true
			case "site-raw":
				return "ignored", true, true
			}
			return "", false, false
		}, "global-salt")

	// Known site: HMAC under the per-site salt, exactly what ingest stores.
	if got, want := svc.EventDistinctID(ctx, "site-salted", "alice@example.com"),
		identity.HashDistinctID("alice@example.com", "site-salt"); got != want || len(got) != identity.HashedIDLength {
		t.Errorf("salted: got %q want %q", got, want)
	}
	// Raw opt-out site: unchanged.
	if got := svc.EventDistinctID(ctx, "site-raw", "alice@example.com"); got != "alice@example.com" {
		t.Errorf("raw site: got %q", got)
	}
	// Unknown site: global-salt fallback, same as ingest.
	if got, want := svc.EventDistinctID(ctx, "nope", "u1"), identity.HashDistinctID("u1", "global-salt"); got != want {
		t.Errorf("fallback: got %q want %q", got, want)
	}
	// Sites do not share identities.
	if svc.EventDistinctID(ctx, "site-salted", "u1") == svc.EventDistinctID(ctx, "nope", "u1") {
		t.Error("different salts must give different ids")
	}
	// No identify value, no identity.
	if got := svc.EventDistinctID(ctx, "site-salted", ""); got != "" {
		t.Errorf("empty raw id hashed to %q", got)
	}
	// Without a lookup wired the global salt is used.
	if got, want := NewExperimentService(nil).WithPrivacy(nil, "g").EventDistinctID(ctx, "s", "u"), identity.HashDistinctID("u", "g"); got != want {
		t.Errorf("no lookup: got %q want %q", got, want)
	}
}

// Conversions attribute to the user's FIRST exposure, never the latest.
func TestFirstExposureAttributionQuery(t *testing.T) {
	if !strings.Contains(firstExposureSQL, "ORDER BY timestamp ASC LIMIT 1") {
		t.Fatalf("attribution must read the earliest exposure:\n%s", firstExposureSQL)
	}
	if strings.Contains(firstExposureSQL, "DESC") {
		t.Fatalf("attribution must not read the latest exposure:\n%s", firstExposureSQL)
	}
	for _, scope := range []string{"experiment_id = $1", "site_id = $2", "user_id = $3"} {
		if !strings.Contains(firstExposureSQL, scope) {
			t.Errorf("query not scoped by %s", scope)
		}
	}
}
