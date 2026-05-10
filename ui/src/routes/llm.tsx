import { useState, useEffect, useCallback } from "preact/hooks";
import { get } from "../api/helpers.js";
import StatusBadge from "../components/shared/StatusBadge.js";
import CodeBlock from "../components/shared/CodeBlock.js";
import ExportButton from "../components/shared/ExportButton.js";
import { useFilters } from "../hooks/useFilters.js";

export const config = { mode: "app" };

const BASE = "/api/v1/llm";

interface LLMStats {
  total_calls: string;
  total_tokens: string;
  total_cost_usd: string;
  avg_latency_ms: string;
  error_count: string;
}

interface ModelStats {
  model: string;
  provider: string;
  call_count: string;
  total_tokens: string;
  total_cost_usd: string;
  avg_latency_ms: string;
}

interface LLMTrace {
  trace_id: string;
  site_id: string;
  session_id: string;
  span_id: string;
  timestamp: number;
  model: string;
  provider: string;
  operation: string;
  prompt_tokens: string;
  completion_tokens: string;
  total_tokens: string;
  cost_usd: string;
  latency_ms: string;
  status: string;
  error_message: string;
  prompt: string;
  completion: string;
  metadata: string;
}

function formatCost(n: string): string {
  const v = parseFloat(n || "0");
  if (v < 0.01) return `$${v.toFixed(4)}`;
  if (v < 1) return `$${v.toFixed(3)}`;
  return `$${v.toFixed(2)}`;
}

function formatNumber(n: string): string {
  return parseInt(n || "0").toLocaleString();
}

function formatTime(ts: number): string {
  try {
    return new Date(ts).toLocaleString("en-US", {
      month: "short", day: "numeric",
      hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false,
    });
  } catch { return String(ts); }
}

