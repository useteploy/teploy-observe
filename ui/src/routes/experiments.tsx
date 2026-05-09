import { useState, useEffect, useCallback } from "preact/hooks";
import { experimentsApi } from "../api/flags.js";
import type { Experiment, ExperimentResults } from "../api/flags.js";
import StatusBadge from "../components/shared/StatusBadge.js";
import Modal from "../components/shared/Modal.js";
import ExportButton from "../components/shared/ExportButton.js";
import "../styles/flags.css";

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

function ResultsPanel({ experimentId }: { experimentId: string }) {
  const [results, setResults] = useState<ExperimentResults | null>(null);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    setLoading(true);
    experimentsApi.results(experimentId)
      .then(r => setResults(r))
      .catch(() => setResults(null))
      .finally(() => setLoading(false));
  }, [experimentId]);

  if (loading) return <div class="obs-empty-state">Loading results...</div>;
  if (!results || !results.variants?.length) return <div class="obs-empty-state">No results yet</div>;

  const maxRate = Math.max(...results.variants.map(v => v.conversion_rate), 0.001);
  const totalExposures = results.variants.reduce((sum, v) => sum + v.exposures, 0);

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
                {(v.conversion_rate * 100).toFixed(2)}%
              </span>
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
    </div>
  );
}

export default function ExperimentsPage() {
  const siteId = typeof window !== "undefined"
    ? new URLSearchParams(window.location.search).get("site_id") || "default"
    : "default";

  const [experiments, setExperiments] = useState<Experiment[]>([]);
  const [loading, setLoading] = useState(true);
  const [showCreate, setShowCreate] = useState(false);
  const [creating, setCreating] = useState(false);
  const [expandedId, setExpandedId] = useState<string | null>(null);

  // Create form
  const [formName, setFormName] = useState("");
  const [formFlagKey, setFormFlagKey] = useState("");
  const [formVariants, setFormVariants] = useState("control,treatment");
  const [formGoal, setFormGoal] = useState("");
  const [formGoalValue, setFormGoalValue] = useState("");
  const [formMinSample, setFormMinSample] = useState("100");

  const fetchExperiments = useCallback(async () => {
    setLoading(true);
    try {
      const data = await experimentsApi.list(siteId);
      setExperiments(data || []);
    } catch { setExperiments([]); }
    finally { setLoading(false); }
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
    setCreating(true);
    try {
      await experimentsApi.create({
        site_id: siteId,
        name: formName.trim(),
        flag_key: formFlagKey.trim(),
        variants: formVariants.trim(),
        goal_metric: formGoal.trim(),
      });
      setShowCreate(false);
      setFormName(""); setFormFlagKey(""); setFormVariants("control,treatment");
      setFormGoal(""); setFormGoalValue(""); setFormMinSample("100");
      fetchExperiments();
    } catch (err) { console.error("Failed to create experiment:", err); }
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
                  <ResultsPanel experimentId={exp.experiment_id} />
                </div>
              )}
            </div>
          ))}
        </div>
      )}

      <Modal open={showCreate} onClose={() => setShowCreate(false)} title="Create Experiment">
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
          <label class="obs-label">Variants (comma-separated)</label>
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
