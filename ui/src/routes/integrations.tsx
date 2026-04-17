import { useState, useEffect, useCallback } from "preact/hooks";
import { get, post, del } from "../api/helpers.js";
import StatusBadge from "../components/shared/StatusBadge.js";
import Modal from "../components/shared/Modal.js";
import ConfirmDialog from "../components/shared/ConfirmDialog.js";
import EmptyState from "../components/shared/EmptyState.js";
import "../styles/settings.css";

export const config = { mode: "app" };

const BASE = "/api/v1/integrations";

interface Integration {
  integration_id: string; site_id: string; name: string;
  type: string; config: string; enabled: string; created_at: string;
}

const TYPES = [
  { value: "slack", label: "Slack" },
  { value: "email", label: "Email" },
  { value: "jira", label: "Jira" },
  { value: "github", label: "GitHub" },
  { value: "pagerduty", label: "PagerDuty" },
];

export default function IntegrationsPage() {
  const siteId = typeof window !== "undefined"
    ? new URLSearchParams(window.location.search).get("site_id") || "default"
    : "default";

  const [items, setItems] = useState<Integration[]>([]);
  const [loading, setLoading] = useState(true);
  const [showCreate, setShowCreate] = useState(false);
  const [creating, setCreating] = useState(false);
  const [deletingId, setDeletingId] = useState<string | null>(null);
  const [deleteLoading, setDeleteLoading] = useState(false);
  const [testingId, setTestingId] = useState<string | null>(null);
  const [testResult, setTestResult] = useState<Record<string, { ok: boolean; message: string }>>({});

  const [formName, setFormName] = useState("");
  const [formType, setFormType] = useState("slack");
  const [formConfig, setFormConfig] = useState("");

  const fetch = useCallback(async () => {
    setLoading(true);
    try {
      const data = await get<Integration[]>(`${BASE}?site_id=${siteId}`);
      setItems(data || []);
    } catch { setItems([]); }
    finally { setLoading(false); }
  }, [siteId]);

  useEffect(() => { fetch(); }, [fetch]);

  const handleCreate = async () => {
    if (!formName.trim()) return;
    setCreating(true);
    try {
      await post<Integration>(BASE, { site_id: siteId, name: formName.trim(), type: formType, config: formConfig.trim() || "{}" });
      setShowCreate(false);
      setFormName(""); setFormConfig("");
      fetch();
    } catch (err) { console.error("Failed to create integration:", err); }
    finally { setCreating(false); }
  };

  const handleDelete = async () => {
    if (!deletingId) return;
    setDeleteLoading(true);
    try {
      await del(`${BASE}/${deletingId}`);
      setItems(prev => prev.filter(i => i.integration_id !== deletingId));
      setDeletingId(null);
    } catch (err) { console.error("Failed to delete:", err); }
    finally { setDeleteLoading(false); }
  };

  const configHint = (type: string): string => {
    switch (type) {
      case "slack": return '{"webhook_url":"https://hooks.slack.com/..."}';
      case "email": return '{"smtp_host":"smtp.gmail.com","from":"alerts@example.com","to":"team@example.com"}';
      case "jira": return '{"base_url":"https://company.atlassian.net","project":"OBS","email":"...","api_token":"..."}';
      case "github": return '{"token":"ghp_...","owner":"org","repo":"repo"}';
      case "pagerduty": return '{"routing_key":"..."}';
      default: return "{}";
    }
  };

  return (
    <div>
      <div class="obs-page-header">
        <h1 class="obs-page-title">Integrations</h1>
        <div class="obs-page-actions">
          <button class="obs-btn obs-btn--primary" onClick={() => setShowCreate(true)}>Add Integration</button>
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
      ) : items.length === 0 ? (
        <EmptyState
          title="No integrations yet"
          description="Connect Slack, Jira, PagerDuty, GitHub, or email to receive alerts. Each integration gets a Send test button once configured."
          icon="package"
          actions={[
            { label: "Add integration", onClick: () => setShowCreate(true), primary: true },
          ]}
        />
      ) : (
        <div class="settings-list">
          {items.map(item => (
            <div key={item.integration_id} class="settings-row">
              <StatusBadge status={item.enabled === "true" ? "enabled" : "disabled"} size="sm" />
              <span class="settings-row-name">{item.name}</span>
              <span style={{ fontSize: "11px", padding: "2px 8px", borderRadius: "var(--obs-radius-full)", background: "var(--obs-surface-hover)", color: "var(--obs-text-secondary)" }}>
                {TYPES.find(t => t.value === item.type)?.label || item.type}
              </span>
              <button
                class="obs-btn obs-btn--sm"
                disabled={testingId === item.integration_id}
                onClick={async () => {
                  setTestingId(item.integration_id);
                  try {
                    const r = await post<{ ok: boolean; message: string }>(`${BASE}/${item.integration_id}/test`, {});
                    setTestResult((prev) => ({ ...prev, [item.integration_id]: r }));
                  } catch (err: any) {
                    setTestResult((prev) => ({ ...prev, [item.integration_id]: { ok: false, message: err?.message || "test failed" } }));
                  } finally {
                    setTestingId(null);
                  }
                }}
              >
                {testingId === item.integration_id ? "Testing..." : "Send test"}
              </button>
              {testResult[item.integration_id] && (
                <span
                  class={`integrations-test-result ${testResult[item.integration_id].ok ? "integrations-test-result--ok" : "integrations-test-result--err"}`}
                  title={testResult[item.integration_id].message}
                >
                  {testResult[item.integration_id].ok ? "✓ delivered" : "✗ failed"}
                </span>
              )}
              <button class="obs-btn obs-btn--sm obs-btn--danger" onClick={() => setDeletingId(item.integration_id)}>Delete</button>
            </div>
          ))}
        </div>
      )}

      <Modal open={showCreate} onClose={() => setShowCreate(false)} title="Add Integration">
        <div class="obs-form-group">
          <label class="obs-label">Name</label>
          <input class="obs-input" placeholder="Production Slack" value={formName}
            onInput={(e) => setFormName((e.target as HTMLInputElement).value)} />
        </div>
        <div class="obs-form-group">
          <label class="obs-label">Type</label>
          <select class="obs-select" value={formType}
            onChange={(e) => { setFormType((e.target as HTMLSelectElement).value); setFormConfig(""); }}>
            {TYPES.map(t => <option key={t.value} value={t.value}>{t.label}</option>)}
          </select>
        </div>
        <div class="obs-form-group">
          <label class="obs-label">Configuration (JSON)</label>
          <textarea class="obs-input" style={{ minHeight: "80px", fontFamily: "var(--obs-font-mono, monospace)", fontSize: "12px" }}
            placeholder={configHint(formType)} value={formConfig}
            onInput={(e) => setFormConfig((e.target as HTMLTextAreaElement).value)} />
        </div>
        <div style={{ display: "flex", justifyContent: "flex-end", gap: "8px", marginTop: "8px" }}>
          <button class="obs-btn" onClick={() => setShowCreate(false)}>Cancel</button>
          <button class="obs-btn obs-btn--primary" onClick={handleCreate}
            disabled={creating || !formName.trim()}>
            {creating ? "Creating..." : "Create"}
          </button>
        </div>
      </Modal>

      <ConfirmDialog open={!!deletingId} onClose={() => setDeletingId(null)} onConfirm={handleDelete}
        title="Delete Integration" message="This integration will stop receiving notifications." loading={deleteLoading} />
    </div>
  );
}
