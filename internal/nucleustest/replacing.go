// Package nucleustest holds test helpers that put a scratch Nucleus into the
// state a long-lived one is actually in.
package nucleustest

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"
)

// AsPlainMergeTree recreates table without its replacing_mergetree engine, and
// restores the replacing form on cleanup.
//
// It exists because a ReplacingMergeTree's read-time collapse is NOT a property
// of the stored table — it is an entry in a process-global registry that
// Nucleus populates when it executes CREATE TABLE, and repopulates on restart
// only from the tables listed in its data directory's engines.json. Tables
// created before engines.json existed are absent from it, so after the next
// restart they read as plain MergeTrees: every superseded version is returned.
// On the live instance engines.json lists exactly one table (audit_events), so
// this is the state EVERY other replacing table is in — issues there holds
// 16,847,389 physical rows and a bare COUNT(*) returns all of them.
//
// A test on a freshly created table therefore proves nothing: the registry is
// warm, the engine collapses, and the buggy and fixed code agree. Recreating
// the table this way reproduces the live read behaviour so the difference is
// observable.
//
// columns is the parenthesised column list and orderBy the parenthesised ORDER
// BY key, both exactly as the table's migration declares them.
func AsPlainMergeTree(t *testing.T, db *nucleus.Client, table, columns, orderBy, versionColumn string) {
	t.Helper()
	ctx := context.Background()
	exec := func(q string) {
		t.Helper()
		if err := ddl(ctx, db, q); err != nil {
			t.Fatalf("nucleustest: %s: %v", q, err)
		}
	}
	exec("DROP TABLE IF EXISTS " + table)
	exec(fmt.Sprintf("CREATE TABLE %s %s WITH (engine = 'mergetree') ORDER BY %s",
		table, columns, orderBy))
	t.Cleanup(func() {
		if err := ddl(ctx, db, "DROP TABLE IF EXISTS "+table); err != nil {
			return
		}
		_ = ddl(ctx, db, fmt.Sprintf(
			"CREATE TABLE %s %s WITH (engine = 'replacing_mergetree', version_column = '%s') ORDER BY %s",
			table, columns, versionColumn, orderBy))
	})
}

// ddl runs one DDL statement, retrying a catalog-persistence failure.
//
// Nucleus rewrites catalog.json and the table metadata on every CREATE / DROP,
// and neither write is safe against a concurrent one: `go test ./...` runs
// packages in parallel, so two of these helpers firing at the same moment
// produce "catalog persistence failed: I/O error: No such file or directory"
// or "metadata persistence failed: meta rename: No such file or directory".
// Both are transient — the retry succeeds on the next attempt — and both are
// Nucleus bugs, not ones this repo can fix from here.
func ddl(ctx context.Context, db *nucleus.Client, q string) error {
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		if _, err = db.SQL().Exec(ctx, q); err == nil {
			return nil
		}
		if !strings.Contains(err.Error(), "persistence failed") {
			return err
		}
		time.Sleep(time.Duration(attempt+1) * 100 * time.Millisecond)
	}
	return err
}
