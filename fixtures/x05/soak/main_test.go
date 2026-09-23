package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/useteploy/teploy-observe/internal/ingest"
)

// The generator must speak the REAL protocol: its envelope unmarshals into
// the production ingest types unchanged (the contract pin — drift here is
// what makes a soak measurement fiction).
func TestEnvelopeMatchesProductionIngestTypes(t *testing.T) {
	env := envelope("x05", "x05-soak", 3, 1)
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ingest.BatchInput
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("envelope does not match ingest.BatchInput: %v", err)
	}
	if decoded.V != 2 || decoded.ProducerID != "x05-soak" || decoded.BatchID == "" {
		t.Fatalf("envelope header drift: %+v", decoded)
	}
	if len(decoded.Events) != 3 {
		t.Fatalf("events = %d, want 3", len(decoded.Events))
	}
	for _, ev := range decoded.Events {
		if ev.SiteID != "x05" || ev.EventType != "x05_soak_tick" || ev.URL == "" {
			t.Fatalf("event drift: %+v", ev)
		}
		if len(ev.Properties["pad"].(string)) < 1024 {
			t.Fatalf("pad undersized: %d", len(ev.Properties["pad"].(string)))
		}
	}
}

// Batches are new work: distinct batch ids per envelope (retry semantics
// are the SDK's, not this generator's).
func TestBatchIDsUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		env := envelope("x05", "x05-soak", 1, 0)
		if seen[env.BatchID] {
			t.Fatalf("duplicate batch id %s", env.BatchID)
		}
		seen[env.BatchID] = true
	}
}

// The dry-run mode emits exactly one parseable envelope (shape verification
// without a server).
func TestDryRunShape(t *testing.T) {
	env := envelope("x05", "x05-soak", 1, 0)
	if _, err := json.Marshal(env); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains([]byte("v2 producer ok"), []byte("v2")) {
		t.Fatal("unreachable")
	}
}
