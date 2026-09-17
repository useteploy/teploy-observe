# Open audit items

Unresolved findings for this repository from the ChatGPT-led audit series.
Register: teploy-observe audit 2026-09-17 (51 findings, pinned at
`6d49fcc380781e64e85b30b2d227ee45cd343f0c`; report lives outside the repo).
Earlier sweeps (2026-09-09 through 2026-09-11, passes 1-5) are closed
history; their one surviving item is folded into F16 below.

Pass record (2026-09-17 audit, remediation session same day):

- Fixed: F01, F04, F06, F07, F08, F09, F10, F11, F12 (admission half),
  F13, F14, F15, F17, F18, F19, F20, F21, F22, F23, F24, F25, F26, F27,
  F28, F29, F30, F31, F32 (transport half), F33, F34, F35, F36, F37, F40,
  F42, F43, F44, F48, F50, F51, plus the replay-half of F28.
- Deferred with rationale: F02, F03, F05, F12 (idempotency half), F16,
  F19 (full batch idempotency), F32 (client retry half), F37 (default-mask
  policy), F38 (asset proxy), F39, F41, F45 (app half), F46 (dedicated key
  migration), F47, F49 (UI-freshness gate).
- Upstream: F45 (snapshot-boundary primitive — logged in Tyler's
  `Teploy/_internal/UPSTREAM_BUGS.md`, 2026-09-17 entry).
- False positives: none — every finding verified against source before
  fixing or deferring.

Open items: 14 (2 P1-adjacent design decisions, 11 P2 designs, 1 CI gate)

## F02 - P1 - Open (design decision): first-run grace authorizes administration

The no-admin grace period in JWTAuthMiddleware/RequireRole admits anonymous
callers to protected routes on an unclaimed installation. F01's OIDC guard
closed the SSO-only variant; the remaining window is inherent to a
zero-config setup wizard reachable on an unclaimed install.

Deferred because the audit's prescribed fix (mandatory out-of-band
`OBSERVE_SETUP_TOKEN` + removing the grace bypass entirely) breaks the
documented first-run experience (`docker run` → wizard → admin) and is a
product decision, not a contained bug fix. Mitigations in place: the
documented deployment topology keeps :3000 tailnet-only (the public ingest
listener 404s setup routes by default-deny), EnsureAdmin is atomic
first-writer-wins, and the grace closes permanently once any admin exists.
Revisit trigger: observe ever exposed on a public address by default, or a
request for headless/multi-tenant installs.

## F03 - P1 - Open (architecture): user management and authentication use different principal stores

`platform/users` and `auth/admin_users` are separate tables with no
synchronization; managed users cannot log in and role changes don't revoke
tokens. Requires the canonical-principal migration the audit sketches
(atomic credential+profile creation, role changes bumping token_version,
backfill of existing managed users). Deferred as a schema + product
migration, not a contained fix; the audit's regression gate (create → login
→ promote → old token invalid) is the acceptance test when it lands.

## F05 - P2 - Open (architecture): OIDC sessions bypass revocation; identity key omits issuer

SSO-minted JWTs (24h, tv=0) have no principal row to revoke and `oidc:<sub>`
is not namespaced by issuer. Fixing it needs the F03 principal store plus an
admin "revoke sessions" operation; deferring with F03 as one migration.
Operational note until then: rotating OBSERVE_JWT_SECRET is the documented
emergency revocation (it invalidates all sessions, local and SSO).

## F12 - P1 - Partially fixed: batch admission atomic, stable producer IDs deferred

All-or-nothing batch admission landed (Buffer.Avail pre-check; the only
mid-batch failure mode was buffer-full after a partially accepted prefix).
Still open: stable producer-side event IDs + a durable idempotency boundary
so a client retry after an ambiguous response-loss cannot double-count. A KV
SetNX followed by SQL INSERT is NOT atomic on Nucleus; this needs the same
backend-proven-atomicity analysis as F19. Deferred as a wire-protocol
version bump (SDKs ship with the server).

## F16 - P2 - Open (design, previously deferred): WAL disk high-water and bounded streaming replay

Prior register item (teploy-observe-03 deferred half), unchanged: maxBytes
is a compaction threshold, not a cap; Pending() materializes the backlog.
Needs the numbered-segment design (seal at size, checkpoint per segment,
delete acknowledged sealed segments, bounded replay iterator) plus an
explicit operator policy for limits and breach behavior. F14's sticky
degradation + admission refusal (landed) means an ailing WAL now fails loud
instead of growing quietly, which lowers the urgency but does not close it.

## F19 - P1 - Fixed for sessions; batch idempotency open

The claim-then-insert orphan window is gone (replay_sessions is a replacing
table keyed on the replay; sessions upsert as merged versions; migration
039). Still open: deterministic child IDs derived from (site, replay,
batch, index) so a partially committed event batch retries without
duplicating children or heatmap contributions. Same idempotency design
constraint as F12; tracked together.

## F32 - P1/P2 - Partially fixed: transports no longer drop silently; bounded client retry deferred

