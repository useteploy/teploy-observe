import type { ComponentChildren } from "preact";
import { useState, useEffect, useCallback } from "preact/hooks";
import { analyticsApi } from "../api/analytics.js";
import type { FunnelStep, FunnelResult, RetentionCohort, JourneyResult, Goal, GoalConversion, Correlation } from "../api/analytics.js";
import { cohortsApi } from "../api/persons.js";
import type { Cohort } from "../api/persons.js";
import Modal from "../components/shared/Modal.js";
import Tabs from "../components/shared/Tabs.js";
import EmptyState from "../components/shared/EmptyState.js";
import { formatMinor, toMinorUnits, fromMinorUnits } from "../utils/money.js";
import "../styles/insights.css";
import { useFilters } from "../hooks/useFilters.js";

export const config = { mode: "app" };

// Five different analyses share this page and a tab label is all that ever
// distinguished them. Each panel now opens with its own heading and a line
// saying what it answers, because "Journeys" alone does not tell anyone
// whether it is the thing they came for.
function PanelHeading({ title, children }: { title: string; children: ComponentChildren }) {
  return (
    <div class="insights-panel-heading">
      <h2 class="insights-panel-title">{title}</h2>
      <p class="insights-panel-desc">{children}</p>
    </div>
  );
}

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
  const [breakdownBy, setBreakdownBy] = useState<string>("");
  const [breakdownResults, setBreakdownResults] = useState<Array<{ breakdown: string; results: FunnelResult[] }>>([]);

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
      if (breakdownBy) {
        const bd = await analyticsApi.funnelBreakdown(siteId, from, to, validSteps, breakdownBy);
        setBreakdownResults(bd || []);
        setResults([]);
      } else {
        const data = await analyticsApi.funnel(siteId, from, to, validSteps);
        setResults(data || []);
        setBreakdownResults([]);
      }
    } catch {
      setResults([]);
      setBreakdownResults([]);
    }
    finally { setLoading(false); }
  };

  const maxVisitors = results.length > 0 ? results[0].visitors : 1;

  return (
    <div>
      <PanelHeading title="Conversion funnels">
        Where visitors drop out of a sequence of pages or events. Add two or
        more steps, then Analyze — nothing is stored, so a funnel can be
        rebuilt any time.
      </PanelHeading>
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
        <div style={{ display: "flex", gap: "8px", marginTop: "4px", alignItems: "center" }}>
          <button class="obs-btn obs-btn--sm" onClick={addStep}>Add Step</button>
          <span style={{ fontSize: "12px", color: "var(--obs-text-muted)", marginLeft: "8px" }}>Breakdown by</span>
          <select class="obs-select obs-select--sm" value={breakdownBy}
            onChange={(e) => setBreakdownBy((e.target as HTMLSelectElement).value)}>
            <option value="">None</option>
            <option value="browser">Browser</option>
            <option value="country">Country</option>
            <option value="device">Device</option>
            <option value="os">OS</option>
          </select>
          <button class="obs-btn obs-btn--primary obs-btn--sm" onClick={analyze}
            disabled={loading || steps.filter(s => s.value.trim()).length < 2}>
            {loading ? "Analyzing..." : "Analyze Funnel"}
          </button>
        </div>
      </div>

      {loading ? <InsightsSkeleton /> : analyzed && breakdownResults.length > 0 ? (
        <div class="funnel-breakdown-grid">
          {breakdownResults.map((bd) => {
            const firstN = bd.results[0]?.visitors || 0;
            const lastN = bd.results[bd.results.length - 1]?.visitors || 0;
            const finalConv = bd.results[bd.results.length - 1]?.conversion || 0;
            return (
              <div key={bd.breakdown} class="funnel-breakdown-card">
                <div class="funnel-breakdown-title">{bd.breakdown}</div>
                <div class="funnel-breakdown-summary">
                  <span><strong>{firstN.toLocaleString()}</strong> → <strong>{lastN.toLocaleString()}</strong></span>
                  <span class="funnel-breakdown-conv">{finalConv.toFixed(1)}%</span>
                </div>
                <div class="funnel-breakdown-steps">
                  {bd.results.map((r, i) => (
                    <div key={i} class="funnel-breakdown-step">
                      <div
                        class="funnel-breakdown-bar"
                        style={{ width: `${firstN > 0 ? (r.visitors / firstN) * 100 : 0}%` }}
                      />
                      <div class="funnel-breakdown-step-label">
                        <span>{r.step.value}</span>
                        <span>{r.visitors.toLocaleString()} ({r.conversion.toFixed(1)}%)</span>
                      </div>
                    </div>
                  ))}
                </div>
              </div>
            );
          })}
        </div>
      ) : analyzed && results.length === 0 && breakdownResults.length === 0 ? (
        <EmptyState
          icon="layers"
          title="No visitors matched these steps"
          description="Every step has to match something already in your events. Check the paths against Dashboard's top pages, and the event names against the Events page — a funnel step is an exact match, not a prefix."
          actions={[{ label: "See top pages", href: "/" }, { label: "See events", href: "/events" }]}
        />
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
  const [view, setView] = useState<"heatmap" | "overlay">("heatmap");

  useEffect(() => {
    setLoading(true);
    analyticsApi.retention(siteId, from, to)
      .then(d => setCohorts(d || []))
      .catch(() => setCohorts([]))
      .finally(() => setLoading(false));
  }, [siteId, from, to]);

  if (loading) return <InsightsSkeleton />;
  if (!cohorts.length) {
    return (
      <div>
        <PanelHeading title="Retention">
          How many of each day's new visitors come back on the days after.
        </PanelHeading>
        <EmptyState
          icon="signal"
          title="Not enough history yet"
          description="Retention compares a cohort's first visit against later ones, so it needs visitors who arrived on one day and returned on another. Keep collecting for a few days and this fills in on its own."
        />
      </div>
    );
  }

  const maxPeriods = Math.max(...cohorts.map(c => c.periods.length));

  const cellColor = (pct: number): string => {
    if (pct <= 0) return "transparent";
    const alpha = Math.min(0.4, (pct / 100) * 0.45 + 0.02);
    return `rgba(34, 197, 94, ${alpha.toFixed(3)})`;
  };

  // Curve colors for overlay view — cycle through the existing palette.
  const overlayColors = ["#6366f1", "#22c55e", "#f59e0b", "#ef4444", "#0ea5e9", "#a855f7", "#14b8a6", "#ec4899"];

  return (
    <div>
      <PanelHeading title="Retention">
        How many of each day's new visitors come back on the days after. Each
        row is a cohort; P0 is the day they arrived.
      </PanelHeading>
      <div class="retention-view-toggle">
        <button
          class={`retention-view-btn ${view === "heatmap" ? "retention-view-btn--active" : ""}`}
          onClick={() => setView("heatmap")}
        >Heatmap</button>
        <button
          class={`retention-view-btn ${view === "overlay" ? "retention-view-btn--active" : ""}`}
          onClick={() => setView("overlay")}
        >Overlay</button>
      </div>

      {view === "heatmap" ? (
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
      ) : (
        <div class="retention-overlay">
          <svg viewBox={`0 0 600 260`} style={{ width: "100%", height: "260px", display: "block" }}>
            {/* Gridlines */}
            {[0, 25, 50, 75, 100].map((yPct) => {
              const y = 20 + ((100 - yPct) / 100) * 200;
              return (
                <g key={yPct}>
                  <line x1={40} x2={590} y1={y} y2={y} stroke="rgba(128,128,128,0.15)" strokeWidth={1} />
                  <text x={36} y={y + 4} textAnchor="end" fontSize={10} fill="currentColor" opacity={0.5}>{yPct}%</text>
                </g>
              );
            })}
            {/* x-axis tick labels */}
            {Array.from({ length: maxPeriods }).map((_, i) => {
              if (maxPeriods === 1) return null;
              const x = 40 + (i / (maxPeriods - 1)) * 550;
              return (
                <text key={i} x={x} y={240} textAnchor="middle" fontSize={10} fill="currentColor" opacity={0.5}>P{i}</text>
              );
            })}
            {cohorts.map((c, idx) => {
              const color = overlayColors[idx % overlayColors.length];
              const points = c.periods.map((pct, i) => {
                const x = 40 + (maxPeriods > 1 ? (i / (maxPeriods - 1)) * 550 : 275);
                const y = 20 + ((100 - pct) / 100) * 200;
                return `${x},${y}`;
              }).join(" ");
              return (
                <g key={c.cohort_date}>
                  <polyline points={points} fill="none" stroke={color} strokeWidth={2} strokeLinejoin="round" />
                  {c.periods.map((pct, i) => {
                    const x = 40 + (maxPeriods > 1 ? (i / (maxPeriods - 1)) * 550 : 275);
                    const y = 20 + ((100 - pct) / 100) * 200;
                    return <circle key={i} cx={x} cy={y} r={3} fill={color} />;
                  })}
                </g>
              );
            })}
          </svg>
          <div class="retention-overlay-legend">
            {cohorts.map((c, idx) => (
              <span key={c.cohort_date} class="retention-overlay-legend-item">
                <span class="retention-overlay-swatch" style={{ background: overlayColors[idx % overlayColors.length] }} />
                {c.cohort_date} <span style={{ color: "var(--obs-text-muted)" }}>({c.cohort_size})</span>
              </span>
            ))}
          </div>
        </div>
      )}
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

  const heading = (
    <PanelHeading title="User journeys">
      The routes visitors actually take through the site: which page follows
      which, and the most common whole paths.
    </PanelHeading>
  );

  if (loading) return <InsightsSkeleton />;
  if (!data || (!data.transitions?.length && !data.top_paths?.length)) {
    return (
      <div>
        {heading}
        <EmptyState
          icon="layers"
          title="No multi-page sessions yet"
          description="A journey needs a session that viewed more than one page. If every visit is a single pageview — or the tracking snippet is only on one page — there is nothing to join up yet."
          actions={[{ label: "Check your install", href: "/onboard" }]}
        />
      </div>
    );
  }

  return (
    <div>
      {heading}
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

// Currencies offered in the picker. Not a complete ISO-4217 list — any code
// the API accepts works, this is just the shortlist that saves most people
// typing. There is deliberately no default selection: Observe never guesses
// that its operator bills in dollars.
const CURRENCY_CHOICES = [
  "USD", "EUR", "GBP", "CAD", "AUD", "NZD", "CHF", "SEK", "NOK", "DKK",
  "PLN", "CZK", "JPY", "CNY", "HKD", "SGD", "INR", "KRW", "BRL", "MXN",
  "ZAR", "AED", "ILS", "TRY",
];

function GoalsPanel({ siteId, from, to }: { siteId: string; from: string; to: string }) {
  const [goals, setGoals] = useState<GoalConversion[]>([]);
  const [loading, setLoading] = useState(true);
  const [showForm, setShowForm] = useState(false);
  const [editing, setEditing] = useState<Goal | null>(null);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const [formName, setFormName] = useState("");
  const [formType, setFormType] = useState("page");
  const [formValue, setFormValue] = useState("");
  // The form holds a major-unit decimal because that is what a person types;
  // it is converted to minor units once, on submit.
  const [formAmount, setFormAmount] = useState("");
  const [formCurrency, setFormCurrency] = useState("");
  const [formSource, setFormSource] = useState("fixed");
  const [formProperty, setFormProperty] = useState("revenue");

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const data = await analyticsApi.goals(siteId, from, to);
      setGoals(data || []);
    } catch { setGoals([]); }
    finally { setLoading(false); }
  }, [siteId, from, to]);

  useEffect(() => { load(); }, [load]);

  const openCreate = () => {
    setEditing(null);
    setFormName(""); setFormType("page"); setFormValue("");
    setFormAmount(""); setFormCurrency(""); setFormSource("fixed"); setFormProperty("revenue");
    setError("");
    setShowForm(true);
  };

  const openEdit = (g: Goal) => {
    setEditing(g);
    setFormName(g.name); setFormType(g.goal_type); setFormValue(g.goal_value);
    setFormCurrency(g.currency || "");
    setFormSource(g.value_source || "fixed");
    setFormProperty(g.value_property || "revenue");
    setFormAmount(g.value_minor && g.currency ? fromMinorUnits(g.value_minor, g.currency) : "");
    setError("");
    setShowForm(true);
  };

  const handleSave = async () => {
    if (!formName.trim() || !formValue.trim()) return;
    // Money is only sent when a currency was chosen. An amount with no
    // currency is a number nobody can read, and the API rejects it.
    let valueMinor = 0;
    if (formCurrency && formSource === "fixed" && formAmount.trim()) {
      const minor = toMinorUnits(formAmount, formCurrency);
      if (minor === null) {
        setError("Value must be an amount like 49.99.");
        return;
      }
      valueMinor = minor;
    }
    if (formSource === "event" && !formCurrency) {
      setError("Per-event values need a currency so the total can be shown.");
      return;
    }
    const payload = {
      site_id: siteId,
      name: formName.trim(),
      goal_type: formType,
      goal_value: formValue.trim(),
      value_minor: valueMinor,
      currency: formCurrency,
      value_source: formCurrency ? formSource : "fixed",
      value_property: formSource === "event" ? formProperty.trim() : "",
    };
    setSaving(true);
    setError("");
    try {
      if (editing) {
        await analyticsApi.updateGoal(editing.goal_id, payload);
      } else {
        await analyticsApi.createGoal(payload);
      }
      setShowForm(false);
      load();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Could not save the goal.");
    } finally { setSaving(false); }
  };

  const handleDelete = async (g: Goal) => {
    if (!confirm(`Delete goal "${g.name}"? Conversions are computed from events, so nothing else is lost.`)) return;
    try {
      await analyticsApi.deleteGoal(g.goal_id, siteId);
      load();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Could not delete the goal.");
    }
  };

  const valued = goals.filter(g => g.goal.currency && g.total_value_minor > 0);
  // A period total is only meaningful per currency — adding dollars to yen
  // would be a number that is wrong in every currency at once.
  const totalsByCurrency = new Map<string, number>();
  for (const g of valued) {
    totalsByCurrency.set(g.goal.currency, (totalsByCurrency.get(g.goal.currency) ?? 0) + g.total_value_minor);
  }

  return (
    <div>
      <PanelHeading title="Goal conversions">
        Named outcomes — a page reached or an event fired — counted over the
        period, and what they were worth if you give them a value.
      </PanelHeading>

      <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", marginBottom: "16px", gap: "12px", flexWrap: "wrap" }}>
        <span style={{ fontSize: "13px", color: "var(--obs-text-secondary)" }}>
          {goals.length} goal{goals.length !== 1 ? "s" : ""}
          {totalsByCurrency.size > 0 && (
            <>
              {" · "}
              {Array.from(totalsByCurrency.entries())
                .map(([code, minor]) => formatMinor(minor, code))
                .join(" + ")}
              {" in this period"}
            </>
          )}
        </span>
        <button class="obs-btn obs-btn--primary obs-btn--sm" onClick={openCreate}>Create goal</button>
      </div>

      {error && !showForm && (
        <div class="obs-form-error" role="alert" style={{ marginBottom: "12px" }}>{error}</div>
      )}

      {loading ? <InsightsSkeleton /> : goals.length === 0 ? (
        <EmptyState
          icon="package"
          title="No goals yet"
          description="A goal is the outcome you care about — a visit to /thank-you, or a purchase event. Nothing appears here until one exists, and conversions are counted from the events you have already collected, so a new goal is populated immediately rather than starting from zero."
          actions={[{ label: "Create goal", onClick: openCreate, primary: true }]}
        />
      ) : (
        <div class="goals-list">
          {goals.map(g => (
            <div class="goal-card" key={g.goal.goal_id}>
              <div class="goal-card-info">
                <div class="goal-card-name">{g.goal.name}</div>
                <div class="goal-card-desc">
                  {g.goal.goal_type}: {g.goal.goal_value}
                  {g.goal.currency && (
                    <>
                      {" · "}
                      {g.goal.value_source === "event"
                        ? `value from event property "${g.goal.value_property}"`
                        : `${formatMinor(g.goal.value_minor, g.goal.currency)} per conversion`}
                    </>
                  )}
                </div>
              </div>
              <div class="goal-card-stats">
                <div>{g.conversions.toLocaleString()} / {g.visitors.toLocaleString()}</div>
                {g.conversion_events !== g.conversions && (
                  <div>{g.conversion_events.toLocaleString()} events</div>
                )}
              </div>
              <div class="goal-card-value">
                {g.goal.currency
                  ? formatMinor(g.total_value_minor, g.goal.currency)
                  : <span class="goal-card-value--unset">no value</span>}
              </div>
              <div class="goal-card-rate">{g.rate.toFixed(1)}%</div>
              <div class="goal-card-actions">
                <button class="obs-btn obs-btn--sm" onClick={() => openEdit(g.goal)}>Edit</button>
                <button class="obs-btn obs-btn--sm" onClick={() => handleDelete(g.goal)}>Delete</button>
              </div>
            </div>
          ))}
        </div>
      )}

      <Modal open={showForm} onClose={() => setShowForm(false)} title={editing ? "Edit goal" : "Create goal"}>
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
          <div class="obs-form-hint">
            What a conversion is matched on. Not a monetary value — that is below.
          </div>
        </div>

        <div class="obs-form-group">
          <label class="obs-label">Currency</label>
          <select class="obs-select" value={formCurrency}
            onChange={(e) => setFormCurrency((e.target as HTMLSelectElement).value)}>
            <option value="">No value — count conversions only</option>
            {CURRENCY_CHOICES.map(c => <option key={c} value={c}>{c}</option>)}
          </select>
        </div>

        {formCurrency && (
          <>
            <div class="obs-form-group">
              <label class="obs-label">Where the value comes from</label>
              <select class="obs-select" value={formSource}
                onChange={(e) => setFormSource((e.target as HTMLSelectElement).value)}>
                <option value="fixed">A fixed amount per conversion</option>
                <option value="event">An amount sent with each event</option>
              </select>
            </div>
            {formSource === "fixed" ? (
              <div class="obs-form-group">
                <label class="obs-label">Value per conversion ({formCurrency})</label>
                <input class="obs-input" inputMode="decimal" placeholder="49.99" value={formAmount}
                  onInput={(e) => setFormAmount((e.target as HTMLInputElement).value)} />
              </div>
            ) : (
              <div class="obs-form-group">
                <label class="obs-label">Event property holding the amount</label>
                <input class="obs-input" placeholder="revenue" value={formProperty}
                  onInput={(e) => setFormProperty((e.target as HTMLInputElement).value)} />
                <div class="obs-form-hint">
                  Send it with the event, in whole {formCurrency}:
                  {" "}<code>observe.track("{formValue || "purchase"}", {"{ " + (formProperty || "revenue") + ": 49.99 }"})</code>.
                  Events without a readable number still count as conversions,
                  they just add nothing to the total.
                </div>
              </div>
            )}
          </>
        )}

        {error && <div class="obs-form-error" role="alert">{error}</div>}

        <div style={{ display: "flex", justifyContent: "flex-end", gap: "8px", marginTop: "8px" }}>
          <button class="obs-btn" onClick={() => setShowForm(false)}>Cancel</button>
          <button class="obs-btn obs-btn--primary" onClick={handleSave}
            disabled={saving || !formName.trim() || !formValue.trim()}>
            {saving ? "Saving..." : editing ? "Save" : "Create"}
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

  const heading = (
    <PanelHeading title="Conversion correlations">
      Which visitor properties — browser, country, campaign — go with a higher
      or lower conversion rate than the site average.
    </PanelHeading>
  );

  if (loading) return <InsightsSkeleton />;
  if (!data.length) {
    return (
      <div>
        {heading}
        <EmptyState
          icon="signal"
          title="Nothing to correlate yet"
          description="This compares converting sessions against the rest, so it needs both: enough traffic to be significant, and a goal that some of it converted on. Create a goal first, then come back once the period covers a few hundred sessions."
        />
      </div>
    );
  }

  return (
    <div>
      {heading}
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

// ─── Cohort filter chip ───
//
// C2: every analytics route can opt in to a cohort filter via
// ?cohort_id=X. The chip lives in the page header so the user can see
// which cohort is active and switch / clear without leaving the page.
// The chip writes to URL state so deep links round-trip.

function CohortFilterChip({ siteId }: { siteId: string }) {
  const [cohorts, setCohorts] = useState<Cohort[]>([]);
  const [active, setActive] = useState<string>(() => {
    if (typeof window === "undefined") return "";
    return new URLSearchParams(window.location.search).get("cohort_id") || "";
  });
  const [open, setOpen] = useState(false);

  useEffect(() => {
    cohortsApi.list(siteId).then(d => setCohorts(d || [])).catch(() => setCohorts([]));
  }, [siteId]);

  const apply = (id: string) => {
    setActive(id);
    setOpen(false);
    const url = new URL(window.location.href);
    if (id) {
      url.searchParams.set("cohort_id", id);
    } else {
      url.searchParams.delete("cohort_id");
    }
    window.history.pushState(null, "", url.toString());
    // Some panels read filter state from URL on mount only — soft
    // navigate so they re-fetch.
    window.dispatchEvent(new PopStateEvent("popstate"));
  };

  const activeCohort = cohorts.find(c => c.cohort_id === active);

  return (
    <div style={{ position: "relative" }}>
      <button class="obs-btn obs-btn--sm" onClick={() => setOpen(!open)}
        data-testid="cohort-filter-chip"
        style={{ display: "inline-flex", alignItems: "center", gap: "6px" }}>
        <svg width="12" height="12" viewBox="0 0 24 24" fill="currentColor">
          <path d="M16 11c1.66 0 2.99-1.34 2.99-3S17.66 5 16 5c-1.66 0-3 1.34-3 3s1.34 3 3 3zm-8 0c1.66 0 2.99-1.34 2.99-3S9.66 5 8 5C6.34 5 5 6.34 5 8s1.34 3 3 3zm0 2c-2.33 0-7 1.17-7 3.5V19h14v-2.5c0-2.33-4.67-3.5-7-3.5z" />
        </svg>
        {activeCohort ? activeCohort.name : "Filter by cohort"}
        {active && (
          <span onClick={(e) => { e.stopPropagation(); apply(""); }}
            style={{ marginLeft: "4px", color: "var(--obs-text-muted)", cursor: "pointer" }}
            title="Clear cohort filter">×</span>
        )}
      </button>

      {open && (
        <div style={{ position: "absolute", top: "100%", right: 0, marginTop: "4px",
          minWidth: "240px", background: "var(--obs-card)",
          border: "1px solid var(--obs-border)", borderRadius: "var(--obs-radius-md)",
          boxShadow: "var(--obs-shadow-md, 0 4px 12px rgba(0,0,0,.15))",
          zIndex: 100, maxHeight: "320px", overflow: "auto" }}>
          {cohorts.length === 0 ? (
            <div style={{ padding: "12px", fontSize: "12px", color: "var(--obs-text-muted)" }}>
              No cohorts yet — <a href="/cohorts" style={{ color: "var(--obs-accent)" }}>create one</a>.
            </div>
          ) : (
            cohorts.map(c => (
              <div key={c.cohort_id} onClick={() => apply(c.cohort_id)}
                data-testid="cohort-filter-option"
                style={{ padding: "8px 12px", cursor: "pointer", fontSize: "13px",
                  borderBottom: "1px solid var(--obs-border)",
                  background: c.cohort_id === active ? "var(--obs-bg)" : "transparent" }}>
                <div>{c.name}</div>
                <div style={{ fontSize: "11px", color: "var(--obs-text-muted)", marginTop: "2px" }}>
                  {c.member_count.toLocaleString()} members
                </div>
              </div>
            ))
          )}
          {active && (
            <div onClick={() => apply("")}
              style={{ padding: "8px 12px", cursor: "pointer", fontSize: "12px",
                color: "var(--obs-text-muted)", textAlign: "center" }}>
              Clear filter
            </div>
          )}
        </div>
      )}
    </div>
  );
}

// ─── Main Page ───

export default function InsightsPage() {
  const { state: { siteId } } = useFilters();

  const now = new Date();
  const from = new Date(now.getTime() - 30 * 86400000).toISOString();
  const to = now.toISOString();

  return (
    <div>
      {/* The route stays /insights — cohorts.tsx deep-links to it with query
          params — but the page no longer calls itself that. "Insights" is a
          category; what is actually here is conversion analysis. */}
      <div class="obs-page-header" style={{ display: "flex", alignItems: "flex-start", gap: "12px" }}>
        <div>
          <h1 class="obs-page-title">Funnels &amp; Goals</h1>
          <p class="insights-panel-desc" style={{ marginTop: "4px" }}>
            Conversion analysis: where visitors drop out, which goals they
            reach and what those are worth, who comes back, and the routes
            they take.
          </p>
        </div>
        <div style={{ marginLeft: "auto" }}>
          <CohortFilterChip siteId={siteId} />
        </div>
      </div>

      <Tabs tabs={[
        {
          key: "funnels",
          label: "Funnels",
          content: <FunnelsPanel siteId={siteId} from={from} to={to} />,
        },
        {
          key: "goals",
          label: "Goal conversions",
          content: <GoalsPanel siteId={siteId} from={from} to={to} />,
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
          key: "correlations",
          label: "Correlations",
          content: <CorrelationsPanel siteId={siteId} from={from} to={to} />,
        },
      ]} />
    </div>
  );
}
