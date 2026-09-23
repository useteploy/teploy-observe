package incidents

// O10: acknowledgment and the incident timeline. Ack is idempotent (a
// re-ack is a read, not a write - exactly one timeline event), survives
// Close, and unknown incidents are refused. Nucleus-gated.

import (
	"context"
	"testing"

	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
	"github.com/useteploy/teploy-observe/internal/schema"
)

func ackFixture(t *testing.T) *Service {
	t.Helper()
	ctx := context.Background()
	db, err := nucleus.Connect(ctx, nucleustest.DSN(t))
	if err != nil {
		t.Skipf("connect: %v", err)
	}
	if err := schema.Apply(ctx, db); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewService(db)
}

func TestAckRecordsOnIncidentAndTimeline(t *testing.T) {
	ctx := context.Background()
	svc := ackFixture(t)
	inc, err := svc.Create(ctx, CreateInput{SiteID: "acksite", Title: "Checkout down", Severity: "critical"}, "alert")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		_, _ = svc.db.SQL().Exec(ctx, `DELETE FROM incidents WHERE incident_id = $1`, inc.IncidentID)
		_, _ = svc.db.SQL().Exec(ctx, `DELETE FROM incident_events WHERE incident_id = $1`, inc.IncidentID)
	})

	if err := svc.Ack(ctx, inc.IncidentID, "tyler"); err != nil {
		t.Fatalf("ack: %v", err)
	}
	got, err := svc.Get(ctx, inc.IncidentID)
	if err != nil || got == nil {
		t.Fatalf("get: %v %v", got, err)
	}
	if got.AcknowledgedAt == 0 || got.AcknowledgedBy != "tyler" {
		t.Fatalf("ack columns = %d/%q", got.AcknowledgedAt, got.AcknowledgedBy)
	}

	// Idempotent: the second ack writes nothing.
	if err := svc.Ack(ctx, inc.IncidentID, "someone-else"); err != nil {
		t.Fatalf("re-ack: %v", err)
	}
	got, _ = svc.Get(ctx, inc.IncidentID)
	if got.AcknowledgedBy != "tyler" {
		t.Fatalf("re-ack overwrote the original: %q", got.AcknowledgedBy)
	}
	timeline, err := svc.Timeline(ctx, inc.IncidentID)
	if err != nil {
		t.Fatalf("timeline: %v", err)
	}
	acks := 0
	for _, ev := range timeline {
		if ev.Kind == EventAck {
			acks++
			if ev.Actor != "tyler" {
				t.Fatalf("ack event actor = %q", ev.Actor)
			}
		}
	}
	if acks != 1 {
		t.Fatalf("ack timeline events = %d, want exactly 1", acks)
	}

	// Close preserves the acknowledgment (the 051 column-carry rule).
	if err := svc.Close(ctx, inc.IncidentID); err != nil {
		t.Fatalf("close: %v", err)
	}
	got, _ = svc.Get(ctx, inc.IncidentID)
	if got.EndedAt == 0 || got.AcknowledgedAt == 0 || got.AcknowledgedBy != "tyler" {
		t.Fatalf("close lost the ack: ended=%d ack=%d by=%q", got.EndedAt, got.AcknowledgedAt, got.AcknowledgedBy)
	}
}

func TestAckUnknownIncidentRefused(t *testing.T) {
	ctx := context.Background()
	svc := ackFixture(t)
	if err := svc.Ack(ctx, "does-not-exist", "tyler"); err == nil {
		t.Fatalf("ack of an unknown incident must be refused, not silently dropped")
	}
	if err := svc.RecordEvent(ctx, "", EventAck, "x", "y"); err == nil {
		t.Fatalf("empty incident id must be refused")
	}
}
