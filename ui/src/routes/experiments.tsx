import { useRequestGuard } from "../hooks/useRequestGuard.js";
import { useState, useEffect, useCallback } from "preact/hooks";
import { experimentsApi } from "../api/flags.js";
import type { Experiment, ExperimentResults } from "../api/flags.js";
import StatusBadge from "../components/shared/StatusBadge.js";
import Modal from "../components/shared/Modal.js";
import ExportButton from "../components/shared/ExportButton.js";
import { formatArmValue, metricKindLabel, secondaryLabel } from "../utils/experimentMetrics.js";
import "../styles/flags.css";
import { useFilters } from "../hooks/useFilters.js";

export const config = { mode: "app" };

function formatDate(iso: string): string {
  if (!iso) return "--";
  try {
    return new Date(iso).toLocaleDateString("en-US", {
      month: "short", day: "numeric", year: "numeric",
    });
  } catch { return iso; }
}

function ExperimentsSkeleton() {
  return (
    <div class="flags-loading">
      {Array.from({ length: 4 }).map((_, i) => (
        <div class="flags-skeleton-row" key={i}>
          <div class="flags-skeleton-bar" style={{ width: "48px" }} />
          <div style={{ flex: 1, display: "flex", flexDirection: "column", gap: "6px" }}>
            <div class="flags-skeleton-bar" style={{ width: "160px" }} />
            <div class="flags-skeleton-bar" style={{ width: "100px", height: "10px" }} />
          </div>
          <div class="flags-skeleton-bar" style={{ width: "70px" }} />
        </div>
      ))}
    </div>
  );
}

