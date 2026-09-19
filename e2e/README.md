# Observe end-to-end tests

Playwright suite that drives the Observe UI against a running instance.

## Run

```
cd e2e
npm install
npm run install-browsers   # one-time Chromium download
OBSERVE_URL=http://localhost:3000 npm test
```

Defaults to http://localhost:3000 and logs in with the default admin/observe
credentials. Override with env vars:

- `OBSERVE_URL` — base URL (default http://localhost:3000)
- `CI=1` — enables retries and GitHub-style reporter

## What's covered

- `smoke.spec.ts` — boot-level smoke (CI): login -> dashboard renders -> one
  seeded replay opens in the player.
- `core.spec.ts` — core product paths (CI): dashboard stat cards render,
  replay transport controls work (play/pause toggle, scrub seeks), the audit
  view renders the trail. Logs in once per worker via the API and injects
  the token (the server rate-limits logins per IP).
- `auth.spec.ts` — login flow; unauthenticated redirect.
- `routes.spec.ts` — every sidebar route returns 200 with no console errors.
- `cmdk.spec.ts` — Cmd-K palette opens, search, Enter navigates.
- `docs-and-api.spec.ts` — Swagger UI + OpenAPI paths count >80.

CI runs smoke + core only; the remaining specs are the fuller local suite.

## Add a test

Drop a `*.spec.ts` under `tests/`. Follow the existing pattern — use
`login()` from `auth.spec.ts` for authenticated flows.
