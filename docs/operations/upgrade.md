# Upgrading Observe

Use the tool that installed Observe. `observe upgrade` refuses to touch a
Homebrew or container install and tells you which command to use instead
(`runUpgrade` in `cmd/observe/main.go`).

## Before every upgrade

1. Take a backup and check it (see [backup-restore.md](backup-restore.md)).
   This is the only way back from a bad schema change; see "What rollback
   covers" below.
2. Read `CHANGELOG.md` for the target version, especially migration notes.
3. Check `/healthz` is `ok` on the current version.

## Commands

```bash
# Direct Linux/systemd install (must be root; binary installed by install.sh)
sudo observe upgrade
sudo observe upgrade --version v1.2.3
#   --version <version>  release to install (default: latest)
#   --service <unit>     systemd unit name (default: observe.service)
#   --health-url <url>   readiness URL (default derived from OBSERVE_ADDR)

# Homebrew
brew upgrade useteploy/tap/observe

# Docker Compose
docker compose pull && docker compose up -d
# pin with OBSERVE_VERSION=1.2.3

# Re-running the install script also upgrades a direct install
OBSERVE_VERSION=v1.2.3 sh install.sh
```

`observe upgrade` requires Linux, root, systemd, and a release build (a
development build cannot be compared). It takes an upgrade lock, refuses to
downgrade (`refusing downgrade X -> Y`) or reinstall the same version, and
requires the service to be active.

## What `observe upgrade` does (internal/upgrade/upgrade.go)

1. Downloads `checksums.txt` and `checksums.txt.sig` for the target release,
   verifies the Ed25519 signature against the public key compiled into the
   binary, then verifies the archive SHA-256. Fails closed; the running server
   stays online during this staging.
2. Asks systemd to stop the unit (graceful: Observe flushes buffers on
   SIGTERM; `TimeoutStopSec=90s`).
3. Atomically replaces the binary, keeping the previous one for restore.
4. Starts the unit and polls `/healthz` until it has returned three
   consecutive healthy responses reporting the exact new version.
5. If start or the readiness check fails, it stops the unit, restores the
   previous binary, restarts it and waits for it to be healthy again.

There is a short restart window with no ingest. SDKs retry; the WAL
(`OBSERVE_QUEUE_DIR`) replays acknowledged-but-unapplied events on the next
start.

## What rollback covers: the binary only

The automatic rollback restores the **previous executable**. It does not
restore data.

- **Migrations run at startup, in the new binary, and are forward-only.**
  `internal/schema/migrations/` contains only `NNN_name.up.sql` files; there
  are no down migrations and no tooling to reverse one.
- If the new binary applied migrations and then failed its readiness check,
  the old binary is restored **against the already-migrated database**. A
  migration that failed part-way may also have left tables half-changed. The
  old binary may or may not run against the new schema; nothing in the repo
  verifies that combination.
- Downgrading deliberately is refused by `observe upgrade`. To go back to an
  older version after a migration you must restore a pre-upgrade backup into
  an empty database with the older binary (see the caveats in
  [backup-restore.md](backup-restore.md)), which loses data written since the
  backup.

### Known risk: ALTER-ADD migrations on populated tables (AUDIT_OPEN L9)

Open P1 item L9 in `AUDIT_OPEN.md`: migrations 048, 049, 051, 052, 054 and 056
use `ALTER TABLE ... ADD COLUMN IF NOT EXISTS` on existing tables. On a
populated Nucleus store, migration 054 failed twice with `corrupt tuple`
and left `llm_traces` unreadable (an upstream engine defect; the table could
not be read afterwards). The test suite cannot catch this because it runs
migrations against empty tables. As of this writing the item is open: there is
no populated-store migration test, and no repair has been decided. Practical
consequences for upgrades:

- Always take and verify a backup first.
- After upgrading across those migrations on a populated install, check the
  LLM views and `/healthz`; a failure there is this issue, not something your
  rollback fixes.
- New migrations are required by project rules to avoid ALTER-ADD on existing
  tables (rename-aside + create + copy instead), but earlier ones are already
  shipped.

## Verify after upgrading

```bash
observe version
curl -fsS http://127.0.0.1:3000/healthz     # status ok, expected version
sudo journalctl -u observe -n 100           # look for "migrations complete"
```

Then spot-check a dashboard page that reads recently written data and confirm
new events arrive.
