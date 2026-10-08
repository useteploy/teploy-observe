package groups

import (
	"context"
	"errors"
	"fmt"
	"github.com/neutron-build/neutron/go/nucleus"
	"github.com/useteploy/teploy-observe/internal/nucleustest"
	"github.com/useteploy/teploy-observe/internal/schema"
	"testing"
	"time"
)

func TestAddMemberRequiresSessionBeforeStorage(t *testing.T) {
	svc := NewGroupService(nil)
	for _, c := range [][4]string{{"site", "group", "", "u1"}, {"site", "group", " ", "u2"}, {"", "group", "s1", ""}, {"site", "", "s1", ""}} {
		if err := svc.AddMember(context.Background(), c[0], c[1], c[2], c[3]); !errors.Is(err, ErrInvalidMember) {
			t.Fatalf("error=%v", err)
		}
	}
}

func TestSessionMembershipMetricsAndSiteBoundary(t *testing.T) {
	ctx := context.Background()
	db, err := nucleus.Connect(ctx, nucleustest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := schema.Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	svc := NewGroupService(db)
	site := fmt.Sprintf("groups-%d", time.Now().UnixNano())
	group, err := svc.Create(ctx, site, "company", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range [][2]string{{"s1", "u1"}, {"s1", "u1"}, {"s2", "u1"}, {"s3", "u2"}} {
		if err := svc.AddMember(ctx, site, group.GroupID, c[0], c[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.AddMember(ctx, site, group.GroupID, "", "u3"); !errors.Is(err, ErrInvalidMember) {
		t.Fatalf("user-only error=%v", err)
	}
	if err := svc.AddMember(ctx, site+"other", group.GroupID, "s4", ""); !errors.Is(err, ErrGroupNotFound) {
		t.Fatalf("wrong-site error=%v", err)
	}
	if err := svc.AddMember(ctx, site, "missing", "s4", ""); !errors.Is(err, ErrGroupNotFound) {
		t.Fatalf("unknown error=%v", err)
	}
	// Legacy empty identities must never match anonymous empty-session events.
	if _, err := db.SQL().Exec(ctx, `INSERT INTO group_members (tenant_id, site_id, group_id, session_id, user_id, joined_at) VALUES ('default', $1, $2, '', 'legacy', 1)`, site, group.GroupID); err != nil {
		t.Fatal(err)
	}
	for i, session := range []string{"s1", "s2", "s3", "outside", ""} {
		if _, err := db.SQL().Exec(ctx, `INSERT INTO events (event_id, tenant_id, site_id, session_id, visit_id, timestamp, event_type, url, pathname) VALUES ($1, 'default', $2, $3, 'visit', $4, 'pageview', '', '/')`, fmt.Sprintf("%s-%d", site, i), site, session, time.Now().UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	metrics, err := svc.GroupMetrics(ctx, site, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(metrics) != 1 || metrics[0].MemberCount != "3" || metrics[0].EventCount != "3" {
		t.Fatalf("metrics=%+v", metrics)
	}
}
