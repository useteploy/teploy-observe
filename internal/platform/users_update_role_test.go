package platform

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
	"github.com/useteploy/teploy-observe/internal/principals"
	"github.com/useteploy/teploy-observe/internal/schema"
)

// TestUpdateRoleReplacesTheRow.
//
// `users` is a plain mergetree: no version column, so nothing collapses it and
// nothing ever collapsed it in Go either — List and Get read the raw table.
// UpdateRole was `INSERT INTO users SELECT ..., $2, $3, ... FROM users WHERE
// user_id = $1`, which appends one row per row already present. After one
// demotion the table holds an 'admin' row and a 'viewer' row for the same
// person and whichever comes back first decides what Get reports, so a demoted
// admin could keep reading as an admin. Three changes take the count to 8.
//
// Without the fix this fails on the row count after the first change (2, not
// 1) and, if that were relaxed, on created_at, which the old statement
// overwrote with the edit time.
func TestUpdateRoleReplacesTheRow(t *testing.T) {
	ctx := context.Background()
	db, err := nucleus.Connect(ctx, nucleustest.DSN(t))
	if err != nil {
		t.Skipf("connect: %v", err)
	}
	defer db.Close()
	if err := schema.Apply(ctx, db); err != nil {
		t.Fatalf("apply schema: %v", err)
	}

	store := principals.NewStore(db)
	svc := NewUserService(store)
	username := "role_" + strconv.FormatInt(time.Now().UnixNano(), 36)

	u, err := svc.Create(ctx, username, username+"@example.com", "hunter2hunter2", "admin", "bootstrap")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.SQL().Exec(context.Background(), `DELETE FROM principals WHERE id = $1`, u.UserID)
	})
	createdAt := u.CreatedAt.UnixMilli()

	// R08 (round 4): demoting the LAST local administrator is refused, so
	// this test keeps a second break-glass admin standing while it cycles
	// the first user's role.
	breakGlass, err := svc.Create(ctx, username+"_bg", username+"-bg@example.com", "hunter2hunter2", "admin", "bootstrap")
	if err != nil {
		t.Fatalf("create break-glass admin: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.SQL().Exec(context.Background(), `DELETE FROM principals WHERE id = $1`, breakGlass.UserID)
	})

	for i, role := range []string{"viewer", "editor", "admin"} {
		if err := svc.UpdateRole(ctx, u.UserID, role); err != nil {
			t.Fatalf("update to %s: %v", role, err)
		}
		if got := userRows(ctx, t, db, u.UserID); got != 1 {
			t.Fatalf("after %d role change(s) the table holds %d rows for one user, want 1 — the write appended instead of replacing", i+1, got)
		}
		got, err := svc.Get(ctx, u.UserID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got == nil {
			t.Fatal("Get lost the user")
		}
		if got.Role != role {
			t.Fatalf("Get reported role %q after the change to %q — it read a superseded row", got.Role, role)
		}
		if got.CreatedAt.UnixMilli() != createdAt {
			t.Fatalf("created_at moved from %d to %d — a role change is not a signup", createdAt, got.CreatedAt.UnixMilli())
		}
		// The DTO no longer carries the hash; read the store directly to
		// prove the replacement kept the credential intact.
		p, err := store.ByID(ctx, u.UserID)
		if err != nil {
			t.Fatalf("store get: %v", err)
		}
		if p.PasswordHash == "" {
			t.Fatal("the replacement dropped the password hash")
		}

		var seen int
		list, err := svc.List(ctx)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		for _, l := range list {
			if l.UserID == u.UserID {
				seen++
			}
		}
		if seen != 1 {
			t.Fatalf("List returned the user %d times, want 1", seen)
		}
	}
}

// TestUpdateRoleRejectsUnknownUser: the replacement reads before it deletes, so
// an unknown id must be a no-op rather than a delete of nothing followed by an
// insert of zero values.
func TestUpdateRoleRejectsUnknownUser(t *testing.T) {
	ctx := context.Background()
	db, err := nucleus.Connect(ctx, nucleustest.DSN(t))
	if err != nil {
		t.Skipf("connect: %v", err)
	}
	defer db.Close()
	if err := schema.Apply(ctx, db); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	if err := NewUserService(principals.NewStore(db)).UpdateRole(ctx, "no-such-user", "admin"); err == nil {
		t.Fatal("UpdateRole on an unknown user returned nil")
	}
}

