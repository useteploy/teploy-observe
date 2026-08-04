// Package schema owns Observe's database migrations.
//
// They used to be embedded directly in package main, which meant no test could
// reach them: every suite needing a real table — auth, api keys, cohorts,
// backup — either created an ad-hoc table of its own or failed with "relation
// does not exist", and since they all skip when no database is present, that
// failure was invisible. The security tests among them had never run.
//
// Owning the migrations here lets main and the tests build the same schema
// from the same source, so a test exercises the tables production has.
package schema

import (
	"context"
	"embed"

	"github.com/neutron-dev/neutron-go/nucleus"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// FS returns the embedded migration files.
func FS() embed.FS { return migrationsFS }

// Apply runs every migration against db, bringing it to the current schema.
func Apply(ctx context.Context, db *nucleus.Client) error {
	migrations, err := nucleus.LoadMigrations(migrationsFS)
	if err != nil {
		return err
	}
	return db.Migrate(ctx, migrations)
}
