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

Open items: 17 (3 P1 product/schema decisions, 12 P2 designs, 2 CI/ops)

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
(the 5402af4 UI fixes were synced in 5fbf61f, 2026-09-17).

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
  time-boxed context. Atomic claim-as-record stays deferred with F03.
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
rationales below): AUD-001 = F02, AUD-004 = F03, AUD-006 = F05,
AUD-011 = F12/F19, AUD-013 = F16, AUD-029 = F37, AUD-042 = F45 (app
half), AUD-043 = F45 (upstream half), AUD-049 = F46, AUD-050 = F47,
AUD-055 = F49.

New deferrals from this round:

- AUD-008 - P2 - Open (protocol): normal access JWTs accepted in query
  strings for EventSource/download prefixes. EventSource cannot set
  headers, so removing the fallback breaks live logs/exports; the fix is
  a short-lived single-purpose stream-ticket mint with audience + route
  binding, or fetch-stream consumers. Referrer-Policy: no-referrer is set
  on those responses as the interim mitigation.
- AUD-017 - Low - Open (hardening): WAL final-component symlink refusal
  and an exclusive writer lock need O_NOFOLLOW/flock build-tagged files;
  local deployment hardening, not remotely reachable.
- AUD-054 (site half) - P2 - Open: audit events default site "default"
  because the outer middleware cannot see downstream-bound context; needs
  the route-level producer migration sketched in the report.
- AUD-056 (remainder) - P2 - Open: required Nucleus integration fixture,
  missing-dependency-fails-required-CI, and browser-level tests of the
  served dashboard/player remain unbuilt.

Upstream: no NEW Nucleus/Neutron defects were confirmed this round; no
framework edits were made from this session. The standing F45
snapshot-boundary report remains the only open upstream item.
