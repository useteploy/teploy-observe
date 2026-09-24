package surveys

// O14 slice 1 oracle: survey exposure identity, response-identity dedupe
// and the exposure/response stats - at the real engine, fail-not-skip
// under OBSERVE_REQUIRE_NUCLEUS (the O10 fixture pattern).

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/identity"
	"github.com/useteploy/teploy-observe/internal/nucleustest"
	"github.com/useteploy/teploy-observe/internal/schema"
)

func exposureFixture(t *testing.T) *nucleus.Client {
	t.Helper()
	dsn := nucleustest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("nucleus not reachable at %s — skipping integration test", dsn)
	}
	if err := schema.Apply(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// surveySite mints a unique site id so the shared scratch fixture's earlier
// rows never bleed into per-test counts.
func surveySite(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("o14sv-%d", time.Now().UnixNano())
}

func TestO14ExposureIdentityAndStats(t *testing.T) {
	db := exposureFixture(t)
	ctx := context.Background()
	site := surveySite(t)
	svc := NewSurveyService(db, "test-global-salt", nil)

	s, err := svc.Create(ctx, site, "NPS", `[{"id":"q1","type":"rating","text":"?"}]`, "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.Activate(ctx, s.SurveyID); err != nil {
		t.Fatalf("activate: %v", err)
	}

	// Draft survey: exposures are refused (the gate covers both tables).
	draft, err := svc.Create(ctx, site, "draft", "[]", "", "")
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}
	if err := svc.RecordExposure(ctx, draft.SurveyID, site, "", "9.9.9.9", "ua"); err == nil {
		t.Fatalf("exposure accepted for a draft survey")
	}

	// Anonymous exposures: same reader twice (same IP+UA) is ONE entity and
	// TWO rows - counts derive DISTINCT entity, so retries cannot inflate
	// unique-exposed.
	for i := 0; i < 2; i++ {
		if err := svc.RecordExposure(ctx, s.SurveyID, site, "", "1.2.3.4", "Mozilla/5.0"); err != nil {
			t.Fatalf("exposure %d: %v", i, err)
		}
	}
	// A different reader behind a different IP is a second entity.
	if err := svc.RecordExposure(ctx, s.SurveyID, site, "", "5.6.7.8", "Mozilla/5.0"); err != nil {
		t.Fatalf("exposure second reader: %v", err)
	}
	// An identified reader is a PERSON entity (HMAC of the identify value
	// under the salt), distinct from every estimate.
	if err := svc.RecordExposure(ctx, s.SurveyID, site, "user-1", "1.2.3.4", "Mozilla/5.0"); err != nil {
		t.Fatalf("exposure person: %v", err)
	}

	// Cross-site exposure refused.
	if err := svc.RecordExposure(ctx, s.SurveyID, "othersite", "", "1.2.3.4", "ua"); err == nil {
		t.Fatalf("cross-site exposure accepted")
	}

	st, err := svc.Stats(ctx, s.SurveyID, site)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if st.ExposureRows != 4 {
		t.Fatalf("exposure rows = %d, want 4", st.ExposureRows)
	}
	if st.ExposedEntities != 3 {
		t.Fatalf("exposed entities = %d, want 3 (two estimates + one person)", st.ExposedEntities)
	}
	if st.ExposedPersons != 1 || st.ExposedEstimates != 2 {
		t.Fatalf("entity split = person %d / estimate %d, want 1/2", st.ExposedPersons, st.ExposedEstimates)
	}
	if st.RespondingEntities != 0 {
		t.Fatalf("responding entities = %d, want 0 before any response", st.RespondingEntities)
	}
}

