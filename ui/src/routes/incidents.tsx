import { useState, useEffect } from "preact/hooks";
import ExportButton from "../components/shared/ExportButton.js";
import { useFilters } from "../hooks/useFilters.js";

export const config = { mode: "app" };

interface Incident {
  incident_id: string;
  site_id: string;
  title: string;
  description: string;
  severity: string;
  source: string;
  rule_id: string;
  started_at: number;
  ended_at: number;
  created_by: string;
  updated_at: number;
}

const token = () =>
  typeof localStorage !== "undefined" ? localStorage.getItem("obs_token") || "" : "";

async function api<T>(path: string, init?: RequestInit): Promise<T> {
  const r = await fetch(path, {
    ...init,
    headers: {
      "Content-Type": "application/json",
      Authorization: "Bearer " + token(),
      ...(init?.headers || {}),
    },
  });
  if (!r.ok) throw new Error(await r.text());
  if (r.status === 204) return undefined as T;
  return r.json();
}

function formatDuration(start: number, end: number): string {
  const endMs = end || Date.now();
  const mins = Math.max(0, Math.round((endMs - start) / 60000));
  if (mins < 60) return `${mins}m`;
  const hrs = Math.floor(mins / 60);
  const rem = mins % 60;
  return `${hrs}h ${rem}m`;
}

export default function IncidentsPage() {
  const { state: { siteId } } = useFilters();
  const [active, setActive] = useState<Incident[]>([]);
  const [recent, setRecent] = useState<Incident[]>([]);
  const [loading, setLoading] = useState(true);
  const [showCreate, setShowCreate] = useState(false);
  const [form, setForm] = useState({ title: "", description: "", severity: "warning" });

  const load = async () => {
    setLoading(true);
    try {
      const now = Date.now();
      const from = now - 7 * 24 * 3600 * 1000;
      const enc = encodeURIComponent(siteId);
      const [activeList, rangeList] = await Promise.all([
        api<Incident[]>(`/api/v1/incidents?site_id=${enc}`),
        api<Incident[]>(`/api/v1/incidents?site_id=${enc}&from=${from}&to=${now}`),
      ]);
      setActive(activeList || []);
      setRecent((rangeList || []).filter(i => i.ended_at !== 0));
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => { load(); }, [siteId]);

  const create = async () => {
    try {
      await api("/api/v1/incidents", {
        method: "POST",
        body: JSON.stringify({
          site_id: siteId,
          title: form.title,
          description: form.description,
          severity: form.severity,
        }),
      });
      setShowCreate(false);
      setForm({ title: "", description: "", severity: "warning" });
      load();
    } catch (e: any) {
      alert("Create failed: " + e.message);
    }
  };

  const close = async (id: string) => {
    await api(`/api/v1/incidents/${id}/close`, { method: "POST" });
    load();
  };

  return (
    <div>
      <div class="obs-page-header" style={{ display: "flex", alignItems: "center", justifyContent: "space-between" }}>
        <h1 class="obs-page-title">Incidents</h1>
        <div style={{ display: "flex", gap: "8px" }}>
          <ExportButton
            filename={`incidents-${Date.now()}.csv`}
            rows={[...active, ...recent]}
            columns={[
              { key: "title", label: "title" },
              { key: "severity", label: "severity" },
              { key: "source", label: "source" },
              { key: "started_at", label: "started_at" },
              { key: "ended_at", label: "ended_at" },
              { key: "description", label: "description" },
            ]}
          />
          <button class="obs-btn obs-btn--primary" onClick={() => setShowCreate(true)}>
            Declare incident
          </button>
        </div>
      </div>

      {loading ? (
        <div class="obs-empty-state">Loading...</div>
      ) : (
        <>
          <section style={{ marginBottom: "24px" }}>
            <h2 style={{ fontSize: "14px", marginBottom: "8px" }}>Active</h2>
            {active.length === 0 && <div class="obs-empty-state">No active incidents.</div>}
            {active.map(inc => (
              <div key={inc.incident_id} style={{ border: "1px solid var(--obs-border)", borderLeft: `3px solid ${sevColor(inc.severity)}`, borderRadius: "6px", padding: "12px", marginBottom: "8px" }}>
                <div style={{ display: "flex", justifyContent: "space-between", alignItems: "start", gap: "12px" }}>
                  <div style={{ flex: 1 }}>
                    <strong>{inc.title}</strong>
                    <div style={{ fontSize: "12px", color: "var(--obs-text-muted)", marginTop: "2px" }}>
                      {new Date(inc.started_at).toLocaleString()} &middot; {inc.source} &middot; running {formatDuration(inc.started_at, 0)}
                    </div>
                    {inc.description && (
                      <div style={{ fontSize: "13px", marginTop: "6px" }}>{inc.description}</div>
                    )}
                  </div>
                  <button class="obs-btn" onClick={() => close(inc.incident_id)}>Close</button>
                </div>
              </div>
            ))}
          </section>

          <section>
            <h2 style={{ fontSize: "14px", marginBottom: "8px" }}>Past 7 days</h2>
            {recent.length === 0 && <div class="obs-empty-state">No incidents in the last 7 days.</div>}
            {recent.map(inc => (
              <div key={inc.incident_id} style={{ border: "1px solid var(--obs-border)", borderLeft: `3px solid ${sevColor(inc.severity)}`, borderRadius: "6px", padding: "12px", marginBottom: "8px", opacity: 0.85 }}>
                <strong>{inc.title}</strong>
                <div style={{ fontSize: "12px", color: "var(--obs-text-muted)" }}>
                  {new Date(inc.started_at).toLocaleString()} &middot; duration {formatDuration(inc.started_at, inc.ended_at)} &middot; {inc.source}
                </div>
              </div>
            ))}
          </section>
        </>
      )}

      {showCreate && (
        <div style={{ position: "fixed", inset: 0, background: "rgba(0,0,0,0.5)", display: "flex", alignItems: "center", justifyContent: "center", zIndex: 999 }}>
          <div style={{ background: "var(--obs-bg)", border: "1px solid var(--obs-border)", borderRadius: "8px", padding: "20px", minWidth: "420px" }}>
            <h2 style={{ marginTop: 0 }}>Declare incident</h2>
            <div style={{ display: "grid", gridTemplateColumns: "100px 1fr", gap: "10px", alignItems: "center" }}>
              <label>Title</label>
              <input class="obs-input" value={form.title} onInput={(e) => setForm({ ...form, title: (e.target as HTMLInputElement).value })} />
              <label>Severity</label>
              <select class="obs-input" value={form.severity} onChange={(e) => setForm({ ...form, severity: (e.target as HTMLSelectElement).value })}>
                <option value="info">Info</option>
                <option value="warning">Warning</option>
                <option value="critical">Critical</option>
              </select>
              <label>Description</label>
              <textarea class="obs-input" rows={3} value={form.description} onInput={(e) => setForm({ ...form, description: (e.target as HTMLTextAreaElement).value })} />
            </div>
            <div style={{ marginTop: "16px", display: "flex", gap: "10px", justifyContent: "flex-end" }}>
              <button class="obs-btn" onClick={() => setShowCreate(false)}>Cancel</button>
              <button class="obs-btn obs-btn--primary" onClick={create} disabled={!form.title}>Declare</button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}

function sevColor(s: string): string {
  switch (s) {
    case "critical": return "#e5484d";
    case "warning": return "#f5a524";
    default: return "#6ea8fe";
  }
}
