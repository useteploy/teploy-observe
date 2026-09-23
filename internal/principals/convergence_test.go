package principals

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"
	"golang.org/x/crypto/bcrypt"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
	"github.com/useteploy/teploy-observe/internal/schema"
)

// connect applies the schema and hands back the client, skipping when no
// scratch Nucleus is configured (the OBSERVE_NUCLEUS_URL convention every
// DB-backed suite shares).
func connect(t *testing.T) (*nucleus.Client, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	db, err := nucleus.Connect(ctx, nucleustest.DSN(t))
	if err != nil {
		cancel()
		t.Skipf("nucleus not reachable: %v", err)
	}
	if err := schema.Apply(ctx, db); err != nil {
		db.Close()
		cancel()
		t.Fatalf("apply schema: %v", err)
	}
	t.Cleanup(func() {
		db.Close()
		cancel()
	})
	return db, ctx
}

// Test040BackfillConvergesExistingInstall replays the migration-040
// convergence path an existing install takes: legacy admin_users/users rows
// exist, principals is empty, and 040 runs against them. It seeds the worst
// realistic shape — duplicate users rows from the pre-fix UpdateRole append
// pattern, and a username held in BOTH legacy tables — and asserts ids,
// token_versions, the newest-row collapse, and the admin-origin login
// tie-break all survive the merge.
func Test040BackfillConvergesExistingInstall(t *testing.T) {
	db, ctx := connect(t)

	// Rewind to pre-040: drop the backfill output and 040's ledger entry so
	// Apply re-runs exactly that migration against the seeded legacy rows.
	if _, err := db.SQL().Exec(ctx, "DELETE FROM principals"); err != nil {
		t.Fatalf("clear principals: %v", err)
	}
	if _, err := db.SQL().Exec(ctx, "DELETE FROM _neutron_migrations WHERE version = 40"); err != nil {
		t.Fatalf("rewind migration ledger: %v", err)
	}

	// Legacy admin: token_version 3 — sessions minted before the migration
	// embed tv=3 and must keep validating after it.
	adminID := "legacy-admin-0001"
	if _, err := db.SQL().Exec(ctx,
		`INSERT INTO admin_users (id, username, password_hash, created_at, role, token_version)
		 VALUES ($1, $2, $3, $4, 'admin', 3)`,
		adminID, "owner", hashForTest(t, "admin-password-1"), "1000",
	); err != nil {
		t.Fatalf("seed admin_users: %v", err)
	}

	// Legacy managed user: two rows for one user_id (the pre-fix UpdateRole
	// appended instead of replacing), original viewer row then a later admin
	// row — the backfill must collapse to the newest values.
	managedID := "legacy-user-0002"
	for i, role := range []string{"viewer", "admin"} {
		if _, err := db.SQL().Exec(ctx,
			`INSERT INTO users (user_id, tenant_id, username, email, password_hash, role, created_at, invited_by)
			 VALUES ($1, 'default', $2, $3, $4, $5, $6, 'bootstrap')`,
			managedID, "worker", "worker@example.com", hashForTest(t, "worker-password-1"), role, strconv.FormatInt(int64(2000+i), 10),
		); err != nil {
			t.Fatalf("seed users row %d: %v", i, err)
		}
	}

	// The collision case: a second managed account whose username collides
	// with the admin's. Only the admin_users row had a working password
	// pre-merge, so the admin-origin row must win username login.
	collideID := "legacy-user-0003"
	if _, err := db.SQL().Exec(ctx,
		`INSERT INTO users (user_id, tenant_id, username, email, password_hash, role, created_at, invited_by)
		 VALUES ($1, 'default', 'owner', 'imposter@example.com', $2, 'viewer', '3000', 'bootstrap')`,
		collideID, hashForTest(t, "imposter-password-1"),
	); err != nil {
		t.Fatalf("seed colliding users row: %v", err)
	}

	t.Cleanup(func() {
		db.SQL().Exec(context.Background(), "DELETE FROM admin_users WHERE id = $1", adminID)
		db.SQL().Exec(context.Background(), "DELETE FROM users WHERE user_id IN ($1, $2)", managedID, collideID)
		db.SQL().Exec(context.Background(), "DELETE FROM principals")
		_ = schema.Apply(context.Background(), db)
	})

	if err := schema.Apply(ctx, db); err != nil {
		t.Fatalf("re-run migration 040: %v", err)
	}

	store := NewStore(db)

	admin, err := store.ByID(ctx, adminID)
	if err != nil {
		t.Fatalf("backfilled admin missing: %v", err)
	}
	if admin.TokenVersion != 3 || admin.Role != "admin" || admin.Origin != "admin" || admin.Kind != KindLocal {
		t.Fatalf("admin backfill wrong: %+v", admin)
	}
	if ok := checkHash("admin-password-1", admin.PasswordHash); !ok {
		t.Fatal("admin password hash did not carry across the backfill")
	}

	managed, err := store.ByID(ctx, managedID)
	if err != nil {
		t.Fatalf("backfilled managed user missing: %v", err)
	}
	if managed.Role != "admin" {
		t.Fatalf("duplicate users rows did not collapse to the newest role: got %q, want admin (created_at 2001)", managed.Role)
	}
	if managed.Email != "worker@example.com" || managed.Origin != "managed" || managed.TokenVersion != 0 {
		t.Fatalf("managed backfill wrong: %+v", managed)
	}

	// The tie-break: username "owner" exists as both an admin-origin and a
	// managed-origin principal; login must resolve to the admin-origin row.
	byName, err := store.LocalByUsername(ctx, "owner")
	if err != nil {
		t.Fatalf("username resolve: %v", err)
	}
	if byName.ID != adminID {
		t.Fatalf("username collision resolved to %q (%s origin), want the admin-origin account %q", byName.Username, byName.Origin, adminID)
	}
}

func hashForTest(t *testing.T, password string) string {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	return string(hash)
}

func checkHash(password, hash string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}
