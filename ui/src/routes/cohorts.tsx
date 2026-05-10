import { useState, useEffect, useCallback, useMemo } from "preact/hooks";
import { cohortsApi, parseRule } from "../api/persons.js";
import type { Cohort, CohortDefinition, CohortRule } from "../api/persons.js";
import Modal from "../components/shared/Modal.js";
import EmptyState from "../components/shared/EmptyState.js";
import { useFilters } from "../hooks/useFilters.js";

export const config = { mode: "app" };

const PROPERTY_KEYS = [
  "country", "region", "city",
  "browser", "os", "device", "language",
  "utm_source", "utm_medium", "utm_campaign",
  "pathname", "referrer",
];

function fmtDate(ms: number): string {
  if (!ms) return "—";
  try {
    return new Date(ms).toLocaleString("en-US", {
      month: "short", day: "numeric",
      hour: "2-digit", minute: "2-digit", hour12: false,
    });
  } catch { return String(ms); }
}

// ---------------------------------------------------------------------------
// Rule editor — builds CohortDefinition (op="and"; v1 doesn't expose OR / nesting)
// ---------------------------------------------------------------------------

function emptyEventRule(): CohortRule {
  return { type: "event", name: "", window: "30d", min_count: 1 };
}
function emptyPropertyRule(): CohortRule {
  return { type: "property", key: "country", operator: "=", value: "" };
}

