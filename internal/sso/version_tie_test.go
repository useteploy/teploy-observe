package sso

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
)

// ssoVersions reads every physical version row for a config, oldest first, so
// a test can assert strict monotonicity between consecutive writes.
func ssoVersions(ctx context.Context, t *testing.T, db *nucleus.Client, ssoID string) []int64 {
	t.Helper()
	type row struct {
		Version string `db:"version"`
	}
	rows, err := nucleus.Query[row](ctx, db.SQL(),
		`SELECT CAST(version AS TEXT) AS version FROM sso_configs WHERE sso_id = $1 ORDER BY version ASC`,
		ssoID)
	if err != nil {
		t.Fatalf("read versions: %v", err)
	}
	out := make([]int64, 0, len(rows))
	for _, r := range rows {
		v, _ := strconv.ParseInt(r.Version, 10, 64)
		out = append(out, v)
	}
	return out
}

// TestSameMillisecondCreateEnableYieldsStrictlyIncreasingVersions is the
// version-tie regression (the live defect behind the moving serial-suite
// failures recorded in AUDIT_OPEN at 70f6eff): Create and Enable both stamp
// version from the clock at millisecond precision, and on a fast machine the
// two writes land in the SAME millisecond. Tied versions make
// argMax(col, version) resolve arbitrarily, so a just-enabled config could
// list as disabled — intermittently, which is why CI never saw it.
//
// The fix stamps the rewriting write GREATEST(now, version + 1) over the
// collapsed prior, so v(enable) > v(create) ALWAYS, tie or not. No sleeps:
// each iteration is the flake's exact shape.
func TestSameMillisecondCreateEnableYieldsStrictlyIncreasingVersions(t *testing.T) {
	ctx := context.Background()
	db, err := nucleus.Connect(ctx, nucleustest.DSN(t))
	if err != nil {
		t.Skipf("connect: %v", err)
	}
	defer db.Close()

	nucleustest.AsPlainMergeTree(t, db, "sso_configs", ssoColumns,
		"(tenant_id, sso_id)", "version")

	svc := NewSSOService(db)
	for i := 0; i < 5; i++ {
		cfg, err := svc.Create(ctx, "saml", "urn:test", "https://idp.example/sso", "CERT", "")
		if err != nil {
			t.Fatalf("iter %d create: %v", i, err)
		}
		if err := svc.Enable(ctx, cfg.SSOID); err != nil {
			t.Fatalf("iter %d enable: %v", i, err)
		}
		versions := ssoVersions(ctx, t, db, cfg.SSOID)
		if len(versions) != 2 {
			t.Fatalf("iter %d: %d version rows, want 2", i, len(versions))
		}
		if versions[1] <= versions[0] {
			t.Fatalf("iter %d: enable version %d does not strictly exceed create version %d — a same-millisecond tie leaves argMax free to resolve the disabled row", i, versions[1], versions[0])
		}
		list, err := svc.List(ctx)
		if err != nil {
			t.Fatalf("iter %d list: %v", i, err)
		}
		for _, c := range list {
			if c.SSOID == cfg.SSOID && c.Enabled != "true" {
				t.Fatalf("iter %d: a just-enabled config lists as enabled=%q — the tie resolved the superseded row", i, c.Enabled)
			}
		}
	}
}

// TestEnableBumpsPastAFutureVersion is the deterministic arm of the same
// mechanism: no timing luck, the prior row's version is seeded directly at a
// FUTURE clock value (the same protection covers a regressed/skewed clock).
// Before the monotonic fix, Enable stamped version=now, which is BELOW the
// seeded prior — the collapse kept the disabled row and the enable vanished.
// After the fix, GREATEST(now, version + 1) = prior + 1 strictly exceeds it
// and the enabled row resolves.
func TestEnableBumpsPastAFutureVersion(t *testing.T) {
	ctx := context.Background()
	db, err := nucleus.Connect(ctx, nucleustest.DSN(t))
	if err != nil {
		t.Skipf("connect: %v", err)
	}
	defer db.Close()

	nucleustest.AsPlainMergeTree(t, db, "sso_configs", ssoColumns,
		"(tenant_id, sso_id)", "version")

	future := strconv.FormatInt(time.Now().UTC().Add(time.Hour).UnixMilli(), 10)
	if _, err := db.SQL().Exec(ctx,
		`INSERT INTO sso_configs (sso_id, tenant_id, provider, entity_id, sso_url, certificate, attribute_map, enabled, created_at, version)
		 VALUES ('fut', 'default', 'saml', 'urn:x', 'https://x', 'C', NULL, 'false', $1, $1)`, future); err != nil {
		t.Fatalf("seed: %v", err)
	}

	svc := NewSSOService(db)
	if err := svc.Enable(ctx, "fut"); err != nil {
		t.Fatalf("enable: %v", err)
	}

	list, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, c := range list {
		if c.SSOID == "fut" && c.Enabled != "true" {
			t.Fatalf("Enable wrote a version at or below the prior row — the disabled row still resolves (enabled=%q)", c.Enabled)
		}
	}
	versions := ssoVersions(ctx, t, db, "fut")
	if len(versions) != 2 || versions[1] <= versions[0] {
		t.Fatalf("versions = %v, want exactly 2 strictly increasing — the rewrite did not bump past the prior", versions)
	}
}
