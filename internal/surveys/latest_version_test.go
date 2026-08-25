package surveys

import (
	"context"
	"os"
	"testing"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
)

// surveys as 007_wave2 declares it.
const surveyColumns = `(
	survey_id      TEXT NOT NULL,
	tenant_id      TEXT NOT NULL DEFAULT 'default',
	site_id        TEXT NOT NULL,
	name           TEXT NOT NULL DEFAULT '',
	questions      JSONB,
	appearance     JSONB,
	targeting      JSONB,
	status         TEXT NOT NULL DEFAULT 'draft',
	created_at     TEXT NOT NULL,
	version        BIGINT NOT NULL DEFAULT 0
)`

// TestSurveyStatusResolvesLatestVersion covers both SDK-facing reads. Activate
// and Close write new versions of the row, and with the superseded ones
// readable, GetActive matched a closed survey through its old status='active'
// row and SubmitResponse — a public, unauthenticated endpoint whose only gate
// is this lookup — decided ownership and status against whichever version came
// back first.
func TestSurveyStatusResolvesLatestVersion(t *testing.T) {
	dsn := os.Getenv("OBSERVE_NUCLEUS_URL")
	if dsn == "" {
		t.Skip("no OBSERVE_NUCLEUS_URL")
	}
	db, err := nucleus.Connect(context.Background(), dsn)
	if err != nil {
		t.Skipf("connect: %v", err)
	}
	nucleustest.AsPlainMergeTree(t, db, "surveys", surveyColumns,
		"(tenant_id, site_id, survey_id)", "version")

	svc := NewSurveyService(db)
	ctx := context.Background()
	const site = "surveysite"

	s, err := svc.Create(ctx, site, "NPS", `[{"id":"q1","type":"rating","text":"?"}]`, "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.Activate(ctx, s.SurveyID); err != nil {
		t.Fatalf("activate: %v", err)
	}
	if _, err := svc.SubmitResponse(ctx, s.SurveyID, site, "u1", map[string]any{"q1": 9}); err != nil {
		t.Fatalf("submit while active: %v", err)
	}
	if err := svc.Close(ctx, s.SurveyID); err != nil {
		t.Fatalf("close: %v", err)
	}

	active, err := svc.GetActive(ctx, site)
	if err != nil {
		t.Fatalf("get active: %v", err)
	}
	for _, got := range active {
		if got.SurveyID == s.SurveyID {
			t.Fatalf("a closed survey is still served as active — status was filtered before the collapse")
		}
	}

	if _, err := svc.SubmitResponse(ctx, s.SurveyID, site, "u2", map[string]any{"q1": 1}); err == nil {
		t.Fatalf("a closed survey still accepts public responses")
	}

	// The ownership half of the same gate: another site must never pass.
	if _, err := svc.SubmitResponse(ctx, s.SurveyID, "othersite", "u3", map[string]any{"q1": 1}); err == nil {
		t.Fatalf("cross-site submission accepted")
	}

	list, err := svc.List(ctx, site)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var seen int
	for _, got := range list {
		if got.SurveyID != s.SurveyID {
			continue
		}
		seen++
		if got.Status != "closed" {
			t.Fatalf("List reported status %q, want closed", got.Status)
		}
	}
	if seen != 1 {
		t.Fatalf("List returned the survey %d times, want 1 — one row per surviving version", seen)
	}
}
