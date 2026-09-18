# Open audit items

Unresolved findings for this repository from the ChatGPT-led audit series.
Register: teploy-observe audit 2026-09-17 (51 findings, pinned at
`6d49fcc380781e64e85b30b2d227ee45cd343f0c`; report lives outside the repo).
Round 2: audit 2026-09-17 (56 findings AUD-001..AUD-056, pinned at
`bbd2fe9038863db0447ac1335554451feb89735a`; report lives outside the repo)
— remediation record below. Earlier sweeps (2026-09-09 through 2026-09-11,
passes 1-5) are closed history; their one surviving item is folded into F16
below.

Pass record (2026-09-17 audit, remediation session same day):

- Fixed: F01, F03, F04, F05, F06, F07, F08, F09, F10, F11, F12 (admission
  half), F13, F14, F15, F17, F18, F19, F20, F21, F22, F23, F24, F25, F26,
  F27, F28, F29, F30, F31, F32 (transport half), F33, F34, F35, F36, F37,
  F40, F42, F43, F44, F48, F50, F51, plus the replay-half of F28.
- Deferred with rationale: F02, F12 (idempotency half), F16,
  F19 (full batch idempotency), F32 (client retry half), F37 (default-mask
  policy), F38 (asset proxy), F39, F41, F45 (app half), F46 (dedicated key
  migration), F47, F49 (UI-freshness gate).
- Upstream: F45 (snapshot-boundary primitive — logged in Tyler's
  `Teploy/_internal/UPSTREAM_BUGS.md`, 2026-09-17 entry).
- False positives: none — every finding verified against source before
  fixing or deferring.

Trust-close session (2026-09-18): F12 (idempotency half) and F19 (full
batch idempotency) FIXED; AUD-008 FIXED; F49 FIXED (ui-freshness gate);
AUD-056 remainder CLOSED for its requested scope (pinned-Nucleus fixture
job + e2e smoke; the fuller browser suite remains future work); F32's
client-retry half is now UNBLOCKED by producer IDs but stays deferred as
SDK feature work. Two NEW upstream engine reports filed (2026-09-18, see
the Upstream section below). F45's app half (KV srcmap in backups) is
unchanged — it waits on the Nucleus snapshot API.

Open items: 12 (1 P1 product/schema decision, 9 P2 designs/halves, 2 hardening/producer notes)

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

## F03 - P1 - Fixed 2026-09-18 (f631c81): user management and authentication use different principal stores

Resolved by migration 040 + internal/principals: one ReplacingMergeTree
store (argMax collapse, 039-style monotonic version stamp) now backs every
identity. platform.UserService writes the same store Login reads, so
created users authenticate immediately; UpdateRole bumps token_version, so
a role change retires every JWT minted under the old role on its next use.
The audit's regression gate (create → login → promote → old token invalid)
is TestF03Gate_CreateLoginPromoteOldTokenInvalid in internal/auth, green
against live Nucleus (docker nucleus-f03). Existing installs converge via
the 040 backfill: legacy ids and token_versions carry across verbatim,
username collisions between the two legacy tables resolve to the
admin-origin account (the one with a working password), and both legacy
tables remain untouched as recovery artifacts. Convergence of an existing
install is corpus-executed: Test040BackfillConvergesExistingInstall
(internal/principals) rewinds the ledger past 040, seeds legacy
admin_users/users rows including the duplicate-row UpdateRole pattern and
a cross-table username collision, and asserts ids, token_versions, the
newest-row collapse, and the admin-origin login tie-break. Follow-ups that
stay open: the atomic bootstrap claim-as-record (AUD-003's deferred half,
unchanged). CI note: the DB-backed suites self-skip without
OBSERVE_NUCLEUS_URL (AUD-056 remainder); they were executed against a live
scratch Nucleus (docker nucleus-f03) at commit time.

