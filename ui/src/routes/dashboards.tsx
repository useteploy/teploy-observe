import { useState, useEffect, useCallback, useRef } from "preact/hooks";
import { get, post, del } from "../api/helpers.js";
import { analyticsApi } from "../api/analytics.js";
import type { TimeSeriesPoint } from "../api/analytics.js";
import Modal from "../components/shared/Modal.js";
import ConfirmDialog from "../components/shared/ConfirmDialog.js";
import EmptyState from "../components/shared/EmptyState.js";
import "../styles/dashboards.css";

function MiniSparkline({ data, color = "var(--obs-accent)" }: { data: number[]; color?: string }) {
  const ref = useRef<HTMLCanvasElement>(null);

  useEffect(() => {
    const canvas = ref.current;
    if (!canvas || data.length < 2) return;
    const ctx = canvas.getContext("2d");
    if (!ctx) return;
    const dpr = window.devicePixelRatio || 1;
    const rect = canvas.getBoundingClientRect();
    canvas.width = rect.width * dpr;
    canvas.height = rect.height * dpr;
    ctx.scale(dpr, dpr);
    const w = rect.width;
    const h = rect.height;
    const max = Math.max(...data, 1);
    const pad = 2;

    ctx.clearRect(0, 0, w, h);

    // Fill area
    ctx.beginPath();
    ctx.moveTo(pad, h - pad);
    data.forEach((v, i) => {
      const x = pad + (i / (data.length - 1)) * (w - pad * 2);
      const y = h - pad - (v / max) * (h - pad * 2);
      ctx.lineTo(x, y);
    });
    ctx.lineTo(w - pad, h - pad);
    ctx.closePath();
    ctx.fillStyle = typeof color === "string" && color.startsWith("var(")
      ? getComputedStyle(canvas).getPropertyValue(color.slice(4, -1)) + "18"
      : (color + "18");
    ctx.fill();

    // Line
    ctx.beginPath();
    data.forEach((v, i) => {
      const x = pad + (i / (data.length - 1)) * (w - pad * 2);
      const y = h - pad - (v / max) * (h - pad * 2);
      if (i === 0) ctx.moveTo(x, y); else ctx.lineTo(x, y);
    });
    ctx.strokeStyle = "#6366f1";
    ctx.lineWidth = 1.5;
    ctx.stroke();
  }, [data, color]);

  if (data.length < 2) return null;
  return <canvas ref={ref} style={{ width: "100%", height: "32px", display: "block", marginTop: "8px" }} />;
}