func userRows(ctx context.Context, t *testing.T, db *nucleus.Client, userID string) int64 {
	t.Helper()
	type row struct {
		N int64 `db:"n"`
	}
	rows, err := nucleus.Query[row](ctx, db.SQL(),
		`SELECT COUNT(*) AS n FROM principals WHERE id = $1`, userID)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if len(rows) == 0 {
		return 0
	}
	return rows[0].N
}

// TestUpdateRoleRefusesLastLocalAdmin (R08, round 4): removing the final
// local administrator strands recovery — setup stays closed (local principals
// exist) while the password-reset path has no account to reset. The demotion
// is refused inside the serialized mutation; promoting that same user back to
// admin stays legal, and demotion is allowed again once a second admin
// exists.
//
// The scratch Nucleus accumulates local admins from other suites, and this
// gate is defined by their ABSENCE — so the test snapshots every other local
// admin row, removes it for the duration, and restores it verbatim (version
// bumped past its prior so the collapse keeps the restored row). Serial
// execution (-p 1) makes the swap safe.
func TestUpdateRoleRefusesLastLocalAdmin(t *testing.T) {
	ctx := context.Background()
	db, err := nucleus.Connect(ctx, nucleustest.DSN(t))
	if err != nil {
		t.Skipf("connect: %v", err)
	}
	defer db.Close()
	if err := schema.Apply(ctx, db); err != nil {
		t.Fatalf("apply schema: %v", err)
	}

	// Snapshot every standing local admin, then remove them for the test's
	// duration.
	type adminRow struct {
		ID           string `db:"id"`
		Kind         string `db:"kind"`
		Username     string `db:"username"`
		Email        string `db:"email"`
		PasswordHash string `db:"password_hash"`
		Role         string `db:"role"`
		Origin       string `db:"origin"`
		CreatedAt    string `db:"created_at"`
		InvitedBy    string `db:"invited_by"`
		TokenVersion int64  `db:"token_version"`
		Version      int64  `db:"version"`
	}
	snapshot, err := nucleus.Query[adminRow](ctx, db.SQL(),
		`SELECT id, argMax(kind, version) AS kind, argMax(username, version) AS username,
		        argMax(email, version) AS email, argMax(password_hash, version) AS password_hash,
		        argMax(role, version) AS role, argMax(origin, version) AS origin,
		        argMax(created_at, version) AS created_at, argMax(invited_by, version) AS invited_by,
		        CAST(argMax(token_version, version) AS BIGINT) AS token_version,
		        MAX(version) AS version
		 FROM principals WHERE kind = 'local' AND role = 'admin'
		 GROUP BY id`)
	if err != nil {
		t.Fatalf("snapshot local admins: %v", err)
	}
	for _, row := range snapshot {
		if _, err := db.SQL().Exec(ctx, `DELETE FROM principals WHERE id = $1`, row.ID); err != nil {
			t.Fatalf("clear admin %s: %v", row.ID, err)
		}
	}
	t.Cleanup(func() {
		for _, row := range snapshot {
			_, _ = db.SQL().Exec(context.Background(),
				`INSERT INTO principals (id, kind, username, email, password_hash, role, origin, created_at, invited_by, token_version, version)
				 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
				row.ID, row.Kind, row.Username, row.Email, row.PasswordHash, row.Role, row.Origin,
				row.CreatedAt, row.InvitedBy, row.TokenVersion, row.Version+1)
		}
	})

	store := principals.NewStore(db)
	svc := NewUserService(store)
	username := "lastadmin_" + strconv.FormatInt(time.Now().UnixNano(), 36)

	solo, err := svc.Create(ctx, username, username+"@example.com", "hunter2hunter2", "admin", "bootstrap")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.SQL().Exec(context.Background(), `DELETE FROM principals WHERE id = $1`, solo.UserID)
	})

	if err := svc.UpdateRole(ctx, solo.UserID, "viewer"); err == nil {
		t.Fatal("demoting the only local admin succeeded — recovery is stranded")
	}
	// Re-promotion of the standing admin is a no-op role change and must
	// stay legal.
	if err := svc.UpdateRole(ctx, solo.UserID, "admin"); err != nil {
		t.Fatalf("re-promoting the standing admin failed: %v", err)
	}

	second, err := svc.Create(ctx, username+"_2", username+"2@example.com", "hunter2hunter2", "admin", "bootstrap")
	if err != nil {
		t.Fatalf("create second admin: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.SQL().Exec(context.Background(), `DELETE FROM principals WHERE id = $1`, second.UserID)
	})
	if err := svc.UpdateRole(ctx, solo.UserID, "viewer"); err != nil {
		t.Fatalf("demotion with a second admin standing was refused: %v", err)
	}
}
