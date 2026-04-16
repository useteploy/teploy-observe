import { useState, useEffect, useCallback } from "preact/hooks";
import { alertsApi } from "../api/alerts.js";
import type { AlertRule, AlertHistoryEntry } from "../api/alerts.js";
import StatusBadge from "../components/shared/StatusBadge.js";
import Modal from "../components/shared/Modal.js";
import ConfirmDialog from "../components/shared/ConfirmDialog.js";
import Pagination from "../components/shared/Pagination.js";
import "../styles/settings.css";

export const config = { mode: "app" };

const METRICS = [
  { value: "error_count", label: "Error Count" },
  { value: "error_rate", label: "Error Rate (%)" },
  { value: "pageviews", label: "Pageviews" },
  { value: "visitors", label: "Visitors" },
];

const OPERATORS = [
  { value: "gt", label: ">" },
  { value: "gte", label: ">=" },
  { value: "lt", label: "<" },
  { value: "lte", label: "<=" },
  { value: "eq", label: "=" },
];

function operatorLabel(op: string): string {
  return OPERATORS.find(o => o.value === op)?.label || op;
}

function metricLabel(metric: string): string {
  return METRICS.find(m => m.value === metric)?.label || metric;
}

function formatDate(iso: string): string {
  if (!iso) return "--";
  try {
    return new Date(iso).toLocaleString("en-US", {
      month: "short", day: "numeric",
      hour: "2-digit", minute: "2-digit", hour12: false,
    });
  } catch { return iso; }
}

function AlertsSkeleton() {
  return (
    <div class="settings-loading">
      {Array.from({ length: 4 }).map((_, i) => (
        <div class="settings-skeleton-row" key={i}>
          <div class="settings-skeleton-bar" style={{ width: "48px" }} />
          <div style={{ flex: 1, display: "flex", flexDirection: "column", gap: "6px" }}>
            <div class="settings-skeleton-bar" style={{ width: "180px" }} />
            <div class="settings-skeleton-bar" style={{ width: "240px", height: "10px" }} />
          </div>
        </div>
      ))}
    </div>
  );
}

function RulesTab({ siteId }: { siteId: string }) {
  const [rules, setRules] = useState<AlertRule[]>([]);
  const [loading, setLoading] = useState(true);
  const [showCreate, setShowCreate] = useState(false);
  const [creating, setCreating] = useState(false);

  const [formName, setFormName] = useState("");
  const [formMetric, setFormMetric] = useState(METRICS[0].value);
  const [formOperator, setFormOperator] = useState("gt");
  const [formThreshold, setFormThreshold] = useState("");
  const [formWindow, setFormWindow] = useState("5");
  const [formCooldown, setFormCooldown] = useState("60");
  const [deletingRuleId, setDeletingRuleId] = useState<string | null>(null);
  const [deleteLoading, setDeleteLoading] = useState(false);

  const fetchRules = useCallback(async () => {
    setLoading(true);
    try {
      const data = await alertsApi.rules(siteId);
      setRules(data || []);
    } catch { setRules([]); }
    finally { setLoading(false); }
  }, [siteId]);

  useEffect(() => { fetchRules(); }, [fetchRules]);

  const handleCreate = async () => {
    if (!formName.trim() || !formThreshold.trim()) return;
    setCreating(true);
    try {
      await alertsApi.createRule({
        site_id: siteId,
        name: formName.trim(),
        metric: formMetric,
        operator: formOperator,
        threshold: parseFloat(formThreshold),
        window_minutes: parseInt(formWindow) || 5,
        cooldown: parseInt(formCooldown) || 60,
      });
      setShowCreate(false);
      setFormName(""); setFormThreshold(""); setFormWindow("5"); setFormCooldown("60");
      fetchRules();
    } catch (err) { console.error("Failed to create alert rule:", err); }
    finally { setCreating(false); }
  };

  const handleDeleteConfirm = async () => {
    if (!deletingRuleId) return;
    setDeleteLoading(true);
    try {
      await alertsApi.deleteRule(deletingRuleId);
      setRules(prev => prev.filter(r => r.rule_id !== deletingRuleId));
      setDeletingRuleId(null);
    } catch (err) { console.error("Failed to delete rule:", err); }
    finally { setDeleteLoading(false); }
  };

  return (
    <div>
      <div class="alerts-toolbar">
        <span style={{ fontSize: "13px", color: "var(--obs-text-secondary)" }}>
          {rules.length} rule{rules.length !== 1 ? "s" : ""}
        </span>
        <button class="obs-btn obs-btn--primary" onClick={() => setShowCreate(true)}>Create Rule</button>
      </div>

      {loading ? <AlertsSkeleton /> : rules.length === 0 ? (
        <div class="obs-empty-state">No alert rules configured</div>
      ) : (
        <div class="alerts-list">
          {rules.map(rule => (
            <div key={rule.rule_id} class="alerts-row">
              <StatusBadge status={rule.enabled ? "enabled" : "disabled"} size="sm" />
              <div class="alerts-row-info">
                <div class="alerts-row-name">{rule.name}</div>
                <div class="alerts-row-desc">
                  {metricLabel(rule.metric)} {operatorLabel(rule.operator)} {rule.threshold} (window: {rule.window_minutes}m, cooldown: {rule.cooldown}m)
                </div>
              </div>
              <div class="alerts-row-meta">
                <button class="obs-btn obs-btn--sm obs-btn--danger" onClick={() => setDeletingRuleId(rule.rule_id)}>
                  Delete
                </button>
              </div>
            </div>
          ))}
        </div>
      )}

      <Modal open={showCreate} onClose={() => setShowCreate(false)} title="Create Alert Rule">
        <div class="obs-form-group">
          <label class="obs-label">Name</label>
          <input class="obs-input" placeholder="High Error Rate" value={formName}
            onInput={(e) => setFormName((e.target as HTMLInputElement).value)} />
        </div>
        <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr 1fr", gap: "12px" }}>
          <div class="obs-form-group">
            <label class="obs-label">Metric</label>
            <select class="obs-select" value={formMetric}
              onChange={(e) => setFormMetric((e.target as HTMLSelectElement).value)}>
              {METRICS.map(m => <option key={m.value} value={m.value}>{m.label}</option>)}
            </select>
          </div>
          <div class="obs-form-group">
            <label class="obs-label">Operator</label>
            <select class="obs-select" value={formOperator}
              onChange={(e) => setFormOperator((e.target as HTMLSelectElement).value)}>
              {OPERATORS.map(o => <option key={o.value} value={o.value}>{o.label} ({o.value})</option>)}
            </select>
          </div>
          <div class="obs-form-group">
            <label class="obs-label">Threshold</label>
            <input class="obs-input" type="number" value={formThreshold}
              onInput={(e) => setFormThreshold((e.target as HTMLInputElement).value)} />
          </div>
        </div>
        <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: "12px" }}>
          <div class="obs-form-group">
            <label class="obs-label">Window (minutes)</label>
            <input class="obs-input" type="number" value={formWindow}
              onInput={(e) => setFormWindow((e.target as HTMLInputElement).value)} />
          </div>
          <div class="obs-form-group">
            <label class="obs-label">Cooldown (minutes)</label>
            <input class="obs-input" type="number" value={formCooldown}
              onInput={(e) => setFormCooldown((e.target as HTMLInputElement).value)} />
          </div>
        </div>
        <div style={{ display: "flex", justifyContent: "flex-end", gap: "8px", marginTop: "8px" }}>
          <button class="obs-btn" onClick={() => setShowCreate(false)}>Cancel</button>
          <button class="obs-btn obs-btn--primary" onClick={handleCreate}
            disabled={creating || !formName.trim() || !formThreshold.trim()}>
            {creating ? "Creating..." : "Create"}
          </button>
        </div>
      </Modal>

      <ConfirmDialog
        open={!!deletingRuleId}
        onClose={() => setDeletingRuleId(null)}
        onConfirm={handleDeleteConfirm}
        title="Delete Alert Rule"
        message="This alert rule will be permanently disabled. This cannot be undone."
        loading={deleteLoading}
      />
    </div>
  );
}