function PanelTimeSeries({ data, labels, label }: { data: number[]; labels: string[]; label: string }) {
  const ref = useRef<HTMLCanvasElement>(null);
  const [hover, setHover] = useState<{ i: number; x: number; y: number } | null>(null);

  useEffect(() => {
    const canvas = ref.current;
    if (!canvas || data.length < 2) return;
    const ctx = canvas.getContext("2d");
    if (!ctx) return;
    const dpr = window.devicePixelRatio || 1;
    const rect = canvas.getBoundingClientRect();
    canvas.width = rect.width * dpr;
    canvas.height = rect.height * dpr;
    ctx.scale(dpr, dpr);
    const w = rect.width;
    const h = rect.height;
    const padL = 40, padR = 12, padT = 8, padB = 24;
    const plotW = w - padL - padR;
    const plotH = h - padT - padB;
    const max = Math.max(...data, 1);

    ctx.clearRect(0, 0, w, h);

    // Gridlines (4 horizontal)
    ctx.strokeStyle = "rgba(128,128,128,0.15)";
    ctx.lineWidth = 1;
    ctx.font = "10px system-ui, sans-serif";
    ctx.fillStyle = "rgba(128,128,128,0.85)";
    ctx.textAlign = "right";
    ctx.textBaseline = "middle";
    for (let g = 0; g <= 4; g++) {
      const y = padT + (g / 4) * plotH;
      ctx.beginPath();
      ctx.moveTo(padL, y);
      ctx.lineTo(w - padR, y);
      ctx.stroke();
      const val = Math.round(max * (1 - g / 4));
      ctx.fillText(String(val), padL - 6, y);
    }

    // Area fill
    ctx.beginPath();
    ctx.moveTo(padL, padT + plotH);
    data.forEach((v, i) => {
      const x = padL + (data.length === 1 ? 0 : (i / (data.length - 1)) * plotW);
      const y = padT + plotH - (v / max) * plotH;
      ctx.lineTo(x, y);
    });
    ctx.lineTo(padL + plotW, padT + plotH);
    ctx.closePath();
    const cs = getComputedStyle(canvas);
    const accent = cs.getPropertyValue("--obs-accent").trim() || "#6366f1";
    ctx.fillStyle = accent + "22";
    ctx.fill();

    // Line
    ctx.beginPath();
    data.forEach((v, i) => {
      const x = padL + (data.length === 1 ? 0 : (i / (data.length - 1)) * plotW);
      const y = padT + plotH - (v / max) * plotH;
      if (i === 0) ctx.moveTo(x, y); else ctx.lineTo(x, y);
    });
    ctx.strokeStyle = accent;
    ctx.lineWidth = 2;
    ctx.stroke();

    // x-axis labels — first, middle, last
    if (labels.length >= 2) {
      ctx.fillStyle = "rgba(128,128,128,0.85)";
      ctx.textAlign = "left";
      ctx.textBaseline = "top";
      ctx.fillText(labels[0], padL, h - padB + 6);
      ctx.textAlign = "right";
      ctx.fillText(labels[labels.length - 1], w - padR, h - padB + 6);
      if (labels.length >= 3) {
        ctx.textAlign = "center";
        ctx.fillText(labels[Math.floor(labels.length / 2)], padL + plotW / 2, h - padB + 6);
      }
    }

    // Hover marker
    if (hover !== null) {
      const i = hover.i;
      const x = padL + (data.length === 1 ? 0 : (i / (data.length - 1)) * plotW);
      const y = padT + plotH - (data[i] / max) * plotH;
      ctx.beginPath();
      ctx.arc(x, y, 4, 0, 2 * Math.PI);
      ctx.fillStyle = accent;
      ctx.fill();
      ctx.strokeStyle = "#fff";
      ctx.lineWidth = 2;
      ctx.stroke();
    }
  }, [data, labels, hover]);

  const onMove = (e: MouseEvent) => {
    const canvas = ref.current;
    if (!canvas || data.length < 2) return;
    const rect = canvas.getBoundingClientRect();
    const x = e.clientX - rect.left;
    const padL = 40, padR = 12;
    const plotW = rect.width - padL - padR;
    const rel = Math.max(0, Math.min(1, (x - padL) / plotW));
    const i = Math.round(rel * (data.length - 1));
    setHover({ i, x: e.clientX - rect.left, y: e.clientY - rect.top });
  };

  if (data.length < 2) {
    return <div class="dashboard-panel-empty">Not enough data yet.</div>;
  }

  const tip = hover !== null ? { label: labels[hover.i], value: data[hover.i] } : null;

  return (
    <div class="dashboard-panel-chart" onMouseLeave={() => setHover(null)}>
      <canvas
        ref={ref}
        onMouseMove={onMove}
        style={{ width: "100%", height: "180px", display: "block" }}
        aria-label={label + " over time"}
      />
      {tip && (
        <div class="dashboard-panel-tooltip" style={{ left: `${hover!.x}px`, top: `${hover!.y - 40}px` }}>
          <div class="dashboard-panel-tooltip-label">{tip.label}</div>
          <div class="dashboard-panel-tooltip-value">{tip.value.toLocaleString()}</div>
        </div>
      )}
    </div>
  );
}

export const config = { mode: "app" };

const BASE = "/api/v1/dashboards";

interface Dashboard {
  dashboard_id: string; site_id: string; name: string;
  description: string; created_by: string; created_at: string;
}

interface Panel {
  panel_id: string; dashboard_id: string; panel_type: string;
  title: string; query_type: string; query_config: string;
  position_x: string; position_y: string; width: string; height: string;
}

interface DashboardDetail {
  dashboard: Dashboard; panels: Panel[];
}

const PANEL_TYPES = [
  { value: "metric", label: "Metric (single number)" },
  { value: "timeseries", label: "Time Series" },
  { value: "table", label: "Table" },
  { value: "bar", label: "Bar Chart" },
];

const QUERY_TYPES = [
  { value: "pageviews", label: "Pageviews" },
  { value: "visitors", label: "Visitors" },
  { value: "errors", label: "Error Count" },
];

// ─── Dashboard Detail ───

