# SDK manifest (O11): dependencies, types, bundle sizes, runtimes

Declared per SDK. Bundle sizes are of the **packaged artifact as built by the
package's own `npm run build`** (tsc output, unminified — the artifact npm
would serve), measured with `gzip -9`. Re-measure with:

```sh
cd sdk/browser && npm ci && npm run build && gzip -9 -c dist/index.js | wc -c
```

None of the four SDKs is published yet; package preparation and tarball
tests proceed without publication (owner-controlled action per the O11
spec). Runtimes below are what the test suites exercise.

| SDK | runtime deps | dev deps | types | bundle raw / gz(-9) | runtimes tested |
|---|---|---|---|---|---|
| `@teploy/observe-browser` 0.1.0 | **0** | typescript 5.4, tsx 4.19, @types/node 22 | `dist/index.d.ts` (11,199 B) shipped | **33,730 B / 11,002 B** | Node 22 (`node --test`, stubbed browser globals + real-loopback transport tests) |
| `@teploy/observe-sentry-shim` 0.1.0 | **0** | typescript 5.4, tsx 4.19, @types/node 22 | `dist/index.d.ts` (10,659 B) shipped | **20,397 B / 6,502 B** | Node >= 18 (`node --test`) |
| `sdk/go` (module `github.com/useteploy/teploy-observe/sdk/go`) | **0** (Go stdlib only) | — | godoc comments | compiled in (Go) | Go 1.22 (`go test`) |
| `sdk/python` (`teploy-observe` 0.1.0) | **0** (stdlib only: urllib/json/threading/contextvars) | — | inline type hints | n/a | CPython 3.8+ (`unittest`) |

## Durability and loss posture (all SDKs)

- Queues are **bounded** (count + bytes) with an explicit overflow policy:
  browser drops **newest** at admission and **oldest** pending requests at
  the retention cap; Go and Python drop the **new** entry at admission and
  a head-of-queue chunk only after its bounded retry budget is exhausted
  (or immediately on a non-retryable 4xx).
- Every drop increments a **loss counter** queryable through the SDK's
  diagnostics API (`getStats()` / `Stats()` / `stats()`) and surfaced as a
  summary at shutdown through the error hook.
- Queues are in-memory only. `kill -9` loses the queue's contents and the
  counters with the process — no durable local spool is claimed.