function RuleEditor({ rules, onChange }:
  { rules: CohortRule[]; onChange: (r: CohortRule[]) => void }) {

  const update = (idx: number, patch: Partial<CohortRule>) => {
    onChange(rules.map((r, i) => i === idx ? { ...r, ...patch } : r));
  };
  const remove = (idx: number) => onChange(rules.filter((_, i) => i !== idx));

  return (
    <div>
      {rules.length === 0 && (
        <div style={{ fontSize: "12px", color: "var(--obs-text-muted)",
          padding: "12px", border: "1px dashed var(--obs-border)",
          borderRadius: "var(--obs-radius-md)", marginBottom: "8px" }}>
          No conditions yet. Add one below.
        </div>
      )}

      {rules.map((rule, i) => (
        <div key={i} style={{ display: "flex", flexDirection: "column", gap: "8px",
          padding: "12px", marginBottom: "8px",
          border: "1px solid var(--obs-border)", borderRadius: "var(--obs-radius-md)",
          background: "var(--obs-card)" }}>
          <div style={{ display: "flex", gap: "8px", alignItems: "center" }}>
            <span style={{ fontSize: "11px", color: "var(--obs-text-muted)",
              textTransform: "uppercase" }}>
              {i === 0 ? "Where" : "And"}
            </span>
            <select class="obs-select obs-select--sm" value={rule.type}
              onChange={(e) => {
                const next = (e.target as HTMLSelectElement).value as CohortRule["type"];
                update(i, next === "event" ? emptyEventRule() : emptyPropertyRule());
              }}>
              <option value="event">Performed event</option>
              <option value="property">Has property</option>
            </select>
            <button class="obs-btn obs-btn--sm" onClick={() => remove(i)} type="button"
              style={{ marginLeft: "auto" }}>
              Remove
            </button>
          </div>

          {rule.type === "event" && (
            <div style={{ display: "flex", gap: "8px", flexWrap: "wrap", alignItems: "center" }}>
              <input class="obs-input" placeholder="event name (e.g. purchase)"
                value={rule.name || ""} style={{ flex: 1, minWidth: "180px" }}
                onInput={(e) => update(i, { name: (e.target as HTMLInputElement).value })} />
              <span style={{ fontSize: "12px", color: "var(--obs-text-muted)" }}>at least</span>
              <input class="obs-input" type="number" min={1} value={rule.min_count ?? 1}
                style={{ width: "70px" }}
                onInput={(e) => update(i, { min_count: parseInt((e.target as HTMLInputElement).value, 10) || 1 })} />
              <span style={{ fontSize: "12px", color: "var(--obs-text-muted)" }}>time(s) in last</span>
              <select class="obs-select obs-select--sm" value={rule.window || "30d"}
                onChange={(e) => update(i, { window: (e.target as HTMLSelectElement).value })}>
                <option value="24h">24 hours</option>
                <option value="7d">7 days</option>
                <option value="30d">30 days</option>
              </select>
            </div>
          )}

          {rule.type === "property" && (
            <div style={{ display: "flex", gap: "8px", flexWrap: "wrap" }}>
              <select class="obs-select obs-select--sm" value={rule.key || "country"}
                style={{ flex: "0 0 140px" }}
                onChange={(e) => update(i, { key: (e.target as HTMLSelectElement).value })}>
                {PROPERTY_KEYS.map(k => <option key={k} value={k}>{k}</option>)}
              </select>
              <select class="obs-select obs-select--sm" value={rule.operator || "="}
                style={{ flex: "0 0 80px" }}
                onChange={(e) => update(i, { operator: (e.target as HTMLSelectElement).value as "=" | "!=" })}>
                <option value="=">equals</option>
                <option value="!=">not equals</option>
              </select>
              <input class="obs-input" placeholder="value"
                value={rule.value || ""} style={{ flex: 1, minWidth: "120px" }}
                onInput={(e) => update(i, { value: (e.target as HTMLInputElement).value })} />
            </div>
          )}
        </div>
      ))}

      <div style={{ display: "flex", gap: "8px", marginTop: "8px" }}>
        <button class="obs-btn obs-btn--sm" type="button"
          onClick={() => onChange([...rules, emptyEventRule()])}>
          + Event condition
        </button>
        <button class="obs-btn obs-btn--sm" type="button"
          onClick={() => onChange([...rules, emptyPropertyRule()])}>
          + Property condition
        </button>
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Builder modal
// ---------------------------------------------------------------------------

function CohortBuilder({ open, onClose, onSave, siteId, initial }:
  {
    open: boolean;
    onClose: () => void;
    onSave: (saved: Cohort) => void;
    siteId: string;
    initial?: Cohort | null;
  }) {
  const [name, setName] = useState(initial?.name || "");
  const [description, setDescription] = useState(initial?.description || "");
  const [rules, setRules] = useState<CohortRule[]>(
    initial ? parseRule(initial.rule).rules : [emptyEventRule()]
  );
  const [previewCount, setPreviewCount] = useState<number | null>(null);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Reset state when re-opened with a different cohort.
  useEffect(() => {
    if (!open) return;
    setName(initial?.name || "");
    setDescription(initial?.description || "");
    setRules(initial ? parseRule(initial.rule).rules : [emptyEventRule()]);
    setPreviewCount(null);
    setError(null);
  }, [open, initial]);

  const def = useMemo<CohortDefinition>(() => ({ op: "and", rules }), [rules]);

  const preview = async () => {
    setError(null);
    try {
      const r = await cohortsApi.preview(siteId, def);
      setPreviewCount(r.count);
    } catch (e: any) {
      setError(e?.message || "Preview failed");
    }
  };

  const save = async () => {
    setError(null);
    if (!name.trim()) { setError("Name required"); return; }
    if (rules.length === 0) { setError("Add at least one condition"); return; }
    setSaving(true);
    try {
      const saved = initial
        ? await cohortsApi.update(initial.cohort_id, { site_id: siteId, name, description, rule: def })
        : await cohortsApi.create({ site_id: siteId, name, description, rule: def });
      onSave(saved);
      onClose();
    } catch (e: any) {
      setError(e?.message || "Save failed");
    } finally {
      setSaving(false);
    }
  };

  return (
    <Modal open={open} onClose={onClose} title={initial ? "Edit cohort" : "New cohort"}>
      <div class="obs-form-group">
        <label class="obs-label">Name</label>
        <input class="obs-input" placeholder="e.g. Power users"
          value={name} onInput={(e) => setName((e.target as HTMLInputElement).value)}
          data-testid="cohort-name-input" />
      </div>

      <div class="obs-form-group">
        <label class="obs-label">Description (optional)</label>
        <input class="obs-input" placeholder="What this cohort represents"
          value={description}
          onInput={(e) => setDescription((e.target as HTMLInputElement).value)} />
      </div>

      <div class="obs-form-group">
        <label class="obs-label">Conditions (all must match)</label>
        <RuleEditor rules={rules} onChange={setRules} />
      </div>

      {error && (
        <div style={{ color: "var(--obs-danger)", fontSize: "12px", marginBottom: "8px" }}>
          {error}
        </div>
      )}

      <div style={{ display: "flex", justifyContent: "space-between",
        alignItems: "center", marginTop: "12px", gap: "8px", flexWrap: "wrap" }}>
        <button class="obs-btn" onClick={preview} type="button" data-testid="cohort-preview-btn">
          Preview count {previewCount !== null && `→ ${previewCount.toLocaleString()}`}
        </button>
        <div style={{ display: "flex", gap: "8px", marginLeft: "auto" }}>
          <button class="obs-btn" onClick={onClose} type="button">Cancel</button>
          <button class="obs-btn obs-btn--primary" onClick={save} type="button"
            disabled={saving} data-testid="cohort-save-btn">
            {saving ? "Saving…" : initial ? "Save" : "Create"}
          </button>
        </div>
      </div>
    </Modal>
  );
}

// ---------------------------------------------------------------------------
// Detail panel
// ---------------------------------------------------------------------------

function CohortDetail({ cohort, siteId, onClose, onChanged }:
  {
    cohort: Cohort;
    siteId: string;
    onClose: () => void;
    onChanged: () => void;
  }) {
  const [editing, setEditing] = useState(false);
  const [refreshing, setRefreshing] = useState(false);
  const [members, setMembers] = useState<string[]>([]);
  const [showMembers, setShowMembers] = useState(false);
  const [memLoading, setMemLoading] = useState(false);
  const [current, setCurrent] = useState<Cohort>(cohort);

  useEffect(() => { setCurrent(cohort); }, [cohort]);

  const refresh = async () => {
    setRefreshing(true);
    try {
      const next = await cohortsApi.refresh(current.cohort_id, siteId);
      setCurrent(next);
      onChanged();
    } finally { setRefreshing(false); }
  };

  const loadMembers = async () => {
    setShowMembers(true);
    setMemLoading(true);
    try {
      const r = await cohortsApi.members(current.cohort_id, siteId, { limit: 100 });
      setMembers(r.members || []);
    } finally { setMemLoading(false); }
  };

  const remove = async () => {
    if (!confirm(`Delete cohort "${current.name}"?`)) return;
    await cohortsApi.delete(current.cohort_id, siteId);
    onChanged();
    onClose();
  };

  const def = parseRule(current.rule);
  const useAsFilter = `/insights?site_id=${encodeURIComponent(siteId)}&cohort_id=${encodeURIComponent(current.cohort_id)}`;

  return (
    <div style={{ marginTop: "16px", padding: "16px", border: "1px solid var(--obs-border)",
      borderRadius: "var(--obs-radius-md)", background: "var(--obs-card)" }}>
      <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between" }}>
        <div>
          <h2 style={{ fontSize: "16px", margin: 0 }}>{current.name}</h2>
          {current.description && (
            <div style={{ fontSize: "12px", color: "var(--obs-text-muted)", marginTop: "4px" }}>
              {current.description}
            </div>
          )}
        </div>
        <button class="obs-btn obs-btn--sm" onClick={onClose}>Close</button>
      </div>

      <div style={{ display: "flex", gap: "16px", marginTop: "12px", flexWrap: "wrap",
        fontSize: "12px", color: "var(--obs-text-muted)" }}>
        <span>{current.member_count.toLocaleString()} members</span>
        <span>Updated {fmtDate(current.updated_at)}</span>
      </div>

      <div style={{ marginTop: "16px" }}>
        <div style={{ fontSize: "11px", color: "var(--obs-text-muted)",
          textTransform: "uppercase", marginBottom: "6px" }}>Rule</div>
        <ul style={{ margin: 0, paddingLeft: "20px", fontSize: "13px" }}>
          {def.rules.map((r, i) => (
            <li key={i}>
              {r.type === "event"
                ? `did "${r.name}" at least ${r.min_count ?? 1} time(s) in ${r.window || "30d"}`
                : `${r.key} ${r.operator || "="} ${r.value || ""}`}
            </li>
          ))}
        </ul>
      </div>

      <div style={{ display: "flex", gap: "8px", marginTop: "16px", flexWrap: "wrap" }}>
        <button class="obs-btn obs-btn--sm" onClick={refresh} disabled={refreshing}
          data-testid="cohort-refresh-btn">
          {refreshing ? "Refreshing…" : "Refresh"}
        </button>
        <button class="obs-btn obs-btn--sm" onClick={() => setEditing(true)}>Edit</button>
        <button class="obs-btn obs-btn--sm" onClick={loadMembers}>View members</button>
        <a class="obs-btn obs-btn--sm obs-btn--primary" href={useAsFilter}
          data-testid="cohort-use-as-filter">
          Use as filter
        </a>
        <button class="obs-btn obs-btn--sm obs-btn--danger" onClick={remove}
          style={{ marginLeft: "auto" }}>
          Delete
        </button>
      </div>

      {showMembers && (
        <div style={{ marginTop: "16px", padding: "12px",
          background: "var(--obs-bg)", borderRadius: "var(--obs-radius-md)" }}>
          <div style={{ fontSize: "11px", color: "var(--obs-text-muted)",
            marginBottom: "6px", textTransform: "uppercase" }}>
            Members ({members.length}{members.length === 100 ? "+" : ""})
          </div>
          {memLoading ? (
            <div style={{ fontSize: "12px" }}>Loading…</div>
          ) : members.length === 0 ? (
            <div style={{ fontSize: "12px", color: "var(--obs-text-muted)" }}>No members</div>
          ) : (
            <div style={{ fontFamily: "var(--obs-font-mono, monospace)",
              fontSize: "11px", maxHeight: "240px", overflow: "auto",
              display: "flex", flexDirection: "column", gap: "2px" }}>
              {members.map(m => <div key={m}>{m}</div>)}
            </div>
          )}
        </div>
      )}

      <CohortBuilder
        open={editing}
        onClose={() => setEditing(false)}
        siteId={siteId}
        initial={current}
        onSave={(updated) => {
          setCurrent(updated);
          onChanged();
        }}
      />
    </div>
  );
}

// ---------------------------------------------------------------------------
// Main page
// ---------------------------------------------------------------------------

export default function CohortsPage() {
  const { state: { siteId } } = useFilters();
  const [cohorts, setCohorts] = useState<Cohort[]>([]);
  const [loading, setLoading] = useState(true);
  const [creating, setCreating] = useState(false);
  const [selectedID, setSelectedID] = useState<string | null>(null);

  const fetchCohorts = useCallback(async () => {
    setLoading(true);
    try {
      const data = await cohortsApi.list(siteId);
      setCohorts(data || []);
    } catch {
      setCohorts([]);
    } finally {
      setLoading(false);
    }
  }, [siteId]);

  useEffect(() => { fetchCohorts(); }, [fetchCohorts]);

  const selected = useMemo(
    () => cohorts.find(c => c.cohort_id === selectedID) || null,
    [cohorts, selectedID]
  );

  return (
    <div>
      <div class="obs-page-header" style={{ display: "flex", alignItems: "center" }}>
        <h1 class="obs-page-title">Cohorts</h1>
        <div style={{ marginLeft: "auto" }}>
          <button class="obs-btn obs-btn--primary" onClick={() => setCreating(true)}
            data-testid="cohort-new-btn">
            + New cohort
          </button>
        </div>
      </div>

      {loading ? (
        <div class="obs-empty-state">Loading cohorts…</div>
      ) : cohorts.length === 0 ? (
        <EmptyState
          title="No cohorts yet"
          description="Cohorts let you slice analytics by user behaviour — e.g. 'people who hit /pricing in the last 7 days' or 'users from the US who clicked Buy'."
          icon="layers"
          actions={[
            { label: "Create your first cohort", onClick: () => setCreating(true), primary: true },
          ]}
        />
      ) : (
        <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fill, minmax(260px, 1fr))",
          gap: "12px" }}>
          {cohorts.map(c => (
            <div key={c.cohort_id}
              onClick={() => setSelectedID(c.cohort_id)}
              data-testid="cohort-card"
              style={{ padding: "14px", border: "1px solid var(--obs-border)",
                borderRadius: "var(--obs-radius-md)", background: "var(--obs-card)",
                cursor: "pointer",
                outline: c.cohort_id === selectedID ? "2px solid var(--obs-accent)" : "none" }}>
              <div style={{ fontSize: "14px", fontWeight: 600 }}>{c.name}</div>
              {c.description && (
                <div style={{ fontSize: "12px", color: "var(--obs-text-muted)",
                  marginTop: "4px", overflow: "hidden", textOverflow: "ellipsis",
                  whiteSpace: "nowrap" }}>
                  {c.description}
                </div>
              )}
              <div style={{ display: "flex", gap: "12px", marginTop: "10px",
                fontSize: "12px", color: "var(--obs-text-muted)" }}>
                <span>{c.member_count.toLocaleString()} members</span>
                <span style={{ marginLeft: "auto" }}>{fmtDate(c.updated_at)}</span>
              </div>
            </div>
          ))}
        </div>
      )}

      {selected && (
        <CohortDetail cohort={selected} siteId={siteId}
          onClose={() => setSelectedID(null)}
          onChanged={fetchCohorts} />
      )}

      <CohortBuilder
        open={creating}
        onClose={() => setCreating(false)}
        siteId={siteId}
        onSave={(saved) => {
          setSelectedID(saved.cohort_id);
          fetchCohorts();
        }}
      />
    </div>
  );
}
