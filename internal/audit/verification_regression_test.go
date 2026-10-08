package audit

import (
	"context"
	"fmt"
	"github.com/useteploy/teploy-observe/internal/dbutil"
	"sync"
	"testing"
)

type memoryVerificationReader struct {
	mu          sync.Mutex
	rows        []AuditEvent
	cp          *Checkpoint
	headStarted chan struct{}
	resumeHead  chan struct{}
	once        sync.Once
}

func (m *memoryVerificationReader) invalid(context.Context) (*verificationHead, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ev := range m.rows {
		if ev.Seq <= 0 {
			return &verificationHead{ev.Seq}, nil
		}
	}
	return nil, nil
}
func (m *memoryVerificationReader) head(context.Context) (int64, error) {
	m.mu.Lock()
	var seq int64
	for _, ev := range m.rows {
		if ev.Seq > seq {
			seq = ev.Seq
		}
	}
	m.mu.Unlock()
	m.once.Do(func() {
		if m.headStarted != nil {
			close(m.headStarted)
			<-m.resumeHead
		}
	})
	return seq, nil
}
func (m *memoryVerificationReader) checkpoint(context.Context) (*Checkpoint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cp == nil {
		return nil, nil
	}
	cp := *m.cp
	return &cp, nil
}
func (m *memoryVerificationReader) page(_ context.Context, last, through int64, limit int) ([]AuditEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rows := []AuditEvent{}
	for _, ev := range m.rows {
		if ev.Seq > last && ev.Seq <= through {
			rows = append(rows, ev)
			if len(rows) == limit {
				break
			}
		}
	}
	return rows, nil
}
func signedRows(s *Service, n int) []AuditEvent {
	rows := []AuditEvent{}
	prev := ""
	for i := 1; i <= n; i++ {
		ev := AuditEvent{AuditID: fmt.Sprint(i), Seq: int64(i), PrevHash: prev, Action: "fixture", KeyID: s.keys.Signer.ID}
		ev.Hash = s.computeHash(ev)
		rows = append(rows, ev)
		prev = ev.Hash
	}
	return rows
}
func TestVerifyRejectsNonpositiveDomain(t *testing.T) {
	for _, seq := range []int64{0, -1} {
		for _, empty := range []bool{false, true} {
			for _, checkpoint := range []bool{false, true} {
				s := NewService(nil, []byte(integrationAuditKey))
				reader := &memoryVerificationReader{}
				s.verification = reader
				if !empty {
					reader.rows = signedRows(s, 3)
				}
				if checkpoint {
					reader.cp = &Checkpoint{Seq: 3}
				}
				reader.rows = append(reader.rows, AuditEvent{Seq: seq, Action: "forged", Hash: "unsigned"})
				result, err := s.Verify(context.Background())
				if err != nil || result.Intact || result.Authenticated {
					t.Fatalf("seq=%d empty=%v cp=%v: %v %v", seq, empty, checkpoint, result, err)
				}
			}
		}
	}
}
func TestVerifySnapshotSerializesCheckpointWriter(t *testing.T) {
	s := NewService(nil, []byte(integrationAuditKey))
	rows := signedRows(s, 4)
	reader := &memoryVerificationReader{rows: rows[:3], cp: &Checkpoint{Seq: 3, HeadHash: rows[2].Hash}, headStarted: make(chan struct{}), resumeHead: make(chan struct{})}
	s.verification = reader
	done := make(chan VerifyResult, 1)
	errs := make(chan error, 1)
	go func() { result, err := s.Verify(context.Background()); done <- result; errs <- err }()
	<-reader.headStarted
	if s.mu.TryLock() {
		s.mu.Unlock()
		t.Error("watermark/checkpoint snapshot did not retain writer lock")
	}
	written := make(chan struct{})
	go func() {
		s.mu.Lock()
		reader.mu.Lock()
		reader.rows = rows
		reader.cp = &Checkpoint{Seq: 4, HeadHash: rows[3].Hash}
		reader.mu.Unlock()
		s.mu.Unlock()
		close(written)
	}()
	close(reader.resumeHead)
	result := <-done
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if !result.Intact || !result.Authenticated || !result.CheckpointMatch || result.VerifiedThroughSeq != 3 {
		t.Fatalf("ordinary checkpoint misclassified: %+v", result)
	}
	<-written
	result, err := s.Verify(context.Background())
	if err != nil || !result.Intact || result.VerifiedThroughSeq != 4 {
		t.Fatalf("new snapshot: %+v %v", result, err)
	}
	// A surviving anchor beyond the actual chain must still detect truncation.
	reader.mu.Lock()
	reader.rows = rows[:3]
	reader.mu.Unlock()
	result, err = s.Verify(context.Background())
	if err != nil || result.Intact {
		t.Fatalf("real truncation accepted: %+v %v", result, err)
	}
}
func TestVerifyAndListNonpositiveRowsAtEngine(t *testing.T) {
	db := checkpointFixture(t)
	ctx := context.Background()
	s := NewService(db, []byte(integrationAuditKey))
	for _, seq := range []int64{0, -1} {
		for _, empty := range []bool{true, false} {
			for _, checkpoint := range []bool{false, true} {
				if _, err := db.SQL().Exec(ctx, "DELETE FROM audit_events"); err != nil {
					t.Fatal(err)
				}
				if _, err := db.SQL().Exec(ctx, "DELETE FROM audit_checkpoints"); err != nil {
					t.Fatal(err)
				}
				s = NewService(db, []byte(integrationAuditKey))
				if !empty || checkpoint {
					if err := s.Record(ctx, AuditEvent{Action: "real"}); err != nil {
						t.Fatal(err)
					}
				}
				if checkpoint {
					if _, err := s.Checkpoint(ctx); err != nil {
						t.Fatal(err)
					}
				}
				if empty && checkpoint {
					if _, err := db.SQL().Exec(ctx, "DELETE FROM audit_events"); err != nil {
						t.Fatal(err)
					}
				}
				_, err := db.SQL().Exec(ctx, "INSERT INTO audit_events (audit_id,tenant_id,site_id,timestamp,actor,actor_type,action,target,result,source_ip,user_agent,metadata,key_id,seq,prev_hash,hash) VALUES ('forged','default','default',9999999999999,'fake','user','forged','','success','','','','',$1,'','unsigned')", dbutil.IntParam(seq))
				if err != nil {
					t.Fatal(err)
				}
				result, err := s.Verify(ctx)
				if err != nil || result.Intact || result.Authenticated {
					t.Fatalf("seq=%d empty=%v checkpoint=%v: %+v %v", seq, empty, checkpoint, result, err)
				}
				list, err := s.List(ctx, Filter{Limit: 10})
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, ev := range list {
					if ev.AuditID == "forged" {
						found = true
					}
				}
				if !found {
					t.Fatal("fixture absent from List")
				}
			}
		}
	}
}