function DashboardView({ dashboardId, siteId, onBack }: { dashboardId: string; siteId: string; onBack: () => void }) {
  const [detail, setDetail] = useState<DashboardDetail | null>(null);
  const [loading, setLoading] = useState(true);
  const [showAddPanel, setShowAddPanel] = useState(false);
  const [adding, setAdding] = useState(false);
  const [panelTitle, setPanelTitle] = useState("");
  const [panelType, setPanelType] = useState("metric");
  const [queryType, setQueryType] = useState("pageviews");
  const [panelValues, setPanelValues] = useState<Record<string, string>>({});
  const [panelSparklines, setPanelSparklines] = useState<Record<string, number[]>>({});
  const [chartLabels, setChartLabels] = useState<string[]>([]);

  const fetchDetail = useCallback(async () => {
    setLoading(true);
    try {
      const data = await get<DashboardDetail>(`${BASE}/${dashboardId}?site_id=${siteId}`);
      setDetail(data);
      if (data?.panels?.length) {
        const now = new Date();
        const from = new Date(now.getTime() - 7 * 86400000).toISOString();
        const to = now.toISOString();

        // Fetch panel values + sparkline data in parallel
        const valPromises = data.panels.filter(p => p.title).map(async (p) => {
          try {
            const r = await post<{ value: string }>(`${BASE}/${dashboardId}/panels/${p.panel_id}/execute`, { site_id: siteId, from, to });
            return { id: p.panel_id, value: r?.value || "0" };
          } catch { return { id: p.panel_id, value: "--" }; }
        });

        // Fetch timeseries for sparklines
        let tsData: TimeSeriesPoint[] = [];
        try {
          tsData = await analyticsApi.timeseries(siteId, from, to, "day") || [];
        } catch {}

        const results = await Promise.all(valPromises);
        const vals: Record<string, string> = {};
        const sparks: Record<string, number[]> = {};
        for (const r of results) { vals[r.id] = r.value; }

        // Map sparkline data to each panel based on query type
        for (const p of data.panels) {
          if (!p.title) continue;
          if (p.query_type === "pageviews") {
            sparks[p.panel_id] = tsData.map(d => d.pageviews);
          } else if (p.query_type === "visitors") {
            sparks[p.panel_id] = tsData.map(d => d.visitors);
          }
        }

        const labels = tsData.map(d =>
          new Date(d.bucket).toLocaleDateString("en-US", { month: "short", day: "numeric" })
        );

        setPanelValues(vals);
        setPanelSparklines(sparks);
        setChartLabels(labels);
      }
    } catch { setDetail(null); }
    finally { setLoading(false); }
  }, [dashboardId, siteId]);

  useEffect(() => { fetchDetail(); }, [fetchDetail]);

  const handleAddPanel = async () => {
    if (!panelTitle.trim()) return;
    setAdding(true);
    try {
      await post<Panel>(`${BASE}/${dashboardId}/panels`, {
        panel_type: panelType,
        title: panelTitle.trim(),
        query_type: queryType,
        width: "6",
        height: "4",
      });
      setShowAddPanel(false);
      setPanelTitle("");
      fetchDetail();
    } catch (err) { console.error("Failed to add panel:", err); }
    finally { setAdding(false); }
  };

  if (loading) return <div class="obs-empty-state">Loading dashboard...</div>;
  if (!detail) return <div class="obs-empty-state">Dashboard not found</div>;

  return (
    <div>
      <button class="errors-back-btn" onClick={onBack} style={{ marginBottom: "16px" }}>
        <svg width="14" height="14" viewBox="0 0 24 24" fill="currentColor">
          <path d="M20 11H7.83l5.59-5.59L12 4l-8 8 8 8 1.41-1.41L7.83 13H20v-2z" />
        </svg>
        Back to dashboards
      </button>

      <div class="dashboard-toolbar">
        <div>
          <h2 style={{ fontSize: "16px", fontWeight: 600, color: "var(--obs-text)", margin: 0 }}>
            {detail.dashboard.name}
          </h2>
          {detail.dashboard.description && (
            <p style={{ fontSize: "12px", color: "var(--obs-text-secondary)", margin: "4px 0 0" }}>
              {detail.dashboard.description}
            </p>
          )}
        </div>
        <button class="obs-btn obs-btn--primary obs-btn--sm" onClick={() => setShowAddPanel(true)}>
          Add Panel
        </button>
      </div>

      {!detail.panels?.length ? (
        <EmptyState
          title="No panels yet"
          description="Panels show a single number, a time-series chart, or a custom query. Drop one in and it updates live as events arrive."
          icon="layers"
          actions={[
            { label: "Add first panel", onClick: () => setShowAddPanel(true), primary: true },
          ]}
        />
      ) : (
        <div class="dashboard-panels">
          {detail.panels.filter(p => p.title).map((panel, idx, all) => {
            const w = parseInt(panel.width) || 6;
            const series = panelSparklines[panel.panel_id];
            const isTimeSeries = panel.panel_type === "timeseries";
            const updateLayout = async (patch: { width?: string; position_y?: string }) => {
              await post(`${BASE}/${dashboardId}/panels/${panel.panel_id}/layout`, patch);
              fetchDetail();
            };
            const setWidth = (next: number) => updateLayout({ width: String(next) });
            const move = (dir: -1 | 1) => {
              const otherIdx = idx + dir;
              if (otherIdx < 0 || otherIdx >= all.length) return;
              const other = all[otherIdx];
              const myY = parseInt(panel.position_y) || idx;
              const otherY = parseInt(other.position_y) || otherIdx;
              // swap position_y values
              Promise.all([
                post(`${BASE}/${dashboardId}/panels/${panel.panel_id}/layout`, { position_y: String(otherY) }),
                post(`${BASE}/${dashboardId}/panels/${other.panel_id}/layout`, { position_y: String(myY) }),
              ]).then(fetchDetail);
            };
            return (
              <div key={panel.panel_id} class={`dashboard-panel ${isTimeSeries ? "dashboard-panel--chart" : ""}`}
                style={{ gridColumn: `span ${Math.min(w, 12)}` }}>
                <div class="dashboard-panel-head">
                  <div class="dashboard-panel-title">{panel.title}</div>
                  <div class="dashboard-panel-controls">
                    <button class="dashboard-panel-ctrl" aria-label="Move up" disabled={idx === 0} onClick={() => move(-1)}>↑</button>
                    <button class="dashboard-panel-ctrl" aria-label="Move down" disabled={idx === all.length - 1} onClick={() => move(1)}>↓</button>
                    <span class="dashboard-panel-ctrl-sep" />
                    {[4, 6, 8, 12].map((n) => (
                      <button key={n}
                        class={`dashboard-panel-ctrl ${w === n ? "dashboard-panel-ctrl--active" : ""}`}
                        aria-label={`Set width ${n}/12`}
                        onClick={() => setWidth(n)}
                      >{n}</button>
                    ))}
                  </div>
                </div>
                {isTimeSeries ? (
                  series && series.length > 1
                    ? <PanelTimeSeries data={series} labels={chartLabels} label={panel.query_type} />
                    : <div class="dashboard-panel-empty">Not enough data yet.</div>
                ) : (
                  <>
                    <div class="dashboard-panel-value">
                      {panelValues[panel.panel_id] ?? "--"}
                    </div>
                    {series?.length > 1 && <MiniSparkline data={series} />}
                  </>
                )}
                <div style={{ fontSize: "11px", color: "var(--obs-text-muted)", marginTop: "4px" }}>
                  {panel.query_type} / {panel.panel_type}
                </div>
              </div>
            );
          })}
        </div>
      )}

      <Modal open={showAddPanel} onClose={() => setShowAddPanel(false)} title="Add Panel">
        <div class="add-panel-form">
          <div class="obs-form-group">
            <label class="obs-label">Title</label>
            <input class="obs-input" placeholder="Total Pageviews" value={panelTitle}
              onInput={(e) => setPanelTitle((e.target as HTMLInputElement).value)} />
          </div>
          <div class="obs-form-group">
            <label class="obs-label">Panel Type</label>
            <select class="obs-select" value={panelType}
              onChange={(e) => setPanelType((e.target as HTMLSelectElement).value)}>
              {PANEL_TYPES.map(t => <option key={t.value} value={t.value}>{t.label}</option>)}
            </select>
          </div>
          <div class="obs-form-group">
            <label class="obs-label">Metric</label>
            <select class="obs-select" value={queryType}
              onChange={(e) => setQueryType((e.target as HTMLSelectElement).value)}>
              {QUERY_TYPES.map(t => <option key={t.value} value={t.value}>{t.label}</option>)}
            </select>
          </div>
          <div style={{ display: "flex", justifyContent: "flex-end", gap: "8px" }}>
            <button class="obs-btn" onClick={() => setShowAddPanel(false)}>Cancel</button>
            <button class="obs-btn obs-btn--primary" onClick={handleAddPanel}
              disabled={adding || !panelTitle.trim()}>
              {adding ? "Adding..." : "Add Panel"}
            </button>
          </div>
        </div>
      </Modal>
    </div>
  );
}