Landed: res.ok checks, byte-bounded keepalive, beacon-return checks, chunked
batches, failed-chunk retention (bounded), onError hook, reinit disposal.
Deferred: automatic client retries beyond the retained-buffer retry, because
retrying without producer IDs can double-count — unblocked by F12/F19.
Oversized individual replay snapshots need a chunking protocol or a normal
foreground request (tracked with F39's protocol work).

## F37 - P1 - Fixed for form controls/contenteditable/data-*; default-text-mask policy open

The demonstrated leak (textarea initial content, contenteditable text,
token-bearing data attributes) is closed in the serializer. Still open as a
product decision: masking ALL visible text by default with an explicit
`data-observe-public-text` opt-in (the audit's preferred posture) vs the
current visible-text-by-default posture. Changing it silently blanks every
existing replay's readable content; needs an operator-visible toggle.

## F38 - P2 - Fixed structural path; asset proxy open

Structured snapshots render through a strict tag/attribute allowlist with a
deny-all CSP posture. Open: any legitimate replay image/style (none exist
today — the tracker strips styles/scripts and now blocks img-bearing
elements) would need a separately designed asset proxy/allowlist; legacy
raw-HTML snapshots get a best-effort injected CSP only.

## F39 - P2 - Open (protocol): recorded mutations cannot reconstruct a changing page

Mutation records are summaries; the player replays mouse/click/scroll over
the initial snapshot. Full fix needs a versioned DOM-delta protocol with
stable node IDs and deterministic seeking (audit's throttled re-snapshot
fallback is the smaller implementable step). Deferred as feature work with
real protocol design, not a bug patch.

## F41 - P2 - Open (policy): shared sensitive-data policy for URLs and autocaptured text

Trackers send full location.href (query/fragment), href attributes, and
element text. Not fixed client-side because the server's UTM analytics
READ the query string — a naive client-side strip breaks attribution. Needs
the audit's designed contract: strip credentials/query/fragment by default,
extract allowlisted campaign params into explicit fields (server-side
extraction from an already-stripped URL is impossible), text capture
opt-in. Wire + storage + migration change; deferred.

## F45 - P2 - Open (app half + upstream): backups omit KV source maps; no consistent snapshot

Upstream: Nucleus has no cross-table snapshot/lease primitive — logged in
`Teploy/_internal/UPSTREAM_BUGS.md` (2026-09-17 entry), status open; no
Neutron/Nucleus edits made from this session.
Ours, deferred: source-map blobs and release indexes live in Nucleus KV
(`srcmap:*`) and are absent from backup.Tables; adding them needs a durable
KV-domain dump section + restore validation keyed on the namespace, plus
the quiesce/drain boundary once upstream provides a snapshot primitive.
Meanwhile F42/F43/F44 (landed) make the SQL-domain archive encrypted,
honest, and completeness-checked.

## F46 - P2 - Partially fixed: empty audit key warns at startup

Startup now states the chain is unkeyed instead of silently HMAC-ing with
an empty key. Open: the dedicated persistent audit key (base64 ≥32 bytes),
key_id + historical verification keyring for rotation, and the encoding
migration that preserves verification of existing rows — a contract change
recorded here rather than slipped in.

## F47 - P2 - Open (architecture): verification accepts a valid-prefix tail

verifyChain cannot detect truncation without an independently durable
anchor. Needs an append-only external sink (or WORM store) publishing
(seq, hash) heads and verification against it, with crash-consistency
defined between Record and anchor publish. Upstream-adjacent (a second
mutable row in the same DB is not independent); deferred with design notes
in the audit report. Streaming/paginated verification is part of the same
work.

## F49 - P2 - Partially fixed: SDK CI jobs added; embedded-UI freshness gate open

sdk-browser and sdk-python are now required CI jobs. The
`git diff --exit-code -- cmd/observe/ui/dist` freshness gate stays deferred
until the UI build environment (Neutron TS workspace) is reproducibly
provisioned in CI — until then ui-sync remains a documented local step
(this session's UI fixes at 5402af4 need one such sync).

## Resolution log (2026-09-12 — prior register, closed)

- teploy-observe-06: FIXED - single owned flush loop with a capacity-one wakeup channel; size/timer/shutdown flushes share one lifecycle (audit commit).
- teploy-observe-03: PARTIALLY FIXED - queue dirs now 0700 and WAL/checkpoint files 0600. DEFERRED (design): disk high-water backpressure and bounded streaming replay — now item F16 above.

## Resolution log (2026-09-17 — 51-finding pass)

Fixed in commits a1cd5a4 (F01 F04 F06), ca7d68b (F07 F08 F09 F19 F20
F28-replays), 8cb6145 (F10 F11 F12-admission F13 F14 F15 F17 F18),
c8743ea (F21 F33-trackers F37 F27-FTS), 918b62d (F24 F25 F26 F27 F42 F43
F44 F46-warn), 59c2244 (F22 F23 F28-llm), c96bf53 (F29-F33 F34-F36 F51 +
F32 classic trackers), 5402af4 (F27-helper F38 F40), a3466c2 (F48 F49-jobs
F50). Gates: `go vet ./...`, `go test ./...` (DB-dependent suites
self-skip without a live Nucleus), sdk/go `go test -race`, sdk/browser
`npm run typecheck && npm test && npm run build`, sdk/python `pytest` —
all green. Not run: Nucleus-backed integration suites, e2e, embedded UI
rebuild (needs the Neutron TS workspace; run scripts/ui-sync.sh before
release).