function ResultsPanel({ experimentId, siteId }: { experimentId: string; siteId: string }) {
  const [results, setResults] = useState<ExperimentResults | null>(null);
  const [loading, setLoading] = useState(true);

  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    let active = true;
    setLoading(true); setResults(null); setError(null);
    experimentsApi.results(experimentId, siteId)
      .then(r => { if (active) setResults(r); })
      .catch(e => { if (active) setError(e instanceof Error ? e.message : "Unable to load results"); })
      .finally(() => { if (active) setLoading(false); });
    return () => { active = false; };
  }, [experimentId, siteId]);

  if (loading) return <div class="obs-empty-state">Loading results...</div>;
  if (error) return <div class="obs-empty-state" role="alert">Unable to load results: {error}</div>;
  if (!results || !results.variants?.length) return <div class="obs-empty-state">No results yet</div>;

  const maxRate = Math.max(...results.variants.map(v => v.conversion_rate), 0.001);
  const totalExposures = results.variants.reduce((sum, v) => sum + v.exposures, 0);
  const an = results.analysis;
  const pct = (x: number) => `${(x * 100).toFixed(2)}%`;
  const kind = results.metric_kind;
  const continuous = kind === "count" || kind === "mean";
  const num = (x: number) => (Math.abs(x) >= 100 ? x.toFixed(0) : x.toFixed(3));
  const pStr = (p: number) => (p < 0.0001 ? "<0.0001" : p.toFixed(4));

  return (
    <div class="experiments-results">
      {/* Summary */}
      <div style={{ padding: "12px 16px", borderBottom: "1px solid var(--obs-border-subtle)", fontSize: "12px", color: "var(--obs-text-secondary)" }}>
        <strong>{totalExposures.toLocaleString()}</strong> total exposures across{" "}
        <strong>{results.variants.length}</strong> variants
        {results.winner && (
          <span style={{ marginLeft: "12px", color: "var(--obs-success)", fontWeight: 600 }}>
            Winner: {results.winner}
          </span>
        )}
      </div>

      {/* Fixed-horizon contract: peeking warning and contaminated users. */}
      {(results.peeking_warning || (results.contaminated_users ?? 0) > 0) && (
        <div style={{ padding: "8px 16px", borderBottom: "1px solid var(--obs-border-subtle)", fontSize: "12px", display: "flex", flexDirection: "column", gap: "4px", color: "var(--obs-text-secondary)" }}>
          {results.peeking_warning && <div style={{ fontWeight: 600 }}>{results.peeking_warning}</div>}
          {(results.contaminated_users ?? 0) > 0 && (
            <div>{results.contaminated_users} user(s) were exposed to more than one variant and are excluded from every count.</div>
          )}
        </div>
      )}

      {/* O09 analysis: SRM diagnostic first (it invalidates trust), then the
          gate state, test and winner-rule trace. Estimates always render;
          the gates only decide the badge. */}
      {an && (
        <div style={{ padding: "10px 16px", borderBottom: "1px solid var(--obs-border-subtle)", fontSize: "12px", display: "flex", flexDirection: "column", gap: "4px" }}>
          {an.srm?.detected && (
            <div style={{ padding: "6px 10px", borderRadius: "var(--obs-radius)", background: "rgba(239, 68, 68, 0.12)", color: "var(--obs-danger, #ef4444)", fontWeight: 600 }}>
              Sample ratio mismatch detected (p={pStr(an.srm.p_value)}) - {an.srm.note || "assignment is broken; do not trust these results"}
            </div>
          )}
          <div style={{ display: "flex", gap: "14px", flexWrap: "wrap", color: "var(--obs-text-secondary)" }}>
            <span>Horizon: {an.horizon_met ? "met" : `waiting (min arm ${an.min_arm_exposures}/${an.min_sample_per_arm})`}</span>
            <span>Test: {an.test}{an.test !== "none" && an.p_value > 0 ? ` (p={pStr(an.p_value)}, df=${an.df})` : ""}</span>
          </div>
          {an.test_note && <div style={{ color: "var(--obs-text-secondary)" }}>{an.test_note}</div>}
        </div>
      )}

      {results.variants.map((v, idx) => {
        const barWidth = maxRate > 0 ? (v.conversion_rate / maxRate) * 100 : 0;
        const isWinner = results.winner === v.variant;
        const isControl = idx === 0;
        const prob = v.prob_beat_control;
        const probPct = Math.round(prob * 100);
        const probClass =
          isControl ? "experiments-prob--control" :
          prob >= 0.95 ? "experiments-prob--strong" :
          prob >= 0.75 ? "experiments-prob--lean" :
          prob >= 0.25 ? "experiments-prob--neutral" :
          "experiments-prob--weak";

        return (
          <div key={v.variant} class="experiments-result-row" style={{ flexDirection: "column", alignItems: "stretch", gap: "6px" }}>
            <div style={{ display: "flex", alignItems: "center", gap: "12px", flexWrap: "wrap" }}>
              <span class="experiments-result-variant" style={{ minWidth: "100px" }}>
                {v.variant}
                {isControl && <span class="experiments-result-control-tag" style={{ marginLeft: "6px" }}>control</span>}
                {isWinner && <span class="experiments-result-winner" style={{ marginLeft: "6px" }}>Winner</span>}
              </span>
              <span class="experiments-result-stat">{v.exposures.toLocaleString()} exposures</span>
              <span class="experiments-result-stat">{v.conversions.toLocaleString()} conversions</span>
              <span class="experiments-result-stat" style={{ fontWeight: 600, color: "var(--obs-text)" }}>
                {formatArmValue(kind, v)}
              </span>
              {!continuous && v.wilson_low !== undefined && v.wilson_high !== undefined && (
                <span class="experiments-result-stat" title="Wilson score 95% interval" style={{ fontSize: "11px" }}>
                  [{pct(v.wilson_low)} - {pct(v.wilson_high)}]
                </span>
              )}
              {!isControl && (
                <span class={`experiments-prob ${probClass}`} title="Bayesian probability this variant beats control">
                  P(beats control) {probPct}%
                </span>
              )}
              {results.significant && isWinner && (
                <span style={{ fontSize: "10px", padding: "2px 6px", borderRadius: "var(--obs-radius-full)", background: "rgba(34, 197, 94, 0.1)", color: "var(--obs-success)", fontWeight: 600 }}>
                  Significant
                </span>
              )}
            </div>
            {/* Conversion rate bar */}
            <div style={{ height: "6px", background: "var(--obs-border-subtle)", borderRadius: "3px", overflow: "hidden" }}>
              <div style={{
                height: "100%", borderRadius: "3px",
                width: `${barWidth}%`,
                background: isWinner ? "var(--obs-success)" : "var(--obs-accent)",
                transition: "width 0.3s ease",
              }} />
            </div>
            {!isControl && (
              <div class="experiments-prob-bar" title={`${probPct}% probability variant beats control`}>
                <div class={`experiments-prob-bar-fill ${probClass}`} style={{ width: `${Math.max(probPct, 2)}%` }} />
               </div>
             )}
           </div>
         );
       })}

      {/* O09 pairwise vs control: absolute + relative lift, Newcombe CI,
          Holm-adjusted p. The Bayesian bars above stay estimates; these are
          the gates a winner claim actually passed. */}
      {an?.pairwise_vs_control?.length > 0 && (
        <div style={{ padding: "10px 16px", fontSize: "12px", borderTop: "1px solid var(--obs-border-subtle)" }}>
          <div style={{ color: "var(--obs-text-secondary)", marginBottom: "6px", fontWeight: 600 }}>
            vs control {an.winner_rule ? `- ${an.winner_rule}` : ""}
          </div>
          {an.pairwise_vs_control.map((pw) => (
            <div key={pw.variant} style={{ display: "flex", gap: "14px", flexWrap: "wrap", padding: "3px 0", color: "var(--obs-text-secondary)" }}>
              <span style={{ minWidth: "100px", color: "var(--obs-text)" }}>{pw.variant}</span>
              <span>lift {pw.lift_absolute >= 0 ? "+" : ""}{continuous ? num(pw.lift_absolute) : pct(pw.lift_absolute)}</span>
              <span>95% CI [{continuous ? num(pw.ci_low) : pct(pw.ci_low)} - {continuous ? num(pw.ci_high) : pct(pw.ci_high)}]</span>
              <span title={pw.used_fisher ? "Fisher exact (small cells)" : "chi-square"}>
                p={pStr(pw.holm_adjusted_p)} (Holm{pw.used_fisher ? ", Fisher" : ""}{continuous ? ", Welch t" : ""})
              </span>
              {pw.significant && (
                <span style={{ color: "var(--obs-success)", fontWeight: 600 }}>significant</span>
              )}
            </div>
          ))}
        </div>
      )}
      {/* Secondary metrics: exploratory, never gate the winner. */}
      {results.secondary && results.secondary.length > 0 && (
        <div style={{ padding: "10px 16px", fontSize: "12px", borderTop: "1px solid var(--obs-border-subtle)" }}>
          <div style={{ color: "var(--obs-text-secondary)", marginBottom: "6px", fontWeight: 600 }}>
            Secondary metrics - {results.multiple_comparison_note}
          </div>
          {results.secondary.map((m) => (
            <div key={m.key} style={{ padding: "4px 0", color: "var(--obs-text-secondary)" }}>
              <div style={{ color: "var(--obs-text)" }}>{secondaryLabel(m)} - {metricKindLabel(m.kind)}</div>
              {m.variants.map((v) => (
                <span key={v.variant} style={{ marginRight: "14px" }}>
                  {v.variant}: {formatArmValue(m.kind, v)} ({v.exposures.toLocaleString()} exposed)
                </span>
              ))}
              {m.analysis.pairwise_vs_control?.map((pw) => (
                <div key={pw.variant}>
                  {pw.variant} vs control: p={pStr(pw.holm_adjusted_p)} (Holm){pw.significant ? " - crossed 0.05, exploratory" : ""}
                </div>
              ))}
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

export default function ExperimentsPage() {
  const { state: { siteId } } = useFilters();

  const [experiments, setExperiments] = useState<Experiment[]>([]);
  const [loading, setLoading] = useState(true);
  const [showCreate, setShowCreate] = useState(false);
  const [creating, setCreating] = useState(false);
  const [expandedId, setExpandedId] = useState<string | null>(null);

  // Create form
  const [createError, setCreateError] = useState<string | null>(null);
  const [formName, setFormName] = useState("");
  const [formFlagKey, setFormFlagKey] = useState("");
  const [formVariants, setFormVariants] = useState("control,treatment");
  const [formGoal, setFormGoal] = useState("");
  const [formGoalValue, setFormGoalValue] = useState("");
  const [formMinSample, setFormMinSample] = useState("100");

  const requestfetchExperiments = useRequestGuard(JSON.stringify([siteId]));
  const fetchExperiments = useCallback(async () => {
    const current = requestfetchExperiments();
    setLoading(true);
    try {
      const data = await experimentsApi.list(siteId);
      if (!current()) return;
      setExperiments(data || []);
    } catch { if (!current()) return; setExperiments([]); }
    finally { if (current()) setLoading(false); }
  }, [siteId]);

  useEffect(() => { fetchExperiments(); }, [fetchExperiments]);

  const handleStart = async (id: string) => {
    try {
      await experimentsApi.start(id);
      setExperiments(prev => prev.map(e =>
        e.experiment_id === id ? { ...e, status: "running" } : e
      ));
    } catch (err) { console.error("Failed to start experiment:", err); }
  };

  const handleStop = async (id: string) => {
    try {
      await experimentsApi.stop(id);
      setExperiments(prev => prev.map(e =>
        e.experiment_id === id ? { ...e, status: "stopped" } : e
      ));
    } catch (err) { console.error("Failed to stop experiment:", err); }
  };

  const handleCreate = async () => {
    if (!formName.trim() || !formFlagKey.trim() || !formGoal.trim()) return;
    if (creating) return;
    setCreating(true); setCreateError(null);
    try {
      const keys = formVariants.split(",").map(k => k.trim());
      const minimum = Number(formMinSample);
      if (keys.length < 2 || keys.some(k => !k) || new Set(keys).size !== keys.length) throw new Error("Enter at least two distinct variant keys");
      if (!Number.isSafeInteger(minimum) || minimum < 1) throw new Error("Minimum sample must be a positive integer");
      await experimentsApi.create({
        site_id: siteId,
        name: formName.trim(),
        flag_key: formFlagKey.trim(),
        variants: JSON.stringify(keys.map(key => ({ key, weight: 1 }))),
        min_sample: minimum,
        goal_value: formGoalValue.trim(),
        goal_metric: formGoal.trim(),
      });
      setShowCreate(false);
      setFormName(""); setFormFlagKey(""); setFormVariants("control,treatment");
      setFormGoal(""); setFormGoalValue(""); setFormMinSample("100");
      fetchExperiments();
    } catch (err) { setCreateError(err instanceof Error ? err.message : "Unable to create experiment"); }
    finally { setCreating(false); }
  };

  return (
    <div>
      <div class="obs-page-header">
        <h1 class="obs-page-title">Experiments</h1>
        <div class="obs-page-actions">
          <ExportButton
            filename={`experiments-${siteId}-${Date.now()}.csv`}
            rows={experiments}
            columns={[
              { key: "name", label: "name" },
              { key: "flag_key", label: "flag_key" },
              { key: "goal_metric", label: "goal" },
              { key: "status", label: "status" },
              { key: "started_at", label: "started_at" },
            ]}
          />
          <button class="obs-btn obs-btn--primary" onClick={() => setShowCreate(true)}>
            Create Experiment
          </button>
        </div>
      </div>

      {loading ? (
        <ExperimentsSkeleton />
      ) : experiments.length === 0 ? (
        <div class="obs-empty-state">No experiments created yet</div>
      ) : (
        <div class="experiments-list">
          {experiments.map(exp => (
            <div key={exp.experiment_id}>
              <div class="experiments-row" onClick={() => setExpandedId(expandedId === exp.experiment_id ? null : exp.experiment_id)}>
                <StatusBadge status={exp.status} size="sm" />
                <div class="experiments-row-info">
                  <div class="experiments-row-name">{exp.name}</div>
                  <div class="experiments-row-key">{exp.flag_key} -- {exp.goal_metric}</div>
                </div>
                <div class="experiments-row-actions" onClick={(e) => e.stopPropagation()}>
                  {exp.status === "draft" && (
                    <button class="obs-btn obs-btn--sm obs-btn--primary" onClick={() => handleStart(exp.experiment_id)}>Start</button>
                  )}
                  {exp.status === "running" && (
                    <button class="obs-btn obs-btn--sm obs-btn--danger" onClick={() => handleStop(exp.experiment_id)}>Stop</button>
                  )}
                </div>
              </div>
              {expandedId === exp.experiment_id && (
                <div style={{ padding: "0 16px 16px", background: "var(--obs-card)" }}>
                  <div style={{ display: "flex", gap: "16px", fontSize: "12px", color: "var(--obs-text-secondary)", marginBottom: "8px" }}>
                    {exp.started_at && <span>Started: {formatDate(exp.started_at)}</span>}
                    {exp.ended_at && <span>Ended: {formatDate(exp.ended_at)}</span>}
                    <span>Created: {formatDate(exp.created_at)}</span>
                    <span>Variants: {exp.variants}</span>
                  </div>
                  <ResultsPanel experimentId={exp.experiment_id} siteId={exp.site_id} />
                </div>
              )}
            </div>
          ))}
        </div>
      )}

      <Modal open={showCreate} onClose={() => setShowCreate(false)} title="Create Experiment">
        {createError && <div role="alert">{createError}</div>}
        <div class="obs-form-group">
          <label class="obs-label">Name</label>
          <input class="obs-input" placeholder="Checkout Flow Test" value={formName}
            onInput={(e) => setFormName((e.target as HTMLInputElement).value)} />
        </div>
        <div class="obs-form-group">
          <label class="obs-label">Flag Key</label>
          <input class="obs-input" placeholder="checkout-redesign" value={formFlagKey}
            onInput={(e) => setFormFlagKey((e.target as HTMLInputElement).value)} />
        </div>
        <div class="obs-form-group">
          <label class="obs-label">Variants (comma-separated, equal allocation; control key or first key is control)</label>
          <input class="obs-input" placeholder="control,treatment" value={formVariants}
            onInput={(e) => setFormVariants((e.target as HTMLInputElement).value)} />
        </div>
        <div class="flags-form-row">
          <div class="obs-form-group">
            <label class="obs-label">Goal Metric</label>
            <input class="obs-input" placeholder="purchase_completed" value={formGoal}
              onInput={(e) => setFormGoal((e.target as HTMLInputElement).value)} />
          </div>
          <div class="obs-form-group">
            <label class="obs-label">Goal Value (optional)</label>
            <input class="obs-input" placeholder="any" value={formGoalValue}
              onInput={(e) => setFormGoalValue((e.target as HTMLInputElement).value)} />
          </div>
        </div>
        <div class="obs-form-group">
          <label class="obs-label">Minimum Sample Size</label>
          <input class="obs-input" type="number" placeholder="100" value={formMinSample}
            onInput={(e) => setFormMinSample((e.target as HTMLInputElement).value)} />
        </div>
        <div style={{ display: "flex", justifyContent: "flex-end", gap: "8px", marginTop: "8px" }}>
          <button class="obs-btn" onClick={() => setShowCreate(false)}>Cancel</button>
          <button class="obs-btn obs-btn--primary" onClick={handleCreate}
            disabled={creating || !formName.trim() || !formFlagKey.trim() || !formGoal.trim()}>
            {creating ? "Creating..." : "Create"}
          </button>
        </div>
      </Modal>
    </div>
  );
}
