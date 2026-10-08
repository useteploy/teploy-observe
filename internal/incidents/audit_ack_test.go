package incidents

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestAckInterruptedTransactionRollsBackAndRetries(t *testing.T) {
	s := ackFixture(t)
	ctx := context.Background()
	inc, err := s.Create(ctx, CreateInput{SiteID: "ack-atomic", Title: "Atomic ack"}, "")
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("interrupted between state and timeline")
	if err := s.ack(ctx, inc.IncidentID, "original", func() error { return injected }); !errors.Is(err, injected) {
		t.Fatalf("fault: %v", err)
	}
	got, err := s.Get(ctx, inc.IncidentID)
	if err != nil || got.AcknowledgedAt != 0 {
		t.Fatalf("half committed ack: %+v %v", got, err)
	}
	timeline, err := s.Timeline(ctx, inc.IncidentID)
	if err != nil || len(timeline) != 0 {
		t.Fatalf("half committed timeline: %+v %v", timeline, err)
	}
	if err := s.Ack(ctx, inc.IncidentID, "original"); err != nil {
		t.Fatal(err)
	}
	if err := s.Ack(ctx, inc.IncidentID, "retry"); err != nil {
		t.Fatal(err)
	}
	timeline, err = s.Timeline(ctx, inc.IncidentID)
	if err != nil || len(timeline) != 1 || timeline[0].Actor != "original" {
		t.Fatalf("retry timeline: %+v %v", timeline, err)
	}
}

func TestAckRepairsLegacyPartialStateWithOriginalActorAndTime(t *testing.T) {
	s := ackFixture(t)
	ctx := context.Background()
	inc, err := s.Create(ctx, CreateInput{SiteID: "ack-legacy", Title: "Partial ack"}, "")
	if err != nil {
		t.Fatal(err)
	}
	// Commit only the state as the old implementation did before its event.
	_, err = s.db.SQL().Exec(ctx, `INSERT INTO incidents (incident_id, tenant_id, site_id, title, description, severity, source, rule_id, started_at, ended_at, acknowledged_at, acknowledged_by, created_by, updated_at)
 SELECT incident_id, tenant_id, site_id, title, description, severity, source, rule_id, started_at, ended_at, 1234, 'original', created_by, updated_at + 1
 FROM incidents WHERE incident_id = $1 ORDER BY updated_at DESC LIMIT 1`, inc.IncidentID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Ack(ctx, inc.IncidentID, "retry-actor"); err != nil {
		t.Fatal(err)
	}
	if err := s.Ack(ctx, inc.IncidentID, "another-retry"); err != nil {
		t.Fatal(err)
	}
	timeline, err := s.Timeline(ctx, inc.IncidentID)
	if err != nil || len(timeline) != 1 || timeline[0].At != 1234 || timeline[0].Actor != "original" {
		t.Fatalf("legacy repair: %+v %v", timeline, err)
	}
}

func TestConcurrentAckAndClosePreserveSingleAck(t *testing.T) {
	s := ackFixture(t)
	ctx := context.Background()
	inc, err := s.Create(ctx, CreateInput{SiteID: "ack-concurrent", Title: "Concurrent ack"}, "")
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, 9)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; errs <- s.Ack(ctx, inc.IncidentID, "actor") }()
	}
	wg.Add(1)
	go func() { defer wg.Done(); <-start; errs <- s.Close(ctx, inc.IncidentID) }()
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Get(ctx, inc.IncidentID)
	if err != nil || got.AcknowledgedAt == 0 || got.EndedAt == 0 {
		t.Fatalf("state lost: %+v %v", got, err)
	}
	timeline, err := s.Timeline(ctx, inc.IncidentID)
	if err != nil || len(timeline) != 1 || timeline[0].Kind != EventAck {
		t.Fatalf("duplicate/lost ack: %+v %v", timeline, err)
	}
}
