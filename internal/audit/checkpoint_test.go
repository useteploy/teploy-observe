package audit

// O14 slice 4 oracle: checkpoint digests and the narrowed verification
// wording - at the real engine, fail-not-skip under OBSERVE_REQUIRE_NUCLEUS.

import (
	"context"
	"strings"
	"testing"

	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
	"github.com/useteploy/teploy-observe/internal/schema"
)

func checkpointFixture(t *testing.T) *nucleus.Client {
	t.Helper()
	dsn := nucleustest.DSN(t)
	ctx := context.Background()
	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("nucleus not reachable at %s — skipping integration test", dsn)
	}
	if err := schema.Apply(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Verify walks the whole shared table and the checkpoint reader picks
	// up any checkpoint: own the tables this test asserts against - and
	// leave them empty again, because the OTHER integration tests sweep
	// only audit_events and a stale checkpoint from this suite would
	// falsely break their Verify. Cleanup order matters (LIFO): sweep
	// BEFORE the connection close, or the sweep no-ops on a closed db.
	sweep := func() {
		if _, err := db.SQL().Exec(ctx, `DELETE FROM audit_events`); err != nil {
			t.Errorf("cleanup sweep audit_events: %v", err)
		}
		if _, err := db.SQL().Exec(ctx, `DELETE FROM audit_checkpoints`); err != nil {
			t.Errorf("cleanup sweep audit_checkpoints: %v", err)
		}
	}
	sweep()
	t.Cleanup(func() { db.Close() })
	t.Cleanup(sweep)
	return db
}

// TestO14CheckpointAndNarrowedWording: the verify result names its own
// limit (in-database chains cannot prove the tail was not truncated), a
// matching checkpoint reports clean, and the digest verifies offline.
func TestO14CheckpointAndNarrowedWording(t *testing.T) {
	db := checkpointFixture(t)
	ctx := context.Background()
	key := []byte(integrationAuditKey)
	svc := NewService(db, key)

	for i := 0; i < 3; i++ {
		if err := svc.Record(ctx, AuditEvent{Actor: "cp-test", Action: "x.do"}); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}
	cp, err := svc.Checkpoint(ctx)
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if cp.Seq != 3 || cp.HeadHash == "" || cp.Digest == "" {
		t.Fatalf("checkpoint = %+v, want seq 3 with hash + digest", cp)
	}
	if err := svc.Record(ctx, AuditEvent{Actor: "cp-test", Action: "x.do"}); err != nil {
		t.Fatalf("record past checkpoint: %v", err)
	}

	res, err := svc.Verify(ctx)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !res.Intact || !res.Authenticated {
		t.Fatalf("clean chain with a matching checkpoint: %+v", res)
	}
	if !res.HasCheckpoint || !res.CheckpointMatch {
		t.Fatalf("checkpoint not reported: %+v", res)
	}
	if res.VerifiedThroughSeq != 4 {
		t.Fatalf("verified through %d, want 4", res.VerifiedThroughSeq)
	}
	// The narrowed wording: the result must SAY what an in-database walk
	// cannot prove, and point at the checkpoint as the extension path.
	if !strings.Contains(res.Detail, "does not prove that its tail was not truncated") {
		t.Fatalf("detail does not carry the narrowed truncation wording: %q", res.Detail)
	}

	// The offline digest check an operator/tooling performs against their
	// externally stored copy.
	if !VerifyCheckpointDigest(cp, key) {
		t.Fatalf("digest does not verify under the signing key")
	}
	if VerifyCheckpointDigest(cp, []byte("wrong-key-material-32-bytes-long!")) {
		t.Fatalf("digest verifies under the WRONG key")
	}
	tampered := cp
	tampered.HeadHash = strings.Repeat("f", 64)
	if VerifyCheckpointDigest(tampered, key) {
		t.Fatalf("digest verifies against a tampered head hash")
	}
}

// TestO14TailTruncationDetectedBySurvivingCheckpoint: deleting the tail is
// invisible to a bare chain walk (the shorter chain still verifies) - but a
// checkpoint row that survives the delete proves the chain once reached
// further, and Verify reports the truncation instead of "intact".
func TestO14TailTruncationDetectedBySurvivingCheckpoint(t *testing.T) {
	db := checkpointFixture(t)
	ctx := context.Background()
	svc := NewService(db, []byte(integrationAuditKey))

	for i := 0; i < 4; i++ {
		if err := svc.Record(ctx, AuditEvent{Actor: "trunc-test", Action: "x.do"}); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}
	if _, err := svc.Checkpoint(ctx); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	// The attacker: truncate everything past seq 1 (checkpoint rows
	// missed - the in-database detection path).
	if _, err := db.SQL().Exec(ctx, `DELETE FROM audit_events WHERE CAST(seq AS BIGINT) > 1`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	res, err := svc.Verify(ctx)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if res.Intact {
		t.Fatalf("a chain truncated below its surviving checkpoint reported intact: %+v", res)
	}
	if res.BrokenAtSeq != 2 || !strings.Contains(res.Detail, "truncated") {
		t.Fatalf("truncation not reported precisely: %+v", res)
	}

	// The attacker deletes the checkpoints too: now the database alone
	// CANNOT detect it - Verify is honestly intact again, and THAT is the
	// exact boundary the narrowed wording + external digest exist for.
	if _, err := db.SQL().Exec(ctx, `DELETE FROM audit_checkpoints`); err != nil {
		t.Fatalf("delete checkpoints: %v", err)
	}
	res, err = svc.Verify(ctx)
	if err != nil {
		t.Fatalf("verify after checkpoint delete: %v", err)
	}
	if !res.Intact || res.HasCheckpoint {
		t.Fatalf("post-delete state misreported: %+v", res)
	}
	if !strings.Contains(res.Detail, "does not prove that its tail was not truncated") {
		t.Fatalf("the honest limit vanished from the wording: %q", res.Detail)
	}
}

// TestO14PeriodicCheckpoint: crossing the CheckpointEvery boundary writes
// the checkpoint automatically.
func TestO14PeriodicCheckpoint(t *testing.T) {
	db := checkpointFixture(t)
	ctx := context.Background()
	old := CheckpointEvery
	CheckpointEvery = 3
	defer func() { CheckpointEvery = old }()

	svc := NewService(db, []byte(integrationAuditKey))
	for i := 0; i < 3; i++ {
		if err := svc.Record(ctx, AuditEvent{Actor: "periodic", Action: "x.do"}); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}
	cp, err := svc.LatestCheckpoint(ctx)
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if cp == nil || cp.Seq != 3 {
		t.Fatalf("periodic checkpoint = %+v, want seq 3 at the boundary", cp)
	}
}