export default function LLMPage() {
  const { state: { siteId } } = useFilters();

  const [stats, setStats] = useState<LLMStats | null>(null);
  const [models, setModels] = useState<ModelStats[]>([]);
  const [traces, setTraces] = useState<LLMTrace[]>([]);
  const [loading, setLoading] = useState(true);
  const [selected, setSelected] = useState<LLMTrace | null>(null);

  const fetch = useCallback(async () => {
    setLoading(true);
    // Compute the window inside the callback so the ref stays stable across
    // renders — otherwise from/to change every render and we infinite-loop.
    const now = new Date();
    const from = new Date(now.getTime() - 7 * 86400000).toISOString();
    const to = now.toISOString();
    try {
      const [s, m, t] = await Promise.all([
        get<LLMStats>(`${BASE}/stats?site_id=${siteId}&from=${from}&to=${to}`).catch(() => null),
        get<ModelStats[]>(`${BASE}/models?site_id=${siteId}&from=${from}&to=${to}`).catch(() => []),
        get<LLMTrace[]>(`${BASE}/traces?site_id=${siteId}&from=${from}&to=${to}&limit=50`).catch(() => []),
      ]);
      setStats(s);
      setModels(m || []);
      setTraces(t || []);
    } finally { setLoading(false); }
  }, [siteId]);

  useEffect(() => { fetch(); }, [fetch]);

  if (selected) {
    return (
      <div>
        <button class="errors-back-btn" onClick={() => setSelected(null)} style={{ marginBottom: "16px" }}>
          <svg width="14" height="14" viewBox="0 0 24 24" fill="currentColor">
            <path d="M20 11H7.83l5.59-5.59L12 4l-8 8 8 8 1.41-1.41L7.83 13H20v-2z" />
          </svg>
          Back to traces
        </button>

        <div class="obs-page-header">
          <h1 class="obs-page-title">{selected.model}</h1>
          <div class="obs-page-actions">
            <StatusBadge status={selected.status === "ok" ? "ok" : "error"} size="md" />
          </div>
        </div>

        <div style={{ display: "grid", gridTemplateColumns: "repeat(4, 1fr)", gap: "12px", marginBottom: "20px" }}>
          {[
            { label: "Provider", value: selected.provider || "unknown" },
            { label: "Operation", value: selected.operation },
            { label: "Latency", value: `${selected.latency_ms}ms` },
            { label: "Cost", value: formatCost(selected.cost_usd) },
            { label: "Prompt Tokens", value: formatNumber(selected.prompt_tokens) },
            { label: "Completion Tokens", value: formatNumber(selected.completion_tokens) },
            { label: "Total Tokens", value: formatNumber(selected.total_tokens) },
            { label: "Timestamp", value: formatTime(selected.timestamp) },
          ].map((c, i) => (
            <div key={i} style={{ background: "var(--obs-surface)", padding: "12px", borderRadius: "var(--obs-radius-md)" }}>
              <div style={{ fontSize: "11px", color: "var(--obs-text-muted)", textTransform: "uppercase", marginBottom: "4px" }}>{c.label}</div>
              <div style={{ fontSize: "14px", fontWeight: 600, color: "var(--obs-text)", fontVariantNumeric: "tabular-nums" }}>{c.value}</div>
            </div>
          ))}
        </div>

        {selected.error_message && (
          <div style={{ padding: "12px", background: "rgba(239,68,68,0.1)", border: "1px solid rgba(239,68,68,0.2)", borderRadius: "var(--obs-radius-md)", color: "var(--obs-danger)", fontSize: "13px", marginBottom: "16px" }}>
            {selected.error_message}
          </div>
        )}

        {selected.prompt && (
          <div style={{ marginBottom: "16px" }}>
            <h3 style={{ fontSize: "13px", fontWeight: 600, color: "var(--obs-text-secondary)", marginBottom: "6px" }}>Prompt</h3>
            <CodeBlock code={selected.prompt} maxHeight="300px" />
          </div>
        )}

        {selected.completion && (
          <div style={{ marginBottom: "16px" }}>
            <h3 style={{ fontSize: "13px", fontWeight: 600, color: "var(--obs-text-secondary)", marginBottom: "6px" }}>Completion</h3>
            <CodeBlock code={selected.completion} maxHeight="300px" />
          </div>
        )}

        {selected.metadata && selected.metadata !== "{}" && (
          <div>
            <h3 style={{ fontSize: "13px", fontWeight: 600, color: "var(--obs-text-secondary)", marginBottom: "6px" }}>Metadata</h3>
            <CodeBlock code={selected.metadata} maxHeight="200px" />
          </div>
        )}
      </div>
    );
  }

  return (
    <div>
      <div class="obs-page-header">
        <h1 class="obs-page-title">LLM Observability</h1>
      </div>

      {loading ? (
        <div class="obs-empty-state">Loading...</div>
      ) : !stats || parseInt(stats.total_calls || "0") === 0 ? (
        <div class="obs-empty-state">
          No LLM calls recorded. POST to /api/v1/llm/ingest to start tracking.
        </div>
      ) : (
        <>
          {/* Summary cards */}
          <div style={{ display: "grid", gridTemplateColumns: "repeat(5, 1fr)", gap: "12px", marginBottom: "20px" }}>
            {[
              { label: "Total Calls", value: formatNumber(stats.total_calls), color: "var(--obs-accent)" },
              { label: "Total Tokens", value: formatNumber(stats.total_tokens), color: "var(--obs-text)" },
              { label: "Total Cost", value: formatCost(stats.total_cost_usd), color: "var(--obs-success)" },
              { label: "Avg Latency", value: `${Math.round(parseFloat(stats.avg_latency_ms || "0"))}ms`, color: "var(--obs-text)" },
              { label: "Errors", value: formatNumber(stats.error_count), color: parseInt(stats.error_count || "0") > 0 ? "var(--obs-danger)" : "var(--obs-text-muted)" },
            ].map((c, i) => (
              <div key={i} style={{ background: "var(--obs-surface)", padding: "14px", borderRadius: "var(--obs-radius-md)", borderLeft: `3px solid ${c.color}` }}>
                <div style={{ fontSize: "11px", color: "var(--obs-text-muted)", textTransform: "uppercase", letterSpacing: "0.5px", marginBottom: "4px" }}>{c.label}</div>
                <div style={{ fontSize: "20px", fontWeight: 700, color: c.color, fontVariantNumeric: "tabular-nums" }}>{c.value}</div>
              </div>
            ))}
          </div>

          {/* Models breakdown */}
          {models.length > 0 && (
            <div style={{ marginBottom: "20px" }}>
              <h2 style={{ fontSize: "14px", fontWeight: 600, color: "var(--obs-text)", marginBottom: "8px" }}>Models</h2>
              <div style={{ background: "var(--obs-surface)", borderRadius: "var(--obs-radius-md)", overflow: "hidden" }}>
                <table style={{ width: "100%", borderCollapse: "collapse", fontSize: "13px" }}>
                  <thead>
                    <tr style={{ background: "var(--obs-card-hover)" }}>
                      <th style={{ padding: "10px 16px", textAlign: "left", fontSize: "11px", fontWeight: 600, color: "var(--obs-text-muted)", textTransform: "uppercase" }}>Model</th>
                      <th style={{ padding: "10px 16px", textAlign: "left", fontSize: "11px", fontWeight: 600, color: "var(--obs-text-muted)", textTransform: "uppercase" }}>Provider</th>
                      <th style={{ padding: "10px 16px", textAlign: "right", fontSize: "11px", fontWeight: 600, color: "var(--obs-text-muted)", textTransform: "uppercase" }}>Calls</th>
                      <th style={{ padding: "10px 16px", textAlign: "right", fontSize: "11px", fontWeight: 600, color: "var(--obs-text-muted)", textTransform: "uppercase" }}>Tokens</th>
                      <th style={{ padding: "10px 16px", textAlign: "right", fontSize: "11px", fontWeight: 600, color: "var(--obs-text-muted)", textTransform: "uppercase" }}>Cost</th>
                      <th style={{ padding: "10px 16px", textAlign: "right", fontSize: "11px", fontWeight: 600, color: "var(--obs-text-muted)", textTransform: "uppercase" }}>Avg Latency</th>
                    </tr>
                  </thead>
                  <tbody>
                    {models.map((m, i) => (
                      <tr key={i} style={{ borderTop: "1px solid var(--obs-border-subtle)" }}>
                        <td style={{ padding: "10px 16px", fontWeight: 500, color: "var(--obs-text)" }}>{m.model}</td>
                        <td style={{ padding: "10px 16px", color: "var(--obs-text-secondary)" }}>{m.provider}</td>
                        <td style={{ padding: "10px 16px", textAlign: "right", fontVariantNumeric: "tabular-nums", color: "var(--obs-text-secondary)" }}>{formatNumber(m.call_count)}</td>
                        <td style={{ padding: "10px 16px", textAlign: "right", fontVariantNumeric: "tabular-nums", color: "var(--obs-text-secondary)" }}>{formatNumber(m.total_tokens)}</td>
                        <td style={{ padding: "10px 16px", textAlign: "right", fontVariantNumeric: "tabular-nums", color: "var(--obs-success)" }}>{formatCost(m.total_cost_usd)}</td>
                        <td style={{ padding: "10px 16px", textAlign: "right", fontVariantNumeric: "tabular-nums", color: "var(--obs-text-secondary)" }}>{Math.round(parseFloat(m.avg_latency_ms || "0"))}ms</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            </div>
          )}

          {/* Recent traces */}
          {traces.length > 0 && (
            <div>
              <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", marginBottom: "8px" }}>
                <h2 style={{ fontSize: "14px", fontWeight: 600, color: "var(--obs-text)", margin: 0 }}>Recent Calls</h2>
                <ExportButton
                  filename={`llm-traces-${siteId}-${Date.now()}.csv`}
                  rows={traces}
                  columns={[
                    { key: "model", label: "model" },
                    { key: "provider", label: "provider" },
                    { key: "operation", label: "operation" },
                    { key: "total_tokens", label: "tokens" },
                    { key: "cost_usd", label: "cost_usd" },
                    { key: "latency_ms", label: "latency_ms" },
                    { key: "status", label: "status" },
                    { key: "timestamp", label: "timestamp_ms" },
                  ]}
                />
              </div>
              <div style={{ background: "var(--obs-surface)", borderRadius: "var(--obs-radius-md)", overflow: "hidden" }}>
                {traces.map((t) => (
                  <div key={t.trace_id} onClick={() => setSelected(t)}
                    style={{ display: "flex", alignItems: "center", gap: "12px", padding: "10px 16px", borderBottom: "1px solid var(--obs-border-subtle)", cursor: "pointer", fontSize: "13px" }}>
                    <StatusBadge status={t.status === "ok" ? "ok" : "error"} size="sm" />
                    <span style={{ flex: 1, fontWeight: 500, color: "var(--obs-text)" }}>{t.model}</span>
                    <span style={{ color: "var(--obs-text-muted)", fontSize: "11px", textTransform: "uppercase" }}>{t.operation}</span>
                    <span style={{ color: "var(--obs-text-secondary)", fontVariantNumeric: "tabular-nums", minWidth: "70px", textAlign: "right" }}>{formatNumber(t.total_tokens)} tok</span>
                    <span style={{ color: "var(--obs-success)", fontVariantNumeric: "tabular-nums", minWidth: "60px", textAlign: "right" }}>{formatCost(t.cost_usd)}</span>
                    <span style={{ color: "var(--obs-text-secondary)", fontVariantNumeric: "tabular-nums", minWidth: "60px", textAlign: "right" }}>{t.latency_ms}ms</span>
                    <span style={{ color: "var(--obs-text-muted)", fontSize: "11px", minWidth: "130px", textAlign: "right" }}>{formatTime(t.timestamp)}</span>
                  </div>
                ))}
              </div>
            </div>
          )}
        </>
      )}
    </div>
  );
}
