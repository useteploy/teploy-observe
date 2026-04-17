import { useEffect, useState } from "preact/hooks";
import { get } from "../api/helpers.js";
import "../styles/meta.css";

export const config = { mode: "app" };

interface IngestRate {
  events_last_1m: number;
  events_last_1h: number;
  errors_last_1m: number;
  logs_last_1m: number;
  spans_last_1m: number;
}
interface TableSize { table: string; rows: number }
interface Policy { table: string; days: number }
interface Snapshot {
  generated_at: string;
  version: string;
  uptime: string;
  ingest: IngestRate;
  tables: TableSize[];
  retention: Policy[];
}

function fmt(n: number): string {
  return n.toLocaleString();
}

export default function MetaPage() {
  const [data, setData] = useState<Snapshot | null>(null);
  const [err, setErr] = useState<string | null>(null);

  useEffect(() => {
    let alive = true;
    const load = () => {
      get<Snapshot>("/api/v1/meta")
        .then((r) => { if (alive) { setData(r); setErr(null); } })
        .catch((e) => { if (alive) setErr(e?.message || "failed to load"); });
    };
    load();
    const t = setInterval(load, 5_000); // self-refresh every 5s
    return () => { alive = false; clearInterval(t); };
  }, []);

  if (err) {
    return <div class="obs-empty-state">Failed to load meta: {err}</div>;
  }
  if (!data) {
    return <div class="obs-empty-state">Loading…</div>;
  }

  const policies = new Map(data.retention.map((p) => [p.table, p.days]));

  return (
    <div>
      <div class="obs-page-header">
        <h1 class="obs-page-title">Observe internals</h1>
        <span class="meta-version">v{data.version} · uptime {data.uptime}</span>
      </div>

      <div class="meta-metrics">
        <div class="meta-stat">
          <div class="meta-stat-label">Events / min</div>
          <div class="meta-stat-value">{fmt(data.ingest.events_last_1m)}</div>
        </div>
        <div class="meta-stat">
          <div class="meta-stat-label">Events / hour</div>
          <div class="meta-stat-value">{fmt(data.ingest.events_last_1h)}</div>
        </div>
        <div class="meta-stat">
          <div class="meta-stat-label">Errors / min</div>
          <div class="meta-stat-value">{fmt(data.ingest.errors_last_1m)}</div>
        </div>
        <div class="meta-stat">
          <div class="meta-stat-label">Logs / min</div>
          <div class="meta-stat-value">{fmt(data.ingest.logs_last_1m)}</div>
        </div>
        <div class="meta-stat">
          <div class="meta-stat-label">Spans / min</div>
          <div class="meta-stat-value">{fmt(data.ingest.spans_last_1m)}</div>
        </div>
      </div>

      <div class="meta-section">
        <h2 class="meta-section-title">Table sizes</h2>
        <table class="meta-table">
          <thead>
            <tr>
              <th>Table</th>
              <th style={{ textAlign: "right" }}>Rows</th>
              <th style={{ textAlign: "right" }}>Retention</th>
            </tr>
          </thead>
          <tbody>
            {data.tables.map((t) => (
              <tr key={t.table}>
                <td><code>{t.table}</code></td>
                <td style={{ textAlign: "right", fontVariantNumeric: "tabular-nums" }}>{fmt(t.rows)}</td>
                <td style={{ textAlign: "right", color: "var(--obs-text-muted)" }}>
                  {policies.has(t.table) ? `${policies.get(t.table)}d` : "—"}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      <div class="meta-section">
        <h2 class="meta-section-title">Retention policies</h2>
        <div class="meta-policies">
          {data.retention.map((p) => (
            <span key={p.table} class="meta-policy-chip">
              <code>{p.table}</code>
              <span>{p.days}d</span>
            </span>
          ))}
        </div>
      </div>

      <div class="meta-footer">
        Snapshot: {new Date(data.generated_at).toLocaleTimeString()} · auto-refresh every 5s
      </div>
    </div>
  );
}
