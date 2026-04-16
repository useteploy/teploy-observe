import { useState, useEffect, useCallback } from "preact/hooks";
import { get, post, del } from "../api/helpers.js";
import StatusBadge from "../components/shared/StatusBadge.js";
import Modal from "../components/shared/Modal.js";
import ConfirmDialog from "../components/shared/ConfirmDialog.js";
import "../styles/settings.css";

export const config = { mode: "app" };

const BASE = "/api/v1/reports";

interface ReportSchedule {
  schedule_id: string; site_id: string; name: string;
  frequency: string; recipients: string; enabled: string;
  last_sent: string; created_at: string;
}

export default function ReportsPage() {
  const siteId = typeof window !== "undefined"
    ? new URLSearchParams(window.location.search).get("site_id") || "default"
    : "default";

  const [reports, setReports] = useState<ReportSchedule[]>([]);
  const [loading, setLoading] = useState(true);
  const [showCreate, setShowCreate] = useState(false);
  const [creating, setCreating] = useState(false);
  const [deletingId, setDeletingId] = useState<string | null>(null);
  const [deleteLoading, setDeleteLoading] = useState(false);

  const [formName, setFormName] = useState("");
  const [formFreq, setFormFreq] = useState("weekly");
  const [formRecipients, setFormRecipients] = useState("");

  const fetch = useCallback(async () => {
    setLoading(true);
    try {
      const data = await get<ReportSchedule[]>(`${BASE}?site_id=${siteId}`);
      setReports(data || []);
    } catch { setReports([]); }
    finally { setLoading(false); }
  }, [siteId]);

  useEffect(() => { fetch(); }, [fetch]);

  const handleCreate = async () => {
    if (!formName.trim() || !formRecipients.trim()) return;
    setCreating(true);
    try {
      await post<ReportSchedule>(BASE, {
        site_id: siteId, name: formName.trim(),
        frequency: formFreq, recipients: formRecipients.trim(),
      });
      setShowCreate(false);
      setFormName(""); setFormRecipients("");
      fetch();
    } catch (err) { console.error("Failed to create report:", err); }
    finally { setCreating(false); }
  };

  const handleDelete = async () => {
    if (!deletingId) return;
    setDeleteLoading(true);
    try {
      await del(`${BASE}/${deletingId}`);
      setReports(prev => prev.filter(r => r.schedule_id !== deletingId));
      setDeletingId(null);
    } catch (err) { console.error("Failed to delete:", err); }
    finally { setDeleteLoading(false); }
  };

  const formatLastSent = (ts: string): string => {
    if (!ts || ts === "0") return "Never";
    try {
      const ms = parseInt(ts);
      if (ms > 0) return new Date(ms).toLocaleDateString("en-US", { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit", hour12: false });
      return "Never";
    } catch { return ts; }
  };

  return (
    <div>
      <div class="obs-page-header">
        <h1 class="obs-page-title">Reports</h1>
        <div class="obs-page-actions">
          <button class="obs-btn obs-btn--primary" onClick={() => setShowCreate(true)}>New Report</button>
        </div>
      </div>

      {loading ? (
        <div class="settings-loading">
          {Array.from({ length: 3 }).map((_, i) => (
            <div class="settings-skeleton-row" key={i}>
              <div class="settings-skeleton-bar" style={{ width: "120px" }} />
              <div class="settings-skeleton-bar" style={{ flex: 1 }} />
            </div>
          ))}
        </div>
      ) : reports.length === 0 ? (
        <div class="obs-empty-state">No scheduled reports. Create one to receive analytics summaries by email.</div>
      ) : (
        <div class="settings-list">
          {reports.map(r => (
            <div key={r.schedule_id} class="settings-row">
              <StatusBadge status={r.enabled === "true" ? "enabled" : "disabled"} size="sm" />
              <span class="settings-row-name">{r.name}</span>
              <span style={{ fontSize: "11px", padding: "2px 8px", borderRadius: "var(--obs-radius-full)", background: "var(--obs-surface-hover)", color: "var(--obs-text-secondary)", textTransform: "capitalize" }}>
                {r.frequency}
              </span>
              <span class="settings-row-value">{r.recipients}</span>
              <span class="settings-row-date">Last: {formatLastSent(r.last_sent)}</span>
              <button class="obs-btn obs-btn--sm obs-btn--danger" onClick={() => setDeletingId(r.schedule_id)}>Delete</button>
            </div>
          ))}
        </div>
      )}

      <Modal open={showCreate} onClose={() => setShowCreate(false)} title="New Scheduled Report">
        <div class="obs-form-group">
          <label class="obs-label">Name</label>
          <input class="obs-input" placeholder="Weekly Summary" value={formName}
            onInput={(e) => setFormName((e.target as HTMLInputElement).value)} />
        </div>
        <div class="obs-form-group">
          <label class="obs-label">Frequency</label>
          <select class="obs-select" value={formFreq}
            onChange={(e) => setFormFreq((e.target as HTMLSelectElement).value)}>
            <option value="daily">Daily</option>
            <option value="weekly">Weekly</option>
          </select>
        </div>
        <div class="obs-form-group">
          <label class="obs-label">Recipients (comma-separated emails)</label>
          <input class="obs-input" placeholder="team@example.com, eng@example.com" value={formRecipients}
            onInput={(e) => setFormRecipients((e.target as HTMLInputElement).value)} />
        </div>
        <div style={{ display: "flex", justifyContent: "flex-end", gap: "8px", marginTop: "8px" }}>
          <button class="obs-btn" onClick={() => setShowCreate(false)}>Cancel</button>
          <button class="obs-btn obs-btn--primary" onClick={handleCreate}
            disabled={creating || !formName.trim() || !formRecipients.trim()}>
            {creating ? "Creating..." : "Create"}
          </button>
        </div>
      </Modal>

      <ConfirmDialog open={!!deletingId} onClose={() => setDeletingId(null)} onConfirm={handleDelete}
        title="Delete Report" message="This report schedule will be permanently removed." loading={deleteLoading} />
    </div>
  );
}
