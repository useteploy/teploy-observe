# SDK adverse-transport test matrix (O11)

One spec, three implementations (per-language test harnesses must produce
byte-identical fault behavior):

| mode | behavior | proves |
|---|---|---|
| `ok` | 200, `{ok:true}` (events ack adds `accepted`/`rejected` as needed) | baseline delivery + counters |
| `status` | fixed status (500, 400, 401, 429) every request | retryable vs non-retryable classification |
| `failN` | first N requests 500, then 200 | bounded retry with backoff recovers, no loss |
| `partial` | 200 with `{ok:true, accepted:k, rejected:n-k}` (events) | per-record outcomes: accepted neighbors kept, rejected counted, batch never resent |
| `slow` | delay > the SDK's request deadline, then 200 | deadline aborts the hang; flush owner not pinned |
| `reset` | destroy the socket mid-exchange | transport-level response loss surfaces as an error and follows the retry path |
| `redirect` | 302 to a third-party origin | credential (X-API-Key) never forwarded off-origin |

Implementations:

- Node (browser SDK + sentry-shim tests): `faultserver.mjs` in this
  directory, imported by `sdk/browser/tests/transport.test.ts` and
  `sdk/sentry-shim/tests/transport.test.ts`.
- Go: `faultServer` in `sdk/go/observe_test.go` (same modes).
- Python: `sdk/python/tests/faultserver.py` (same modes).

Kill -9 coverage is intentionally NOT a fault-server mode: every SDK queue
is in-memory, so a killed process loses queue and counters together. The
shutdown-deadline paths (`Shutdown(ctx)`, `close(timeout)`, `flush(timeout)`,
pagehide) are the tested, honest boundary; `docs/sdk/MANIFEST.md` states the
no-durable-spool trade-off.
