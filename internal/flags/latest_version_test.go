package flags

import (
	"context"
	"os"
	"testing"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
)

// feature_flags as 007_wave2 declares it.
const flagColumns = `(
	flag_id        TEXT NOT NULL,
	tenant_id      TEXT NOT NULL DEFAULT 'default',
	site_id        TEXT NOT NULL,
	flag_key       TEXT NOT NULL,
	name           TEXT NOT NULL DEFAULT '',
	description    TEXT NOT NULL DEFAULT '',
	flag_type      TEXT NOT NULL DEFAULT 'boolean',
	enabled        TEXT NOT NULL DEFAULT 'false',
	rollout_pct    TEXT NOT NULL DEFAULT '100',
	variants       JSONB,
	targeting      JSONB,
	created_at     TEXT NOT NULL,
	version        BIGINT NOT NULL DEFAULT 0
)`

// TestEvaluateResolvesLatestToggle covers the SDK-facing evaluation path.
// Toggle writes a new version rather than updating in place, and Evaluate read
// the raw table and took rows[0] — an arbitrary version. With the superseded
// rows readable, a flag turned on could evaluate false and, worse, a flag
// turned OFF could keep evaluating true for every SDK caller. Without the fix
// this test fails on the first assertion that disagrees with the toggle.
func TestEvaluateResolvesLatestToggle(t *testing.T) {
	dsn := os.Getenv("OBSERVE_NUCLEUS_URL")
	if dsn == "" {
		t.Skip("no OBSERVE_NUCLEUS_URL")
	}
	db, err := nucleus.Connect(context.Background(), dsn)
	if err != nil {
		t.Skipf("connect: %v", err)
	}
	nucleustest.AsPlainMergeTree(t, db, "feature_flags", flagColumns,
		"(tenant_id, site_id, flag_id)", "version")

	svc := NewFlagService(db)
	ctx := context.Background()
	const site = "flagsite"
	const key = "new-checkout"

	flag, err := svc.Create(ctx, site, key, "New checkout", "", "boolean", "", "", 100)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	res, err := svc.Evaluate(ctx, site, key, "user-1", nil)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if res.Enabled {
		t.Fatalf("a freshly created flag evaluated true")
	}

	// Toggle it on and off again, so several versions exist and the newest is
	// not the one an arbitrary read would pick.
	for _, want := range []bool{true, false, true} {
		if err := svc.Toggle(ctx, flag.FlagID, want); err != nil {
			t.Fatalf("toggle %v: %v", want, err)
		}
		res, err := svc.Evaluate(ctx, site, key, "user-1", nil)
		if err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		if res.Enabled != want {
			t.Fatalf("after toggling to %v, Evaluate returned %v — it resolved a superseded version", want, res.Enabled)
		}
	}

	// The dashboard list has to agree with the SDK.
	list, err := svc.List(ctx, site)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var seen int
	for _, f := range list {
		if f.FlagID != flag.FlagID {
			continue
		}
		seen++
		if !f.Enabled {
			t.Fatalf("List reported the flag disabled after it was toggled on")
		}
	}
	if seen != 1 {
		t.Fatalf("List returned the flag %d times, want 1 — one row per surviving version", seen)
	}
}