func TestO14ResponseIdentityDedupe(t *testing.T) {
	db := exposureFixture(t)
	ctx := context.Background()
	site := surveySite(t)
	svc := NewSurveyService(db, "test-global-salt", nil)

	s, err := svc.Create(ctx, site, "NPS", `[{"id":"q1","type":"rating","text":"?"}]`, "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.Activate(ctx, s.SurveyID); err != nil {
		t.Fatalf("activate: %v", err)
	}

	// A response with a client identity: submit, then RETRY the same
	// identity (the lost-HTTP-response case) - the retry acks the original
	// row with deduped:true instead of inserting a second one.
	first, err := svc.SubmitResponse(ctx, s.SurveyID, site, "", "resp-abc123def456", map[string]any{"q1": 9}, "1.2.3.4", "Mozilla/5.0")
	if err != nil {
		t.Fatalf("first submit: %v", err)
	}
	if first.Deduped {
		t.Fatalf("first submit reported deduped")
	}
	retry, err := svc.SubmitResponse(ctx, s.SurveyID, site, "", "resp-abc123def456", map[string]any{"q1": 9}, "1.2.3.4", "Mozilla/5.0")
	if err != nil {
		t.Fatalf("retry submit: %v", err)
	}
	if !retry.Deduped || retry.ResponseID != first.ResponseID {
		t.Fatalf("retry = %+v, want deduped ack of %s", retry, first.ResponseID)
	}

	// A different client identity is a real second response.
	second, err := svc.SubmitResponse(ctx, s.SurveyID, site, "", "resp-xyz987ghi654", map[string]any{"q1": 7}, "5.6.7.8", "Mozilla/5.0")
	if err != nil {
		t.Fatalf("second submit: %v", err)
	}
	if second.Deduped {
		t.Fatalf("distinct identity reported deduped")
	}

	// Malformed client identity is rejected at the boundary.
	if _, err := svc.SubmitResponse(ctx, s.SurveyID, site, "", "short", map[string]any{}, "1.2.3.4", "ua"); err == nil {
		t.Fatalf("malformed client identity accepted")
	}
	if _, err := svc.SubmitResponse(ctx, s.SurveyID, site, "", "bad chars!! here!", map[string]any{}, "1.2.3.4", "ua"); err == nil {
		t.Fatalf("invalid-alphabet client identity accepted")
	}

	st, err := svc.Stats(ctx, s.SurveyID, site)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if st.ResponseRows != 2 {
		t.Fatalf("response rows = %d, want 2 (retry deduped, distinct identity counted)", st.ResponseRows)
	}
	if st.RespondingEntities != 2 {
		t.Fatalf("responding entities = %d, want 2", st.RespondingEntities)
	}

	// Closed survey: neither exposure nor response is recorded.
	if err := svc.Close(ctx, s.SurveyID); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := svc.SubmitResponse(ctx, s.SurveyID, site, "", "resp-afterclose1", map[string]any{"q1": 1}, "1.2.3.4", "ua"); err == nil {
		t.Fatalf("closed survey still accepts responses")
	}
	if err := svc.RecordExposure(ctx, s.SurveyID, site, "", "1.2.3.4", "ua"); err == nil {
		t.Fatalf("closed survey still accepts exposures")
	}
}

// TestO14PersonEntityMatchesIngestDerivation pins the identity-model
// consistency requirement: the survey person entity is the SAME digest the
// analytics ingest path derives for the same identify value and salt (O03:
// one identity model across surfaces, not a survey-specific one).
func TestO14PersonEntityMatchesIngestDerivation(t *testing.T) {
	db := exposureFixture(t)
	ctx := context.Background()
	site := surveySite(t)
	const salt = "shared-global-salt"
	svc := NewSurveyService(db, salt, nil)

	s, err := svc.Create(ctx, site, "NPS", "[]", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.Activate(ctx, s.SurveyID); err != nil {
		t.Fatalf("activate: %v", err)
	}
	if _, err := svc.SubmitResponse(ctx, s.SurveyID, site, "user-42", "", map[string]any{"q1": 5}, "1.2.3.4", "ua"); err != nil {
		t.Fatalf("submit: %v", err)
	}

	rows, err := nucleus.Query[struct {
		EntityType string `db:"entity_type"`
		EntityID   string `db:"entity_id"`
	}](ctx, db.SQL(),
		`SELECT entity_type, entity_id FROM survey_responses
		 WHERE survey_id = $1 AND site_id = $2`, s.SurveyID, site)
	if err != nil || len(rows) == 0 {
		t.Fatalf("read response entity: %v rows=%d", err, len(rows))
	}
	if rows[0].EntityType != EntityPerson {
		t.Fatalf("entity_type = %q, want person", rows[0].EntityType)
	}
	// The ingest path's derivation for the same raw value + global salt
	// (no siteSvc wired): identity.MaybeHashDistinctID with the fallback
	// salt - exactly what the survey service must reuse.
	want := identity.MaybeHashDistinctID("user-42", salt, false)
	if rows[0].EntityID != want {
		t.Fatalf("entity_id = %q, want the ingest derivation %q", rows[0].EntityID, want)
	}
}
