import { useState, useEffect, useCallback } from "preact/hooks";
import { monitoringApi } from "../api/monitoring.js";
import type { UptimeMonitor, UptimeResult, CronMonitor, InfraHost, InfraMetric } from "../api/monitoring.js";
import StatusBadge from "../components/shared/StatusBadge.js";
import Modal from "../components/shared/Modal.js";
import "../styles/monitoring.css";

export const config = { mode: "app" };

function formatDate(iso: string): string {
  if (!iso) return "--";
  try {
    return new Date(iso).toLocaleString("en-US", {
      month: "short", day: "numeric",
      hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false,
    });
  } catch { return iso; }
}

function formatInterval(seconds: string | number): string {
  const s = Number(seconds);
  if (s < 60) return `${s}s`;
  if (s < 3600) return `${Math.round(s / 60)}m`;
  return `${Math.round(s / 3600)}h`;
}

function metricColor(pct: number): string {
  if (pct < 60) return "var(--obs-success)";
  if (pct < 85) return "var(--obs-warning)";
  return "var(--obs-danger)";
}

function MonitoringSkeleton() {
  return (
    <div class="monitoring-loading">
      {Array.from({ length: 4 }).map((_, i) => (
        <div class="monitoring-skeleton-row" key={i}>
          <div class="monitoring-skeleton-bar" style={{ width: "10px", height: "10px", borderRadius: "50%" }} />
          <div style={{ flex: 1, display: "flex", flexDirection: "column", gap: "6px" }}>
            <div class="monitoring-skeleton-bar" style={{ width: "160px" }} />
            <div class="monitoring-skeleton-bar" style={{ width: "200px", height: "10px" }} />
          </div>
          <div class="monitoring-skeleton-bar" style={{ width: "50px" }} />
        </div>
      ))}
    </div>
  );
}

// ─── Uptime Tab ───

interface MonitorWithStatus extends UptimeMonitor {
  lastResult?: UptimeResult;
  uptimePct?: number;
  avgResponseMs?: number;
}

