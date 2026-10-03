# Multi-tenancy and per-site RBAC (NOT BUILT)

Status: design note. Nothing here is implemented. Observe is single-tenant per
deployment, and roles are global to the instance.

## Current state

- **One tenant.** Rows carry a `tenant_id` column (for example `sites`,
  `logs`, the rollup tables; migration 001 calls it a "placeholder for future
  multi-tenant"), but the only value ever written is `"default"`
  (`internal/sites/sites.go` `EnsureDefault` inserts `tenant_id = "default"`).
  No code resolves a tenant from a request, and nothing enforces isolation by
  tenant. `docs/migrations/from-sentry.md` states "each Observe deployment is
  one tenant".
- **Roles are instance-wide.** The JWT carries one `role` claim
  (`admin` / `editor` / `viewer`, `internal/auth/auth.go`,
  `internal/auth/middleware.go`); local users store a single `role`
  (migration 010 on `admin_users`, unified principals in 040); OIDC maps IdP
  claims or groups to that single role (`internal/auth/oidc.go`). No table
  links a user to a set of sites (searched the migrations and Go code for a
  user-to-site or site-ACL structure; none exists). Any authenticated user can
  name any `site_id` in a request, subject only to the role gate.
- **API keys are site-scoped** (`internal/auth/apikeys.go`): a key writes only
  to its own site. That is the one per-site boundary that exists, and it
  covers ingest and publish-scope uploads only.

## Requirements if built

1. A membership model: user (or IdP group) -> site -> role, with a rule for
   the default (instance admin sees all sites).
2. Every site-addressed read and write path (dashboard API, SQL explorer, MCP
   tools, exports, share links, replays, source maps) checks membership, not
   only role. This is the large part: `cmd/observe/main.go` registers on the
   order of 180 routes, most taking `site_id` from the query or body.
3. The SQL explorer (`POST /api/v1/query`) lets an editor run arbitrary
   read-only SQL; a role check alone cannot scope it per site. It needs
   rewriting through site-filtered views, or to stay admin-only.
4. Real multi-tenancy additionally needs `tenant_id` enforced in every query
   and rollup, separate salts and secrets per tenant, per-tenant quotas
   (`OBSERVE_RATE_LIMIT` and the WAL budget are global today), and
   tenant-aware backup/restore (`internal/backup` dumps all tables as one
   unit).
5. OIDC role mapping extended to per-site group mappings.

## Risks

- **IDOR across sites** is the main hazard; the project already treats it as a
  mandatory adversarial test class. A missed handler is a data leak.
- Retrofitting site filtering onto Nucleus queries cannot be verified by CI
  here (integration tests need a live Nucleus and are skipped).
- ALTER-ADD migrations are banned on populated tables (AUDIT_OPEN L9), so a
  membership table must be a new `CREATE TABLE IF NOT EXISTS`, and adding a
  column to existing tables means rename-aside.

## Recommended approach

Do per-site RBAC (not multi-tenancy) first, as deny-by-default middleware: a
`user_site_roles` table, a `RequireSiteAccess(role)` wrapper applied centrally
where handlers resolve `site_id`, and a test that enumerates the route table
and fails on any `site_id`-taking route without the wrapper. Defer true
tenants; if customer isolation is needed, run one Observe per tenant (the model
that works today).

## Open owner decisions

- Is the target "teams inside one company" (per-site RBAC) or "separate
  customers" (tenancy)? They are different projects.
- Should the SQL explorer remain admin-only once site-restricted users exist?
- Do site-restricted users get share links and exports?
- Where do memberships come from: the local admin UI only, or OIDC groups too?
