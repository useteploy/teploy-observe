# Changelog

All notable changes to Observe are recorded here.

## Unreleased

### Added
- Disk-backed ingest WAL (`internal/ingest/queue.go`) — events survive
  `SIGKILL` and replay on restart.
- Per-site rate limiting with per-site caps (`sites.ratelimit_per_second`).
- Role-based access control: JWT carries a `role` claim; writes require
  `admin` or `editor`.  Viewer is reads-only.
- SQL explorer `POST /api/v1/query/explain` returns a Nucleus plan for
  editor and admin roles.
- Instance-wide AI-assistant groundwork (stubbed, ships in 0.2).

### Changed
- Ingest batch writes now use parameterized multi-row `INSERT` via
  Nucleus' SimpleProtocol.  `escapeSQL` and the string-concatenated
  `INSERT` path have been removed.
- Explorer read-only guard replaced with a tokenising lexer; comment
  and stacked-statement bypasses are rejected.
- `?token=` query-string authentication accepted only on `GET` and only
  for the streaming / download route allowlist.
- `ChangePassword` now uses `UPDATE`; the previous blind `INSERT`
  created duplicate rows.

### Fixed
- `ChangePassword` row duplication on repeated password changes.
