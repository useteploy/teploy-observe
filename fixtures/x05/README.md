# X05 — fixture application + soak generator

Programme §95 (X05): a maintained fixture with frontend, API, background
job, database, slow endpoint, error, experiment, dynamic DOM and
identifiable release. **Expected results live HERE — outside the
implementation under test.** The app never asserts its own correctness;
harnesses read `/api/stats` and compare against this table.

## Status (honest)

| Leg | State |
|---|---|
| frontend + dynamic DOM | LANDED (mutating counter, form, scrollable growing list, assignment fetch) |
| API | LANDED (hit/assign/stats/slow/error) |
| background job | LANDED (5s durable heartbeat) |
| database | v0: append-only fsync'd JSONL, replayable (see below) |
| slow endpoint | LANDED (`/slow?ms=`) |
| error endpoint | LANDED (`/error`, counted) |
| experiment | LANDED (deterministic sha256 assignment, exposures durable by arm) |
| identifiable release | LANDED (`-release` flag → `/api/stats.release`) |
| soak generator | LANDED (`../soak`) |
| rrweb A/B page | NEXT (the O06 overhead experiment mounts on `/`; thresholds pre-declared in DELEGATED_DECISIONS §8.2) |
| SSH/Docker/Caddy scratch hosts | NEXT (X05 acceptance runs; the CLI-side deploy fixtures) |
| defect-caught-by-harness proofs | PARTIAL (failpoints land; per-harness catch proofs arrive with each harness) |

The database leg is deliberately v0: a JSONL counter log with fsync-before-
ack and exact replay. The durable CONTRACT is what the fixture pins; the
SQL engine arrives with the stateful-service slice (C07 era) when a real
schema is worth maintaining.

## Expected results (the external truth table)

After N POSTs to `/api/hit`, M distinct-client GETs of
`/api/experiment/assign?client=...`, E GETs of `/error`, and U seconds of
uptime (heartbeats at 5s), `/api/stats` MUST answer:

- `counters.hit == N` (exactly — durability before acknowledgment)
- `counters.exposure_control + counters.exposure_variant == M` (each
  assignment lands in exactly one arm, deterministic per (site, client):
  arm = sha256(site NUL client)[0] & 1)
- `counters.error_injected == E`
- `counters.heartbeat == floor(U / 5)` minus at most 1 (ticker phase)
- `release == <the -release flag>` (identifiable release)
- all counters SURVIVE a restart of the process (replay)

Failpoints (`TEPLOY_X05_FAILPOINT`): `stats_read` → `/api/stats` 500s;
`bg_commit` → heartbeats silently stop. Each is the intentionally
introduced defect a harness must CATCH before that harness is trusted
(X05 acceptance) — a harness that passes with `bg_commit` set is broken.

## Running

    go run ./fixtures/x05/app -addr 127.0.0.1:8085 -release demo

## Soak: the Nucleus compression-factor measurement

The pre-declared owner experiment (DELEGATED_DECISIONS §8.3): at the
declared load (100 events/s, ~1 KiB average → 8.24 GiB/day raw), measure
bytes-on-disk vs raw inflow through the REAL pipeline.

1. Record before: `psql <observe_nucleus_dsn> -tAc "SELECT
   pg_database_size(current_database())"`
2. Run the generator against a running observe (site key required):
   `go run ./fixtures/x05/soak -url http://127.0.0.1:8090 -key <key> \
      -rate 100 -duration 24h`
   It prints `RAW_BYTES_SENT` and `EVENTS_ADMITTED` at exit and progress
   every 30s.
3. Record after (same SQL), then:
   `compression_factor = RAW_BYTES_SENT / (after - before)`

Shape verification without a server: `go run ./fixtures/x05/soak
-dry-run`. The envelope is contract-pinned against the production ingest
types (`TestEnvelopeMatchesProductionIngestTypes`) — drift there is what
would make the measurement fiction.

A short calibrated run (e.g. `-duration 10m`) should precede the 24h run;
the 24h run is the owner-experiment input and is deliberately not started
by CI.
