# Open audit items

Unresolved findings for this repository from the ChatGPT-led audit series.
Register: teploy-observe audit 2026-09-17 (51 findings, pinned at
`6d49fcc380781e64e85b30b2d227ee45cd343f0c`; report lives outside the repo).
Round 2: audit 2026-09-17 (56 findings AUD-001..AUD-056, pinned at
`bbd2fe9038863db0447ac1335554451feb89735a`; report lives outside the repo)
— remediation record below. Round 3: audit 2026-09-18 (54 findings
TO-001..TO-054, pinned at `fd7acf68803abd4e43f23f8438802b3b65f5ba23`;
report lives outside the repo) — remediation record below. Round 4: audit
2026-09-19 (45 findings R01..R45, pinned at
`5e2108db4d7b36ac7af1b1309e9064de484bcd58`; report lives outside the repo)
— remediation record below. Earlier sweeps (2026-09-09 through 2026-09-11,
passes 1-5) are closed history; their one surviving item is folded into F16
below.

## Round-4 register (2026-09-19 audit, 45 findings R01..R45)

Audited revision `5e2108d`. Every finding was verified against local source
before disposition; none was a false positive. Fixed this round (commits in
the round-4 series):

- R02: setup decodes through a shared bounded reader (8 KiB, single JSON
  document, unknown fields rejected), checks setup-completeness BEFORE any
  parsing/hashing (the authoritative atomic check stays in EnsureAdmin), and
  enforces the shared credential length policy (username <=128, password
  <=72) at the boundary.
- R04: error ingestion requires the authenticated context site and applies
  the BoundSite invariant — a key for site A can no longer write errors
  under site B by naming it in the body. Missing authenticated context is
  refused outright (keyless ingest has been gone since AUD-002).
- R05: the legacy (site_id, slug) check-in routes are RETIRED (410 with a
  pointing message); the service layer additionally refuses slug check-ins
  for any monitor carrying a ping token. Token check-ins are the only
  public heartbeat path.
- R06: cron listings strip ping_token for anyone below editor; webhook
  listings reduce the URL to its host for viewers (a Slack incoming-webhook
  URL is a posting credential). Creation remains the editor-only one-time
  reveal; the HMAC secret was already excluded.
- R07: API key capabilities (migration 044, rename-aside + create + copy).
  Existing keys backfill to telemetry-ONLY — publish is never silently
  granted; operators mint a dedicated CI key via POST
  /api/v1/sites/{id}/keys {"scopes":["publish"]}. The sourcemap upload's
  key path requires the publish scope (editor JWT unchanged); the ingest
  middleware requires telemetry, so a publish-only key is not an ingest
  credential. docs/migrations/from-sentry.md updated.
- R08: SetRole refuses to demote the LAST local administrator inside the
  serialized principal mutation (count query failure fails closed). OIDC
  role refresh (UpsertOIDC) is deliberately unguarded — the IdP is
  authoritative; SSO-only installs document break-glass via
  OBSERVE_ADMIN_USER provisioning.
- R09: stream tickets inherit the PARENT token's version, never the current
  one — a concurrent demotion/revocation can no longer be upgraded into a
  fresh-version ticket with stale role claims. Equality recheck preserved.
- R10: the logs SSE handler revalidates the principal's token_version on a
  30s ticker and closes the stream on revocation/role change/password reset
  (fails closed on a recheck error). The 2-minute ticket remains a
  connection-opening credential only.
- R11: ipRateLimitMW keys on the trusted-proxy-resolved context IP (RemoteAddr
  fallback) — clients behind a proxy no longer share one login bucket.
- R12: ErrAuthUnavailable distinguishes an auth-store outage (503 +
  Retry-After, both middleware paths) from an invalid key (401). Absence
  detection switched to a rows-empty check so not-found is not conflated
  with an error.
- R13: the error buffer bounds count AND retained bytes (64 MiB default,
  256 KiB per record) across queued plus in-flight; buffered records are
  frozen serialized snapshots (caller mutation after Push changes nothing);
  reservations retire only on final disposition; queued/bytes surfaced via
  /healthz.
- R14 (contained half): each flushed record gets its own bounded context —
  one slow storage call can no longer exhaust a shared deadline and take
  the whole batch tail with it. The durable idempotent-inbox half is
  LANDED 2026-09-22 (O01 slice 2, see the slice record below): stable
  producer event identity + payload digest + error_inbox ledger claimed
  inside the error_events apply transaction, durable WAL acks, PENDING
  retry instead of drop-on-flush-failure.
- R15: /healthz returns 503 with status "degraded" for flush-worker-failed,
  error-worker-failed, wal-degraded, and memory-only (WAL attach failure —
  never a deliberate mode in this app), plus the DB probe. The error
  worker's panic recovery latches a fatal workerErr and closes admission.
  Error-buffer backlog (queued count + bytes) is reported.
- R16: every SSE frame (logs stream AND live stats) is written under a
  renewed finite write deadline via http.NewResponseController — the 10s
  total WriteTimeout no longer kills 25s/15s keepalive streams. Wrappers in
  the neutron router implement Unwrap, so the controller reaches the real
  connection.
- R17: uptime probe and persistence contexts are separate — a target
  timeout records its down row under a detached bounded context; scheduler
  cancellation records nothing; recordResult returns its error.
- R18: RunChecks admits only the checks that can actually start (worker
  capacity, oldest-last-check first, id as tie-break); unadmitted monitors
  stay due for the next tick. lastCheck is no longer premarked for monitors
  that inherit an expired context in a queue.
- R19: replacement versions are strictly-monotonic for monitor/cron delete,
  dashboard delete, panel add/update/delete (read latest version, write
  max(now, prior+1)) — same-millisecond mutations and clock rollback can no
  longer collapse away a delete. The principal store already had this.
- R20: monitor creation validates interval (10..86400) and expected status
  (100..599) with defaults only for zero values; Enabled is tri-state in
  the DTOs (omitted = true, explicit false preserved). Cron creation
  validates name/slug/schedule bounds and grace (0..86400); an empty
  schedule stays legal (the documented grace-only mode). Handler maps
  validation to 400, backend errors stay 5xx.
- R21: feedback List caps limit at 200 (default 20); monitor ListResults
  caps at 500 (default 50); both with stable total-order tie-breaks.
- R22 (contained): webhook delivery runs on lifecycle-owned bounded
  workers (4) with a bounded queue (1000, overflow drops OLDEST loudly),
  ONE stable delivery id per firing reused across bounded in-process
  retries (3 attempts), and Shutdown drains. Durable across-restart
  delivery stays DEFERRED (outbox design, same class as R14/R26).
- R23: process diagnostics go to stderr — `observe backup` owns stdout
  exclusively (the unencrypted-backup warning used to prefix the tar
  stream). Per-table errors were already stderr-side.
- R27: v2 replay identity fields (producer_id, batch_id, replay_id) are
  validated against the canonical bounded alphabet [A-Za-z0-9_-]{8,64}
  AFTER the ledger check — committed retries still dedupe; only batches
  about to be written must be collision-free, making the pipe-delimiter
  child-id collision unconstructible for new writes (v2 child ids unchanged
  — retries of committed work keep their identity).
- R28: error events and replay sessions sanitize their captured URL through
  the SAME shared policy as analytics (ingest.CapturedURL: userinfo, query,
  fragment stripped; non-http(s)/garbage dropped), applied before
  grouphash/persistence and to click page attribution. Stack text, source
  map filenames, and free-form contexts remain under their documented
  policies.
- R29: the classic tracker sanitizes document.referrer at send, form
  actions at capture (relative resolves against the document; empty action
  falls back to the page URL), and sanitizeURL now takes a base so relative
  hrefs are no longer lost; non-http(s) schemes drop.
- R30: share pages carry Cache-Control private,no-store, Referrer-Policy
  no-referrer, X-Content-Type-Options nosniff.
- R31: the classic tracker's flush path is a single-flight self-draining
  transport: frozen immutable batches, count+byte budgets across
  queued+in-flight, UTF-8 byte-measured body cap, automatic tail drain
  after success, bounded backoff retry of the SAME frozen batch (stable
  batch/event ids — server admission dedupes), XHR branch got real
  onload/onerror handling, and the keyless sendBeacon path is gone
  (keyless ingest no longer exists; parity with TO-051).
- R32: events are serialized to immutable JSON at capture — caller
  mutation after track() cannot change what is sent, cyclic/BigInt values
  are a capture-time diagnostic rejection (droppedEvents() counter) instead
  of a flush-time throw that detached the batch; revenue() no longer writes
  into the caller's object.
- R33: campaignFields decodes each query pair under its own try/catch —
  one malformed percent escape can no longer abort the initial pageview and
  tracker initialization; valid allowlisted attribution still ships.
- R34: every raw-handler error response goes through encoding/json (shared
  writeJSONError) — Go %q escapes are not JSON and produced unparseable
  responses for control characters.
- R35: parseTimeRange rejects malformed explicit timestamps and
  empty/reversed ranges (400); omitted values keep the documented defaults.
  All 21 call sites updated.
- R36: one validatePanel gate for add AND update (empty/unknown panel or
  query type rejected — the invisible-tombstone create; query_config must
  be valid JSON; position/size fields integer + range-checked), and
  ExecutePanel reports a config parse error instead of ignoring it.
