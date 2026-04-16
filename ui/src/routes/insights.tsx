import { useState, useEffect, useCallback } from "preact/hooks";
import { analyticsApi } from "../api/analytics.js";
import type { FunnelStep, FunnelResult, RetentionCohort, JourneyResult, GoalConversion, Correlation } from "../api/analytics.js";
import Modal from "../components/shared/Modal.js";
import Tabs from "../components/shared/Tabs.js";
import "../styles/insights.css";

export const config = { mode: "app" };

function InsightsSkeleton() {
  return (
    <div class="insights-loading">
      {Array.from({ length: 5 }).map((_, i) => (
        <div class="insights-skeleton-row" key={i}>
          <div class="insights-skeleton-bar" style={{ width: "32px", height: "32px", borderRadius: "50%" }} />
          <div style={{ flex: 1, display: "flex", flexDirection: "column", gap: "6px" }}>
            <div class="insights-skeleton-bar" style={{ width: "200px" }} />
            <div class="insights-skeleton-bar" style={{ width: "140px", height: "10px" }} />
          </div>
          <div class="insights-skeleton-bar" style={{ width: "120px", height: "24px" }} />
          <div class="insights-skeleton-bar" style={{ width: "60px" }} />
        </div>
      ))}
    </div>
  );
}

// ─── Funnels ───

function FunnelsPanel({ siteId, from, to }: { siteId: string; from: string; to: string }) {
  const [steps, setSteps] = useState<FunnelStep[]>([
    { type: "page", value: "/" },
    { type: "page", value: "" },
  ]);
  const [results, setResults] = useState<FunnelResult[]>([]);
  const [loading, setLoading] = useState(false);
  const [analyzed, setAnalyzed] = useState(false);

  const updateStep = (idx: number, field: "type" | "value", val: string) => {
    setSteps(prev => prev.map((s, i) => i === idx ? { ...s, [field]: val } : s));
  };
  const addStep = () => setSteps(prev => [...prev, { type: "page", value: "" }]);
  const removeStep = (idx: number) => setSteps(prev => prev.filter((_, i) => i !== idx));

  const analyze = async () => {
    const validSteps = steps.filter(s => s.value.trim());
    if (validSteps.length < 2) return;
    setLoading(true);
    setAnalyzed(true);
    try {
      const data = await analyticsApi.funnel(siteId, from, to, validSteps);
      setResults(data || []);
    } catch { setResults([]); }
    finally { setLoading(false); }
  };

  const maxVisitors = results.length > 0 ? results[0].visitors : 1;

  return (
    <div>
      <div class="funnel-builder">
        {steps.map((step, i) => (
          <div class="funnel-builder-row" key={i}>
            <span style={{ fontSize: "12px", color: "var(--obs-text-muted)", width: "50px" }}>Step {i + 1}</span>
            <select class="obs-select" value={step.type} style={{ width: "100px" }}
              onChange={(e) => updateStep(i, "type", (e.target as HTMLSelectElement).value)}>
              <option value="page">Page</option>
              <option value="event">Event</option>
            </select>
            <input class="obs-input" placeholder={step.type === "page" ? "/signup" : "click_buy"}
              value={step.value} style={{ flex: 1 }}
              onInput={(e) => updateStep(i, "value", (e.target as HTMLInputElement).value)} />
            {steps.length > 2 && (
              <button class="obs-btn obs-btn--sm" onClick={() => removeStep(i)}>x</button>
            )}
          </div>
        ))}
        <div style={{ display: "flex", gap: "8px", marginTop: "4px" }}>
          <button class="obs-btn obs-btn--sm" onClick={addStep}>Add Step</button>
          <button class="obs-btn obs-btn--primary obs-btn--sm" onClick={analyze}
            disabled={loading || steps.filter(s => s.value.trim()).length < 2}>
            {loading ? "Analyzing..." : "Analyze Funnel"}
          </button>
        </div>
      </div>

      {loading ? <InsightsSkeleton /> : analyzed && results.length === 0 ? (
        <div class="obs-empty-state">No data for this funnel</div>
      ) : results.length > 0 ? (
        <div>
          {/* SVG funnel shape */}
          <svg width="100%" height={results.length * 80 + 20} viewBox={`0 0 600 ${results.length * 80 + 20}`} style={{ display: "block", marginBottom: "16px" }}>
            {results.map((r, i) => {
              const pct = maxVisitors > 0 ? r.visitors / maxVisitors : 0;
              const nextPct = i < results.length - 1 && maxVisitors > 0
                ? results[i + 1].visitors / maxVisitors : pct * 0.8;
              const topW = Math.max(pct * 500, 40);
              const botW = Math.max(nextPct * 500, 30);
              const cx = 300;
              const y = i * 80 + 10;
              const h = 60;
              const opacity = 1 - (i * 0.12);

              return (
                <g key={i}>
                  <polygon
                    points={`${cx - topW / 2},${y} ${cx + topW / 2},${y} ${cx + botW / 2},${y + h} ${cx - botW / 2},${y + h}`}
                    fill="var(--obs-accent)"
                    opacity={opacity}
                    rx="4"
                  />
                  <text x={cx} y={y + 24} textAnchor="middle" fill="#fff" fontSize="14" fontWeight="700">
                    {r.conversion.toFixed(1)}%
                  </text>
                  <text x={cx} y={y + 42} textAnchor="middle" fill="rgba(255,255,255,0.7)" fontSize="11">
                    {r.visitors.toLocaleString()} - {r.step.value}
                  </text>
                  {r.drop_off > 0 && (
                    <text x={cx + topW / 2 + 12} y={y + 35} fill="var(--obs-danger)" fontSize="11" fontWeight="600">
                      -{r.drop_off.toFixed(1)}%
                    </text>
                  )}
                </g>
              );
            })}
          </svg>
        </div>
      ) : null}
    </div>
  );
}