## F05 - P2 - Fixed 2026-09-18 (f631c81): OIDC sessions bypass revocation; identity key omits issuer

Landed with F03 as the one migration (040). Principal ids are
issuer-namespaced — oidc:<sha256(issuer)[:16]:<sub> — so two issuers
reusing a subject are two identities. Every SSO sign-in upserts a principal
row and mints its JWT with the row's token_version; the middleware version
check is unconditional, so a row-less token (every pre-040 "oidc:<sub>"
session) is dead on its next request — live SSO sessions were implicitly
rotated by the migration. New admin operation
POST /api/v1/platform/users/{user_id}/revoke-sessions retires one
principal's sessions; rotating OBSERVE_JWTSECRET remains the
all-sessions emergency. An IdP re-sign-in refreshes profile and role but
preserves token_version, so it cannot resurrect a revoked session.
Covered by TestF05OIDCSessionsRevocableAndIssuerScoped,
TestF05SubjectIDIssuerNamespaced, and TestF05Pre040OIDCTokenShapeRejected
against live Nucleus.

## F12 - P1 - Fixed 2026-09-18 (idempotency half; admission half landed 2026-09-17)

The full idempotency boundary for the events path landed with F19 (same
session, see below): producers assign stable event ids at record time and
keep them through requeue/retry (protocol v2: `v`, `producer_id`,
`batch_id`, per-event `event_id` — sdk/browser, observe.js, observe-replay.js
and the Python SDK's event path all ship it; the Go SDK has no
analytics-event producer, so nothing to bump there). Server side has two
layers:

1. Admission cache (in-process, TTL 10 min, bounded): a v2 batch this
   process already admitted is acked `{ok, deduped:true}` without
   re-buffering — the response-lost retry never re-enters the pipeline.
   The key is recorded only after successful admission, so a 429'd first
   attempt stays retryable.