- R37: logs UI guards every state update with a request generation (stale
  requests can't overwrite newer results; unmount-safe), and a failed load
  renders an explicit error state with Retry instead of "No logs yet".
- R38: pagination resets on site/query/level/service change; live entries
  live in their own capped list that historical fetches cannot overwrite;
  pagination renders only for historical results; text search disables
  live tail (the live stream cannot evaluate it) instead of pretending.
- R39: a blank API key edit FAILS when the existing key cannot be loaded
  (read/decrypt error) instead of silently erasing the credential.
- R40: AI query generation gets a principal-keyed rate limit (10/min), a
  process-wide concurrency gate (4), a bounded question (8 KiB body, 4000
  chars), and upstream error logs drop the response body (it can echo the
  user's prompt) — status only.
- R41: public feedback submission is site-existence-checked (fail-closed
  503 on lookup error, 404 for invented sites) and rate-limited per client
  IP AND per site (10/min each). Survey respond keeps its existing bounds;
  its route-level limiter ride-along is future work if abuse shows up.
- R42: login audit events carry the resolved client IP and user agent from
  the request-info context (UA capped at 1024 + UTF-8 sanitized); audit
  write failures are logged.
- R43: the compose TLS topology puts Caddy (fixed 172.30.10.2) and observe
  (fixed .3) on an isolated frontend network and the database on an
  internal backend network Caddy cannot reach; observe trusts ONLY the
  Caddy address by default and publishes OBSERVE_PUBLIC_URL. Verified
  parseable with the nucleus service definition intact.
- R44: e2e-smoke and nucleus-lease generate per-run JWT secrets (openssl
  rand -hex 24, prefixed to satisfy nothing but policy — length is what
  matters); readiness loops capture child logs and fail fast when the
  process dies instead of waiting out the timeout.
- R45 (partial, hardening): CI declares contents:read permissions,
  cancels superseded same-branch runs, and every job carries a
  timeout-minutes. Digest-pinning the production compose images is NOT
  done — substituting fabricated digests is worse than mutable tags; it
  needs a reviewed dependency process that records real digests (residual
  below).

Round-4 deferrals (new):

- R01 = F02 (unchanged standing deferral — first-run grace is a product
  decision; mitigations unchanged).
- R03 = AUD-003/TO-006 (unchanged — atomic bootstrap claim-as-record).
- R24/R25 = TO-021/TO-022 + F39-full (unchanged — replay pagination +
  causal ordering ride the versioned player protocol).
- R26 = TO-020 (trace half LANDED 2026-09-22 via O01 slice 3 — the
  derived-work outbox; the replay-heatmap half of the finding remains
  open, see the slice record below).
- R14 durable half, R22 durable half: R14's durable half LANDED
  2026-09-22 (O01 slice 2); R22's durable half (across-restart webhook
  delivery) remains deferred — the `derived_outbox` table + `webhook`
  kind now exist (O01 slice 3, migration 046) but alert fire still
  enqueues to the in-memory queue; wiring the alert-fire origin through
  the outbox is the next slice.
- R45 digest-pinning residual: needs real reviewed digests; the permissions
  /concurrency/timeout hardening landed.

Round-4 restatements of standing items: none beyond the mappings above.
AUD-017 and AUD-054's site-attribution half remain unchanged open hardening
notes.

## O01 implementation slices (durable ingest, under ADR contract)

Behavior changes on the ingest path land against
`docs/O01_DURABLE_INGEST_ADR.md` §5 and update that ADR's pinning tests in
the same change (the O03 rule the ADR carries). Record of slices:

- **2026-09-22, slice 1 — events group commit (ADR §5.1-§5.3, §5.10).**
  Durable mode is the events default: an ingest 200 now means the batch's
  WAL frame is fsynced (group commit — one shared fsync per ≤25 ms window
  or 1 MiB of unsynced bytes; `OBSERVE_WAL_GROUP_COMMIT_MAX_DELAY`), the
  disk high-water REFUSES with a retryable 503 + Retry-After instead of
  deleting uncheckpointed segments, and `OBSERVE_WAL_LOSSY=true` opts back
  into the era-1 fast-ack/delete semantics with the loss budget visible at
  /healthz (`events.accepted|durably_acked|replayed_on_restart`,
  `wal.mode|refused_high_water|unsynced_events`). Capacity refusals stay
  429 (consumer contract unchanged); durability refusals are 503.
  Deliberate pin updates in the same change:
  `internal/ingest/o01_pin_test.go` (all three era-1 pins),
  `TestDiskQueue_HighWaterBreachDropsOldestSegmentLoudly` (lossy-mode
  pin). New pins: `internal/ingest/o01_group_commit_test.go` (durable-ack
  ordering, fsync coalescing N-events-to-M-fsyncs via a counting seam,
  high-water refusal + retry-after-checkpoint, lossy fast-ack/delete,
  sync-failure latch, crash-at-ack recovery, replayed-on-restart counter,
  429/503 split, Retry-After middleware). Mutation-checked both ways
  (  no-fsync-before-ack fails 6 pins; delete-at-high-water fails the
  refusal pin). ADR era-2 record + §3 events row updated. Remaining O01
  scope: error inbox (slice 2), derived-work outbox, quarantine + full
  counter block, ledger retention, OTLP duplicate mitigation (§6.2-6.6).

- **2026-09-22, slice 2 — error inbox + durable error path (ADR §5.6,
  §5.4; closes R14's deferred half).** The errors signal rides the SAME
  generalized WAL machinery as its own "errors" queue instance
  (`wal_version:2` records frames; one DiskQueue implementation, frames
  never interchange across kinds). An errors 200 means fsynced
  (group commit; `OBSERVE_WAL_LOSSY` + window env vars apply to both
  queues); apply failures leave records PENDING (requeued, checkpoint
  stops at the gap, retried — never dropped); undecodable-after-admission
  poison diverts to a bounded quarantine spool, counted, stream
  unblocked; K-attempt SQL-layer diversion stays slice 4. Identity: the
  SDK audit found NO producer error id on the wire — sentry-shim now
  sends the id it already minted, browser SDK + tracker mint per capture
  (`[A-Za-z0-9_-]{8,64}`, optional `producer_id`); digest is sha256 of
  the frozen body. Dedupe: 10-min admission cache (duplicate →
  `{ok,deduped:true}`, conflict → 409 + counter) over the durable
  `error_inbox` ledger (migration 045) checked+claimed inside the
  error_events apply tx; post-restart conflicts counted at flush, never
  applied/merged; no event_id → v1 duplicate posture, documented. Status
  split: 429 capacity (unchanged) / 503 durability + Retry-After /
  409 conflict / 400 malformed identity — additive for existing SDKs.
  healthz `errors` counter block (accepted/durably_acked/applied/
  deduped/quarantined/conflicting_id/pending + queued/bytes/
  replayed_on_restart/flush_failing) + `errors_wal` stats + degraded
  states (errors-memory-only, error-wal-degraded, error-flush-failing).
  Deliberate pin update: `internal/errors/o01_pin_test.go` rewritten to
  era 3. New pins: `internal/errors/o01_inbox_test.go` (duplicate,
  conflict at admission + after restart, crash-after-ack, crash-mid-
  apply, lost-response retry, identity-less v1, quarantine, pending-gap
  checkpoint — Nucleus-gated, self-migrating), `internal/ingest/
  o01_records_frame_test.go`, `cmd/observe/error_ingest_o01_test.go`
  (status split). Mutation-checked both ways: skipping WaitCommit fails
  the durable-ack pin; drop-on-flush-failure fails the PENDING pin and
  the gap test. SDKs bumped in the same change (sentry-shim, browser,
  tracker snippet) with per-capture event_id tests. ADR era-3 record,
  §3 errors row, §5.6 marked IMPLEMENTED. Remaining O01 scope:
  derived-work outbox (slice 3), WAL fence-and-quarantine + SQL-layer
  K-attempt diversion + events-side counter block (slice 4), ledger
  retention incl. error_inbox 30d (slice 5), OTLP duplicate mitigation
  (slice 6).

- **2026-09-22, slice 3 — derived-work outbox, trace signal end to end
  (ADR §5.7, era 4; lands the trace half of TO-020/R26).** Migration 046
  (`derived_outbox`, ReplacingMergeTree keyed on intent id — the
  state-advancing-row pattern of 041/045, worker marks as strictly-
  monotonic version rewrites collapsed by the argMax form). Trace ingest
  is now ONE transaction: span chunks + a `rollup` intent + a `detector`
  intent commit together — the detached post-response goroutine is GONE,
  and a failing span chunk rolls back the whole export (the OTLP
  committed-prefix duplicate window for traces closes as a side effect;
  503+retry now reprocesses cleanly). Payloads are the exact derived
  inputs frozen at ingest (per-bucket rollup aggregates; computed
  detector issues) — NOT span re-reads, which an exporter retry's
  duplicate prefix would double-count. Worker (`internal/outbox`):
  lifecycle-owned drain, immediate startup pass = crash resume,
  exponential backoff, DURABLE dead-letter sentinel (next_attempt_at =
  -1 — dead is a row state, not a worker-config property; a restart with
  a bigger budget never resurrects a dead letter), last_error kept,
  per-kind healthz counters (`outbox.by_kind.{pending, processed,
  failed, dead_lettered}`). Idempotency inventory: rollup writes are
  key-collapsed identical-row rewrites (idempotent today); detector
  persistence was NOT (KV count accumulation + count-summing read) —
  made idempotent per intent via KV completion markers keyed
  `outbox:perf:<intent>:<fingerprint>`; documented residual: a crash in
  the write-then-mark window can double-count one fingerprint's severity
  count (over-counted diagnostics, never lost detections). Webhook
  sends are NOT originated here (alert evaluation fires them — ADR §1.8)
  and still ride the R22 in-memory queue; their dedupe key when they
  ride the outbox is the existing stable X-Observe-Delivery id. Seed
  path (IngestSync) derives synchronously via ProcessIDs. Single-process
  claim (AUD-018 posture); multi-replica needs lease/CAS before the
  webhook kind rides. New oracle: `internal/outbox/outbox_test.go` +
  `internal/tracing/o01_outbox_test.go` + storage-free payload
  round-trip pin; red-first (contract tests failed against the detached
  code — at ack time zero durable record carried the derived work, and
  the goroutine's best-effort writes visibly raced test teardown in the
  red logs); mutations both ways (enqueue outside the tx fails the
  atomicity pin; due-scan skipping fresh intents fails the resume pins).
  `derived_outbox` added to `backup.Tables` (F44 gate caught it).
  Residuals for later slices: alert-fire webhook origination (R22
  durable half), replay heatmap origination (TO-020 replay half),
  processed-intent + KV-marker retention (slice 5), per-kind dead-letter
  alerting beyond the healthz counter. Gates: `go vet ./...` clean;
  touched packages green incl. `-race`; full serial suite
  (`OBSERVE_NUCLEUS_URL` fixture, `-p 1 -count=1 ./...`) green.

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

Backup/SDK close session (2026-09-18, later): F45 FIXED (app half) and
F32 FIXED (retry half) — see their entries. Both 2026-09-18 upstream
engine reports are RESOLVED upstream (Neutron `6286531a`, verified live:
a repo-built engine now applies the full migration ladder) and the
snapshot-lease primitive has LANDED in the tree, which unblocked F45.
Three NEW upstream reports filed from verifying F45 against the live
repo-built engine (lease scope gaps — see the Upstream section); the
observe side carries documented, tested workarounds for each.

Close-out session (2026-09-18, later still): F16 FIXED (numbered WAL
segments + bounded streaming replay + disk high-water), F38 FIXED (asset
proxy), F41 FIXED (URL/sensitive-data contract, wire+storage+trackers),
F46 FIXED (persistent audit key + key_id + rotation keyring, migration
042), F39's smaller step LANDED (throttled mid-session re-snapshots; the
full DOM-delta protocol stays deferred), and the e2e CI coverage extended
to the core product paths (login, dashboard, replay transport controls,
audit view). The three 2026-09-18 lease-scope upstream reports are
RESOLVED upstream (Neutron `4c7c4367`, verified live against a repo-built
engine); the observe-side engine canaries were reconciled to the fixed
shapes (see the Upstream section).

Open items: 9 (F02 product decision, 3 P2 deferred designs, 2 hardening
notes, 1 future-work remainder, 2 round-3 deferred designs)

## Round-3 register (2026-09-18 audit, 54 findings TO-001..TO-054)

Audited revision `fd7acf6` (the round-2 close-out state). Every finding
was verified against local source before disposition; none was a false
positive. Fixed this round (commits 913ad74, 824cf23, 8b75884, b02bf97,
cdfe08f, 54faec7, 151f2b9, 8172bb4, a0131ae, 81e61db, 20f4c17):

- TO-001: empty-key audit downgrade. key_id='' rows verify against SECRET
  legacy candidates only; an empty-key match classifies as UNKEYED and
  Verify reports Authenticated=false + UnkeyedCount instead of presenting
  the history as tamper-evident (compliance control warns). Residual: a
  fully-downgraded chain is still reported Intact (internally
  consistent) with the unauthenticated classification carried alongside —
  detecting a truncated/downgraded tail outright needs the F47 external
  anchor.
- TO-002: startup no longer logs RotationSpec() (the signing secret in
  transportable form); rotation instructions point at the protected key
  file. Keys already emitted to old logs should be treated as exposed
  (rotate; keep the old key in OBSERVE_AUDIT_KEYRING for verification).
- TO-003: UpsertOIDC bumps token_version when the IdP-supplied role
  changes; a pre-downgrade admin JWT dies at its next use (live gate
  test).
- TO-004: principal lookups distinguish absence ((nil,nil)) from store
  errors; CreateLocal/UpsertOIDC/replace fail closed instead of treating
  a SELECT failure as free username/absent principal.
- TO-005 (deployment half): compose binds the dashboard port to loopback
  by default (the tls profile included); header documents the
  tailnet/public-ingest topologies. The middleware grace half is F02
  below (unchanged deferral — product decision).
- TO-007: persistent-key publication is no-replace (link/O_EXCL) with a
  canonical re-read (concurrent starts converge); an existing-but-
  unusable key file refuses startup instead of silently downgrading the
  signer.
- TO-008: OBSERVE_AUDIT_KEYRING secrets join the legacy candidates —
  rotated-out keys still verify the pre-042 rows they signed.
- TO-009: colliding keyring ids naming different bytes are refused.
- TO-011: Verify pages at pageSize+1 and rejects a duplicate sequence at
  a page boundary instead of skipping it.
- TO-012: WAL segment bases are immutable for the queue's lifetime (the
  GC rebase mapped outstanding checkpoint targets past uncommitted
  records; regression test fails against the old code).
