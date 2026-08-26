package incidents

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
	"github.com/useteploy/teploy-observe/internal/schema"
)

// TestEnsureOpenReusesTheOpenIncident is the regression for the incident flood.
//
// A repeating detector — the missed-cron check on a 45s tick, the alert rule
// check on a 30s one — calls this every tick for as long as the condition
// holds. It must open ONE incident and keep returning that one. The live
// instance had 12,398 incident rows from ten cron monitors; a detector that
// declares per tick is how a table gets there, and the chart renders one shaded
// band per incident, so it also filled the analytics plot with solid orange.
//
// Reverting cmd/observe/main.go's call site to Create fails this at the second
// EnsureOpen with "2 incidents open for the rule, want 1".
func TestEnsureOpenReusesTheOpenIncident(t *testing.T) {
	ctx := context.Background()
	db, err := nucleus.Connect(ctx, nucleustest.DSN(t))
	if err != nil {
		t.Skipf("connect: %v", err)
	}
	defer db.Close()
	if err := schema.Apply(ctx, db); err != nil {
		t.Fatalf("apply schema: %v", err)
	}

	svc := NewService(db)
	// Unique per run: DELETE against a Nucleus mergetree does not reliably
	// remove every row, so a fixed id would let one run's leftovers fail the
	// next one.
	site := fmt.Sprintf("ensuresite-%d", time.Now().UnixNano())
	rule := fmt.Sprintf("cron:ensure-open-%d", time.Now().UnixNano())

	in := CreateInput{
		SiteID:   site,
		Title:    "Cron missed: nightly",
		Severity: "warning",
		Source:   SourceCron,
		RuleID:   rule,
	}

	first, created, err := svc.EnsureOpen(ctx, in, "cron")
	if err != nil {
		t.Fatalf("first EnsureOpen: %v", err)
	}
	if !created {
		t.Fatal("first EnsureOpen reported no write, want a newly created incident")
	}

	// Ten more ticks with the monitor still silent.
	for i := 2; i <= 11; i++ {
		got, created, err := svc.EnsureOpen(ctx, in, "cron")
		if err != nil {
			t.Fatalf("EnsureOpen tick %d: %v", i, err)
		}
		if created {
			t.Fatalf("tick %d declared a SECOND incident for %s — the detector reopens on every tick", i, rule)
		}
		if got.IncidentID != first.IncidentID {
			t.Fatalf("tick %d returned incident %s, want the already-open %s", i, got.IncidentID, first.IncidentID)
		}
		active, err := svc.ActiveByRule(ctx, rule)
		if err != nil {
			t.Fatalf("ActiveByRule tick %d: %v", i, err)
		}
		if len(active) != 1 {
			t.Fatalf("after tick %d: %d incidents open for the rule, want 1", i, len(active))
		}
	}

	// Recovery closes it, and a later relapse is a genuinely NEW incident —
	// reuse must not extend to a resolved one.
	if err := svc.CloseByRule(ctx, rule); err != nil {
		t.Fatalf("close by rule: %v", err)
	}
	active, err := svc.ActiveByRule(ctx, rule)
	if err != nil {
		t.Fatalf("ActiveByRule after close: %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("%d incidents still open after CloseByRule, want 0", len(active))
	}

	second, created, err := svc.EnsureOpen(ctx, in, "cron")
	if err != nil {
		t.Fatalf("EnsureOpen after recovery: %v", err)
	}
	if !created || second.IncidentID == first.IncidentID {
		t.Fatal("a relapse after recovery reused the CLOSED incident, want a new one")
	}
}

// TestEnsureOpenRequiresARule: without a rule_id there is nothing to dedup on,
// so EnsureOpen would silently degrade into Create. Refuse instead.
func TestEnsureOpenRequiresARule(t *testing.T) {
	ctx := context.Background()
	db, err := nucleus.Connect(ctx, nucleustest.DSN(t))
	if err != nil {
		t.Skipf("connect: %v", err)
	}
	defer db.Close()
	if err := schema.Apply(ctx, db); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	svc := NewService(db)
	if _, created, err := svc.EnsureOpen(ctx, CreateInput{SiteID: "s", Title: "t"}, "x"); err == nil || created {
		t.Fatalf("EnsureOpen with no rule_id: created=%v err=%v, want an error and no write", created, err)
	}
}

// TestInRangeCollapsesVersions pins the read side of the marker overlay: the
// collapse to the highest-updated_at row per incident runs in the database now,
// and a closed incident must come back closed. If the collapse regressed to
// returning raw rows, a closed incident's original ended_at=0 row would surface
// and the chart would draw a band running to the right edge forever.
func TestInRangeCollapsesVersions(t *testing.T) {
	ctx := context.Background()
	db, err := nucleus.Connect(ctx, nucleustest.DSN(t))
	if err != nil {
		t.Skipf("connect: %v", err)
	}
	defer db.Close()
	if err := schema.Apply(ctx, db); err != nil {
		t.Fatalf("apply schema: %v", err)
	}

	svc := NewService(db)
	site := fmt.Sprintf("rangesite-%d", time.Now().UnixNano())

	closed, err := svc.Create(ctx, CreateInput{SiteID: site, Title: "closed", Severity: "warning"}, "tyler")
	if err != nil {
		t.Fatalf("create closed: %v", err)
	}
	if err := svc.Close(ctx, closed.IncidentID); err != nil {
		t.Fatalf("close: %v", err)
	}
	open, err := svc.Create(ctx, CreateInput{SiteID: site, Title: "open", Severity: "warning"}, "tyler")
	if err != nil {
		t.Fatalf("create open: %v", err)
	}

	list, err := svc.InRange(ctx, site, 0, 1<<62)
	if err != nil {
		t.Fatalf("in range: %v", err)
	}
	seen := map[string]Incident{}
	for _, inc := range list {
		if _, dup := seen[inc.IncidentID]; dup {
			t.Fatalf("InRange returned incident %s twice — the collapse is not running", inc.IncidentID)
		}
		seen[inc.IncidentID] = inc
	}
	if got := seen[closed.IncidentID]; got.EndedAt == 0 {
		t.Fatal("InRange reports a closed incident as ongoing — it would draw to the right edge of every chart")
	}
	if got := seen[open.IncidentID]; got.EndedAt != 0 {
		t.Fatalf("InRange reports the open incident as ended at %d, want 0", got.EndedAt)
	}
}
