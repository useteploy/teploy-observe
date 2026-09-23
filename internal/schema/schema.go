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
	"fmt"
	"strings"

	"github.com/neutron-build/neutron/go/nucleus"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// FS returns the embedded migration files.
func FS() embed.FS { return migrationsFS }

// Apply runs every migration against db, bringing it to the current schema.
func Apply(ctx context.Context, db *nucleus.Client) error {
	_, err := ApplyWithAdoption(ctx, db)
	return err
}

// legacyHistoryRefusal matches the M04 runner's contract refusal string
// ("... is recorded in a legacy history format and must be adopted once
// before this runner continues"). The phrase is the cross-implementation
// contract wording (Go and TS SDKs emit it); the SDK exports no sentinel,
// so this narrow match is the seam. An upstream sentinel would replace it.
const legacyHistoryRefusal = "must be adopted once before this runner continues"

// ApplyWithAdoption runs every migration against db, adopting a legacy
// migration history first when the M04 runner refuses it.
//
// Adoption is the SDK's explicit, transactional graduation (verified rows
// re-recorded under the v2 checksum; unprovable rows adopted with a NULL
// checksum and reported, never silently baselined). The report is returned
// so the caller can surface what was trusted and what was not; it is nil
// when no adoption happened.
func ApplyWithAdoption(ctx context.Context, db *nucleus.Client) (*nucleus.MigrationAdoptionReport, error) {
	migrations, err := nucleus.LoadMigrations(migrationsFS)
	if err != nil {
		return nil, err
	}
	err = db.Migrate(ctx, migrations)
	if err == nil || !strings.Contains(err.Error(), legacyHistoryRefusal) {
		return nil, err
	}
	report, err := db.AdoptMigrations(ctx, migrations)
	if err != nil {
		return nil, fmt.Errorf("adopt legacy migration history: %w", err)
	}
	return report, db.Migrate(ctx, migrations)
}
