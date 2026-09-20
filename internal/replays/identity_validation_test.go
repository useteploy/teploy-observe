package replays

import (
	"testing"

	"github.com/neutron-dev/neutron-go/neutron"
)

// R27 (round 4): the reviewed delimiter collision — two different
// (producer, batch) tuples hashing to the SAME deterministic child id.
func TestDeterministicChildIDDelimiterCollision(t *testing.T) {
	a := DeterministicChildID("site", "replay", "producer|extra", "batch", 0)
	b := DeterministicChildID("site", "replay", "producer", "extra|batch", 0)
	if a != b {
		t.Fatalf("expected the reviewed collision (guards the regression test's premise): %s vs %s", a, b)
	}
}

// R27: the canonical identity alphabet rejects pipe-bearing and
// mis-sized identities, and accepts everything first-party producers emit.
func TestValidProtocolID(t *testing.T) {
	for _, ok := range []string{
		"producer-001_abcXYZ", "0123456789abcdef0123456789abcdef",
		"a-b_cD90", "12345678",
	} {
		if !validProtocolID(ok) {
			t.Fatalf("legitimate identity rejected: %q", ok)
		}
	}
	for _, bad := range []string{
		"", "short", "producer|extra", "has space", "has/slash",
		"colon:id", "unicode-ünïcode-identity-values",
		"012345678901234567890123456789012345678901234567890123456789012345",
	} {
		if validProtocolID(bad) {
			t.Fatalf("noncanonical identity accepted: %q", bad)
		}
	}
}

// R27: a batch about to be written with noncanonical identities is
// rejected with a 400-grade error.
func TestValidReplayIdentitiesRejectsPipes(t *testing.T) {
	in := &IngestInput{ProducerID: "producer|extra", BatchID: "batch0001"}
	err := validReplayIdentities(in, "replay0001")
	if err == nil {
		t.Fatal("pipe-bearing producer_id accepted for write")
	}
	if _, ok := err.(*neutron.AppError); !ok {
		t.Fatalf("expected a neutron AppError (bad request), got %T", err)
	}
	in2 := &IngestInput{ProducerID: "producer0001", BatchID: "batch00001"}
	if err := validReplayIdentities(in2, "replay00001"); err != nil {
		t.Fatalf("canonical identity rejected: %v", err)
	}
	// v1 batches carry no identity and are out of scope for this check.
	if err := validReplayIdentities(&IngestInput{}, ""); err != nil {
		t.Fatalf("v1 batch rejected: %v", err)
	}
}

// R28 (round 4): error/replay URL storage shares the analytics boundary.
// The shared helper lives in internal/ingest; the replay-side consumer is
// exercised end-to-end by the live ingest tests. Here: the helper itself.
func TestTelemetryPageURLMatchesSharedPolicy(t *testing.T) {
	if got := telemetryPageURL("https://u:p@example.com/path?q=1#frag"); got != "https://example.com/path" {
		t.Fatalf("userinfo/query/fragment not stripped: %q", got)
	}
	if got := telemetryPageURL("javascript:alert(1)"); got != "" {
		t.Fatalf("non-http scheme not dropped: %q", got)
	}
	if got := telemetryPageURL("not a url"); got != "" {
		t.Fatalf("garbage not dropped: %q", got)
	}
}
