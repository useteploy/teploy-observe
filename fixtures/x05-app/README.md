# X05 fixture app — replay instrumentation target + O06 experiment harness

The maintained fixture application (programme X05, first slice) and the
measurement harness for the O06 replay-engine decision (DELEGATED_DECISIONS
2026-09-23 section 8.2, experiment 1).

## Layout

- `page/index.html` — realistic single-page app: ~700-node DOM (header, KPIs,
  12-card catalog, form, modal), a live ticker that MUTATES the DOM every 2 s
  (text updates + node add/remove — the churn that separates snapshot-only
  from incremental capture), tab navigation, and a deterministic 1-in-5
  submit error for the error-path fixtures.
- `arm-a/` — the CURRENT recorder: served from `cmd/observe/tracker/
  observe-replay.js` by the harness (structural snapshots, mutations counted).
- `arm-b/recorder.mjs` — the rrweb 2.1.6 wrapper in the adopted integration
  shape: `record()` emits through a sanitizer seam (pass-through for the
  experiment — Observe's own rules run in both futures), batches ride v2-shape
  POSTs to the same endpoint; sampling = rrweb defaults (mousemove 50 ms,
  scroll 100 ms, input 'last'), `checkoutEveryNms: 30000` to match arm A's
  re-snapshot cadence (keyframe parity is what makes bytes comparable).
  Built with esbuild to `arm-b/recorder.js` (committed; rebuild with
  `npm run build:arm-b`).
- `harness/measure.mjs` — playwright-core over SYSTEM Chrome
  (`channel: 'chrome'`, no download). Serves the page, substitutes the arm's
  recorder tag INTO the HTML before load (post-load injection would make
  every LCP comparison vacuous), fulfills the ingest endpoint with 200, and
  counts POSTed body bytes at the transport.

## Running

```
npm install
npm run build:arm-b
npm run measure:lcp        # 20 loads per arm; LCP/FCP/long tasks
npm run measure:session    # one identical 5-minute scripted journey per arm
```

## Experiment protocol (pre-declared before the harness existed)

A/B on the same fixture page and journey: effective gz bytes of the shipped
recorder, p95 LCP delta, long-task time, session bytes at default sampling.
Thresholds (breach = rrweb reversal condition 3): recorder <= 100 kB gz
effective, <= 100 ms p95 LCP delta, session <= 2 MiB / 5 min sampled.

## Results — 2026-09-23, rrweb 2.1.6, Chrome (system, headless), this harness

| Metric | Arm A (current) | Arm B (rrweb) | Threshold | Verdict |
|---|---|---|---|---|
| Recorder, gz | 10.2 kB (30,901 B raw) | **57.0 kB** (182,665 B raw) | <= 100 kB | PASS (43% headroom) |
| LCP p95 delta | p95 48 ms (median 42, mean 43.0) | p95 120 ms (median 48, mean 59.2) | <= 100 ms | PASS (+72 ms; see variance note) |
| Long tasks | 0 (0 ms) | 0 (0 ms) | informational | none |
| Session bytes / 5 min | 320,208 B (313 KiB, 32 posts) | **907,589 B** (886 KiB, 32 posts) | <= 2 MiB | PASS (44% headroom) |

Variance note (honest): two independent n=20 LCP runs measured the p95 delta
at +8 ms and +72 ms — both pass, and the stable statistics tell the real
story: mean delta +7 to +16 ms, median delta +6 to +10 ms, zero long tasks
in every run of either arm. The p95 estimator at n=20 is the 19th order
statistic and swings on one outlier; the threshold holds with margin in both
runs.

Session-bytes note: rrweb ships ~2.8x the structural recorder on this
mutation-heavy fixture (it captures every incremental mutation; arm A only
counts them). 886 KiB / 5 min at default sampling leaves 44% headroom to the
2 MiB envelope; the ingest byte caps and per-arm sampling remain the
operator-side valves.

## Verdict

**All three pre-declared thresholds PASS. Reversal condition 3 does NOT
fire. The rrweb 2.x adoption (decision 17) stands; the O06 integration
slice is unblocked.**

Method notes for reproduction: bytes are counted at the HTTP transport
(request body sizes, pre-gzip — what the server receives); the bundle gz
figure is `gzip -c` of the exact script served; both arms run the identical
journey (mouse paths, scrolls, card opens, form fills, tab switches) against
the identical DOM including its 2 s mutation cadence.
