# Contributing to Observe

Thanks for the interest. Observe is a solo-founder project aiming for
self-host-first UX. Contributions that keep that surface small and
correct are most welcome.

## Ground rules

1. **Correctness over features.** A shipped feature without tests isn't
   shipped. `scripts/audit.sh` must pass at zero failures before a PR is
   merged.
2. **Preserve the single-binary story.** New dependencies need a reason.
   If a feature needs Kafka / Clickhouse / Kubernetes, it belongs in a
   different project.
3. **No emojis in product output.** Colorful icons in a professional
   observability tool look amateur. Clean SVG icons are fine.
4. **Dogfood Neutron / Nucleus.** If you hit a limitation in the
   framework, file the upstream issue AND link it from the workaround.

## Workflow

1. Fork and create a branch off `main`:

       git checkout -b feat/short-description

2. Build and test locally:

       scripts/ui-sync.sh         # rebuild UI + Go binary
       scripts/dev.sh &            # start Nucleus + Observe
       scripts/audit.sh            # must be 0 FAIL
       scripts/smoketest.sh        # 51-endpoint smoke

3. Commit with a conventional prefix: `feat:`, `fix:`, `refactor:`,
   `docs:`, `test:`, `chore:`.

4. Open a PR. Describe the "why" in one paragraph. Keep the diff small.

## Architecture notes

- Go binary embeds the Neutron/TS dashboard via `cmd/observe/ui/dist`.
- Nucleus is a sidecar process speaking pgwire on :5432. Observe connects
  via the `neutron-go/nucleus` client.
- All numeric SQL parameters go through `dbutil.IntParam` and are
  `CAST($N AS BIGINT)` in queries — a current Nucleus wire-protocol
  quirk.
- Ingest is WAL-backed (`internal/ingest/queue.go`). Every `Push` is
  durable before flush to Nucleus.

## Reporting security issues

Do not open a public GitHub issue for security problems. See `SECURITY.md`.
