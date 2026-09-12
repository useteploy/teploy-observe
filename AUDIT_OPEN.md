# Open audit items

Unresolved findings for this repository from the ChatGPT-led audit series (2026-09-09 through 2026-09-11, passes 1-5; register: teploy-neutron-lullmail expanded audit). Every P0/P1 finding has been fixed and verified; the items below are the remaining P2/P3 tail plus one item needing validation. Fields are quoted from the audit register; line references point at the review commits listed per item where recorded.

Open items: 1 P2 improvement (1 total)

## useteploy__teploy-observe-03 - P2 - Open improvement

**Use private WAL permissions and explicit disk/replay limits**

- Kind: Improvement
- Evidence: The queue creates directories with 0755 and log/checkpoint files with 0644. Events contain URLs, identifiers and arbitrary properties. maxBytes is a compaction threshold only: the WAL can exceed it while uncheckpointed data remains, and Pending builds a slice of all pending events.
- Impact: On a host whose parent directories and umask permit access, other local accounts may read sensitive telemetry. A prolonged database outage can also grow disk usage and startup replay memory. Deployment-level directory protection may already mitigate the first concern.
- Proposed fix: Prefer 0700 directories and 0600 queue files, validate existing permissions where appropriate, document data sensitivity, and add disk high-water/backpressure and bounded streaming-replay policies.
- Acceptance test: Test resulting permissions under a permissive umask, sustained database outage beyond the configured threshold, and recovery with more pending events than the in-memory buffer target.
- Review commit: `c2b3fe1c3d9078729d1b25b266134aedb3846843` (last reviewed 2026-09-10)


## Resolution log (2026-09-12)

- teploy-observe-06: FIXED - single owned flush loop with a capacity-one wakeup channel; size/timer/shutdown flushes share one lifecycle (audit commit).
- teploy-observe-03: PARTIALLY FIXED - queue dirs now 0700 and WAL/checkpoint files 0600. DEFERRED (design): disk high-water backpressure and bounded streaming replay are capacity-planning features, not defects; they need an explicit policy decision (limits, behavior on breach) before implementation.
