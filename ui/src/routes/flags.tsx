import { useState, useEffect, useCallback } from "preact/hooks";
import { flagsApi } from "../api/flags.js";
import type { FeatureFlag, FlagHistoryEntry } from "../api/flags.js";
import Modal from "../components/shared/Modal.js";
import CodeBlock from "../components/shared/CodeBlock.js";
import EmptyState from "../components/shared/EmptyState.js";
import "../styles/flags.css";

export const config = { mode: "app" };

const TARGETING_OPERATORS = [
  { value: "eq", label: "equals" },
  { value: "neq", label: "not equals" },
  { value: "in", label: "in list" },
  { value: "not_in", label: "not in list" },
  { value: "contains", label: "contains" },
];

interface TargetingRule {
  attribute: string;
  operator: string;
  value: string;
}

interface Variant {
  key: string;
  value: string;
  weight: number;
}

function parseTargeting(raw: string): TargetingRule[] {
  if (!raw) return [];
  try {
    const p = JSON.parse(raw);
    return Array.isArray(p) ? p : [];
  } catch { return []; }
}

function parseVariants(raw: string): Variant[] {
  if (!raw) return [];
  try {
    const p = JSON.parse(raw);
    return Array.isArray(p) ? p : [];
  } catch { return []; }
}

function FlagsSkeleton() {
  return (
    <div class="flags-loading">
      {Array.from({ length: 5 }).map((_, i) => (
        <div class="flags-skeleton-row" key={i}>
          <div class="flags-skeleton-bar" style={{ width: "36px", height: "20px", borderRadius: "10px" }} />
          <div style={{ flex: 1, display: "flex", flexDirection: "column", gap: "6px" }}>
            <div class="flags-skeleton-bar" style={{ width: "140px" }} />
            <div class="flags-skeleton-bar" style={{ width: "100px", height: "10px" }} />
          </div>
          <div class="flags-skeleton-bar" style={{ width: "50px" }} />
        </div>
      ))}
    </div>
  );
}

// ─── Targeting Rule Builder ───

function TargetingRuleBuilder({ rules, onChange }: { rules: TargetingRule[]; onChange: (r: TargetingRule[]) => void }) {
  const addRule = () => onChange([...rules, { attribute: "", operator: "eq", value: "" }]);
  const removeRule = (i: number) => onChange(rules.filter((_, idx) => idx !== i));
  const updateRule = (i: number, field: keyof TargetingRule, val: string) => {
    const updated = [...rules];
    updated[i] = { ...updated[i], [field]: val };
    onChange(updated);
  };

  return (
    <div>
      <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", marginBottom: "8px" }}>
        <label class="obs-label" style={{ margin: 0 }}>Targeting Rules</label>
        <button class="obs-btn obs-btn--sm" onClick={addRule} type="button">Add Rule</button>
      </div>
      {rules.length === 0 && (
        <div style={{ fontSize: "12px", color: "var(--obs-text-muted)", padding: "8px 0" }}>
          No targeting rules. Flag applies to all users within rollout %.
        </div>
      )}
      {rules.map((rule, i) => (
        <div key={i} style={{ display: "flex", gap: "6px", marginBottom: "6px", alignItems: "center" }}>
          <input class="obs-input" placeholder="attribute" value={rule.attribute}
            onInput={(e) => updateRule(i, "attribute", (e.target as HTMLInputElement).value)}
            style={{ flex: 1 }} />
          <select class="obs-select" value={rule.operator} style={{ flex: 1 }}
            onChange={(e) => updateRule(i, "operator", (e.target as HTMLSelectElement).value)}>
            {TARGETING_OPERATORS.map(o => <option key={o.value} value={o.value}>{o.label}</option>)}
          </select>
          <input class="obs-input" placeholder="value" value={rule.value}
            onInput={(e) => updateRule(i, "value", (e.target as HTMLInputElement).value)}
            style={{ flex: 1 }} />
          <button class="obs-btn obs-btn--sm obs-btn--danger" onClick={() => removeRule(i)} type="button"
            style={{ padding: "4px 8px" }}>
            <svg width="12" height="12" viewBox="0 0 24 24" fill="currentColor">
              <path d="M19 6.41L17.59 5 12 10.59 6.41 5 5 6.41 10.59 12 5 17.59 6.41 19 12 13.41 17.59 19 19 17.59 13.41 12z" />
            </svg>
          </button>
        </div>
      ))}
    </div>
  );
}

// ─── Variant Editor ───

