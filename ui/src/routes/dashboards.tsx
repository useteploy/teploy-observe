import { useState, useEffect, useCallback } from "preact/hooks";
import { get, post, del } from "../api/helpers.js";
import Modal from "../components/shared/Modal.js";
import ConfirmDialog from "../components/shared/ConfirmDialog.js";
import "../styles/dashboards.css";

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

  const fetchDetail = useCallback(async () => {
    setLoading(true);
    try {
      const data = await get<DashboardDetail>(`${BASE}/${dashboardId}?site_id=${siteId}`);
      setDetail(data);
      // Fetch panel values
      if (data?.panels?.length) {
        const now = new Date();
        const from = new Date(now.getTime() - 86400000).toISOString();
        const to = now.toISOString();
        const vals: Record<string, string> = {};
        for (const p of data.panels) {
          if (p.title) {
            try {
              const r = await post<{ value: string }>(`${BASE}/${dashboardId}/panels/${p.panel_id}/execute`, { site_id: siteId, from, to });
              vals[p.panel_id] = r?.value || "0";
            } catch { vals[p.panel_id] = "--"; }
          }
        }
        setPanelValues(vals);
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
        <div class="obs-empty-state">No panels yet. Add one to get started.</div>
      ) : (
        <div class="dashboard-panels">
          {detail.panels.filter(p => p.title).map(panel => {
            const w = parseInt(panel.width) || 6;
            return (
              <div key={panel.panel_id} class="dashboard-panel"
                style={{ gridColumn: `span ${Math.min(w, 12)}` }}>
                <div class="dashboard-panel-title">{panel.title}</div>
                <div class="dashboard-panel-value">
                  {panelValues[panel.panel_id] ?? "--"}
                </div>
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
