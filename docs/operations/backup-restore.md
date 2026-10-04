# Backup and restore

Source: `cmd/observe/main.go` (`runBackup`, `runRestore`) and
`internal/backup/` (`backup.go`, `restore.go`, `crypto.go`, `lease.go`,
`kvsrcmap.go`). Both commands connect to Nucleus using `OBSERVE_NUCLEUS_URL`
(same variable as the server). Diagnostics go to stderr; the archive goes to
stdout (backup) or is read from stdin (restore).

## Take a backup

```bash
observe backup | zstd > observe-$(date +%F).tar.zst
```

(`observe help` prints the same example with the legacy `teploy-observe`
name.) Exit status is 0 on success, 1 for a bad encryption configuration or
connection failure, 2 if the dump finished with per-table errors. Always check
the exit status of the pipeline (`set -o pipefail`); a status 2 archive is
incomplete and its trailing results record marks the failed tables, which
restore refuses.

What is in it:

- Every table listed in `backup.Tables` (a test, `TestTablesMatchSchema`,
  keeps that list equal to the tables the migrations create). Excluded:
  `_neutron_migrations` (rebuilt by migrations) and `<name>_preNNN`
  rename-aside recovery copies.
- The KV source-map section (uploaded source maps), when present.
- A manifest recording the table list, whether the dump ran inside a Nucleus
  snapshot lease (one point-in-time moment across SQL tables; writers wait at
  the engine while it is held, so expect a brief ingest stall on a large
  database), and the non-secret audit-key id.

What is **not** in it, and must be backed up separately:

- `$OBSERVE_DATA_DIR/audit.key` (default `./data`, `/var/lib/observe` in
  Compose and systemd) or the `OBSERVE_AUDIT_KEY` / `OBSERVE_AUDIT_KEYRING`
  values. Without it, keyed audit history stops verifying after a restore onto
  a new host. The manifest only says which key id was in use.
- `OBSERVE_SECRET_KEY` (decrypts the stored LLM key and S3/R2 credentials),
  `OBSERVE_JWT_SECRET`, `OBSERVE_SESSION_SALT` and the rest of
  `/etc/observe/observe.env`. Keep them with the backup key, but in a
  separate secret store.
- The ingest WAL (`$OBSERVE_QUEUE_DIR`). Events acknowledged but not yet
  applied to Nucleus live there; a database-level backup does not include
  them. Stop the server gracefully (it flushes) before a final backup you
  plan to restore from.

## Encryption

Archives contain password hashes, API keys, integration and webhook secrets
and share tokens as plaintext JSON. Set `OBSERVE_BACKUP_ENCRYPTION_KEY` to
wrap the whole archive in AES-256-GCM (chunked, 64 KiB per chunk, magic header
`OBSBKv1`):

```bash
export OBSERVE_BACKUP_ENCRYPTION_KEY="$(openssl rand -base64 32)"   # 32 raw bytes, standard base64
observe backup > observe-$(date +%F).obsbk
```

Encryption happens inside `observe backup`, so its output is already
ciphertext and will not compress; compress nothing (or use a filesystem that
does).

With the variable unset, `observe backup` writes a plaintext archive and logs
a warning. A malformed key is a hard error (exit 1). Restore needs the **same**
variable set to decrypt; losing the key loses the backup. An encrypted archive cannot be read without it.

## Restore

```bash
zstdcat observe-2026-04-17.tar.zst | observe restore
# encrypted:
OBSERVE_BACKUP_ENCRYPTION_KEY=... observe restore < observe-2026-04-17.obsbk
```

Behaviour (`internal/backup/restore.go`):

1. The stream is spooled to a local temp file (bounded at 200 GiB), so make
   sure the temp directory has room.
2. A full validation pass runs before anything is written: manifest present
   and compatible, table names on the allowlist, every row structurally valid,
   exactly one trailing results record with no failed tables, row counts
   matching. A bad, truncated or partial archive changes nothing.
3. **The target must be empty.** Restore appends; if any backed-up table
   (or the KV source-map namespace, when the archive has KV data) already
   holds rows it refuses with `restore target is not empty - nothing was
   written`. There is no merge mode.
4. Rows are applied in batches per table. It is not one transaction: an
   infrastructure failure mid-apply can leave earlier batches and tables
   committed, and the command exits non-zero naming the table. Recover by
   discarding that database and starting the restore over on a fresh one.

Restore does not create tables. The schema must already exist, which today
means applying the migrations. `observe migrate` (added 2026-10-04) does
exactly that and nothing else: no site seeding, no bootstrap admin, no
listener. A restore into a database the SERVER has ever started against
is still refused — boot inserts the `default` site and the bootstrap
admin, and both tables are restore tables.

## Restore drill (do this before you need it)

Run on a scratch host or a second Compose project, never the live database.

1. Create a fresh, empty Nucleus (new data directory or volume).
2. Point `OBSERVE_NUCLEUS_URL` at it, set the same `OBSERVE_SECRET_KEY`,
   `OBSERVE_JWT_SECRET` and `OBSERVE_SESSION_SALT` as production, leave
   `OBSERVE_ADMIN_PASSWORD` unset, and copy `audit.key` into the new
   `OBSERVE_DATA_DIR`.
3. Run `observe migrate` once so the schema exists, then `observe
   restore` with the encryption key; confirm exit status 0 and the log
   line `restore complete`. (Do NOT start the server first — its seeding
   writes rows the emptiness check refuses.)
4. Start `observe`. Check `/healthz` is `ok`.
5. Log in with a production account; compare site list, a known issue, a
   known dashboard and a recent day of stats against production.
6. Open a source-mapped stack trace to confirm the KV source maps came back.
7. Check the audit log still verifies (the audit chain is keyed by
   `audit.key`; the manifest records the expected key id).
8. Record: archive size, backup duration, restore duration, and the gaps you
   hit. Repeat on a schedule and after every upgrade that adds migrations.
10. Destroy the scratch instance; it holds production data.
