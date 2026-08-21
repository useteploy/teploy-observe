package audit

import (
	"context"
	"testing"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
)

// --- Pure tamper-evidence tests (no DB) --------------------------------------

func TestComputeHash_DeterministicAndSensitive(t *testing.T) {
	s := NewService(nil, []byte("key1"))
	ev := AuditEvent{Seq: 1, PrevHash: "p", Timestamp: 100, AuditID: "id", Actor: "a", Action: "x.do", Result: "success"}

	h1 := s.computeHash(ev)
	if h1 != s.computeHash(ev) {
		t.Fatal("hash must be deterministic")
	}
	// A different key yields a different hash (so a DB attacker without the key
	// can't recompute the chain).
	if h1 == NewService(nil, []byte("key2")).computeHash(ev) {
		t.Error("hash must depend on the key")
	}
	// Changing any field changes the hash.
	ev2 := ev
	ev2.Actor = "b"
	if s.computeHash(ev2) == h1 {
		t.Error("hash must depend on the actor field")
	}
	// Length-prefixing must prevent field-boundary collisions:
	// ("ab","c") and ("a","bc") must not hash equal.
	x := AuditEvent{Actor: "ab", Action: "c"}
	y := AuditEvent{Actor: "a", Action: "bc"}
	if s.computeHash(x) == s.computeHash(y) {
		t.Error("field boundaries must be unambiguous (length-prefixing)")
	}
}

// chain builds a valid n-record chain using the service's own hashing.
func chain(s *Service, n int) []AuditEvent {
	rows := make([]AuditEvent, 0, n)
	prev := ""
	for i := 1; i <= n; i++ {
		ev := AuditEvent{Seq: int64(i), PrevHash: prev, Timestamp: int64(i * 10),
			AuditID: genID(), Actor: "a", Action: "x.do", Result: "success"}
		ev.Hash = s.computeHash(ev)
		rows = append(rows, ev)
		prev = ev.Hash
	}
	return rows
}

func TestVerifyChain_Intact(t *testing.T) {
	s := NewService(nil, []byte("k"))
	res := verifyChain(chain(s, 5), s.computeHash)
	if !res.Intact || res.Count != 5 {
		t.Fatalf("intact chain should verify: %+v", res)
	}
}

func TestVerifyChain_DetectsModification(t *testing.T) {
	s := NewService(nil, []byte("k"))
	rows := chain(s, 5)
	rows[2].Actor = "attacker" // edit a field without fixing the hash
	res := verifyChain(rows, s.computeHash)
	if res.Intact || res.BrokenAtSeq != 3 {
		t.Fatalf("modified row must be detected at seq 3: %+v", res)
	}
}

func TestVerifyChain_DetectsDeletion(t *testing.T) {
	s := NewService(nil, []byte("k"))
	rows := chain(s, 5)
	rows = append(rows[:2], rows[3:]...) // delete seq 3 → gap
	res := verifyChain(rows, s.computeHash)
	if res.Intact || res.BrokenAtSeq != 4 {
		t.Fatalf("deletion (seq gap) must be detected: %+v", res)
	}
}

func TestVerifyChain_DetectsRelink(t *testing.T) {
	s := NewService(nil, []byte("k"))
	rows := chain(s, 5)
	rows[3].PrevHash = "forged" // relink without a valid predecessor hash
	rows[3].Hash = s.computeHash(rows[3])
	res := verifyChain(rows, s.computeHash)
	if res.Intact || res.BrokenAtSeq != 4 {
		t.Fatalf("relink must be detected at seq 4: %+v", res)
	}
}

// --- Integration: chain persists + verifies against real Nucleus -------------

func TestChain_Integration(t *testing.T) {
	dsn := nucleustest.DSN(t)
	ctx := context.Background()
	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("nucleus not reachable at %s — skipping integration test", dsn)
	}
	defer db.Close()
	if _, err := db.SQL().Exec(ctx, `CREATE TABLE IF NOT EXISTS audit_events (
		audit_id TEXT NOT NULL, tenant_id TEXT NOT NULL DEFAULT 'default',
		site_id TEXT NOT NULL DEFAULT 'default', timestamp BIGINT NOT NULL,
		actor TEXT NOT NULL DEFAULT '', actor_type TEXT NOT NULL DEFAULT 'user',
		action TEXT NOT NULL, target TEXT NOT NULL DEFAULT '',
		result TEXT NOT NULL DEFAULT 'success', source_ip TEXT NOT NULL DEFAULT '',
		user_agent TEXT NOT NULL DEFAULT '', metadata TEXT NOT NULL DEFAULT '{}',
		seq BIGINT NOT NULL DEFAULT 0, prev_hash TEXT NOT NULL DEFAULT '', hash TEXT NOT NULL DEFAULT ''
	) WITH (engine = 'mergetree') ORDER BY (tenant_id, site_id, timestamp)`); err != nil {
		t.Fatalf("create table: %v", err)
	}

	// Must be the key the other integration tests use. They share this table,
	// and Verify walks every row: a chain signed with one key does not verify
	// under another, so a second key here reports "contents modified" for
	// records that were never touched. That is the verifier working correctly,
	// but it makes this test fail for a reason that has nothing to do with
	// tamper-evidence.
	svc := NewService(db, []byte(integrationAuditKey))
	for i := 0; i < 3; i++ {
		if err := svc.Record(ctx, AuditEvent{Actor: "chain-test", Action: "x.do"}); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	res, err := svc.Verify(ctx)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	// Records from other tests share the table; the chain must still be intact
	// end-to-end (Verify walks all rows).
	if !res.Intact {
		t.Fatalf("chain should be intact after clean writes: %+v", res)
	}
	if res.Count < 3 {
		t.Errorf("expected at least our 3 records, got %d", res.Count)
	}
}