// ─── Main Page ───

export default function DashboardsPage() {
  const siteId = typeof window !== "undefined"
    ? new URLSearchParams(window.location.search).get("site_id") || "default"
    : "default";

  const [dashboards, setDashboards] = useState<Dashboard[]>([]);
  const [loading, setLoading] = useState(true);
  const [showCreate, setShowCreate] = useState(false);
  const [creating, setCreating] = useState(false);
  const [formName, setFormName] = useState("");
  const [formDesc, setFormDesc] = useState("");
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [deletingId, setDeletingId] = useState<string | null>(null);
  const [deleteLoading, setDeleteLoading] = useState(false);

  const fetchDashboards = useCallback(async () => {
    setLoading(true);
    try {
      const data = await get<Dashboard[]>(`${BASE}?site_id=${siteId}`);
      setDashboards((data || []).filter(d => d.name));
    } catch { setDashboards([]); }
    finally { setLoading(false); }
  }, [siteId]);

  useEffect(() => { fetchDashboards(); }, [fetchDashboards]);

  const handleCreate = async () => {
    if (!formName.trim()) return;
    setCreating(true);
    try {
      await post<Dashboard>(BASE, { site_id: siteId, name: formName.trim(), description: formDesc.trim() });
      setShowCreate(false);
      setFormName(""); setFormDesc("");
      fetchDashboards();
    } catch (err) { console.error("Failed to create dashboard:", err); }
    finally { setCreating(false); }
  };

  const handleDelete = async () => {
    if (!deletingId) return;
    setDeleteLoading(true);
    try {
      await del(`${BASE}/${deletingId}`);
      setDashboards(prev => prev.filter(d => d.dashboard_id !== deletingId));
      setDeletingId(null);
    } catch (err) { console.error("Failed to delete dashboard:", err); }
    finally { setDeleteLoading(false); }
  };

  if (selectedId) {
    return <DashboardView dashboardId={selectedId} siteId={siteId} onBack={() => { setSelectedId(null); fetchDashboards(); }} />;
  }

  return (
    <div>
      <div class="obs-page-header">
        <h1 class="obs-page-title">Dashboards</h1>
        <div class="obs-page-actions">
          <button class="obs-btn obs-btn--primary" onClick={() => setShowCreate(true)}>New Dashboard</button>
        </div>
      </div>

      {loading ? (
        <div class="obs-empty-state">Loading...</div>
      ) : dashboards.length === 0 ? (
        <div class="obs-empty-state">No custom dashboards. Create one to build your own views.</div>
      ) : (
        <div class="dashboards-grid">
          {dashboards.map(d => (
            <div key={d.dashboard_id} class="dashboard-card" onClick={() => setSelectedId(d.dashboard_id)}>
              <div class="dashboard-card-name">{d.name}</div>
              {d.description && <div class="dashboard-card-desc">{d.description}</div>}
              <div class="dashboard-card-meta">
                <span>{d.created_by || "admin"}</span>
                <button class="obs-btn obs-btn--sm obs-btn--danger" style={{ marginLeft: "auto" }}
                  onClick={(e) => { e.stopPropagation(); setDeletingId(d.dashboard_id); }}>
                  Delete
                </button>
              </div>
            </div>
          ))}
        </div>
      )}

      <Modal open={showCreate} onClose={() => setShowCreate(false)} title="New Dashboard">
        <div class="obs-form-group">
          <label class="obs-label">Name</label>
          <input class="obs-input" placeholder="My Dashboard" value={formName}
            onInput={(e) => setFormName((e.target as HTMLInputElement).value)} />
        </div>
        <div class="obs-form-group">
          <label class="obs-label">Description (optional)</label>
          <input class="obs-input" placeholder="Overview of key metrics" value={formDesc}
            onInput={(e) => setFormDesc((e.target as HTMLInputElement).value)} />
        </div>
        <div style={{ display: "flex", justifyContent: "flex-end", gap: "8px", marginTop: "8px" }}>
          <button class="obs-btn" onClick={() => setShowCreate(false)}>Cancel</button>
          <button class="obs-btn obs-btn--primary" onClick={handleCreate}
            disabled={creating || !formName.trim()}>
            {creating ? "Creating..." : "Create"}
          </button>
        </div>
      </Modal>

      <ConfirmDialog
        open={!!deletingId}
        onClose={() => setDeletingId(null)}
        onConfirm={handleDelete}
        title="Delete Dashboard"
        message="This dashboard and all its panels will be permanently deleted."
        loading={deleteLoading}
      />
    </div>
  );
}