function UptimeTab() {
  const siteId = typeof window !== "undefined"
    ? new URLSearchParams(window.location.search).get("site_id") || "default"
    : "default";

  const [monitors, setMonitors] = useState<MonitorWithStatus[]>([]);
  const [loading, setLoading] = useState(true);
  const [showCreate, setShowCreate] = useState(false);
  const [creating, setCreating] = useState(false);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [results, setResults] = useState<UptimeResult[]>([]);
  const [loadingResults, setLoadingResults] = useState(false);

  // Create form
  const [formName, setFormName] = useState("");
  const [formUrl, setFormUrl] = useState("");
  const [formMethod, setFormMethod] = useState("GET");
  const [formExpected, setFormExpected] = useState("200");
  const [formInterval, setFormInterval] = useState("60");

  const fetchMonitors = useCallback(async () => {
    setLoading(true);
    try {
      const data = await monitoringApi.uptimeList(siteId);
      const monitorsRaw = data || [];

      // Fetch latest results for each monitor to derive real status
      const enriched = await Promise.all(monitorsRaw.map(async (m) => {
        try {
          const results = await monitoringApi.uptimeResults(m.monitor_id, 50);
          const lastResult = results?.[0];
          const upCount = (results || []).filter(r => r.is_up).length;
          const uptimePct = results?.length ? (upCount / results.length) * 100 : undefined;
          const responseTimes = (results || []).map(r => Number(r.response_ms)).filter(n => !isNaN(n) && n > 0);
          const avgResponseMs = responseTimes.length ? responseTimes.reduce((a, b) => a + b, 0) / responseTimes.length : undefined;
          return { ...m, lastResult, uptimePct, avgResponseMs } as MonitorWithStatus;
        } catch {
          return { ...m } as MonitorWithStatus;
        }
      }));

      setMonitors(enriched);
    } catch { setMonitors([]); }
    finally { setLoading(false); }
  }, [siteId]);

  useEffect(() => { fetchMonitors(); }, [fetchMonitors]);

  const handleSelect = async (monitorId: string) => {
    if (selectedId === monitorId) { setSelectedId(null); return; }
    setSelectedId(monitorId);
    setLoadingResults(true);
    try {
      const data = await monitoringApi.uptimeResults(monitorId);
      setResults(data || []);
    } catch { setResults([]); }
    finally { setLoadingResults(false); }
  };

  const handleCreate = async () => {
    if (!formName.trim() || !formUrl.trim()) return;
    setCreating(true);
    try {
      await monitoringApi.uptimeCreate({
        site_id: siteId,
        name: formName.trim(),
        url: formUrl.trim(),
        method: formMethod,
        expected_status: parseInt(formExpected) || 200,
        interval_seconds: parseInt(formInterval) || 60,
      });
      setShowCreate(false);
      setFormName(""); setFormUrl(""); setFormMethod("GET"); setFormExpected("200"); setFormInterval("60");
      fetchMonitors();
    } catch (err) { console.error("Failed to create monitor:", err); }
    finally { setCreating(false); }
  };

  const getStatusClass = (m: MonitorWithStatus) => {
    if (!m.lastResult) return "monitoring-status-dot--unknown";
    const isUp = m.lastResult.is_up;
    return isUp ? "monitoring-status-dot--up" : "monitoring-status-dot--down";
  };

  return (
    <div>
      <div class="monitoring-toolbar">
        <span style={{ fontSize: "13px", color: "var(--obs-text-secondary)" }}>
          {monitors.length} monitor{monitors.length !== 1 ? "s" : ""}
        </span>
        <button class="obs-btn obs-btn--primary" onClick={() => setShowCreate(true)}>
          Add Monitor
        </button>
      </div>

      {loading ? <MonitoringSkeleton /> : monitors.length === 0 ? (
        <div class="obs-empty-state">No uptime monitors configured</div>
      ) : (
        <div class="monitoring-list">
          {monitors.map(m => (
            <div key={m.monitor_id}>
              <div class="monitoring-row" onClick={() => handleSelect(m.monitor_id)}>
                <div class={`monitoring-status-dot ${getStatusClass(m)}`} />
                <div class="monitoring-row-info">
                  <div class="monitoring-row-name">{m.name}</div>
                  <div class="monitoring-row-url">{m.method} {m.url}</div>
                </div>
                <div class="monitoring-row-meta">
                  {m.avgResponseMs !== undefined && (
                    <span class="monitoring-row-response">{Math.round(m.avgResponseMs)}ms avg</span>
                  )}
                  {m.uptimePct !== undefined && (
                    <span class="monitoring-row-response" style={{
                      color: m.uptimePct >= 99.9 ? "var(--obs-success)" : m.uptimePct >= 99 ? "var(--obs-warning)" : "var(--obs-danger)"
                    }}>
                      {m.uptimePct.toFixed(1)}%
                    </span>
                  )}
                  <span class="monitoring-row-interval">every {formatInterval(m.interval_seconds)}</span>
                </div>
              </div>
              {selectedId === m.monitor_id && (
                <div style={{ padding: "0 16px 16px", background: "var(--obs-card)" }}>
                  {loadingResults ? (
                    <div class="obs-empty-state">Loading results...</div>
                  ) : results.length === 0 ? (
                    <div class="obs-empty-state">No check results yet</div>
                  ) : (
                    <div class="monitoring-results">
                      {/* Status bar */}
                      <div class="monitoring-status-bar">
                        {[...results].reverse().slice(-30).map((r, i) => {
                          const isUp = r.is_up;
                          return (
                            <div key={i}
                              class={`monitoring-status-bar-item ${isUp ? "monitoring-status-bar-item--up" : "monitoring-status-bar-item--down"}`}
                              title={`${formatDate(r.timestamp)} - ${isUp ? "UP" : "DOWN"} (${r.response_ms}ms)`}
                            />
                          );
                        })}
                      </div>
                      {/* Response time chart */}
                      <div class="monitoring-response-chart">
                        {(() => {
                          const recent = [...results].reverse().slice(-30);
                          const maxMs = Math.max(...recent.map(r => Number(r.response_ms) || 0), 1);
                          return recent.map((r, i) => {
                            const ms = Number(r.response_ms) || 0;
                            const height = Math.max((ms / maxMs) * 40, 2);
                            const isUp = r.is_up;
                            return (
                              <div key={i} class="monitoring-response-bar"
                                style={{ height: `${height}px`, background: isUp ? "var(--obs-accent)" : "var(--obs-danger)" }}
                                title={`${ms}ms`}
                              />
                            );
                          });
                        })()}
                      </div>
                      {/* Result list */}
                      {results.slice(0, 20).map(r => {
                        const isUp = r.is_up;
                        return (
                          <div key={r.result_id} class="monitoring-result-row">
                            <span class="monitoring-result-ts">{formatDate(r.timestamp)}</span>
                            <span class={`monitoring-result-status ${isUp ? "monitoring-result-status--up" : "monitoring-result-status--down"}`}>
                              {isUp ? "UP" : "DOWN"}
                            </span>
                            <span class="monitoring-result-response">{r.status_code} / {r.response_ms}ms</span>
                            {r.error_message && <span class="monitoring-result-error">{r.error_message}</span>}
                          </div>
                        );
                      })}
                    </div>
                  )}
                </div>
              )}
            </div>
          ))}
        </div>
      )}

      <Modal open={showCreate} onClose={() => setShowCreate(false)} title="Add Uptime Monitor">
        <div class="obs-form-group">
          <label class="obs-label">Name</label>
          <input class="obs-input" placeholder="Production API" value={formName}
            onInput={(e) => setFormName((e.target as HTMLInputElement).value)} />
        </div>
        <div class="obs-form-group">
          <label class="obs-label">URL</label>
          <input class="obs-input" placeholder="https://api.example.com/health" value={formUrl}
            onInput={(e) => setFormUrl((e.target as HTMLInputElement).value)} />
        </div>
        <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr 1fr", gap: "12px" }}>
          <div class="obs-form-group">
            <label class="obs-label">Method</label>
            <select class="obs-select" value={formMethod}
              onChange={(e) => setFormMethod((e.target as HTMLSelectElement).value)}>
              <option value="GET">GET</option>
              <option value="POST">POST</option>
              <option value="HEAD">HEAD</option>
            </select>
          </div>
          <div class="obs-form-group">
            <label class="obs-label">Expected Status</label>
            <input class="obs-input" type="number" value={formExpected}
              onInput={(e) => setFormExpected((e.target as HTMLInputElement).value)} />
          </div>
          <div class="obs-form-group">
            <label class="obs-label">Interval (sec)</label>
            <input class="obs-input" type="number" value={formInterval}
              onInput={(e) => setFormInterval((e.target as HTMLInputElement).value)} />
          </div>
        </div>
        <div style={{ display: "flex", justifyContent: "flex-end", gap: "8px", marginTop: "8px" }}>
          <button class="obs-btn" onClick={() => setShowCreate(false)}>Cancel</button>
          <button class="obs-btn obs-btn--primary" onClick={handleCreate}
            disabled={creating || !formName.trim() || !formUrl.trim()}>
            {creating ? "Creating..." : "Create"}
          </button>
        </div>
      </Modal>
    </div>
  );
}

