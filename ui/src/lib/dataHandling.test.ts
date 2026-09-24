import { test } from "node:test";
import assert from "node:assert/strict";
import {
  signalRows,
  formatDays,
  EXPORT_NOTES,
  DELETION_NOTES,
  IDENTITY_NOTES,
} from "./dataHandling.ts";

// The live policy set from internal/jobs/retention.go defaults, used as the
// representative input.
const LIVE = [
  { table: "events", days: 30 },
  { table: "events_recent", days: 7 },
  { table: "stats_hourly", days: 365 },
  { table: "sessions", days: 90 },
  { table: "error_events", days: 180 },
  { table: "logs", days: 30 },
  { table: "spans", days: 14 },
  { table: "service_stats", days: 30 },
  { table: "replay_sessions", days: 14 },
  { table: "alert_evaluations", days: 14 },
  { table: "error_inbox", days: 14 },
  { table: "replay_batches", days: 14 },
  { table: "derived_outbox", days: 7 },
  { table: "notification_outbox", days: 7 },
];

test("every known signal gets a row, in order", () => {
  const rows = signalRows(LIVE);
  assert.equal(rows.length, 14);
  assert.deepEqual(
    rows.map((r) => r.signal).slice(0, 5),
    ["Raw events", "Recent-events cache", "Hourly rollups", "Sessions", "Error events"],
  );
});

test("days come from the matching table, not a neighbor", () => {
  const rows = signalRows(LIVE);
  const bySignal = new Map(rows.map((r) => [r.signal, r]));
  assert.equal(bySignal.get("Raw events")?.days, 30);
  assert.equal(bySignal.get("Hourly rollups")?.days, 365);
  assert.equal(bySignal.get("Session replays")?.days, 14);
});

test("a signal with no policy shows null days, never a borrowed number", () => {
  const rows = signalRows([{ table: "events", days: 30 }]);
  const bySignal = new Map(rows.map((r) => [r.signal, r]));
  assert.equal(bySignal.get("Raw events")?.days, 30);
  assert.equal(bySignal.get("Sessions")?.days, null);
  assert.equal(bySignal.get("Logs")?.days, null);
});

test("outbox rows say processed-only pruning", () => {
  const rows = signalRows(LIVE);
  const bySignal = new Map(rows.map((r) => [r.signal, r]));
  assert.match(bySignal.get("Derived-data outbox")?.deletion ?? "", /PROCESSED intents only/);
  assert.match(bySignal.get("Notification outbox")?.deletion ?? "", /DELIVERED\/SUPPRESSED intents only/);
});

test("formatDays states absent policies honestly", () => {
  assert.equal(formatDays(null), "no automatic deletion");
  assert.equal(formatDays(0), "disabled");
  assert.equal(formatDays(90), "90 days");
});

test("notes make no anonymization or completeness claims", () => {
  const all = [...EXPORT_NOTES, ...DELETION_NOTES, ...IDENTITY_NOTES].join(" ").toLowerCase();
  // The honesty floor: these words must not appear as claims.
  for (const banned of ["anonymized", "anonymous", "gdpr compliant", "right to be forgotten"]) {
    assert.equal(all.includes(banned), false, `notes must not claim: ${banned}`);
  }
  // And the load-bearing caveats must be present.
  assert.match(all, /no per-person/);
  assert.match(all, /still personal data/);
  assert.match(all, /no server-side request record/);
});
