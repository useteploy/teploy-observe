// Data Handling surface (O13): pure mappings from the live /api/v1/meta
// retention policies into the per-signal rows the settings page renders, plus
// the copy for what export/deletion actually do. Kept pure so the grouping
// and the honesty of each row's wording can be pinned by unit tests — the
// page itself only fetches and renders.
//
// The rule this module enforces: every row says what the CODE does, never
// what a privacy policy would want to be true. Hashed identifiers are not
// anonymized; absent policies are shown as "no automatic deletion"; export
// and deletion state only claims mechanisms that exist.

export interface MetaPolicy {
  table: string;
  days: number;
}

export interface SignalRow {
  /** Signal name in operator language. */
  signal: string;
  /** Policy days, or null when no policy covers this signal. */
  days: number | null;
  /** Table names the signal's retention comes from. */
  tables: string[];
  /** What deletion actually does for this signal, per the code. */
  deletion: string;
}

// Which tables back which operator-facing signal. The groupings follow the
// retention policy tables in internal/jobs/retention.go.
const SIGNAL_GROUPS: Array<{ signal: string; tables: string[] }> = [
  { signal: "Raw events", tables: ["events"] },
  { signal: "Recent-events cache", tables: ["events_recent"] },
  { signal: "Hourly rollups", tables: ["stats_hourly"] },
  { signal: "Sessions", tables: ["sessions"] },
  { signal: "Error events", tables: ["error_events"] },
  { signal: "Logs", tables: ["logs"] },
  { signal: "Traces (spans)", tables: ["spans"] },
  { signal: "Service metrics rollups", tables: ["service_stats"] },
  { signal: "Session replays", tables: ["replay_sessions"] },
  { signal: "Alert evaluation ledger", tables: ["alert_evaluations"] },
  { signal: "Error ingest ledger", tables: ["error_inbox"] },
  { signal: "Replay ingest ledger", tables: ["replay_batches"] },
  { signal: "Derived-data outbox", tables: ["derived_outbox"] },
  { signal: "Notification outbox", tables: ["notification_outbox"] },
];

const DELETION_COPY: Record<string, string> = {
  events: "Deleted automatically once older than the policy window.",
  events_recent: "Deleted automatically once older than the policy window.",
  stats_hourly: "Deleted automatically once older than the policy window.",
  sessions: "Deleted automatically once older than the policy window.",
  error_events: "Deleted automatically once older than the policy window.",
  logs: "Deleted automatically once older than the policy window.",
  spans: "Deleted automatically once older than the policy window.",
  service_stats: "Deleted automatically once older than the policy window.",
  replay_sessions: "Deleted automatically once older than the policy window.",
  alert_evaluations:
    "Deleted automatically once older than the policy window (log, not dedupe set).",
  error_inbox:
    "Expiring from the dedupe ledger means a retried record is processed as new.",
  replay_batches:
    "Expiring from the dedupe ledger means a retried record is processed as new.",
  derived_outbox:
    "Prunes PROCESSED intents only; pending/retrying/dead-lettered rows are never auto-deleted.",
  notification_outbox:
    "Prunes DELIVERED/SUPPRESSED intents only; pending/retrying/dead-lettered rows are never auto-deleted.",
};

// Signals with no table-level policy at all — listed explicitly so the page
// can show "no automatic deletion" rather than omitting them.
export const UNPOLICED_SIGNALS: Array<{ signal: string; note: string }> = [
  { signal: "Source maps", note: "Kept per site for the last 10 releases (pruned when a new release is uploaded); older releases are deleted." },
  { signal: "Daily rollups, cohorts, dashboards, issues, users, audit log", note: "No time-based deletion — kept until deleted by hand or by deleting the instance's data." },
];

/** Group the live policy list into per-signal rows, in a stable order.
 * A signal whose tables carry no policy shows days=null ("no automatic
 * deletion") instead of inheriting a neighbor's number. */
export function signalRows(policies: MetaPolicy[]): SignalRow[] {
  const byTable = new Map(policies.map((p) => [p.table, p.days]));
  return SIGNAL_GROUPS.map((g) => {
    const known = g.tables.filter((t) => byTable.has(t));
    const days = known.length
      ? Math.max(...known.map((t) => byTable.get(t) ?? 0))
      : null;
    return {
      signal: g.signal,
      days,
      tables: g.tables,
      deletion: known.length === 1
        ? (DELETION_COPY[known[0]] ?? DELETION_COPY.events)
        : "Deleted automatically once older than the policy window.",
    };
  });
}

export function formatDays(days: number | null): string {
  if (days === null) return "no automatic deletion";
  if (days <= 0) return "disabled";
  return `${days} days`;
}

/** The honest export story. There is no request object for ad-hoc exports —
 * the download IS the completion — so the only queryable status is the
 * scheduled SQL export history. */
export const EXPORT_NOTES = [
  "Ad-hoc export (Settings or any panel's Export button) streams events or sessions as CSV/JSON for a site and date range. There is no server-side request record: the download completing is the only status.",
  "Each ad-hoc export is capped at about six months per request; narrow the range for longer history.",
  "Ad-hoc export covers events and sessions only. Replays, source maps, traces and logs have no export format yet.",
  "Scheduled SQL exports (below) run a SELECT on a schedule and upload NDJSON/CSV to S3-compatible storage; their last run status and any error are shown as they are recorded.",
];

/** The honest deletion story, matching what the handlers actually do. */
export const DELETION_NOTES = [
  "There is no per-person or per-subject deletion request workflow. Deleting data means age-based retention (the table above) or deleting a whole site.",
  "Deleting a site removes the site record and revokes its API keys immediately. The events, sessions, replays and errors it captured are NOT deleted at that moment — they remain until each signal's retention window expires, and are then removed by the scheduled cleanup.",
  "Backups made before a deletion (or before retention expired) still contain the deleted data. Restoring a backup restores it. Manage backup retention separately.",
];

/** Identity handling — the hashing-is-not-anonymization statement. */
export const IDENTITY_NOTES = [
  "Visitor identifiers are stored as salted hashes (per-site session salt). A hash of an identifier is still personal data: it links events, and a known identifier can be re-hashed to match it.",
  "Sites can opt into storing raw distinct IDs (Settings API, site privacy flag) — the raw_distinct_id flag is per site.",
  "Observe is cookieless, but cookieless tracking is not a compliance claim. Any legal compliance statement about this instance requires its own review.",
];