function HistoryTab({ siteId }: { siteId: string }) {
  const PAGE_SIZE = 20;
  const [history, setHistory] = useState<AlertHistoryEntry[]>([]);
  const [rules, setRules] = useState<AlertRule[]>([]);
  const [loading, setLoading] = useState(true);
  const [page, setPage] = useState(1);

  useEffect(() => {
    setLoading(true);
    Promise.all([
      alertsApi.history(siteId, PAGE_SIZE, (page - 1) * PAGE_SIZE),
      alertsApi.rules(siteId),
    ]).then(([h, r]) => {
      setHistory(h || []);
      setRules(r || []);
    }).catch(() => {
      setHistory([]);
      setRules([]);
    }).finally(() => setLoading(false));
  }, [siteId, page]);

  const getRuleName = (ruleId: string) => {
    const rule = rules.find(r => r.rule_id === ruleId);
    return rule?.name || ruleId.slice(0, 8);
  };

  if (loading) return <AlertsSkeleton />;
  if (!history.length) return <div class="obs-empty-state">No alerts have triggered</div>;

  return (
    <div>
      <div class="alerts-list">
        {history.map(h => (
          <div key={h.alert_id} class="alerts-row">
            <StatusBadge status="error" size="sm" />
            <div class="alerts-row-info">
              <div class="alerts-row-name">{getRuleName(h.rule_id)}</div>
              <div class="alerts-row-desc">
                value: {h.metric_value} (threshold: {h.threshold})
              </div>
            </div>
            <div class="alerts-row-meta">
              <span style={{ fontSize: "11px", color: "var(--obs-text-muted)" }}>{formatDate(h.triggered_at)}</span>
            </div>
          </div>
        ))}
      </div>
      <Pagination page={page} pageSize={PAGE_SIZE} resultCount={history.length} onPageChange={(p) => { setPage(p); window.scrollTo(0, 0); }} />
    </div>
  );
}

export default function AlertsPage() {
  const siteId = typeof window !== "undefined"
    ? new URLSearchParams(window.location.search).get("site_id") || "default"
    : "default";

  const [activeTab, setActiveTab] = useState("rules");

  return (
    <div>
      <div class="obs-page-header">
        <h1 class="obs-page-title">Alerts</h1>
      </div>

      <div class="obs-tabs-bar" style={{ marginBottom: "20px" }}>
        {["rules", "history"].map(tab => (
          <button key={tab}
            class={`obs-tab ${activeTab === tab ? "obs-tab--active" : ""}`}
            onClick={() => setActiveTab(tab)}>
            {tab.charAt(0).toUpperCase() + tab.slice(1)}
          </button>
        ))}
      </div>

      {activeTab === "rules" && <RulesTab siteId={siteId} />}
      {activeTab === "history" && <HistoryTab siteId={siteId} />}
    </div>
  );
}