2. Flush-time event-id existence filter (durable): every flush chunk is
   filtered against already-committed event ids inside the chunk's own
   transaction, serialized by the single flush lock — the boundary that
   covers restarts, WAL replay, admission-cache misses, and concurrent
   duplicate submits. Fail-open on lookup error (a possible duplicate
   beats certain loss); the lookup is bounded by a 24 h dedupe horizon
   (a retry's events carry fresh server timestamps, so the original is
   older — the floor reaches 24 h below the retry's own timestamps).

This is deliberately NOT the KV SetNX + SQL INSERT pair the audit rejected:
the final arbiter is a write-shaped existence check at the serialized
flush, with no non-atomic claim anywhere. Multi-replica deployments keep
the standing single-process boundary (same as AUD-018). Proofs:
internal/ingest dedupe unit tests + live-Nucleus integration tests
(duplicate batch -> single count; mixed duplicate+fresh -> only fresh).
Wire-protocol version bumped to v2 with the SDKs, as the audit allowed.

## F16 - P2 - Open (design, previously deferred): WAL disk high-water and bounded streaming replay

Prior register item (teploy-observe-03 deferred half), unchanged: maxBytes
is a compaction threshold, not a cap; Pending() materializes the backlog.
Needs the numbered-segment design (seal at size, checkpoint per segment,
delete acknowledged sealed segments, bounded replay iterator) plus an
explicit operator policy for limits and breach behavior. F14's sticky
degradation + admission refusal (landed) means an ailing WAL now fails loud
instead of growing quietly, which lowers the urgency but does not close it.

## F19 - P1 - Fixed 2026-09-18 (batch idempotency; sessions half landed 2026-09-17)

Deterministic child IDs + a durable batch ledger close the replay-side
idempotency gap. Protocol v2 replay batches carry (producer_id, batch_id,
client-generated replay_id). Server side:

- `replay_batches` ledger (migration 041, ReplacingMergeTree keyed on the
  full identity) records event count + sha256 of the batch's event list.
  It is written INSIDE the same transaction as the session upsert and the
  child inserts — the claim IS a row in the same atomic SQL unit, closing
  the audit's core objection (KV SetNX then INSERT).
- Retry of a committed batch: ledger hit + digest match -> `{ok,
  replay_id, deduped:true}`, zero writes — children, session aggregates,
  and heatmap rollups cannot double-count because none of them run.
- Retry of a rolled-back batch: no ledger row AND no children (one tx),
  full reprocess with child ids re-derived deterministically from
  (site, replay, batch, index) — identical rows land.
- Same (producer, batch) with different content -> 409 ErrBatchIDReuse;
  v2 identity without a client replay_id -> 409 (the key would never be
  stable). v1 payloads keep today's semantics exactly.

The in-process boundary is the striped replay lock + transaction (same
single-process posture as AUD-018); the deterministic child ids make the
children of any cross-process duplicate bit-identical, so a future
unique-key/CAS scheme collapses them retroactively. Proofs:
internal/replays retries integration tests (live Nucleus): duplicate batch
-> no double children/page_count/heatmap; interrupted-batch retry ->
identical child ids; batch-id reuse -> 409. NOTE: the engine's
ReplacingMergeTree has an intermittent committed-upsert-loss defect on
accumulated data (see Upstream below) — the F20 metadata test
(TestIngest_LaterBatchesExtendMetadata) flakes on it; the F12/F19
idempotency tests themselves are unaffected (children and the ledger
write path do not hit the defect).

## F32 - P1/P2 - Partially fixed: transports no longer drop silently; bounded client retry deferred (now UNBLOCKED)

Landed: res.ok checks, byte-bounded keepalive, beacon-return checks, chunked
batches, failed-chunk retention (bounded), onError hook, reinit disposal.
Deferred: automatic client retries beyond the retained-buffer retry —
UNBLOCKED 2026-09-18 by the F12/F19 producer-side identity (a retried batch
now carries stable event/batch ids and the server dedupes it), but the
retry POLICY in each SDK (backoff, budget, giving up) is still unbuilt
feature work. Oversized individual replay snapshots need a chunking
protocol or a normal foreground request (tracked with F39's protocol work).

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

## F49 - P2 - Fixed 2026-09-18: embedded-UI freshness gate in CI

The `ui-freshness` job clones Neutron at THIS repo's pinned submodule
revision (resolved per-run from `git ls-tree HEAD Neutron`), provisions the
TS workspace (`pnpm install --frozen-lockfile`), runs scripts/ui-sync.sh,
and fails on `git diff --exit-code -- cmd/observe/ui/dist`. A source edit
without a ui-sync run can no longer ship a stale dashboard silently.
First push may need provisioning fixes on the runner — the flow is
verified locally (ui-sync is byte-idempotent against the working tree),
but the job itself has not executed on GitHub's runners yet.

## Resolution log (2026-09-12 — prior register, closed)

- teploy-observe-06: FIXED - single owned flush loop with a capacity-one wakeup channel; size/timer/shutdown flushes share one lifecycle (audit commit).
- teploy-observe-03: PARTIALLY FIXED - queue dirs now 0700 and WAL/checkpoint files 0600. DEFERRED (design): disk high-water backpressure and bounded streaming replay — now item F16 above.

## Resolution log (2026-09-17 — release close-out)

- Embedded UI rebuilt via scripts/ui-sync.sh and committed (5fbf61f): every
  hashed asset rotated against the 2026-09-17 audit source changes; Go
  binary builds.
- Nucleus-backed integration suites run for the first time, against a
  fresh live Nucleus (debug build of the current Neutron working tree):
  migrations 038/039 verified on both the fresh-install and pending-upgrade
  paths; 43 packages green, 0 skips, run serially (`-p 1` — concurrent
  packages racing `schema.Apply` trip the known upstream Migrate ledger
  TOCTOU, reconfirmed and logged).
- Defects the run exposed, fixed in dd182f3: 038/039 em-dash prose panicked
  the Nucleus query cache-key normalizer (connection dropped mid-migration;
  migrations from 038 now guarded pure-ASCII); upsertSession selected bare
  non-key columns next to a GROUP BY (invalid SQL, 0A000) and computed
  per-batch instead of session-spanning duration (F20); the api-key
  revocation test predated F09's site-existence check; the raw-retention
  coverage seed was UTC-midnight dependent.
- Upstream reports appended to `Teploy/_internal/UPSTREAM_BUGS.md` (new:
  cache-key normalizer byte-slice panic; reconfirmed: Migrate ledger
  TOCTOU). No Neutron/Nucleus edits made from this session.
- Still not run: e2e (Playwright).

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

## Round-2 register (2026-09-17 audit, 56 findings AUD-001..AUD-056)

Audited revision `bbd2fe9` (post-round-1 state incl. rebuilt UI dist and
the live-Nucleus close-out fixes). Every finding was verified against
local source before disposition; none was a false positive. Several
restate round-1 deferrals against the remediated code — those keep their
standing F-numbers and rationales below unless materially new evidence
appeared (none did).

Fixed this round (with the residual, where a contained fix landed but the
architectural half stays open):

- AUD-002: the no-API-key ingest grace path is GONE — keyless requests
  401 regardless of key-table state; caller-chosen X-Observe-Site/body
  sites can no longer route writes. Behavior change: fresh installs accept
  no telemetry until an admin provisions a key.
- AUD-003 (contained): bootstrap-claim release runs on a detached,
  time-boxed context. Atomic claim-as-record stays deferred (was parked
  with F03; the principal store landed 2026-09-18 without it — see F03).
- AUD-005: credential mutations serialized (EnsureAdmin/ChangePassword/
  ForceReset); created_at preserved on the DELETE+INSERT replacement.
- AUD-007: partial OIDC config is a startup error; issuer URL validated
  (absolute, no userinfo/query/fragment, https unless
  OBSERVE_OIDC_ALLOW_HTTP_ISSUER=true).
- AUD-009 (contained): one exported auth.ValidatePassword behind setup,
  env provisioning, change, reset; flush-ms range-checked pre-multiply;
  length floors for explicitly set JWT/audit secrets. Persistent
  operator-owned key store stays open (folded into F46-class work).
- AUD-010: Buffer.PushBatch — whole-batch admission under one lock
  (count + byte budget) writing ONE versioned WAL frame; BatchHandler
  prepares side-effect-free then admits atomically. Pending decodes both
  frame shapes.
- AUD-012 (contained): unattached-but-created queue closed on attach
  failure; Pending flushes userspace-buffered appends so in-process
  replay is honest. Strict fsync-per-ack (group commit) deferred as a
  throughput contract decision — the async model is documented in
  queue.go.
- AUD-014: corrupt-complete-record replay is an ERROR (was skip-then-
  checkpoint-away); identity-less records rejected; in-range mid-record
  checkpoints refused at open.
- AUD-015 (contained): serialized-byte budget across queued+in-flight
  (OBSERVE_MAX_BUFFERED_BYTES, default 256 MiB), 64 KiB per-event stored
  cap, unserializable properties rejected at admission, UTF-8-safe
  truncation.
- AUD-016: started/stopped/workerErr lifecycle; post-Stop admission
  refused; double-Start can't race Stop; panicked flush worker latched
  and surfaced via /healthz.
- AUD-018 (single-process): striped per-replay locks around owner-check +
  upsert + children. Multi-replica needs the stable-key/CAS design.
- AUD-019 (transactional half): session row + child events commit in ONE
  tx. Stable batch IDs + durable heatmap outbox stay deferred (F12/F19).
- AUD-020 (contained): deterministic ORDER BY timestamp, event_id.
  Keyset pagination + UI window loading stay deferred with F39's player
  protocol work.
- AUD-021: browser SDK transport is strict; fire-and-forget boundaries
  catch and report; the retention path is live code again.
- AUD-022: single-flight flush with owned, spliced batches; exact
  prepend on failure.
- AUD-023: re-init sends flat per-chunk events arrays.
- AUD-024: classic tracker detaches only on acceptance (res.ok / XHR
  status); failed chunks requeued (bounded) and reported via onError.
- AUD-025: sendBeacon never used when an API key is configured.
- AUD-026: byte-aware packing (1 MiB budgets) in browser SDK, classic
  tracker, Python, and Go.
- AUD-027: tracker start/stop lifecycle — listeners removed, observer
  disconnected, history restored, capture gated on active;
  stop({discard}) withdraws pending capture.
- AUD-028: constant trusted document envelope (doctype + CSP-first head)
  for every replay path; untrusted doctype never emitted; legacy path
  CSP prepended at byte zero.
- AUD-030 (contained): referrer fail-closed + userinfo strip server-side;
  tracker sends origin+path for clicks/navigation/batch URL. The full
  URL-capture policy stays deferred (F41).
- AUD-031: bounded shape-validating snapshot renderer with explicit
  placeholder; playback guards non-object data/non-finite timestamps;
  tracker serialization budgeted (nodes/depth/text).
- AUD-032 (player half): parsed derived from prop; keyframe selection at
  playhead; cursor/ripple/scroll reset. Mutation protocol stays F39.
- AUD-033: clicks carry page_url + viewport captured at click time;
  server groups heatmap attribution per click page (legacy fallback to
  batch URL).
- AUD-034: rage window empties into a fresh burst; threshold=1 fires;
  numeric script attributes bounded.
- AUD-035: Python flush retains failed batches under a flush lock and
  reports via on_error; the worker no longer swallows exceptions.
- AUD-036: Python + Go queues store admission-time-encoded bytes.
- AUD-037: Python options validated pre-thread; open/closing/closed
  states; TimeoutError on incomplete close; post-close log() raises.
- AUD-038: Go queue byte-bounded (8 MiB); flushLogs leaves the prefix
  queued until its request succeeds; failures reported via OnError.
- AUD-039 (contained): 10s per-request deadline in postRaw even for
  timeout-free custom clients. Context-aware exception/close APIs remain
  deferred design work.
- AUD-040: declared-to-result coverage bijection; undeclared results and
  negative counts rejected.
- AUD-041: strict shared row decoder (UseNumber, trailing reject,
  nonempty); numeric lexemes exact.
- AUD-044: restore refuses a nonempty target before writing — isolated
  restore is now the enforced default (merge mode is future work).
- AUD-045: archive entries 0600.
- AUD-046: v2 base64url-injective source-map keys (legacy keys still
  readable; prune matches both encodings per key, no raw glob over the
  release name).
- AUD-047: covering-segment selection (greatest generated column <=
  target); generated-only segments unmap their region; invalid source
  indices are unmapped, not nominal.
- AUD-048: KV read failures are errors, not absent maps; 8 MiB parse
  budget + version check; per-request map cache with memoized failures.
- AUD-051: Verify keyset-pages through a captured watermark in 500-row
  batches.
- AUD-052 (contained): failed audit append drops the cached head so the
  next Record re-derives seq/prev-hash from storage. Durable intent
  reconciliation stays deferred with F47-class work.
- AUD-053: audit middleware records on a detached 2s context and logs
  failures.
- AUD-054 (contained): survey create/activate audited again; only the
  public POST /surveys/respond excluded (exact match). Site attribution
  from downstream handlers remains open (producer migration).
- AUD-056 (partial): CI race-detector gate for the concurrency-heavy
  packages, tracker contract job (node --test over the real script),
  sdk-go under -race. Required pinned-Nucleus fixture and the embedded-UI
  freshness gate remain deferred (F49/AUD-055).

Deferred, standing round-1 items restated by this audit (unchanged
rationales below): AUD-001 = F02, AUD-011 = F12/F19, AUD-013 = F16,
AUD-029 = F37, AUD-042 = F45 (app half), AUD-043 = F45 (upstream half),
AUD-049 = F46, AUD-050 = F47, AUD-055 = F49. (AUD-004/AUD-006 closed
2026-09-18 with F03/F05 — f631c81.)

New deferrals from this round:

- AUD-008 - P2 - Fixed 2026-09-18: normal access JWTs are REJECTED in
  query strings everywhere (the old `?token=` fallback is gone, with a
  pointing error message). EventSource/download consumers mint a
  short-lived single-purpose stream ticket: `POST /api/v1/auth/stream-ticket`
  (JWT-header auth) returns a 2-minute token bound to ONE route prefix
  (aud "observe-stream" + route claim). The middleware accepts tickets only
  as `?ticket=` on their bound prefix, rejects them as bearer headers, and
  applies the standard token_version revocation check to ticket use (a
  revoked session's tickets die with it). UI consumers switched (logs live
  tail re-mints on EventSource reconnect instead of retrying a dead
  credential forever; dashboard CSV export mints per click). Verified live:
  bound route 200 / non-bound route 401 / ticket-as-header 401 / normal
  JWT in query 401 / invalid route mint 400, plus the middleware unit
  suite. Share-token (`share_token=`) machine reads are a separate,
  designed mechanism and unchanged.
- AUD-017 - Low - Open (hardening): WAL final-component symlink refusal
  and an exclusive writer lock need O_NOFOLLOW/flock build-tagged files;
  local deployment hardening, not remotely reachable.
- AUD-054 (site half) - P2 - Open: audit events default site "default"
  because the outer middleware cannot see downstream-bound context; needs
  the route-level producer migration sketched in the report.
- AUD-056 (remainder) - P2 - Closed 2026-09-18 for its requested scope:
  the `nucleus-integration` CI job runs the full suite against a pinned
  published engine image (see the engine-selection rationale in ci.yml and
  the upstream notes below), fresh per run, serially (`-p 1`, the known
  Migrate TOCTOU), after a migration boot; the `e2e-smoke` job boots the
  real binary + engine + demo seed and runs a Playwright smoke
  (login -> dashboard renders -> one replay plays), skipping cleanly when
  no instance is at baseURL. Remaining future work (not audit blockers):
  the fuller 27-spec e2e suite is still local-only, and the fixture image
  carries the two engine defects recorded below.

Upstream (2026-09-18, from the trust-close session; both logged in
Tyler's `Teploy/_internal/UPSTREAM_BUGS.md` with standalone reproducers;
no Neutron/Nucleus edits made from this session):

- Fresh-install migration failure: engines built from the CURRENT Neutron
  tree AND from this repo's pinned submodule fail the migration ladder on
  an empty database at 027 — a table renamed earlier in a multi-statement
  script is invisible to the script's later statements (the runner sends
  whole files as one Exec), and the engine's error text is double-wrapped.
  The published v0.1.5 image fails one step later (028). The v0.1.8 image
  applies the whole 001-041 ladder — which is why the CI fixture pins it
  and why nucleustest's documented scratch-engine invocation was updated
  to v0.1.8.
- Intermittent committed-upsert loss (v0.1.8 image, accumulated data):
  a same-key ReplacingMergeTree insert inside a multi-table transaction
  (the exact replay_sessions upsert shape since 039) commits successfully
  but never becomes visible, roughly 1-in-3 once prior rows accumulate.
  Reproduced with pure SQL (no observe code). This makes the pre-existing
  F20 metadata test flaky on v0.1.8; the F12/F19 idempotency proofs are
  unaffected. If the new CI job flakes, this is the first suspect.

Earlier standing upstream item: F45's snapshot-boundary primitive
(2026-09-17 entry) remains the only other open one.