// ─── Retention ───

function RetentionPanel({ siteId, from, to }: { siteId: string; from: string; to: string }) {
  const [cohorts, setCohorts] = useState<RetentionCohort[]>([]);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    setLoading(true);
    analyticsApi.retention(siteId, from, to)
      .then(d => setCohorts(d || []))
      .catch(() => setCohorts([]))
      .finally(() => setLoading(false));
  }, [siteId, from, to]);

  if (loading) return <InsightsSkeleton />;
  if (!cohorts.length) return <div class="obs-empty-state">Not enough data for retention analysis</div>;

  const maxPeriods = Math.max(...cohorts.map(c => c.periods.length));

  const cellColor = (pct: number): string => {
    if (pct <= 0) return "transparent";
    // Smooth 10-step gradient
    const alpha = Math.min(0.4, (pct / 100) * 0.45 + 0.02);
    return `rgba(34, 197, 94, ${alpha.toFixed(3)})`;
  };

  return (
    <div class="retention-grid">
      <table class="retention-table">
        <thead>
          <tr>
            <th>Cohort</th>
            <th>Size</th>
            {Array.from({ length: maxPeriods }).map((_, i) => (
              <th key={i}>P{i}</th>
            ))}
          </tr>
        </thead>
        <tbody>
          {cohorts.map(c => (
            <tr key={c.cohort_date}>
              <td style={{ textAlign: "left", fontWeight: 500, color: "var(--obs-text)" }}>{c.cohort_date}</td>
              <td>{c.cohort_size.toLocaleString()}</td>
              {c.periods.map((pct, i) => (
                <td key={i} class="retention-cell"
                  style={{ background: cellColor(pct), color: pct > 0 ? "var(--obs-text)" : "var(--obs-text-muted)" }}>
                  {pct.toFixed(0)}%
                </td>
              ))}
              {Array.from({ length: maxPeriods - c.periods.length }).map((_, i) => (
                <td key={`empty-${i}`} style={{ color: "var(--obs-text-muted)" }}>--</td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

// ─── Journeys ───

function JourneysPanel({ siteId, from, to }: { siteId: string; from: string; to: string }) {
  const [data, setData] = useState<JourneyResult | null>(null);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    setLoading(true);
    analyticsApi.journeys(siteId, from, to)
      .then(d => setData(d || null))
      .catch(() => setData(null))
      .finally(() => setLoading(false));
  }, [siteId, from, to]);

  if (loading) return <InsightsSkeleton />;
  if (!data || (!data.transitions?.length && !data.top_paths?.length)) {
    return <div class="obs-empty-state">Not enough data for journey analysis</div>;
  }

  return (
    <div>
      {data.transitions?.length > 0 && (
        <div>
          <h3 style={{ fontSize: "13px", fontWeight: 600, color: "var(--obs-text)", marginBottom: "8px" }}>
            Page Transitions ({data.total_paths} total paths)
          </h3>
          <div class="journey-transitions">
            {data.transitions.slice(0, 20).map((t, i) => (
              <div class="journey-edge" key={i}>
                <span class="journey-edge-from">{t.from || "(entry)"}</span>
                <span class="journey-edge-arrow">-&gt;</span>
                <span class="journey-edge-to">{t.to}</span>
                <span class="journey-edge-count">{t.count.toLocaleString()}</span>
              </div>
            ))}
          </div>
        </div>
      )}

      {data.top_paths?.length > 0 && (
        <div class="journey-paths">
          <h3 style={{ fontSize: "13px", fontWeight: 600, color: "var(--obs-text)", marginBottom: "8px" }}>
            Top Paths
          </h3>
          {data.top_paths.slice(0, 15).map((p, i) => (
            <div class="journey-path" key={i}>
              <div class="journey-path-chain">
                {p.path.map((step, j) => (
                  <span key={j} class="journey-path-node-wrap">
                    {j > 0 && (
                      <svg width="16" height="12" viewBox="0 0 16 12" class="journey-path-arrow">
                        <path d="M0 6h12M9 2l4 4-4 4" stroke="var(--obs-text-muted)" strokeWidth="1.5" fill="none" />
                      </svg>
                    )}
                    <span class="journey-path-node">{step}</span>
                  </span>
                ))}
              </div>
              <span class="journey-path-count">{p.count.toLocaleString()}</span>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

// ─── Goals ───

function GoalsPanel({ siteId, from, to }: { siteId: string; from: string; to: string }) {
  const [goals, setGoals] = useState<GoalConversion[]>([]);
  const [loading, setLoading] = useState(true);
  const [showCreate, setShowCreate] = useState(false);
  const [creating, setCreating] = useState(false);
  const [formName, setFormName] = useState("");
  const [formType, setFormType] = useState("page");
  const [formValue, setFormValue] = useState("");

  const fetch = useCallback(async () => {
    setLoading(true);
    try {
      const data = await analyticsApi.goals(siteId);
      setGoals(data || []);
    } catch { setGoals([]); }
    finally { setLoading(false); }
  }, [siteId]);

  useEffect(() => { fetch(); }, [fetch]);

  const handleCreate = async () => {
    if (!formName.trim() || !formValue.trim()) return;
    setCreating(true);
    try {
      await analyticsApi.createGoal({ site_id: siteId, name: formName.trim(), goal_type: formType, goal_value: formValue.trim() });
      setShowCreate(false);
      setFormName(""); setFormValue(""); setFormType("page");
      fetch();
    } catch (err) { console.error("Failed to create goal:", err); }
    finally { setCreating(false); }
  };

  return (
    <div>
      <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", marginBottom: "16px" }}>
        <span style={{ fontSize: "13px", color: "var(--obs-text-secondary)" }}>{goals.length} goal{goals.length !== 1 ? "s" : ""}</span>
        <button class="obs-btn obs-btn--primary obs-btn--sm" onClick={() => setShowCreate(true)}>Create Goal</button>
      </div>

      {loading ? <InsightsSkeleton /> : goals.length === 0 ? (
        <div class="obs-empty-state">No goals configured. Create one to track conversions.</div>
      ) : (
        <div class="goals-list">
          {goals.map(g => (
            <div class="goal-card" key={g.goal.goal_id}>
              <div class="goal-card-info">
                <div class="goal-card-name">{g.goal.name}</div>
                <div class="goal-card-desc">{g.goal.goal_type}: {g.goal.goal_value}</div>
              </div>
              <div class="goal-card-stats">
                {g.conversions.toLocaleString()} / {g.visitors.toLocaleString()}
              </div>
              <div class="goal-card-rate">{g.rate.toFixed(1)}%</div>
            </div>
          ))}
        </div>
      )}

      <Modal open={showCreate} onClose={() => setShowCreate(false)} title="Create Goal">
        <div class="obs-form-group">
          <label class="obs-label">Name</label>
          <input class="obs-input" placeholder="Signup completion" value={formName}
            onInput={(e) => setFormName((e.target as HTMLInputElement).value)} />
        </div>
        <div class="obs-form-group">
          <label class="obs-label">Type</label>
          <select class="obs-select" value={formType}
            onChange={(e) => setFormType((e.target as HTMLSelectElement).value)}>
            <option value="page">Page visit</option>
            <option value="event">Custom event</option>
          </select>
        </div>
        <div class="obs-form-group">
          <label class="obs-label">{formType === "page" ? "Path" : "Event name"}</label>
          <input class="obs-input" placeholder={formType === "page" ? "/thank-you" : "purchase"}
            value={formValue} onInput={(e) => setFormValue((e.target as HTMLInputElement).value)} />
        </div>
        <div style={{ display: "flex", justifyContent: "flex-end", gap: "8px", marginTop: "8px" }}>
          <button class="obs-btn" onClick={() => setShowCreate(false)}>Cancel</button>
          <button class="obs-btn obs-btn--primary" onClick={handleCreate}
            disabled={creating || !formName.trim() || !formValue.trim()}>
            {creating ? "Creating..." : "Create"}
          </button>
        </div>
      </Modal>
    </div>
  );
}

// ─── Correlations ───

function CorrelationsPanel({ siteId, from, to }: { siteId: string; from: string; to: string }) {
  const [data, setData] = useState<Correlation[]>([]);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    setLoading(true);
    analyticsApi.correlations(siteId, from, to)
      .then(d => setData(d || []))
      .catch(() => setData([]))
      .finally(() => setLoading(false));
  }, [siteId, from, to]);

  if (loading) return <InsightsSkeleton />;
  if (!data.length) return <div class="obs-empty-state">Not enough data for correlation analysis</div>;

  return (
    <div>
      <div style={{ fontSize: "12px", color: "var(--obs-text-muted)", marginBottom: "12px" }}>
        Baseline conversion: {data[0]?.baseline_rate?.toFixed(1)}%. Properties with statistically significant impact:
      </div>
      <div style={{ display: "flex", flexDirection: "column", gap: "1px", borderRadius: "var(--obs-radius-md)", overflow: "hidden" }}>
        {data.filter(c => c.significant).map((c, i) => (
          <div class="correlation-row" key={i}>
            <span class="correlation-prop">{c.property}</span>
            <span class="correlation-value">{c.value}</span>
            <span class={`correlation-uplift ${c.uplift >= 0 ? "correlation-uplift--positive" : "correlation-uplift--negative"}`}>
              {c.uplift >= 0 ? "+" : ""}{c.uplift.toFixed(1)}%
            </span>
            <span style={{ width: "60px", textAlign: "right", fontSize: "12px", color: "var(--obs-text-secondary)" }}>
              {c.rate.toFixed(1)}%
            </span>
            <span class="correlation-sig">sig</span>
          </div>
        ))}
      </div>
      {data.filter(c => !c.significant).length > 0 && (
        <div style={{ marginTop: "16px" }}>
          <div style={{ fontSize: "12px", color: "var(--obs-text-muted)", marginBottom: "8px" }}>
            Non-significant ({data.filter(c => !c.significant).length})
          </div>
          <div style={{ display: "flex", flexDirection: "column", gap: "1px", borderRadius: "var(--obs-radius-md)", overflow: "hidden" }}>
            {data.filter(c => !c.significant).slice(0, 10).map((c, i) => (
              <div class="correlation-row" key={i} style={{ opacity: 0.6 }}>
                <span class="correlation-prop">{c.property}</span>
                <span class="correlation-value">{c.value}</span>
                <span class="correlation-uplift" style={{ color: "var(--obs-text-muted)" }}>
                  {c.uplift >= 0 ? "+" : ""}{c.uplift.toFixed(1)}%
                </span>
                <span style={{ width: "60px", textAlign: "right", fontSize: "12px", color: "var(--obs-text-muted)" }}>
                  {c.rate.toFixed(1)}%
                </span>
              </div>
            ))}
          </div>
        </div>
      )}
    </div>
  );
}

// ─── Main Page ───

export default function InsightsPage() {
  const siteId = typeof window !== "undefined"
    ? new URLSearchParams(window.location.search).get("site_id") || "default"
    : "default";

  const now = new Date();
  const from = new Date(now.getTime() - 30 * 86400000).toISOString();
  const to = now.toISOString();

  return (
    <div>
      <div class="obs-page-header">
        <h1 class="obs-page-title">Insights</h1>
      </div>

      <Tabs tabs={[
        {
          key: "funnels",
          label: "Funnels",
          content: <FunnelsPanel siteId={siteId} from={from} to={to} />,
        },
        {
          key: "retention",
          label: "Retention",
          content: <RetentionPanel siteId={siteId} from={from} to={to} />,
        },
        {
          key: "journeys",
          label: "Journeys",
          content: <JourneysPanel siteId={siteId} from={from} to={to} />,
        },
        {
          key: "goals",
          label: "Goals",
          content: <GoalsPanel siteId={siteId} from={from} to={to} />,
        },
        {
          key: "correlations",
          label: "Correlations",
          content: <CorrelationsPanel siteId={siteId} from={from} to={to} />,
        },
      ]} />
    </div>
  );
}
