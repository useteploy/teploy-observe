package platform

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
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

	svc := NewUserService(db)
	username := "role_" + strconv.FormatInt(time.Now().UnixNano(), 36)

	u, err := svc.Create(ctx, username, username+"@example.com", "hunter2hunter2", "admin", "bootstrap")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.SQL().Exec(context.Background(), `DELETE FROM users WHERE user_id = $1`, u.UserID)
	})
	createdAt := u.CreatedAt.UnixMilli()

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
		if got.PasswordHash != u.PasswordHash && got.PasswordHash == "" {
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
	if err := NewUserService(db).UpdateRole(ctx, "no-such-user", "admin"); err == nil {
		t.Fatal("UpdateRole on an unknown user returned nil")
	}
}

func userRows(ctx context.Context, t *testing.T, db *nucleus.Client, userID string) int64 {
	t.Helper()
	type row struct {
		N int64 `db:"n"`
	}
	rows, err := nucleus.Query[row](ctx, db.SQL(),
		`SELECT COUNT(*) AS n FROM users WHERE user_id = $1`, userID)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if len(rows) == 0 {
		return 0
	}
	return rows[0].N
}