- TO-013: NewDiskQueueWithLimits takes both caps up front; construction
  reclaims only acknowledged segments and never enforces the high-water
  before replay/config; main.go passes the configured cap into
  construction; the sub-segment clamp is warned.
- TO-014: AttachQueue recovers write-through (per-chunk commit through
  insertBatch, checkpoint once at the end, nothing partially staged);
  peak replay memory is one chunk.
- TO-015: negative JSON checkpoint offsets refused at open.
- TO-016: a successful periodic fsync clears the dirty flag.
- TO-017: the v2 admission cache key is (site, producer, batch) with a
  per-entry digest of the prepared events' identities; same key +
  different content is PROCESSED, not falsely acknowledged.
- TO-018: the durable event-id dedupe is site-scoped (site_id, event_id);
  cross-site id collisions store both records (live gate test).
- TO-019: replay ledger/child ids/ErrBatchIDReuse are producer-scoped
  (migration 043, legacy rows honored via fallback lookup); partial v2
  identities and unknown versions rejected.
- TO-023: img/src is handled before the SAFE_ATTRS gate — the F38 asset
  proxy rewrite was unreachable dead code; img renders as a void element.
- TO-024: the snapshot loader is generation-guarded (no stale-async
  overwrite of a newer seek), the asset ticket is a per-load local, the
  keyboard listener is its own effect.
- TO-025: the asset ETag is a digest of the fetched body (a changed
  asset at the same URL is re-served, not 304'd stale forever).
- TO-026: asset fetch/read failures log host + failure class only (the
  raw *url.Error leaked the signed URL).
- TO-027: one bounded error-preserving body read (a truncated image is a
  502, not a 200).
- TO-028: capture-time attribute allowlist + sanitized img src + opaque
  head subtree — meta content, signed href/src, and style URLs never
  leave the browser; node budget matches the player's.
- TO-029: listeners store their capture flag and are gated at entry
  (capture-phase removal actually lands; held handlers are inert after
  stop); discard aborts in-flight delivery and obsoletes every delivery
  callback; reportRageClick no longer logs success as failure.
- TO-030: an event that cannot fit any legal request is dropped at flush
  with a report instead of becoming a permanently failing retry head.
- TO-031: the rage window prunes and resets BEFORE pushing the current
  click — second bursts report again.
- TO-032: browser delivery units are frozen immutable requests (identity
  once, exact-bytes retries, never merged into the live buffer) — the
  response-lost repack can no longer lose the new tail (regression test
  replays the scenario; pairs with the TO-017 server fix).
- TO-033: track() snapshots records at admission (unserializable isolated
  + reported; identity fields assigned last).
- TO-034: admission-time count+byte budget across queued+pending, fetch
  deadline, clamped options.
- TO-035: batch acknowledgments are read and validated; partial
  rejections surface through onError and are not retried.
- TO-036: serialization/retry state binds to the owning client (no
  module-global producer; old drains keep their endpoint and identity).
- TO-037 (Go+Python+browser): redirects refused everywhere and only 2xx
  is success — a cross-origin 307 can no longer forward X-API-Key.
- TO-038: span admission joins the owned worker (post-close refused,
  budgeted, failures requeue and surface at Close).
- TO-039: metric exports freeze a retained envelope re-sent until the
  POST succeeds; serialized; idle buffers export nothing.
- TO-040: histograms export DELTA temporality with interval timestamps;
  counters stay cumulative with a series start time.
- TO-041: unambiguous JSON series keys (no delimiter collision, no
  __name__ pseudo-label); duplicate/empty label keys rejected.
- TO-042: non-finite values, bad bounds, incompatible redefinitions, and
  counter overflow rejected at the boundary with reports.
- TO-043: metric series/gauge-point budgets; recording after Close
  refused.
- TO-044 (Python): serialized default replacement that validates first,
  propagates a failed close, and registers ONE module-level exit handler.
- TO-045: the backup manifest records the audit key's non-secret
  fingerprint (status + key id), making the external data/audit.key
  recovery dependency explicit per archive. Residual: the full encrypted
  secret-bundle export/import workflow is future work (the fingerprint
  identifies the dependency; it cannot reconstruct the key).
- TO-046: the client-side lease window starts before the acquisition
  round trip; the configured timeout is bounded.
- TO-047: lease live tests fail (not skip) under
  OBSERVE_REQUIRE_SNAPSHOT_LEASE=1; a new nucleus-lease CI job builds
  the engine from the pinned Neutron revision (repo-built engines pass
  the ladder since upstream 6286531a — the stale ci.yml rationale is
  corrected) and runs the lease-required suite. First run on GitHub's
  runners pending push (the flow is verified locally against the
  repo-built engine).
- TO-048: the UI freshness gate consumes .ci/pnpm-version (the Neutron
  workspace's packageManager pin) with validation instead of pnpm@latest.
- TO-049: compose defaults OBSERVE_SEED_DEMO to false (demo seeding is
  explicit; matches the application default and the README).
- TO-050: the compose healthcheck override is removed (the image's
  measured cold-connection budget applies).
- TO-051: the browser SDK's keyless sendBeacon fallback is gone (keyless
  ingest no longer exists), and the quick-start shows the site-scoped
  API key.
- TO-052: a configured OIDC redirect is startup-validated (absolute,
  exact callback path, HTTPS except localhost under the dev flag, no
  userinfo/query/fragment) and pins the state cookie's Secure attribute
  off request headers. Residual: the unconfigured local-dev Host
  derivation remains, documented (PKCE + state cookie + IdP-registered
  redirect URIs bound the flow; a spoofed Host breaks SSO, does not
  redirect it).
- TO-053: any non-empty OBSERVE_OIDC_* variable (except the documented
  ALLOW_HTTP_ISSUER dev modifier) counts as configuration and requires
  the core fields.
- TO-054: span attributes are normalized and snapshotted at
  SetAttribute; unsupported types (and uint overflow, non-finite floats)
  are reported and dropped, never silently encoded as empty strings.

Round-3 deferrals (new):

- TO-006 = the standing AUD-003/F03 deferred half (atomic bootstrap
  claim-as-record). Unchanged: the KV claim cannot join the SQL principal
  insert in one engine transaction (KV writes commit through, documented
  upstream scope note); needs a SQL-authoritative claim row plus an
  engine-uniqueness story. The contained detach/cleanup landed in round 2.
- TO-010 = F47 below (external witness). Unchanged.
- TO-020 - P2 - Open (design): durable heatmap outbox. The replay
  transaction's post-commit heatmap aggregation is best-effort; a crash
  or aggregation failure after commit permanently skips that batch's
  derived work (a producer retry is ledger-deduped away). The code
  documents the gap; the fix is a derived-work table + idempotent worker
  (migration + exactly-once contribution design), not a contained patch.
  UPDATE 2026-09-22 (O01 slice 3): the derived-work table + idempotent
  worker now EXIST (`derived_outbox`, migration 046) and the trace
  signal rides them end to end; what remains open is wiring the REPLAY
  heatmap origination through the same outbox (the `heatmap` kind is
  reserved).
- TO-021/TO-022 - P2 - Open (design): replay pagination + causal
  ordering. GetReplayEvents is unbounded and equal-timestamp events order
  by deterministic-but-not-causal (timestamp, event_id); the fix is the
  keyset-paginated windowed player protocol with a producer-scoped
  capture sequence — the same versioned-cutover work F39's full
  DOM-delta protocol tracks. TO-022's per-recorder monotonic sequence is
  the schema half of that protocol.

Round-3 items that restated standing deferrals, kept under their F/AUD
numbers: TO-005 middleware half = F02; TO-037's classic-tracker retry
port = F32's documented remainder. AUD-017 (WAL symlink/flock hardening)
and AUD-054's site-attribution half are unchanged open hardening notes.



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

## F16 - P2 - Fixed 2026-09-18 (later still): WAL disk high-water and bounded streaming replay

Numbered segments with a real cap: an append that would push the active
segment past maxBytes rolls to a fresh `wal-NNNNNN.log` first — the cap is
enforced by rolling, not only by compaction. `current.log` stays segment 0,
so a never-rolled install keeps the exact legacy on-disk layout (decimal
checkpoint included); the checkpoint gains a segment-scoped JSON form once
writes live in numbered segments, and the legacy decimal form still reads.
Acknowledged sealed segments are deleted once the checkpoint moves strictly
past them (never the segment a checkpoint boundary sits in — deleting it
would clamp the next open to a full-log replay through the best-effort
dedup); a fully-checkpointed over-sized active segment is rolled away
instead of truncated. Replay streams frame by frame (StreamPending) with
the buffer's dedup in 500-event chunks, so the backlog is no longer
materialized twice; Pending() remains as the collecting wrapper. Disk
high-water: OBSERVE_WAL_MAX_TOTAL_BYTES (default 512 MiB; per-segment cap
now OBSERVE_WAL_MAX_SEGMENT_BYTES, default 64 MiB) drops the oldest
segment on breach — counted, logged loudly, surfaced in /healthz via
Stats() — keeping admission alive at the documented cost of the dropped
events' crash-recovery copy (F14's sticky degradation still governs WAL
write FAILURES). Proofs: internal/ingest queue tests (roll + cross-segment
replay, segment-scoped checkpoints, acknowledged-segment GC with the
boundary rule, breach drop + counters + admission survival, streaming
frames + consumer abort, legacy layout both directions), all green under
-race and against the live repo-built engine.

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

