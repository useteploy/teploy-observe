package surveys

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
)

// TestSameMillisecondCreateActivateResolvesActive is the version-tie
// regression (AUDIT_OPEN 70f6eff mechanism): Create and Activate both stamp
// version from the clock at millisecond precision, so on a fast machine they
// tie and argMax resolves arbitrarily — a just-activated survey can flip
// inactive, and a just-closed one keep serving. The fix stamps the rewriting
// write GREATEST(now, version + 1); no sleeps, the flake's exact shape.
func TestSameMillisecondCreateActivateResolvesActive(t *testing.T) {
	dsn := os.Getenv("OBSERVE_NUCLEUS_URL")
	if dsn == "" {
		dsn = nucleustest.DefaultDSN
	}
	db, err := nucleus.Connect(context.Background(), dsn)
	if err != nil {
		t.Skipf("connect: %v", err)
	}
	nucleustest.AsPlainMergeTree(t, db, "surveys", surveyColumns,
		"(tenant_id, site_id, survey_id)", "version")

	svc := NewSurveyService(db, "", nil)
	ctx := context.Background()
	const site = "survey-tie-site"

	for i := 0; i < 5; i++ {
		s, err := svc.Create(ctx, site, "NPS", `[{"id":"q1","type":"rating","text":"?"}]`, "", "")
		if err != nil {
			t.Fatalf("iter %d create: %v", i, err)
		}
		if err := svc.Activate(ctx, s.SurveyID); err != nil {
			t.Fatalf("iter %d activate: %v", i, err)
		}
		if err := svc.Close(ctx, s.SurveyID); err != nil {
			t.Fatalf("iter %d close: %v", i, err)
		}

		active, err := svc.GetActive(ctx, site)
		if err != nil {
			t.Fatalf("iter %d get active: %v", i, err)
		}
		for _, got := range active {
			if got.SurveyID == s.SurveyID {
				t.Fatalf("iter %d: a closed survey is still served as active — the close row tied or lost the version collapse", i)
			}
		}
		list, err := svc.List(ctx, site)
		if err != nil {
			t.Fatalf("iter %d list: %v", i, err)
		}
		for _, got := range list {
			if got.SurveyID == s.SurveyID && got.Status != "closed" {
				t.Fatalf("iter %d: List reported status %q, want closed — a same-millisecond tie resolved a superseded version", i, got.Status)
			}
		}
	}
}

// TestActivateBumpsPastAFutureVersion is the deterministic arm: the prior row
// is seeded directly at a FUTURE version (the same protection covers a
// regressed/skewed clock). Before the fix, Activate stamped version=now below
// the seed and the draft kept winning; after, GREATEST(now, version + 1)
// makes the active row win.
func TestActivateBumpsPastAFutureVersion(t *testing.T) {
	dsn := os.Getenv("OBSERVE_NUCLEUS_URL")
	if dsn == "" {
		dsn = nucleustest.DefaultDSN
	}
	db, err := nucleus.Connect(context.Background(), dsn)
	if err != nil {
		t.Skipf("connect: %v", err)
	}
	nucleustest.AsPlainMergeTree(t, db, "surveys", surveyColumns,
		"(tenant_id, site_id, survey_id)", "version")

	future := strconv.FormatInt(time.Now().UTC().Add(time.Hour).UnixMilli(), 10)
	if _, err := db.SQL().Exec(context.Background(),
		`INSERT INTO surveys (survey_id, tenant_id, site_id, name, questions, appearance, targeting, status, created_at, version)
		 VALUES ('fut', 'default', 'survey-fut-site', 'F', NULL, NULL, NULL, 'draft', $1, $1)`, future); err != nil {
		t.Fatalf("seed: %v", err)
	}

	svc := NewSurveyService(db, "", nil)
	ctx := context.Background()
	if err := svc.Activate(ctx, "fut"); err != nil {
		t.Fatalf("activate: %v", err)
	}

	active, err := svc.GetActive(ctx, "survey-fut-site")
	if err != nil {
		t.Fatalf("get active: %v", err)
	}
	found := false
	for _, got := range active {
		if got.SurveyID == "fut" {
			found = true
		}
	}
	if !found {
		t.Fatal("Activate wrote at or below the seeded future version — the draft row still resolves and the survey stays inactive")
	}
}