// ─── Cron Tab ───

function CronTab() {
  const siteId = typeof window !== "undefined"
    ? new URLSearchParams(window.location.search).get("site_id") || "default"
    : "default";

  const [monitors, setMonitors] = useState<CronMonitor[]>([]);
  const [loading, setLoading] = useState(true);
  const [showCreate, setShowCreate] = useState(false);
  const [creating, setCreating] = useState(false);

  const [formName, setFormName] = useState("");
  const [formSlug, setFormSlug] = useState("");
  const [formSchedule, setFormSchedule] = useState("");
  const [formGrace, setFormGrace] = useState("300");

  useEffect(() => {
    setLoading(true);
    monitoringApi.cronList(siteId)
      .then(d => setMonitors(d || []))
      .catch(() => setMonitors([]))
      .finally(() => setLoading(false));
  }, [siteId]);

  const handleCreate = async () => {
    if (!formName.trim() || !formSlug.trim() || !formSchedule.trim()) return;
    setCreating(true);
    try {
      await monitoringApi.cronCreate({
        site_id: siteId, name: formName.trim(), slug: formSlug.trim(),
        schedule: formSchedule.trim(), grace_seconds: parseInt(formGrace) || 300,
      });
      setShowCreate(false);
      setFormName(""); setFormSlug(""); setFormSchedule(""); setFormGrace("300");
      const data = await monitoringApi.cronList(siteId);
      setMonitors(data || []);
    } catch (err) { console.error("Failed to create cron monitor:", err); }
    finally { setCreating(false); }
  };

  return (
    <div>
      <div class="monitoring-toolbar">
        <span style={{ fontSize: "13px", color: "var(--obs-text-secondary)" }}>
          {monitors.length} cron monitor{monitors.length !== 1 ? "s" : ""}
        </span>
        <button class="obs-btn obs-btn--primary" onClick={() => setShowCreate(true)}>Add Cron Monitor</button>
      </div>

      {loading ? <MonitoringSkeleton /> : monitors.length === 0 ? (
        <div class="obs-empty-state">No cron monitors configured</div>
      ) : (
        <div class="monitoring-list">
          {monitors.map(m => (
            <div key={m.monitor_id} class="monitoring-row">
              <StatusBadge status={m.enabled ? "enabled" : "disabled"} size="sm" />
              <div class="monitoring-row-info">
                <div class="monitoring-row-name">{m.name}</div>
                <div class="monitoring-row-url">{m.slug}</div>
              </div>
              <div class="monitoring-row-meta">
                <span class="monitoring-cron-schedule">{m.schedule}</span>
                <span class="monitoring-row-interval">grace: {formatInterval(m.grace_seconds)}</span>
              </div>
            </div>
          ))}
        </div>
      )}

      <Modal open={showCreate} onClose={() => setShowCreate(false)} title="Add Cron Monitor">
        <div class="obs-form-group">
          <label class="obs-label">Name</label>
          <input class="obs-input" placeholder="Nightly Backup" value={formName}
            onInput={(e) => setFormName((e.target as HTMLInputElement).value)} />
        </div>
        <div class="obs-form-group">
          <label class="obs-label">Slug</label>
          <input class="obs-input" placeholder="nightly-backup" value={formSlug}
            onInput={(e) => setFormSlug((e.target as HTMLInputElement).value)} />
        </div>
        <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: "12px" }}>
          <div class="obs-form-group">
            <label class="obs-label">Schedule (cron)</label>
            <input class="obs-input" placeholder="0 2 * * *" value={formSchedule}
              onInput={(e) => setFormSchedule((e.target as HTMLInputElement).value)} />
          </div>
          <div class="obs-form-group">
            <label class="obs-label">Grace Period (sec)</label>
            <input class="obs-input" type="number" value={formGrace}
              onInput={(e) => setFormGrace((e.target as HTMLInputElement).value)} />
          </div>
        </div>
        <div style={{ display: "flex", justifyContent: "flex-end", gap: "8px", marginTop: "8px" }}>
          <button class="obs-btn" onClick={() => setShowCreate(false)}>Cancel</button>
          <button class="obs-btn obs-btn--primary" onClick={handleCreate}
            disabled={creating || !formName.trim() || !formSlug.trim() || !formSchedule.trim()}>
            {creating ? "Creating..." : "Create"}
          </button>
        </div>
      </Modal>
    </div>
  );
}