## F32 - P1/P2 - Fixed 2026-09-18 (later session): bounded automatic retry in the browser SDK; transport half fixed earlier

Landed (transport half, 2026-09-17 pass): res.ok checks, byte-bounded
keepalive, beacon-return checks, chunked batches, failed-chunk retention
(bounded), onError hook, reinit disposal.
Landed (retry half, backup/SDK close session): the browser SDK now
re-sends a failed batch automatically on a doubling backoff
(`retryBackoffMs`, cap 60 s) with a per-batch attempt budget
(`maxRetryAttempts`, default 5) — a batch that exhausts the budget is
dropped with a loud onError report instead of retrying forever, retention
still drop-oldest-bounded at 200 events, and the new `onRetry` hook makes
the policy visible to the application. Retried batches keep their F12
producer/event/batch ids, so the server's admission dedupe makes every
redelivery safe. Tests in sdk/browser/tests/unit.test.ts cover the backoff
gate, the doubling, identity-stable retry, budget exhaustion + recovery,
and the retention cap under sustained failure. Still deferred: the classic
snippet tracker (cmd/observe/tracker/observe.js) keeps its F32
transport-half fixes without an automatic retry loop (its retained-buffer
retry on the next interval remains); porting the policy there is future
work if the snippet ever needs it. Oversized individual replay snapshots
need a chunking protocol or a normal foreground request (tracked with
F39's protocol work).

## F37 - P1 - Fixed for form controls/contenteditable/data-*; default-text-mask policy open

The demonstrated leak (textarea initial content, contenteditable text,
token-bearing data attributes) is closed in the serializer. Still open as a
product decision: masking ALL visible text by default with an explicit
`data-observe-public-text` opt-in (the audit's preferred posture) vs the
current visible-text-by-default posture. Changing it silently blanks every
existing replay's readable content; needs an operator-visible toggle.

## F38 - P2 - Fixed 2026-09-18 (later still): asset proxy landed

`GET /api/v1/replay-assets?u=<absolute http(s) url>` behind JWT auth, the
route added to the stream-ticket set (img elements cannot carry headers;
the player mints a per-snapshot ticket). Designed allowlist:
OBSERVE_REPLAY_ASSET_HOSTS names the destination hosts (empty = proxy
disabled; a miss is 403, distinguishable from the disabled 404). Bounded
size (5 MiB default; over-cap rejects, never truncates), MIME sniffed from
leading bytes against a raster-image allowlist (SVG deliberately excluded
— script-bearing), `Cache-Control: private` + content-hash ETag with 304
revalidation and upstream cache headers never forwarded (same-site
caching), redirects rejected instead of followed (following would defeat
the allowlist in one hop), egress confined at dial time via netsafe
(loopback/private/link-local/metadata refused even for allowlisted host
names — DNS-rebinding safe). The player renders img through the tag
allowlist with src rewritten to the proxy (relative srcs resolve against
the session URL; unproxyable srcs drop — the pre-F38 behavior) and
img-src 'self' in the replay CSP. Legacy raw-HTML snapshots keep the
injected-CSP best-effort, unchanged. Residual, deliberate: share-token
replay views get no assets (stream tickets need a session token; images
fail closed there), and non-image asset classes (fonts, stylesheets) stay
out until a real consumer exists. Proofs: internal/replays assets tests
(sniffed-type win over a lying header, private caching + 304, allowlist
403/disabled 404, redirect refusal, html/svg 415, 413-not-truncate,
dial-time private refusal for an allowlisted host, malformed-target 400s).

## F39 - P2 - Smaller step landed 2026-09-18; full protocol deferred

LANDED (the audit's throttled re-snapshot fallback): observe-replay.js
records fresh mid-session snapshots periodically
(data-resnapshot-interval, default 30 s) and early on mutation bursts
(data-resnapshot-burst, default 150), throttled by a minimum gap
(data-resnapshot-min-gap, default 10 s) so a churning page cannot snapshot
itself to death. The player already selects the most recent keyframe at
or before the playhead (AUD-032), so long replays no longer drift
indefinitely off a single initial DOM. Contract-tested in
tests/tracker/observe-replay.test.mjs (burst over threshold past the gap
records a second snapshot).

DEFERRED (unchanged): the full versioned DOM-delta protocol — stable node
IDs, deterministic seeking over deltas, keyset-paginated event windows
(AUD-020's UI half rides with it). Real protocol design, not a bug patch;
revisit when a product need asks for sub-snapshot fidelity or replay
payloads need to shrink.

## F41 - P2 - Fixed 2026-09-18 (later still): URL/sensitive-data contract

Wire side: trackers send location.href as origin+path — credentials,
fragment, and query never leave the page (observe.js, observe-errors.js,
observe-feedback.js, sdk/browser; observe-replay.js already did). Campaign
attribution rides explicit utm_* fields extracted client-side from the
allowlisted params (utm_source/medium/campaign/term/content — exactly the
stored columns); element-text autocapture is opt-in
(data-capture-text on observe.js), click/outbound hrefs and dead_click
page_url are sanitized to origin+path, and navigation breadcrumbs carry
origin+path. Server side: the explicit fields are the primary attribution
path; for legacy producers still sending a query string, the same
allowlisted params are extracted from it per-field before the stored URL is
sanitized to scheme+host+path (fail-closed on non-http) — so attribution
survives the upgrade and nothing outside the allowlist is ever read or
stored. Storage: the utm columns already existed; no migration. History:
rows written before this change keep their full URLs until retention ages
them out (same forward-only posture as AUD-030's referrer tightening; a
retroactive rewrite of unbounded event history at boot was judged worse
than the window). Error-breadcrumb element text (64 chars, diagnostic
surface) deliberately keeps its pre-F41 behavior — the opt-in gates
ANALYTICS text capture. Deliberate boundary recorded here rather than
slipped in. SDKs ship with the server (F12/F19 bump-together pattern);
Go SDK has no analytics-event producer, Python passes caller URLs through
the now-sanitizing server. Proofs: internal/ingest F41 handler tests
(explicit-fields precedence, per-field legacy fallback, userinfo/fragment/
non-http fail-closed), tests/tracker/observe.test.mjs (pageview
origin+path + utm fields + non-allowlisted params never leave, text
opt-in off/on, outbound href sanitized), sdk/browser unit tests (query
strip + all five utm fields + reserved-field passthrough).

## F45 - P2 - Fixed 2026-09-18 (later session): snapshot-lease dumps + KV srcmap archive section; upstream lease-scope caveats registered

Upstream primitive: LANDED — Nucleus grew `ACQUIRE SNAPSHOT LEASE
[TIMEOUT ms]` (release at COMMIT/ROLLBACK/disconnect/expiry); the
2026-09-17 upstream report for the missing primitive is closed. The
2026-09-18 migration-ladder report is also resolved upstream
(`6286531a`, rename visibility in-tx) — verified live: a repo-built
engine applies the full 001-041 ladder.

Ours, landed (internal/backup): the dump wraps in one lease-holding
transaction (`lease.go`), so every SQL table is read at one moment while
other sessions' DML waits at the engine gate; engines predating the
statement fall back to pool reads with the downgrade recorded in the
manifest (`lease.held=false`) and warned. The KV srcmap domain
(`srcmap:*` blobs, `releases` sets, `relage` zsets) rides in the archive
as its own JSONL entry (`kvsrcmap.go`) with the F44 completeness contract
extended to it: manifest-declared sections, per-line strict validation
pre-apply, three-way reconciliation against the completion record, a
typed post-apply completeness proof (blobs byte-identical, set members
present, zset scores exact), and restore-target emptiness enforced for
the namespace when the archive carries it. Live proofs
(internal/backup/lease_live_test.go, kvsrcmap_live_test.go): concurrent
atomic cross-domain units are never cut in half below the straddling
transaction; SQL mutations block at the gate until release; a churning
srcmap namespace fails the dump loudly and recovers when quiesced; the
KV round-trip restores every key with exact values.

Honest limits, each verified live against the repo-built engine and
logged upstream (three NEW 2026-09-18 reports; see the Upstream section):
the lease gate does NOT cover KV scalar writes and the holder's KV reads
are not snapshot-pinned — the KV section therefore carries its own
convergence proof (two deep-equal consecutive namespace reads plus a
closing relist) instead of inheriting the lease moment; KV_KEYS cannot
enumerate set/zset keys — the dump reconstructs the release indexes from
site ids derived off the blob keys (a residual hole for orphaned indexes
of blob-less sites is documented in kvsrcmap.go); and the holder sees
another session's in-flight UNCOMMITTED writes (the lease's MVCC promise
is not delivered) — a dump can archive a row its writer subsequently
rolls back. Engine canaries pin all three shapes
(TestLease_KVScalarWritesBypassTheGate,
TestLease_HolderSeesInFlightUncommittedWrites) and fail when upstream
closes them, at which point the workarounds should be simplified.

## F46 - P2 - Fixed 2026-09-18 (later still): dedicated persistent audit key with rotation keyring

Migration 042 rebuilds audit_events with a key_id column (rename-aside +
create + copy, rows preserved verbatim with key_id='' — the legacy
encoding verified against the configured legacy candidates). Key
resolution (internal/audit keys.go): OBSERVE_AUDIT_KEY (base64 >=32
decoded bytes or raw >=32 bytes; a value that IS valid base64 is held to
the decoded floor) when set; else a persistent generated 32-byte key file
at data/audit.key (0600, temp+fsync+rename — every install's chain is
keyed without operator action, and the key lives on the observe host
rather than in the database it protects); else the JWT-secret fallback;
unkeyed is the loud last resort (the earlier empty-key warning, now only
reachable when the key file is unusable and no fallback exists). Rows
stamp the signer's id (first 8 hex of sha256(key)); verification selects
the key per row — '' tries the legacy candidates (signer, the RAW env
interpretation, JWT fallback, empty — covering every key a pre-042 process
could have signed with), any other id must resolve in the keyring or the
chain reports it broken with a rotation-specific detail. Rotation:
OBSERVE_AUDIT_KEYRING holds historical "id:base64" verification keys
(ids must match the derived id or startup refuses — a mistyped entry
would silently drop verification of the history it covers); startup logs
the ready-to-paste entry for rotating off the file key. The compliance
control distinguishes dedicated/persistent (pass) from jwt-fallback and
unkeyed (warn). Live-verified against the repo-built engine: the ladder
applies through 042, rows carry key ids, and a chain written under one
signer verifies after rotation with the old key in the keyring.

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
verified locally (ui-sync is byte-idempotent against the working tree for
every hashed asset) — but the job itself has not executed on GitHub's
runners yet. RESIDUAL (found 2026-09-19, round-3 close-out): the
canonical build stamps `generatedAt` (and byte-saved counters that
reference it) into `.neutron-static-policy.json` /
`.neutron-adapter-static.json`, so a rebuild of UNCHANGED source still
differs in that metadata and the gate's `git diff --exit-code` would trip
on it. Needs an upstream build normalization (stable timestamp or none)
or the gate scoping those two metadata files out; framework-owned, filed
for the upstream handover rather than worked around here.

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
  Migrate TOCTOU), after a migration boot; the e2e job boots the real
  binary + engine + demo seed and runs the Playwright suites. Extended
  2026-09-18 (later still): core.spec.ts joins smoke.spec.ts in CI —
  dashboard stat cards render against the live API, replay transport
  controls work (play/pause toggle, scrub seeks), and the audit view
  renders the trail; one API login per worker (the server's per-IP login
  rate limit trips when five specs each drive the form). Remaining future
  work (not audit blockers): the fuller 27-spec e2e suite is still
  local-only, and the fixture image carries the two engine defects
  recorded below (both resolved in the tree; see the upstream section).

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
  to v0.1.8. RESOLVED UPSTREAM 2026-09-18 (`6286531a`, same-transaction
  rename visibility on the disk stack), verified live from this session:
  a repo-built engine of the current tree passes the full ladder
  (schema.Apply on an empty database reaches the end). The CI fixture
  stays on the published image for reproducibility; nucleustest's comment
  now records that repo-built engines work again.
- Intermittent committed-upsert loss (v0.1.8 image, accumulated data):
  a same-key ReplacingMergeTree insert inside a multi-table transaction
  (the exact replay_sessions upsert shape since 039) commits successfully
  but never becomes visible, roughly 1-in-3 once prior rows accumulate.
  Reproduced with pure SQL (no observe code). This makes the pre-existing
  F20 metadata test flaky on v0.1.8; the F12/F19 idempotency proofs are
  unaffected. If the new CI job flakes, this is the first suspect.
  RESOLVED UPSTREAM 2026-09-18 (recorded in the engine tree's docs commit
  `952a446b` alongside the rename fix); the live suite in this session ran
  the replay idempotency paths green against the repo-built engine.

Upstream (2026-09-18, from the backup/SDK close session — three NEW
reports, all lease-scope findings hit while verifying F45 against a live
repo-built engine; logged in
`Teploy/_internal/UPSTREAM_BUGS.md` with standalone reproducers; no
Neutron/Nucleus edits made from this session):

- Lease gate bypass for KV writes: `SELECT KV_SET/SADD/ZADD/DEL(...)`
  from another session commits straight through a held ACQUIRE SNAPSHOT
  LEASE (DML waits; KV scalar functions do not), and the holder's own KV
  reads are not snapshot-pinned — mid-lease the holder sees another
  session's committed KV writes immediately. The lease module's own scope
  note carves out non-executor writes, so this is a documented limitation
  rather than a pure defect, but it breaks the dump boundary F45 needed.
  Observe workaround (internal/backup/kvsrcmap.go): the KV section is
  dumped under a convergence proof — two deep-equal consecutive namespace
  reads plus a closing relist — and a churning namespace fails the dump
  loudly (proven by TestDump_KVSrcmapChurnFailsLoudlyThenRecovers).
- KV_KEYS cannot enumerate collection keys: `KvStore::keys` iterates only
  the string shards; sets/zsets (lists/hashes/etc.) live in the
  collections store and are invisible to KV_KEYS regardless of how they
  were created. Observe workaround: the srcmap release indexes are
  reconstructed from site ids derived off the listed blob keys (the
  namespace's key names are deterministic per site); residual hole for
  orphaned indexes of blob-less sites documented in kvsrcmap.go. Restore
  verifies per key by type instead of by relisting.
- Holder sees in-flight uncommitted writes (dirty reads under the lease):
  a statement already past the writer gate when the lease is acquired
  stays visible to the holder BEFORE its commit and vanishes if that
  transaction rolls back — the lease's "MVCC snapshot, stable across
  statements" promise is not delivered; visibility is live-with-a-commit-
  gate. Consequence: a dump can archive a row its writer later rolls
  back. No observe-side workaround is possible (commit state is not
  visible to the reader); the consistency proof tolerates exactly one
  straddling unit (TestDump_LeaseConsistentMomentUnderConcurrentWrites)
  and a canary test pins the shape for removal when upstream fixes it.

Earlier standing upstream item: F45's snapshot-boundary primitive
(2026-09-17 entry) — CLOSED 2026-09-18: the primitive landed in the
tree (see the F45 entry above).

Upstream update (2026-09-18, close-out session): the three lease-scope
reports above are RESOLVED UPSTREAM — Neutron `4c7c4367` (lease-scope
fixes) plus `6d7c3ffa` (MVCC GC tail compaction), verified live from this
session against a freshly built repo-based engine:

- KV-write bypass / unpinned holder reads: the holder's KV reads are now
  snapshot-pinned, so a dump taken under active KV churn converges by
  construction. The KV-scalar writes themselves still commit through a
  held lease (the write-side half of the gap is unchanged;
  TestLease_KVScalarWritesBypassTheGate still pins it), which is now
  harmless to the dump boundary. The convergence proof in kvsrcmap.go is
  REDUNDANT with the pin but harmless — it runs as a cheap invariant;
  simplifying it out is follow-up work, recorded here rather than slipped
  into this session. The churn test was reconciled to the fixed shape
  (TestDump_KVSrcmapChurnConvergesUnderPinnedReads: churning dump
  SUCCEEDS and declares its kv section).
- Dirty reads under the lease: ACQUIRE now refuses while another
  session's writer transaction is in flight ("timed out waiting for N
  in-flight writer transaction(s)"), delivering the promised isolation by
  exclusion. The canary became the contract test
  TestLease_AcquireExcludesInFlightWriters (acquire under a parked writer
  fails naming the in-flight writer; after the writer resolves, acquire
  succeeds and sees exactly the committed state). The straddler exemption
  in TestDump_LeaseConsistentMomentUnderConcurrentWrites is likewise now
  redundant (a straddling unit can no longer be archived) and its removal
  rides the same kvsrcmap simplification follow-up.
- KV_KEYS collection enumeration: NOT part of the fixed set — the
  reconstruct-from-blob-keys workaround and its orphaned-index residual
  hole stand unchanged (see kvsrcmap.go).

Closure notes appended to `Teploy/_internal/UPSTREAM_BUGS.md`. No
Neutron/Nucleus edits were made from this session; the canary
reconciliation is observe-side test code only.

## 2026-09-19 reported default-site selection — fixed

The filter provider wrote its bootstrap selection into URL/storage before
checking for a prior selection, so initial site discovery never ran. Capture
initial intent before effects, defer persistence until discovery completes,
and prefer a configured site over the bootstrap default. Explicit URL and
remembered choices (including default) still win. Default Site remains a
valid fallback; no stored telemetry or site records are removed.

Validation: Go suite (DB-dependent cases require Nucleus), UI unit suite,
production UI rebuild, four Chromium regressions covering fresh selection,
URL precedence, remembered selection, and bootstrap-only installs.

## 2026-09-21 OTLP response conformance — fixed

All three OTLP transport boundaries (/v1/traces, /v1/metrics, /v1/logs) now
answer with the official OTLP/HTTP response contract instead of custom JSON.
Responses follow the request's wire format: application/x-protobuf requests
get Content-Type application/x-protobuf + proto-encoded
Export*ServiceResponse; application/json requests get protobuf-JSON (int64
as string — the official mapping). Full success is the zero-value message
(empty proto body / "{}"); logs partial rejection carries
ExportLogsPartialSuccess{rejectedLogRecords, errorMessage:""}. Storage
failure is 503 + Retry-After: 5 on all three signals (the logs audit-F13
precedent; tracing/metrics previously 500'd, which OTLP exporters treat as
non-retryable and drop). Unknown/unparseable content types are 415 naming
the received type (previously any non-proto prefix silently took the JSON
decoder — a text/plain body 400'd as "invalid JSON"); media-type parameters
(;charset=) are now tolerated via proper parsing. An oversized decompressed
body is 413 (previously silent LimitReader truncation ended as a misleading
400), and the chain's neutron.BodyLimit raw-cap error is translated to 413
the same way. Request decoding is unchanged — the hand-rolled JSON decoders
stay (OTLP/JSON hex ids cannot be parsed by protojson); only responses
changed encoding. Handler seams: one narrow per-package interface over the
existing ingest method (traceIngester/metricIngester/logIngester), concrete
services unchanged; constructors keep accepting the concrete types.

Evidence: conformance suites in internal/tracing/otlp_http_test.go,
internal/metrics/otlp_http_test.go, internal/logs/otlp_http_test.go — real
serialized requests (proto.Marshal of hand-built Export*ServiceRequest, plus
the JSON shapes) through ServeHTTP with the site ID injected via the real
ingest.WithSiteID helper, through the real gzip path and the real
neutron.BodyLimit chain wrapper; each response decoded with the official
type (proto/protojson) and the stub-verified decoded record count asserted.
TDD: suites written first and failing against the old handlers (8 failing
per signal on content type, official decode, 503, 415, 413), then
implementation, then green; one mutation (encoder selection swapped in
tracing) failed both full-success tests naming the seam, then reverted.
e2e/tests/metrics.spec.ts updated to the official ingest shape (200 +
application/json + no partialSuccess; point counts verified via the
per-region query filters). Not run in this session: the three
Nucleus-gated round-trip tests (TestOTLP*RoundTrip*) and the Playwright
suite — no Nucleus fixture / running server available; gated tests skip via
nucleustest.DSN by design. Real-exporter/Collector diagnostics inspection
and unsupported metric shapes remain programme O02 work; durable processing
remains O01.

## 2026-09-21 O03 pre-edit obligations locked — identity model ADR + oracle fixtures

Programme workstream O03 ("One explicit identity and session model")
requires expected answers locked BEFORE any identity code changes. Done,
with NO production identity code changed:

- docs/IDENTITY_MODEL_ADR.md — the entity model (anonymous visitor
  ESTIMATE — explicitly an estimate, not people; persistent anonymous
  client ID as a target entity; identified person; visit/session naming;
  replay session; device-as-attribute), the query-visible entity per
  surface (funnel/retention/stats group on session_id = the monthly
  estimate; persons groups on distinct_id), identify/alias/merge/logout
  semantics targets, salt persistence + rotation policy (rotation opens a
  salt era: historical rows untouched/versioned, new rows use the new
  era, cross-era queries report mixed coverage), event-time vs
  ingestion-time (current code is ingestion-time everywhere — the wire
  protocol has no client timestamp; the model REQUIRES event-time), and
  the recommended default per the programme decision table (anonymous
  and identified both supported explicitly; query entity visible).
- internal/session/reference_test.go — the executable regression oracle
  against the real derivation functions for the five mandatory
  scenarios: (a) anonymous→login→logout under one browser — login/
  logout changes nothing about session/visit derivation; person exists
  only on identified events (visitors=1, sessions=2, persons=1);
  (b) two people behind one NAT — different UAs split, IDENTICAL UAs
  merge into one estimate (the pinned limitation; UA enters the
  fingerprint byte-exact, case-sensitive); (c) one person two devices —
  distinct_id unifies the person, session surfaces double-count
  (visitors=2, sessions=2, persons=1); (d) month boundary — the UTC
  month key is in the hash preimage, so the same visitor splits into two
  estimates across a boundary (pinned as the documented anonymous-
  estimate limitation, NOT correct person identity); (e) delayed
  delivery — no event-time field exists; a late event lands in the
  ingestion-hour visit, ingestion-month estimate, and is stored with the
  ingestion timestamp. Plus TestReference_SaltEras: fixed salt = stable
  across restarts (derivation is stateless); global-salt rotation
  re-keys session_id/visit_id but NOT distinct_id on site-backed
  installs (per-site DB salt), while the unknown-site fallback (global
  salt as HMAC key) DOES re-key persons. Golden literal + a
  month-key-equivalence proof let cross-month rows run through the
  documented formula while remaining chained to production ID().
- Era rule recorded in both files: any future behavior change (O04+)
  bumps an era and updates the tables consciously; history is never
  rewritten.

Validation: the eight new storage-free tests in internal/session only
(`go test ./internal/session/ -run 'TestReference_|TestReference_SaltEras'`,
all pass) and `go vet ./internal/session/` (clean). No Nucleus-gated
suite, no containers, per the shared-fixture constraint. Residual O03
scope (next slices): the model implementation itself (persistent
anonymous client ID, era stamping, event-time acceptance), alias/merge
resolution, person-level funnel/retention modes, UI entity labeling.

## 2026-09-21 OPEN DEFECT — serial suite fails intermittently against live Nucleus v1.1.1 (moving target)

Reproducible locally (podman machine, arm64, Nucleus v1.1.1, migrations
booted, `OBSERVE_NUCLEUS_URL=... go test -p 1 -count=1 ./...`): one
package fails per full run and the failing package MOVES between runs
(observed: internal/incidents TestInRangeCollapsesVersions +
internal/sso TestEnableWritesOneRowAndListResolvesLatest together;
later bench; later cmd/observe). Every failure passes standalone on the
same store immediately after, and a subset run excluding the previously
failing packages shifts the failure elsewhere. All 147 storage-gated
tests otherwise pass against v1.1.1 (down from 147 skips to 2). The
v0.1.8 (amd64-emulated) subset runs clean — CI's full-suite job pins
v0.1.8, so CI cannot see this. v0.1.8's arm64 image is broken outright
(GLIBC_2.38 not found) — Apple Silicon contributors get silent skips.

Classification pending (next slice): sustained-serial-load instability
on v1.1.1 — engine-side (→ Teploy/_internal/UPSTREAM_BUGS.md with a
minimal reproducer once the common assertion is isolated) vs
order-unsafe Observe tests. Not blocking committed slices (all pass
within their runs and standalone). Do not claim the v1.1.1 full-suite
green until this is closed; CI needs a v1.1.1 arm64 job either way
(see _internal/X01_COMPATIBILITY_MANIFEST_2026-09-21.md).

**RESOLVED 2026-09-22 (supersedes the entry above; classification
settled as OURS, not an engine defect).** Root cause: Observe's own
version-tie ambiguity. Every version-rewriting write stamped `version`
(or its updated_at-as-version equivalent) from
`time.Now().UTC().UnixMilli()`, and every latest-row read resolves via
`argMax(col, version) ... GROUP BY key` (internal/query/replacing.go and
the incidents/mcp/exports/aiquery argMax-or-ORDER-DESC variants). Two
writes to the same key inside one millisecond produce TIED versions;
argMax then resolves the tie arbitrarily — a just-enabled SSO config
lists disabled, a revoked MCP token surfaces, a closed incident reads
ongoing, a survey flips inactive. Native arm64 + Nucleus v1.1.1 runs
ops sub-millisecond, so tight test sequences collided routinely; slow
and emulated environments never collided, which is why CI (v0.1.8,
amd64-emulated) could not see it and why the failing package MOVED.
Engine behavior on tied argMax versions is unspecified-but-acceptable —
newest-wins is only promised for STRICTLY greater versions, so the
defect lived in our write stamps, not in Nucleus. No upstream report is
warranted.

Fix: strictly-monotonic versions per key at 18 write sites (working
tree at the time of this entry; see the per-file
`GREATEST(CAST($now AS BIGINT), version + 1)` stamps and the two
Go-side max(now, prior+1) stamps):

- INSERT..SELECT-from-latest, version := GREATEST(now, version + 1)
  (16 statements): sso Enable; surveys Activate + Close; flags Toggle;
  platform DeleteRule + Delete (webhooks); integrations Delete;
  logs/pipelines Delete; reports Delete + markSent; errors bumpIssue +
  UpdateStatus; experiments Start + Stop.
- updated_at-as-version, same GREATEST stamp: incidents Close and
  jobs/exports recordRun (both keep their load-bearing ORDER BY DESC
  LIMIT 1).
- Go-side max(now, prior+1) on VALUES-form writes: mcp TokenStore
  (prior carried from the collapsed read) and aiquery writeSetting
  (prior read first).

First inserts keep version = now — safe because every rewriting write
now reads the latest version and bumps past it, which also protects the
same-ms create-then-rewrite burst. Verified already-monotonic and left
alone: monitoring + dashboards (R19 stamps), principals (nextVersion),
replays upsertSession (039 stamp), cohorts Update/Delete (Go-side
force), rollups + goals + boards + views (delete-then-insert or hard
delete). Sites examined and confirmed NOT tie-exposed: share_links and
api_keys (plain tables with real UPDATEs), audit_events (sequence
chain, single writer), service_stats/service_dependencies and
click_heatmaps (no argMax consumer; raw spans/ SUM reads are the source
of truth), and the append-only tables (llm_traces, feedback, logs,
events, spans, exposures/conversions, results/checkins, deliveries,
replay children/ledger). `version` is never interpreted as wall-clock
time anywhere (all DTO fields are `json:"-"`; ordering is by created_at;
payload timestamps like last_sent/started_at/ended_at deliberately
remain wall-clock, only the version column went monotonic), so
monotonic-but-occasionally-now+1 changes no observable semantics.

Evidence: deterministic mechanism tests (future-seeded prior version —
the same GREATEST arm, zero timing luck) + tight same-ms loop
regressions (the flake's exact shape, no sleeps) in internal/sso,
internal/incidents, internal/mcp, internal/surveys, internal/platform,
internal/flags, internal/experiments, internal/errors, plus an
engine-level expression pin in internal/query (version_bump_test.go).
Red demonstrated pre-fix on sso (tie at 1790108686374 ==
1790108686374), incidents, surveys, and mcp's deterministic arm; green
post-fix. Gates: go vet ./... clean; touched packages green serially
and under -race against live Nucleus v1.1.1; the four worst packages
(sso, incidents, mcp, surveys) 20/20 full-suite loop runs green, other
fixed packages 5/5. The moving serial-suite failures close with this
entry. Remaining honest caveats, unchanged: Nucleus v1.1.1 arm64 still
lacks CI coverage (X01 item) and v0.1.8's arm64 image remains broken.

## 2026-09-22 O01 pre-implementation contract — durable-ingest inventory + ADR + oracle

Programme workstream O01 ("durable acceptance and idempotent processing
for every signal", P0) locked its expected answers BEFORE any ingest
code changes, in the O03 ADR+oracle pattern. NO production ingest
behavior changed; the one production-adjacent edit is a README
truth-labeling fix (below). Audited tree `4697beb` (the checked-out
branch tip; the handoff's `fd2986c` is an unbranched amended variant of
the same fix whose tree additionally carries a `tmp_repro/main.go`
scratch file and a `data/audit.key` blob — neither is on the branch,
and the dangling commit should never be pushed as-is).

Deliverables:

- **ADR:** `docs/O01_DURABLE_INGEST_ADR.md` — per-signal
  acknowledgment/commit diagrams (mermaid), the honest CURRENT
  guarantees table (hop-by-hop: memory-only / WAL-with-periodic-sync /
  synchronous SQL), the fault-model matrix (crash after ACK, crash
  apply-before-checkpoint, lost response, concurrent duplicate, storage
  unavailable, full disk, corrupt/truncated tail), and the DESTINATION
  contract per the programme default: bounded group commit before
  acknowledgment for durable mode, disk-budget admission refusal instead
  of deleting uncheckpointed segments, explicit lossy mode with visible
  counters, count/byte/disk quota design points, poison-record
  quarantine + replay horizon, the finite dedupe-retention-window
  statement (24 h events horizon; `replay_batches` ledger currently
  UNBOUNDED — needs an explicit policy), the error-inbox design (R14's
  durable half, replay-ledger pattern), the derived-work outbox
  (TO-020/R22-durable class), and the per-signal counter taxonomy
  (accepted/rejected/refused/quarantined/applied/pending + the
  dropped-post-ack counter that exists nowhere today).
- **Oracle:** `internal/ingest/o01_pin_test.go` (3 storage-free tests)
  pins the WAL periodic-sync loss window (an AppendBatch success — the
  basis of the HTTP 200 — leaves bytes in the 64 KiB userspace bufio,
  recoverable only after the 500 ms periodic fsync; crash-equivalent
  restart then replays them), and the buffer-level ack ordering (200
  sent before any storage hop). `internal/errors/o01_pin_test.go`
  (Nucleus-gated) pins the errors path: ack is memory-only AND a flush
  failure DROPS the acked record (budget released, nothing requeued).
  Each test names the destination-contract line it guards; the slices
  must flip them deliberately. Already-pinned semantics verified, not
  duplicated: high-water breach drop, torn-tail repair, corrupt-frame
  refusal, checkpoint bounds/monotonicity, replay/flush dedupe, replay
  batch ledger (409 on digest conflict).
- **README truth-labeling:** "Ingest is WAL-backed" overclaimed a
  durable mirror — now names the periodic-sync window and the
  high-water's lossy default, cites the ADR.

Inventory findings — every place a success response precedes durability
(ADR §1/§3 for citations):

1. **Analytics events:** 200 OK after memory admission + an UNSYNCED
   WAL buffer write; up to 500 ms of acked events lost on hard crash
   (pinned). Under disk pressure the high-water DELETES UNCHECKPOINTED
   segments to keep admitting — an availability-over-durability DEFAULT
   without an explicit lossy opt-in (counted, but the programme contract
   says durable mode must refuse instead).
2. **Errors:** 200 OK after a memory-only Push; storage failure at flush
   drops the acked record (pinned); no producer identity, no inbox —
   R14's durable half, designed in ADR §5.6.
3. **OTLP logs/traces/metrics:** 200 comes after synchronous commit
   (honest), BUT the chunked-autocommit + 503-retry shape duplicates the
   committed prefix when an exporter retries (no signal identity — the
   documented O02 limit). Traces additionally schedule rollups +
   detectors in a detached post-response goroutine with NO outbox:
   committed spans can silently miss their derived work forever
   (TO-020 class).
4. **Replays (v2):** the reference durable shape — ledger + digest +
   deterministic children in ONE tx, ack after commit, 409 on
   conflicting batch reuse. Residual: `replay_batches` has NO retention
   policy (unbounded dedupe memory) and post-commit heatmaps are
   best-effort (TO-020).
5. **Corrupt WAL frame at startup** = replay error → AttachQueue fails
   → process silently degrades to memory-only analytics unless
   OBSERVE_REQUIRE_WAL — a poison record kills durability for the whole
   signal instead of being quarantined (ADR §5.9).
6. **Webhook deliveries:** in-memory queue (R22 contained) — restart
   drops undelivered alerts; rides the derived-work outbox slice.

Implementation slices this unblocks, in order (ADR §6): (1) events
group commit + refusal high-water + explicit lossy mode; (2) error
inbox + durable error path (closes R14 durable half); (3) derived-work
outbox (closes TO-020's class + R22 durable half); (4) quarantine +
healthz counter block; (5) ledger retention policies; (6) OTLP retry-
duplicate mitigation (post-O02, only if measurably distorting). The Dash
admission-write replay note from the implementation handoff is recorded
in ADR §5.11: auto-replaying queued records changes a safety contract —
resolve explicitly before restart-resume; Observe's WAL auto-replay is
dedupe-guarded by design, but the pattern must not be exported to
admission-write queues unguarded.

Validation this session: `go vet ./internal/ingest/ ./internal/errors/`
clean; both packages green with `-count=1` and `-race` against the
shared Nucleus v1.1.1 fixture (`OBSERVE_NUCLEUS_URL`), tests
`TestO01_*` (4 new, all passing; the Nucleus-gated one self-skips
without the fixture). Full suite deliberately NOT run (shared-fixture
constraint); `gofmt` note: `internal/ingest/queue.go` is unformatted at
HEAD — pre-existing, untouched by this slice.

## 2026-09-22 vendor-vs-pin drift resolution — renamed neutron module, pin-parity vendor, required parity gate

Finding (orchestrator-verified, resolved this session at `231fa1f`):
`go.mod` replaced `github.com/neutron-dev/neutron-go` with the live
workspace `../../Neutron/go` (then UNPUSHED `75819324`, dirty tree), while
`vendor/github.com/neutron-dev/neutron-go` was generated from a
pre-rename workspace snapshot — 7 files differed from the submodule pin
(`5b5d0a3`) and `nucleus/retry.go` was vendored but absent from it. The
shipped build compiled code that matched neither the pin nor the reviewed
workspace. Upstream then renamed the module: at origin/main `e5c6e9fe`
(PUSHED, ancestor of workspace HEAD, contains `retry.go`) the module is
`github.com/neutron-build/neutron/go`.

Resolution (uncommitted, this session):

- Import path rewrite `github.com/neutron-dev/neutron-go` →
  `github.com/neutron-build/neutron/go` across 162 `.go` files plus
  `go.mod`, `.github/workflows/ci.yml`, `AGENTS.md`, `CLAUDE.md`
  (166 files total; `_internal/` audit history and the gitignored `observe`
  binary deliberately untouched).
- vendor/ regenerated from the PIN TREE (`git worktree` of `e5c6e9fe`
  at `/tmp/neutron-pin-worktree`, replace pointed there for
  `go mod vendor` only), then the standing replace restored to
  `../../Neutron/go`. Regeneration procedure documented as a comment in
  `go.mod` (worktree the pin → replace → vendor → restore replace →
  align the two `# ... =>` annotation lines in `vendor/modules.txt`,
  which record the generation-time replace target and otherwise break
  `-mod=vendor` consistency → re-pin the submodule). Old
  `vendor/github.com/neutron-dev/` removed.
- Submodule `Neutron/` re-pinned `5b5d0a3` → `e5c6e9fe` (records the
  pin without touching upstream; workspace at `75819324` left alone).
- CI `vendor-pin-parity` job rewritten: was a WRONG whole-tree
  `diff -r` (the pin legitimately contains packages vendor/ doesn't
  import — README, examples, cmd — which would false-fail). Now enforces
  the correct invariant: every file under
  `vendor/github.com/neutron-build/neutron/go` byte-identical to the
  same path under the `Neutron/` submodule, vendored files absent from
  the pin are drift (VENDOR-ONLY/CONTENT-DIFF), LICENSE exempt. REQUIRED
  (no continue-on-error) because local parity passes with 0 diffs.

Gates: `go build ./...` green with the new vendor; `go vet ./...` clean;
parity 0 diffs; old vendor path gone. Full serial suite
(`OBSERVE_NUCLEUS_URL` fixture, `-p 1 -count=1`): 43 packages ok,
`internal/auth` FAIL — see the behavior-difference finding below; NOT
adapted, awaiting an owner decision.

Behavior difference, old vendored snapshot vs pin (REPORTED, not papered
over): upstream `086e0253` (audit GO-14..GO-23, between the old pin and
`e5c6e9fe`) added a strict HS256 policy — `neutronauth.GenerateToken`/
`ParseToken` now refuse secrets shorter than 32 bytes
(`jwtMinSecretLen`). Six `internal/auth` tests
(TestJWTAuthMiddleware_RevokedTokenRejected,
TestJWTAuthMiddleware_StreamTicketContract,
TestF03Gate_CreateLoginPromoteOldTokenInvalid,
TestF05OIDCSessionsRevocableAndIssuerScoped,
TestF05Pre040OIDCTokenShapeRejected,
TestTO003_OIDCRoleDowngradeRetiresOldJWT) mint through the shared helper
`newTestService` (`internal/auth/apikeys_test.go:55`) which passes the
11-byte `"test-secret"` — they passed against the old vendored snapshot
only because that snapshot predates the hardening. Production exposure
of the same policy shift: `internal/config/config.go:162` still
validates `OBSERVE_JWT_SECRET` at >=16 bytes, so a configured 16–31-byte
secret that passes config validation will now fail at mint time. The
Observe-side decisions (lengthen the test secret vs raise the config
floor to 32) are deliberately left open here per the stop-and-report
rule; the CI integration jobs that run this suite against the fixture
will fail on these six tests until it is decided.

Residuals:

- Workspace Neutron commits after `e5c6e9fe` (through `75819324`) are
  NOT included in vendor/ or the pin until a future re-pin; the
  standing replace pointing at the workspace remains build-inert while
  vendor/ is present (it only matters for regeneration).
- The regeneration procedure depends on a `git worktree` of the pin
  existing at a temp path; if the pin SHA is absent locally the
  submodule remote must be fetched first.

## 2026-09-22 O08 correctness slice — flag evaluation: valid false vs unavailable vs invalid

Programme O08 (P0): "Distinguish a valid false evaluation from
unavailable configuration... Validate targeting/variants at write and
read boundaries; invalid stored JSON must not silently remove targeting
restrictions." Two source-confirmed defects, both fixed in this slice
(working tree at `ef52aee` + this change, uncommitted):

- Defect 1 (conflation): `internal/flags/flags.go` Evaluate read the
  config with `if err != nil || len(rows) == 0 { return
  &EvaluationResult{Enabled:false}, nil }` — a DATABASE READ FAILURE and
  an absent flag produced the identical silent false. No log, no
  response difference; the handler's 500 branch (cmd/observe) was dead
  code for storage failures because Evaluate never returned an error.
- Defect 2 (targeting hole): targeting JSON was parsed per evaluation
  under `if err := json.Unmarshal(...); err == nil && len(rules) > 0` —
  on parse failure the whole targeting block was SKIPPED, so invalid
  stored rules made a flag evaluable by everyone. Same class on the
  variants path (multivariate parse errors silently dropped variant
  selection). Recon correction: the JSONB column already refuses
  syntactically-broken JSON at the store level (opaque 500), so the real
  storable hole is VALID JSON of the wrong shape (an unarrayed rule
  object) and semantically invalid rulesets (unknown operator, missing
  value) — exactly what the write boundary now rejects.

Three-condition semantics (carried additively on every
`POST /api/v1/flags/evaluate` response as `reason` + `detail`;
`enabled`/`variant` unchanged, legacy decoders unaffected):

| condition   | reason        | enabled | detail                    | server side                |
|-------------|---------------|---------|---------------------------|----------------------------|
| real decision from valid config | `evaluated` | the decision (false AND true both representable) | "flag disabled" / "rollout" / "targeting" / "flag not found" | eval recorded only on the true path (unchanged) |
| config read failed | `unavailable` | false (fail-safe default) | "flags config read failed: {deadline\|canceled\|storage}" | full error LOGGED + `config_unavailable_total` counter |
| stored config failed validation | `invalid` | false (fail-safe default, evaluation REFUSED — never unrestricted) | names the part + the validation error (bounded 200 chars) | quarantined: `invalid_config_total` + `invalid_config_distinct` counters, first occurrence per flag+part logged |

Fail-safe default (documented per-flag contract, flags.go Evaluate doc):
`unavailable` and `invalid` both answer Enabled=false. Flags gate
feature exposure and the create-path default is disabled, so neither an
outage nor corrupt config may widen exposure. A per-flag fail-OPEN
override (kill-switch pattern) is an O08 residual below.

Boundaries: WRITE — Create validates targeting (JSON array of rules;
non-empty attribute; operator in eq/neq/in/not_in/contains; value
present) and variants (array; non-empty unique keys; rollout_pct
0..100) BEFORE storing; rejections return as `*flags.ValidationError`
which createFlagHandler maps to 400 naming the error (previously shape
errors stored silently and only syntactic breakage surfaced as a 500
from the JSONB column). READ — Evaluate validates BEFORE the enabled
short-circuit (operators learn their config is corrupt without
enabling the flag first); invalid targeting or variants quarantine the
flag. Toggle copies rules verbatim (introduces no new JSON) and is
untouched. Quarantine counters surface at `/healthz` under `flags`
(additive). Consumers: evaluate response additive only (ui
flags.ts decodes `{enabled, variant?}` — extra fields ignored);
dashboard List and MCP ListFlags shapes unchanged.

Evidence (TDD red first, both defects demonstrated against original
behavior with only a behavior-neutral fetch-seam extraction added):
`internal/flags/o08_test.go` — red run showed ReadFailure tests failing
(reason "" not "unavailable"; no error surfaced), Create storing the
unarrayed ruleset, the corrupt-row fixture evaluating as a decision;
post-fix 16/16 green incl. live Nucleus (absent/disabled/rollout/
targeting-hit/miss all reason "evaluated" with both false and true
asserted; corrupt-row repair resumes evaluation — quarantine is
per-read, not sticky). Mutations both ways: restoring
skip-on-parse-error fails TestO08_InvalidStoredTargeting...,
TestO08_DisabledFlagWithInvalid... and TestO08_LegacyCorruptRow...;
restoring error-as-absent fails both TestO08_ReadFailure tests.
Gates: `go vet ./...` clean; `internal/flags` + `cmd/observe` green
under `-race` against the fixture; full serial suite
(`OBSERVE_NUCLEUS_URL`, `-p 1 -count=1 ./...`) 45 packages ok, 0
failures.

O08 residuals (recorded, out of this slice's scope):
- Versioned rules: targeting/variants are rewritten as verbatim strings
  on version-rewriting writes; no schema/version stamp for rule
  changes themselves.
- Bounded-version hashing / SDK-agreement: hashUser (sha256 first two
  bytes mod 100) has no version negotiation with any client-side
  evaluation; a future local-eval SDK must agree on bucketing.
- Kill switch / per-flag fail-open default: unavailable+invalid answer
  false platform-wide; a flag whose safe state is ON needs a stored
  per-flag default plus an SDK contract to express it.
- Exposure dedup: flag_evaluations rows are appended per enabled
  evaluation with no per-user dedupe window.
- Local (client-side) evaluation: none exists; all evaluation is
  server-side through the public endpoint.

## 2026-09-22 O04 slice 1 — funnel/retention entity + window/order semantics (pinned)

Programme O04 (P0): "Funnels need selectable entity, sequence/order,
conversion window, exclusions, repeated steps and property attribution;
timestamp ties need deterministic handling. Retention must define
first-seen history, cohort entry event, return event, period/timezone
and incomplete periods." Working tree at `06ccf1e` + this change,
uncommitted. Oracle-first per the O03 rule: the expected-answer tables
were extended BEFORE the implementation, and the era was NOT bumped
(no identity derivation changed — the queries label what they already
grouped and add new grouping modes).

Deliverables:

- **Entity parameter** (funnel `/api/v1/stats/funnel` +
  `/funnel/breakdown`, retention `/api/v1/stats/retention`):
  `entity=visit|person|visitor-estimate` (O03 ADR vocabulary,
  verbatim). Default "" = visitor-estimate = the pre-O04 grouping,
  now honestly LABELED. Response surfaces name the entity additively
  (per-row `entity` + `entity_limitation` fields on FunnelResult /
  FunnelBreakdownResult / RetentionCohort — the arrays keep their
  shape, so existing UI callers decode unchanged). Unknown values are
  rejected (400 via neutron.ErrBadRequest), never silently degraded.
  Person mode groups identified events only (distinct_id '' excluded);
  visit mode groups the clock-hour bucket; visitor-estimate keeps
  session_id.
- **Pinned funnel semantics** (`internal/query/funnel.go` doc
  comments + tests): ordering by (timestamp, event_id) ASC — a total
  order, event_id unique per site — replacing the old unstable
  sort-on-timestamp-only (tie order was engine-arrival order,
  unspecified); greedy earliest-chain progression, each step a distinct
  strictly-later event (repeated steps re-fire; conversion keyed on
  first progression); bounded conversion window
  (`conversion_window_ms`, measured from the FIRST step-0 event,
  edge ts == e0+W INCLUSIVE, default 0 = query range only); exclusion
  steps (`exclusions`) — the first exclusion-matching event strictly
  after the step-0 event terminates progression, pre-entry events
  never disqualify.
- **Pinned retention semantics** (`internal/query/retention.go`):
  cohort entry = sessions-rollup first_ts for the default
  visitor-estimate path (unchanged — events never create cohorts
  there), or the entity's FIRST matching event when
  `cohort_event=event_type` is set / for visit+person modes
  (raw-events-bounded first-seen history — documented limitation);
  return activity = any event or `return_event=event_type`; period =
  periodDays (default 1, 7 when range > 30d), buckets are absolute
  UTC-epoch multiples (7-day buckets start THURSDAY — pinned in a
  test), timezone UTC-only and documented; incomplete periods are
  EXCLUDED from `periods` and flagged per row via `incomplete_periods`
  (a column is complete once fully elapsed by `to`; the pre-O04 code
  rendered the current period as a misleadingly-low value). Two grid
  repairs landed as part of the pin: the dead trailing bucket
  (starting exactly at `to`) is no longer a grid column, and the
  legacy first_ts string-scan (CAST-parse via digits-only parseInt64)
  was replaced by the native int64 scan the Sessions browser already
  uses — the old path was never under test and parsed nothing when the
  driver handed back a number.
- **Oracle extension** (`internal/session/reference_test.go`, O04
  section): HAND-COMPUTED tables for the O03 scenarios a/b/c under
  each entity mode (entity partitions asserted against the real
  derivation functions; funnel/retention count literals written as
  spec), plus the timestamp-tie pin, window-edge pin, and the
  late-arrival truth pin. Executable binding:
  `internal/query/funnel_semantics_test.go` (storage-free — the walk,
  entity dispatch, and grid math) +
  `internal/query/funnel_retention_nucleus_test.go` (Nucleus-gated —
  seeds the scenarios with production-derived ids, asserts the same
  literals end to end, plus range edges, validation, and
  default-path identity).

**The late-event truth (what the queries ACTUALLY order by):** the
stored `timestamp` column, which era-1 ingest sets to INGESTION time
— the wire protocol has no client event-time field (O03 ADR D8).
Funnels therefore sequence a late-delivered event at its ingestion
position (pinned: funnel [B,A] over a late-delivered B does not
convert even though B truly preceded A; under event time it would).
Retention buckets return activity on ingestion time identically.
This is recorded as the O03-implementation dependency it is: when
event-time lands on the wire (O03 D8), funnels/retention must
re-pin these tables in the same change and bump the era.

TDD evidence: semantics tests written first, failed to compile
against the pre-O04 code (walkFunnelEvents / entityKeyOf /
buildRetentionCohorts did not exist); during bring-up three
hand-computed literals were CORRECTED AGAINST the tests (scenario C
step-0 count is 1 not 2 — only the laptop did signup; the trailing
grid column is structurally dead; the incomplete-count definition
narrowed to the cohort's current period) — each fix landed in the
oracle comments and the tests together, which is the tables doing
their job. Mutations both ways: entity dispatch hard-wired to
session_id fails the person-mode tables across scenarios a/b/c
(6+ assertions) AND the storage-free dispatch/grouping tests;
tie-break flipped to event_id DESC fails the tie table exactly.
Gates: `go vet ./...` clean; `internal/query` + `internal/session`
green under `-race` against the fixture; full serial suite
(`OBSERVE_NUCLEUS_URL`, `-p 1 -count=1 ./...`) green, 0 failures.

O04 remainder (next slices):
- **Property attribution** (funnel steps keyed on event properties,
  not just event_type/pathname) — the biggest unpinned piece.
- **Exclusions depth**: windowed/between-step exclusions (current
  pin: first exclusion after entry ends progression globally);
  exclusion scopes per step.
- **Cohorts agreement**: the `cohort_id` filter (resolveFilters)
  expands person ids into filters built on distinct_id while the
  surrounding charts may group a different entity — entity-aware
  cohort resolution needs a pass.
- **Budgets/materialization**: funnels/retention fetch full-range raw
  events per request (bounded only by the 186-day retention clamp +
  12-column grid); no materialized per-entity step/cohort state. A
  budget story is needed before these surfaces face large sites.
- **UI entity labeling** (O03 D6/D11): the API names the entity; the
  dashboard panels and the envelope-vs-array response shape ride that
  slice.
- **Retention timezone parameter** (UTC-only today, documented) and
  pathname-scoped cohort/return events (event_type exact match today).
