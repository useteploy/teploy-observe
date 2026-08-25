package sso

import (
	"context"
	"testing"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
)

// sso_configs as 008_wave4 declares it, with the BIGINT version 024 converted
// it to.
const ssoColumns = `(
	sso_id         TEXT NOT NULL,
	tenant_id      TEXT NOT NULL DEFAULT 'default',
	provider       TEXT NOT NULL DEFAULT 'saml',
	entity_id      TEXT NOT NULL DEFAULT '',
	sso_url        TEXT NOT NULL DEFAULT '',
	certificate    TEXT NOT NULL DEFAULT '',
	attribute_map  JSONB,
	enabled        TEXT NOT NULL DEFAULT 'false',
	created_at     TEXT NOT NULL,
	version        BIGINT NOT NULL DEFAULT 0
)`

// TestEnableWritesOneRowAndListResolvesLatest.
//
// Enable was `INSERT INTO sso_configs SELECT ... FROM sso_configs WHERE sso_id
// = $1`, which writes one row per row already present, and List read the raw
// table — so a provider turned on could still list as disabled, and the row
// count doubled on every call. Repeated Enable is the realistic shape here:
// the UI's toggle is idempotent, so an admin clicking it twice was enough.
//
// Without the fix this fails at the first List assertion (two rows for one
// config) and, in the counts, at the second Enable (4 rows, not 3).
func TestEnableWritesOneRowAndListResolvesLatest(t *testing.T) {
	ctx := context.Background()
	db, err := nucleus.Connect(ctx, nucleustest.DSN(t))
	if err != nil {
		t.Skipf("connect: %v", err)
	}
	defer db.Close()

	nucleustest.AsPlainMergeTree(t, db, "sso_configs", ssoColumns,
		"(tenant_id, sso_id)", "version")

	svc := NewSSOService(db)
	cfg, err := svc.Create(ctx, "saml", "urn:test", "https://idp.example/sso", "CERT", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	for i := 1; i <= 3; i++ {
		if err := svc.Enable(ctx, cfg.SSOID); err != nil {
			t.Fatalf("enable %d: %v", i, err)
		}
		if got, want := ssoRows(ctx, t, db, cfg.SSOID), int64(i+1); got != want {
			t.Fatalf("after %d Enable call(s) the table holds %d rows, want %d — the write re-inserted one row per existing version", i, got, want)
		}
		list, err := svc.List(ctx)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		var seen int
		for _, c := range list {
			if c.SSOID != cfg.SSOID {
				continue
			}
			seen++
			if c.Enabled != "true" {
				t.Fatalf("List reported enabled=%q after Enable — it resolved a superseded version", c.Enabled)
			}
		}
		if seen != 1 {
			t.Fatalf("List returned the config %d times, want 1 — one row per surviving version", seen)
		}
	}
}

func ssoRows(ctx context.Context, t *testing.T, db *nucleus.Client, ssoID string) int64 {
	t.Helper()
	type row struct {
		N int64 `db:"n"`
	}
	rows, err := nucleus.Query[row](ctx, db.SQL(),
		`SELECT COUNT(*) AS n FROM sso_configs WHERE sso_id = $1`, ssoID)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if len(rows) == 0 {
		return 0
	}
	return rows[0].N
}
