# High availability and clustering (NOT BUILT)

Status: design note. Observe runs as one instance on one Nucleus. There is no
failover, no multi-node storage and no supported way to run two Observe
processes against the same database.

## Current state

- README, "Operational limits": "There is no clustering, no multi-node
  storage; scale is a single instance plus the budgets above."
- Ingest durability is a local-disk WAL under `$OBSERVE_QUEUE_DIR`
  ([O01 durable ingest ADR](../O01_DURABLE_INGEST_ADR.md)). A 200 means fsynced
  on that machine. The ADR's outbox section states the single-process claim:
  "Single-process claim (AUD-018 posture) - multi-replica needs a lease/CAS
  design before externally-visible kinds ride it." Its dedupe horizons (24 h
  flush filter, 10 min admission cache) are in-process state.
- Other in-process state that assumes one instance: the metrics series
  registry (bounded, in-memory), rate-limiter buckets, background jobs
  (rollups, retention, alerts, cron/uptime monitors, scheduled exports), and
  the upgrade tool (stop, swap, start on one host).
- Nucleus: `docker-compose.yml` starts it with `--cluster-port 5433` and
  `NUCLEUS_ALLOW_INSECURE_CLUSTER` / `NUCLEUS_ALLOW_INSECURE_REPLICATION` set
  to `1`, but Observe does not use or test replication. Nothing in this repo
  configures a replica or failover, or reads from one.
- Recovery today is restore from backup
  ([backup-restore.md](../operations/backup-restore.md)) plus WAL replay on
  restart. For database loss the recovery point is the last backup (the WAL
  covers only the unapplied tail); recovery time is operator time.

## Requirements if built

1. Database HA depends entirely on Nucleus replication and failover; Observe
   cannot provide it above pgwire. Upstream capabilities, consistency
   guarantees (async vs sync), promotion procedure and client reconnect
   behaviour must be documented by Nucleus first. This repo cannot verify them
   (no live Nucleus here).
2. Multiple Observe replicas need a leader lease/CAS for every singleton job
   (rollups, retention, alert evaluation, monitors, scheduled exports, outbox
   drain, audit checkpoints), a story for each node's WAL (acknowledged events
   are replayable only by the node that holds them), and externalized dedupe
   state. `internal/backup/lease.go` is a Nucleus snapshot lease, not a leader
   election.
3. Shared rate limiting and WAL budgets, or an explicit per-node contract.
4. A load-balancer story for the ingest and dashboard listeners
   (`OBSERVE_INGEST_ADDR` already separates them).
5. Rolling upgrades: migrations are forward-only and run at boot, so old and
   new binaries running together need compatibility rules that do not exist
   (see [upgrade.md](../operations/upgrade.md), AUDIT_OPEN L9).

## Risks

- A naive "run two replicas" setup double-runs singleton jobs, double-fires
  alerts, and loses acknowledged events if a node's disk dies.
- Cannot be validated here; every claim needs a live multi-node Nucleus.
- Each in-memory structure above is a separate change.

## Recommended approach

Stay single-node and make recovery boring: tested backups, a documented
restore drill, and a warm standby that is a restored copy rather than a live
replica. Revisit HA once Nucleus ships and documents replication with failover
semantics; do Nucleus HA first (Observe is a client and may only need
reconnect handling) and leave multi-replica Observe for last.

## Open owner decisions

- What recovery point and time objectives are promised to users? That decides
  whether backup-and-restore is enough.
- Is Nucleus replication on the upstream roadmap, and who owns documenting it
  (an upstream report, not a fix from this repo)?
- Is multi-replica Observe wanted at all, or only database HA?