function VariantEditor({ variants, onChange }: { variants: Variant[]; onChange: (v: Variant[]) => void }) {
  const addVariant = () => onChange([...variants, { key: "", value: "", weight: 50 }]);
  const removeVariant = (i: number) => onChange(variants.filter((_, idx) => idx !== i));
  const updateVariant = (i: number, field: keyof Variant, val: string | number) => {
    const updated = [...variants];
    updated[i] = { ...updated[i], [field]: val };
    onChange(updated);
  };

  return (
    <div>
      <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", marginBottom: "8px" }}>
        <label class="obs-label" style={{ margin: 0 }}>Variants</label>
        <button class="obs-btn obs-btn--sm" onClick={addVariant} type="button">Add Variant</button>
      </div>
      {variants.length === 0 && (
        <div style={{ fontSize: "12px", color: "var(--obs-text-muted)", padding: "8px 0" }}>
          No variants. Boolean flag returns true/false.
        </div>
      )}
      {variants.map((v, i) => (
        <div key={i} style={{ display: "flex", gap: "6px", marginBottom: "6px", alignItems: "center" }}>
          <input class="obs-input" placeholder="key" value={v.key}
            onInput={(e) => updateVariant(i, "key", (e.target as HTMLInputElement).value)}
            style={{ flex: 1 }} />
          <input class="obs-input" placeholder="value" value={v.value}
            onInput={(e) => updateVariant(i, "value", (e.target as HTMLInputElement).value)}
            style={{ flex: 1 }} />
          <input class="obs-input" type="number" placeholder="weight" value={v.weight}
            onInput={(e) => updateVariant(i, "weight", parseInt((e.target as HTMLInputElement).value) || 0)}
            style={{ width: "70px" }} />
          <button class="obs-btn obs-btn--sm obs-btn--danger" onClick={() => removeVariant(i)} type="button"
            style={{ padding: "4px 8px" }}>
            <svg width="12" height="12" viewBox="0 0 24 24" fill="currentColor">
              <path d="M19 6.41L17.59 5 12 10.59 6.41 5 5 6.41 10.59 12 5 17.59 6.41 19 12 13.41 17.59 19 19 17.59 13.41 12z" />
            </svg>
          </button>
        </div>
      ))}
    </div>
  );
}

// ─── Flag Detail (Expandable) ───

