package cohorts

import (
	"context"
	"github.com/useteploy/teploy-observe/internal/queryguard"
	"strings"
	"testing"
	"time"
)

func TestAuditCohortAdmissionAndCumulativeBudget(t *testing.T) {
	svc := NewService(nil).WithQueryGuard(nil, queryguard.Budgets{MaxScanRows: 2, Timeout: time.Second})
	ctx, done, err := svc.beginRead(context.Background(), "s")
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	if err = svc.accountRows(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if err = svc.accountRows(ctx, 1); err == nil {
		t.Fatal("cumulative work escaped budget")
	}
	if !strings.HasSuffix(svc.leafLimitSQL(), "3") || !strings.HasSuffix(svc.staticMembersSQL(), "3") {
		t.Fatal("reads not bounded one past configured budget")
	}
}
func TestAuditStaticRetryReconcilesDurableCount(t *testing.T) {
	ctx, db, done := connect(t)
	defer done()
	svc := NewService(db)
	site := "audit_retry_" + cohortsToken()
	c, err := svc.CreateStatic(ctx, site, "members", "", []string{"u1"})
	if err != nil {
		t.Fatal(err)
	}
	// Durable state after member completion and a failed summary write.
	stale := *c
	stale.MemberCount = 0
	stale.UpdatedAt++
	if err := svc.putCohortRow(ctx, stale); err != nil {
		t.Fatal(err)
	}
	got, changed, err := svc.AddMembers(ctx, site, c.CohortID, []string{"u1"})
	if err != nil || changed != 0 || got.MemberCount != 1 {
		t.Fatalf("add retry: %+v %d %v", got, changed, err)
	}
	got, _, err = svc.RemoveMembers(ctx, site, c.CohortID, []string{"u1"})
	if err != nil {
		t.Fatal(err)
	}
	stale = *got
	stale.MemberCount = 1
	stale.UpdatedAt++
	if err := svc.putCohortRow(ctx, stale); err != nil {
		t.Fatal(err)
	}
	got, changed, err = svc.RemoveMembers(ctx, site, c.CohortID, []string{"u1"})
	if err != nil || changed != 0 || got.MemberCount != 0 {
		t.Fatalf("remove retry: %+v %d %v", got, changed, err)
	}
	if err := svc.Delete(ctx, site, c.CohortID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.AddMembers(ctx, site, c.CohortID, []string{"u2"}); err == nil {
		t.Fatal("edit resurrected deleted cohort")
	}
}
