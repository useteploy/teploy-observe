import { useState, useEffect, useCallback } from "preact/hooks";
import { auditApi } from "../api/audit.js";
import type { AuditEvent } from "../api/audit.js";

export const config = { mode: "app" };

const RESULTS = ["", "success", "failure", "denied"] as const;

function fmtTime(ms: number): string {
  if (!ms) return "";
  try {
    return new Date(ms).toLocaleString();
  } catch {
    return String(ms);
  }
}

function resultColor(r: string): string {
  if (r === "failure" || r === "denied") return "#f85149";
  return "#3fb950";
}

export default function Audit() {
  const [events, setEvents] = useState<AuditEvent[]>([]);
  const [loading, setLoading] = useState(false);
  const [err, setErr] = useState("");
  const [actor, setActor] = useState("");
  const [action, setAction] = useState("");
  const [result, setResult] = useState("");

  const load = useCallback(() => {
    setLoading(true);
    setErr("");
    auditApi
      .list({ actor: actor || undefined, action: action || undefined, result: result || undefined, limit: 500 })
      .then((rows) => setEvents(rows || []))
      .catch((e) => setErr(e?.status === 403 ? "Audit log is admin-only." : "Failed to load audit log."))
      .finally(() => setLoading(false));
  }, [actor, action, result]);

  useEffect(() => {
    load();
  }, []);

  return (
    <div style={{ padding: "24px", maxWidth: "1100px", margin: "0 auto" }}>
      <h1 style={{ fontSize: "20px", marginBottom: "4px" }}>Audit log</h1>
      <p style={{ color: "var(--muted, #8b93a1)", marginTop: 0, fontSize: "13px" }}>
        Immutable record of who did what, when, and from where. Admin-only.
      </p>

      <form
        onSubmit={(e) => {
          e.preventDefault();
          load();
        }}
        style={{ display: "flex", gap: "8px", flexWrap: "wrap", margin: "16px 0" }}
      >
        <input placeholder="actor" value={actor} onInput={(e) => setActor((e.target as HTMLInputElement).value)} />
        <input placeholder="action (e.g. auth.login)" value={action} onInput={(e) => setAction((e.target as HTMLInputElement).value)} />
        <select value={result} onChange={(e) => setResult((e.target as HTMLSelectElement).value)}>
          {RESULTS.map((r) => (
            <option value={r}>{r || "any result"}</option>
          ))}
        </select>
        <button type="submit">Filter</button>
      </form>

      {err && <div style={{ color: "#f85149", marginBottom: "12px" }}>{err}</div>}
      {loading && <div style={{ color: "var(--muted, #8b93a1)" }}>Loading…</div>}

      {!loading && !err && events.length === 0 && (
        <div style={{ color: "var(--muted, #8b93a1)", padding: "24px 0" }}>No audit events match.</div>
      )}

      {events.length > 0 && (
        <div style={{ overflowX: "auto" }}>
          <table style={{ width: "100%", borderCollapse: "collapse", fontSize: "13px" }}>
            <thead>
              <tr style={{ textAlign: "left", borderBottom: "1px solid var(--line, #262b33)" }}>
                <th style={{ padding: "8px" }}>Time</th>
                <th style={{ padding: "8px" }}>Actor</th>
                <th style={{ padding: "8px" }}>Action</th>
                <th style={{ padding: "8px" }}>Target</th>
                <th style={{ padding: "8px" }}>Result</th>
                <th style={{ padding: "8px" }}>Source IP</th>
              </tr>
            </thead>
            <tbody>
              {events.map((ev) => (
                <tr style={{ borderBottom: "1px solid var(--line, #262b33)" }}>
                  <td style={{ padding: "8px", whiteSpace: "nowrap" }}>{fmtTime(ev.timestamp)}</td>
                  <td style={{ padding: "8px" }}>
                    {ev.actor || <span style={{ color: "var(--muted, #8b93a1)" }}>system</span>}
                    {ev.actor_type && ev.actor_type !== "user" ? ` (${ev.actor_type})` : ""}
                  </td>
                  <td style={{ padding: "8px", fontFamily: "monospace" }}>{ev.action}</td>
                  <td style={{ padding: "8px" }}>{ev.target}</td>
                  <td style={{ padding: "8px", color: resultColor(ev.result) }}>{ev.result}</td>
                  <td style={{ padding: "8px", fontFamily: "monospace" }}>{ev.source_ip}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