function FlagDetail({ flag }: { flag: FeatureFlag }) {
  const [testUserId, setTestUserId] = useState("");
  const [testResult, setTestResult] = useState<{ enabled: boolean; variant?: string } | null>(null);
  const [testing, setTesting] = useState(false);
  const [history, setHistory] = useState<FlagHistoryEntry[]>([]);

  const targeting = parseTargeting(flag.targeting);
  const variants = parseVariants(flag.variants);

  useEffect(() => {
    let alive = true;
    flagsApi.history(flag.flag_id)
      .then((h) => { if (alive) setHistory(h || []); })
      .catch(() => { if (alive) setHistory([]); });
    return () => { alive = false; };
  }, [flag.flag_id]);

  const handleTest = async () => {
    if (!testUserId.trim()) return;
    setTesting(true);
    try {
      const result = await flagsApi.evaluate(flag.site_id, flag.flag_key, testUserId.trim());
      setTestResult(result);
    } catch { setTestResult(null); }
    finally { setTesting(false); }
  };

  return (
    <div class="flags-detail">
      {flag.description && (
        <div style={{ fontSize: "12px", color: "var(--obs-text-secondary)", marginBottom: "12px" }}>
          {flag.description}
        </div>
      )}

      <div style={{ display: "flex", gap: "16px", flexWrap: "wrap", fontSize: "12px", color: "var(--obs-text-secondary)", marginBottom: "12px" }}>
        <span>Type: <strong>{flag.flag_type || "boolean"}</strong></span>
        <span>Rollout: <strong>{flag.rollout_pct}%</strong></span>
        <span>Key: <code style={{ fontSize: "11px" }}>{flag.flag_key}</code></span>
      </div>

      {targeting.length > 0 && (
        <div style={{ marginBottom: "12px" }}>
          <div style={{ fontSize: "11px", fontWeight: 600, color: "var(--obs-text-muted)", textTransform: "uppercase", marginBottom: "6px" }}>
            Targeting Rules ({targeting.length})
          </div>
          <CodeBlock code={JSON.stringify(targeting, null, 2)} maxHeight="200px" />
        </div>
      )}

      {variants.length > 0 && (
        <div style={{ marginBottom: "12px" }}>
          <div style={{ fontSize: "11px", fontWeight: 600, color: "var(--obs-text-muted)", textTransform: "uppercase", marginBottom: "6px" }}>
            Variants ({variants.length})
          </div>
          <CodeBlock code={JSON.stringify(variants, null, 2)} maxHeight="200px" />
        </div>
      )}

      {/* Evaluation Tester */}
      <div style={{ borderTop: "1px solid var(--obs-border-subtle)", paddingTop: "12px" }}>
        <div style={{ fontSize: "11px", fontWeight: 600, color: "var(--obs-text-muted)", textTransform: "uppercase", marginBottom: "6px" }}>
          Test Evaluation
        </div>
        <div style={{ display: "flex", gap: "8px", alignItems: "center" }}>
          <input class="obs-input" placeholder="User ID" value={testUserId}
            onInput={(e) => setTestUserId((e.target as HTMLInputElement).value)}
            onKeyDown={(e) => e.key === "Enter" && handleTest()}
            style={{ flex: 1 }} />
          <button class="obs-btn obs-btn--sm obs-btn--primary" onClick={handleTest} disabled={testing || !testUserId.trim()}>
            {testing ? "..." : "Evaluate"}
          </button>
        </div>
        {testResult !== null && (
          <div style={{ marginTop: "8px", padding: "8px 12px", background: "var(--obs-bg)", border: "1px solid var(--obs-border-subtle)", borderRadius: "var(--obs-radius)", fontSize: "12px" }}>
            <span style={{ fontWeight: 600, color: testResult.enabled ? "var(--obs-success)" : "var(--obs-text-muted)" }}>
              {testResult.enabled ? "ENABLED" : "DISABLED"}
            </span>
            {testResult.variant && (
              <span style={{ marginLeft: "8px", color: "var(--obs-text-secondary)" }}>
                variant: <strong>{testResult.variant}</strong>
              </span>
            )}
          </div>
        )}
      </div>

      {/* History */}
      <div style={{ borderTop: "1px solid var(--obs-border-subtle)", paddingTop: "12px", marginTop: "12px" }}>
        <div style={{ fontSize: "11px", fontWeight: 600, color: "var(--obs-text-muted)", textTransform: "uppercase", marginBottom: "6px" }}>
          History ({history.length})
        </div>
        {history.length === 0 ? (
          <div style={{ fontSize: "12px", color: "var(--obs-text-muted)" }}>No changes recorded.</div>
        ) : (
          <div class="flags-history">
            {history.map((h, i) => (
              <div key={i} class={`flags-history-entry flags-history-entry--${h.action}`}>
                <span class="flags-history-action">{h.action}</span>
                <span class="flags-history-detail">
                  {h.action === "toggle" && (h.enabled ? "enabled" : "disabled")}
                  {h.action === "created" && `initial rollout ${h.rollout_pct ?? 100}%`}
                </span>
                <span class="flags-history-time">
                  {new Date(h.timestamp).toLocaleString("en-US", { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit", hour12: false })}
                </span>
              </div>
            ))}
          </div>
        )}
      </div>
    </div>
  );
}

// ─── Main Page ───

export default function FlagsPage() {
  const siteId = typeof window !== "undefined"
    ? new URLSearchParams(window.location.search).get("site_id") || "default"
    : "default";

  const [flags, setFlags] = useState<FeatureFlag[]>([]);
  const [loading, setLoading] = useState(true);
  const [showCreate, setShowCreate] = useState(false);
  const [creating, setCreating] = useState(false);
  const [expandedId, setExpandedId] = useState<string | null>(null);

  // Create form state
  const [formKey, setFormKey] = useState("");
  const [formName, setFormName] = useState("");
  const [formDesc, setFormDesc] = useState("");
  const [formType, setFormType] = useState("boolean");
  const [formRollout, setFormRollout] = useState("100");
  const [formTargeting, setFormTargeting] = useState<TargetingRule[]>([]);
  const [formVariants, setFormVariants] = useState<Variant[]>([]);

  const fetchFlags = useCallback(async () => {
    setLoading(true);
    try {
      const data = await flagsApi.list(siteId);
      setFlags(data || []);
    } catch { setFlags([]); }
    finally { setLoading(false); }
  }, [siteId]);

  useEffect(() => { fetchFlags(); }, [fetchFlags]);

  const handleToggle = async (flag: FeatureFlag) => {
    const newEnabled = !flag.enabled;
    try {
      await flagsApi.toggle(flag.flag_id, newEnabled);
      setFlags(prev => prev.map(f =>
        f.flag_id === flag.flag_id ? { ...f, enabled: newEnabled } : f
      ));
    } catch (err) { console.error("Failed to toggle flag:", err); }
  };

  const handleCreate = async () => {
    if (!formKey.trim() || !formName.trim()) return;
    setCreating(true);
    try {
      await flagsApi.create({
        site_id: siteId,
        flag_key: formKey.trim(),
        name: formName.trim(),
        description: formDesc.trim() || undefined,
        flag_type: formType,
        rollout_pct: parseInt(formRollout) || 100,
        targeting: formTargeting.length > 0 ? JSON.stringify(formTargeting) : undefined,
        variants: formVariants.length > 0 ? JSON.stringify(formVariants) : undefined,
      });
      setShowCreate(false);
      resetForm();
      fetchFlags();
    } catch (err) { console.error("Failed to create flag:", err); }
    finally { setCreating(false); }
  };

  const resetForm = () => {
    setFormKey(""); setFormName(""); setFormDesc(""); setFormType("boolean");
    setFormRollout("100"); setFormTargeting([]); setFormVariants([]);
  };

  return (
    <div>
      <div class="obs-page-header">
        <h1 class="obs-page-title">Feature Flags</h1>
        <div class="obs-page-actions">
          <button class="obs-btn obs-btn--primary" onClick={() => setShowCreate(true)}>
            Create Flag
          </button>
        </div>
      </div>

      {loading ? (
        <FlagsSkeleton />
      ) : flags.length === 0 ? (
        <EmptyState
          title="No feature flags yet"
          description="Gate features by user, rollout %, or targeting rules. Call POST /api/v1/flags/evaluate from your backend — responses take under 10ms."
          icon="layers"
          actions={[
            { label: "Create first flag", onClick: () => setShowCreate(true), primary: true },
          ]}
        />
      ) : (
        <div class="flags-list">
          {flags.map(flag => {
            const isEnabled = flag.enabled;
            const isExpanded = expandedId === flag.flag_id;
            const targeting = parseTargeting(flag.targeting);
            return (
              <div key={flag.flag_id}>
                <div class="flags-row" onClick={() => setExpandedId(isExpanded ? null : flag.flag_id)} style={{ cursor: "pointer" }}>
                  <label class="flags-toggle" onClick={(e) => e.stopPropagation()}>
                    <input type="checkbox" checked={isEnabled} onChange={() => handleToggle(flag)} />
                    <div class="flags-toggle-track" />
                    <div class="flags-toggle-thumb" />
                  </label>
                  <div class="flags-row-info">
                    <div class="flags-row-name">{flag.name}</div>
                    <div class="flags-row-key">{flag.flag_key}</div>
                  </div>
                  <div class="flags-row-meta">
                    {targeting.length > 0 && (
                      <span class="flags-row-type" style={{ background: "rgba(99, 102, 241, 0.1)", color: "var(--obs-accent)" }}>
                        {targeting.length} rule{targeting.length > 1 ? "s" : ""}
                      </span>
                    )}
                    <span class="flags-row-type">{flag.flag_type || "boolean"}</span>
                    <span class="flags-row-rollout">{flag.rollout_pct}%</span>
                  </div>
                </div>
                {isExpanded && (
                  <div style={{ padding: "0 16px 16px", borderBottom: "1px solid var(--obs-border-subtle)" }}>
                    <FlagDetail flag={flag} />
                  </div>
                )}
              </div>
            );
          })}
        </div>
      )}

      <Modal open={showCreate} onClose={() => { setShowCreate(false); resetForm(); }} title="Create Feature Flag">
        <div class="obs-form-group">
          <label class="obs-label">Flag Key</label>
          <input class="obs-input" placeholder="my-feature" value={formKey}
            onInput={(e) => setFormKey((e.target as HTMLInputElement).value)} />
        </div>
        <div class="obs-form-group">
          <label class="obs-label">Name</label>
          <input class="obs-input" placeholder="My Feature" value={formName}
            onInput={(e) => setFormName((e.target as HTMLInputElement).value)} />
        </div>
        <div class="obs-form-group">
          <label class="obs-label">Description</label>
          <input class="obs-input" placeholder="Optional description" value={formDesc}
            onInput={(e) => setFormDesc((e.target as HTMLInputElement).value)} />
        </div>
        <div class="flags-form-row">
          <div class="obs-form-group">
            <label class="obs-label">Type</label>
            <select class="obs-select" value={formType}
              onChange={(e) => setFormType((e.target as HTMLSelectElement).value)}>
              <option value="boolean">Boolean</option>
              <option value="string">String</option>
              <option value="number">Number</option>
              <option value="json">JSON</option>
            </select>
          </div>
          <div class="obs-form-group">
            <label class="obs-label">Rollout %</label>
            <input class="obs-input" type="number" min="0" max="100" value={formRollout}
              onInput={(e) => setFormRollout((e.target as HTMLInputElement).value)} />
          </div>
        </div>

        <div class="obs-form-group">
          <TargetingRuleBuilder rules={formTargeting} onChange={setFormTargeting} />
        </div>

        {formType !== "boolean" && (
          <div class="obs-form-group">
            <VariantEditor variants={formVariants} onChange={setFormVariants} />
          </div>
        )}

        <div style={{ display: "flex", justifyContent: "flex-end", gap: "8px", marginTop: "8px" }}>
          <button class="obs-btn" onClick={() => { setShowCreate(false); resetForm(); }}>Cancel</button>
          <button class="obs-btn obs-btn--primary" onClick={handleCreate} disabled={creating || !formKey.trim() || !formName.trim()}>
            {creating ? "Creating..." : "Create"}
          </button>
        </div>
      </Modal>
    </div>
  );
}