// ─── Infra Tab ───

function InfraTab() {
  const siteId = typeof window !== "undefined"
    ? new URLSearchParams(window.location.search).get("site_id") || "default"
    : "default";

  const [hosts, setHosts] = useState<InfraHost[]>([]);
  const [loading, setLoading] = useState(true);
  const [selectedHost, setSelectedHost] = useState<string | null>(null);
  const [history, setHistory] = useState<InfraMetric[]>([]);
  const [loadingHistory, setLoadingHistory] = useState(false);

  useEffect(() => {
    setLoading(true);
    monitoringApi.infraHosts(siteId)
      .then(d => setHosts(d || []))
      .catch(() => setHosts([]))
      .finally(() => setLoading(false));
  }, [siteId]);

  const handleHostClick = async (hostname: string) => {
    if (selectedHost === hostname) { setSelectedHost(null); return; }
    setSelectedHost(hostname);
    setLoadingHistory(true);
    const now = new Date();
    const from = new Date(now.getTime() - 86400000).toISOString();
    try {
      const data = await monitoringApi.infraHistory(hostname, from, now.toISOString());
      setHistory(data || []);
    } catch { setHistory([]); }
    finally { setLoadingHistory(false); }
  };

  if (loading) return <MonitoringSkeleton />;
  if (!hosts.length) return <div class="obs-empty-state">No infrastructure hosts reporting</div>;

  return (
    <div>
      <div class="monitoring-infra-grid">
        {hosts.map(h => (
          <div key={h.host_id} class="monitoring-infra-card" onClick={() => handleHostClick(h.hostname)}
            style={{ cursor: "pointer" }}>
            <div class="monitoring-infra-hostname">{h.hostname}</div>
            <div style={{ fontSize: "11px", color: "var(--obs-text-muted)", marginBottom: "12px" }}>
              Last report: {formatDate(h.last_report)}
            </div>
            <div class="monitoring-infra-metrics">
              {[
                { label: "CPU", value: h.cpu_pct },
                { label: "Memory", value: h.memory_pct },
                { label: "Disk", value: h.disk_pct },
              ].map(m => (
                <div key={m.label} class="monitoring-infra-metric">
                  <span class="monitoring-infra-metric-label">{m.label}</span>
                  <span class="monitoring-infra-metric-value" style={{ color: metricColor(m.value) }}>
                    {m.value.toFixed(0)}%
                  </span>
                  <div class="monitoring-infra-metric-bar">
                    <div class="monitoring-infra-metric-fill"
                      style={{ width: `${m.value}%`, background: metricColor(m.value) }} />
                  </div>
                </div>
              ))}
            </div>
          </div>
        ))}
      </div>

      {selectedHost && (
        <div style={{ marginTop: "16px" }}>
          <h3 style={{ fontSize: "14px", fontWeight: 600, color: "var(--obs-text)", marginBottom: "12px" }}>
            {selectedHost} - 24h History
          </h3>
          {loadingHistory ? <MonitoringSkeleton /> : history.length === 0 ? (
            <div class="obs-empty-state">No metric history available</div>
          ) : (
            <div class="monitoring-list">
              {history.slice(0, 50).map((m, i) => (
                <div key={i} class="monitoring-result-row">
                  <span class="monitoring-result-ts">{formatDate(m.timestamp)}</span>
                  <span class="monitoring-result-response" style={{ color: metricColor(m.cpu_pct) }}>
                    CPU {m.cpu_pct.toFixed(0)}%
                  </span>
                  <span class="monitoring-result-response" style={{ color: metricColor(m.memory_pct) }}>
                    Mem {m.memory_pct.toFixed(0)}%
                  </span>
                  <span class="monitoring-result-response" style={{ color: metricColor(m.disk_pct) }}>
                    Disk {m.disk_pct.toFixed(0)}%
                  </span>
                </div>
              ))}
            </div>
          )}
        </div>
      )}
    </div>
  );
}

// ─── Main ───

export default function MonitoringPage() {
  const [activeTab, setActiveTab] = useState("uptime");

  return (
    <div>
      <div class="obs-page-header">
        <h1 class="obs-page-title">Monitoring</h1>
      </div>

      <div class="obs-tabs-bar" style={{ marginBottom: "20px" }}>
        {["uptime", "cron", "infrastructure"].map(tab => (
          <button key={tab}
            class={`obs-tab ${activeTab === tab ? "obs-tab--active" : ""}`}
            onClick={() => setActiveTab(tab)}>
            {tab.charAt(0).toUpperCase() + tab.slice(1)}
          </button>
        ))}
      </div>

      {activeTab === "uptime" && <UptimeTab />}
      {activeTab === "cron" && <CronTab />}
      {activeTab === "infrastructure" && <InfraTab />}
    </div>
  );
}
