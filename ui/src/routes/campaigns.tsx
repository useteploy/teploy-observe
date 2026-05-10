import { useState, useEffect, useCallback } from "preact/hooks";
import { analyticsApi } from "../api/analytics.js";
import type { UTMStat, AttributionRow, AttributionModel } from "../api/analytics.js";
import { useFilters } from "../hooks/useFilters.js";

export const config = { mode: "app" };

function formatNumber(n: number): string {
  return n.toLocaleString();
}

// Attribution rows can be fractional (linear model splits 1/N per unique
// source) so we render with one decimal when non-integer, integer otherwise.
function formatCredit(n: number): string {
  if (Number.isInteger(n)) return n.toLocaleString();
  return n.toFixed(2);
}

interface CampaignGroupProps {
  title: string;
  data: UTMStat[];
  total: number;
  color: string;
}

function CampaignGroup({ title, data, total, color }: CampaignGroupProps) {
  if (!data.length) {
    return (
      <div class="obs-card-static">
        <h3 class="obs-section-title" style="margin-bottom:12px;">{title}</h3>
        <div class="obs-empty-state" style="min-height:100px;">No data</div>
      </div>
    );
  }

  const max = Math.max(...data.map(d => d.visitors), 1);

  return (
    <div class="obs-card-static">
      <h3 class="obs-section-title" style="margin-bottom:12px;">{title}</h3>
      <div style={{ display: "flex", flexDirection: "column", gap: "2px" }}>
        {data.slice(0, 10).map((item, i) => {
          const pct = (item.visitors / max) * 100;
          const sharePct = total > 0 ? (item.visitors / total) * 100 : 0;
          return (
            <div key={i} style={{ position: "relative", padding: "6px 10px", fontSize: "12px", borderRadius: "var(--obs-radius)" }}>
              <div style={{
                position: "absolute", left: 0, top: 0, bottom: 0,
                width: `${pct}%`, background: color, opacity: 0.1, borderRadius: "var(--obs-radius)",
              }} />
              <div style={{ display: "flex", justifyContent: "space-between", position: "relative", zIndex: 1, gap: "8px" }}>
                <span style={{ color: "var(--obs-text)", overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>
                  {item.value || "(not set)"}
                </span>
                <span style={{ color: "var(--obs-text-secondary)", fontVariantNumeric: "tabular-nums", flexShrink: 0 }}>
                  {formatNumber(item.visitors)}
                  <span style={{ color: "var(--obs-text-muted)", marginLeft: "6px" }}>{sharePct.toFixed(1)}%</span>
                </span>
              </div>
            </div>
          );
        })}
      </div>
    </div>
  );
}

// Inline tab type so we don't have to plumb it through the file footer.
type TabId = "overview" | "attribution";

interface TabsProps {
  active: TabId;
  onChange: (id: TabId) => void;
}

function Tabs({ active, onChange }: TabsProps) {
  const tabs: Array<{ id: TabId; label: string }> = [
    { id: "overview", label: "Overview" },
    { id: "attribution", label: "Attribution" },
  ];
  return (
    <div
      data-testid="campaigns-tabs"
      style={{
        display: "flex",
        gap: "4px",
        marginBottom: "16px",
        borderBottom: "1px solid var(--obs-border)",
      }}
    >
      {tabs.map(t => {
        const isActive = t.id === active;
        return (
          <button
            key={t.id}
            type="button"
            data-testid={`campaigns-tab-${t.id}`}
            onClick={() => onChange(t.id)}
            style={{
              background: "transparent",
              border: "none",
              padding: "8px 14px",
              fontSize: "13px",
              fontWeight: isActive ? 600 : 500,
              color: isActive ? "var(--obs-accent)" : "var(--obs-text-secondary)",
              borderBottom: isActive ? "2px solid var(--obs-accent)" : "2px solid transparent",
              cursor: "pointer",
              marginBottom: "-1px",
            }}
          >
            {t.label}
          </button>
        );
      })}
    </div>
  );
}

interface AttributionTabProps {
  siteId: string;
  from: string;
  to: string;
}

function AttributionTab({ siteId, from, to }: AttributionTabProps) {
  const [model, setModel] = useState<AttributionModel>("first");
  const [rows, setRows] = useState<AttributionRow[]>([]);
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState<string | null>(null);

  const load = useCallback(async () => {
    setLoading(true);
    setErr(null);
    try {
      const r = await analyticsApi.attribution(siteId, from, to, model);
      setRows(r || []);
    } catch (e) {
      setErr((e as Error).message);
      setRows([]);
    } finally {
      setLoading(false);
    }
  }, [siteId, from, to, model]);

  useEffect(() => { load(); }, [load]);

  const models: Array<{ id: AttributionModel; label: string; hint: string }> = [
    { id: "first", label: "First-touch", hint: "Credit goes to the first utm_source in each session." },
    { id: "last", label: "Last-touch", hint: "Credit goes to the last utm_source in each session." },
    { id: "linear", label: "Linear", hint: "Credit is split equally across every utm_source in each session." },
  ];
  const activeHint = models.find(m => m.id === model)?.hint ?? "";

  return (
    <div data-testid="campaigns-attribution-panel">
      <div style={{ display: "flex", alignItems: "center", gap: "8px", marginBottom: "8px", flexWrap: "wrap" }}>
        <div role="radiogroup" aria-label="Attribution model" style={{ display: "inline-flex", gap: "4px" }}>
          {models.map(m => {
            const isActive = m.id === model;
            return (
              <button
                key={m.id}
                type="button"
                role="radio"
                aria-checked={isActive}
                data-testid={`attribution-model-${m.id}`}
                onClick={() => setModel(m.id)}
                style={{
                  background: isActive ? "var(--obs-accent)" : "var(--obs-surface)",
                  color: isActive ? "white" : "var(--obs-text)",
                  border: "1px solid var(--obs-border)",
                  borderRadius: "var(--obs-radius)",
                  padding: "6px 12px",
                  fontSize: "12px",
                  fontWeight: 500,
                  cursor: "pointer",
                }}
              >
                {m.label}
              </button>
            );
          })}
        </div>
        <span style={{ fontSize: "11px", color: "var(--obs-text-muted)" }}>{activeHint}</span>
      </div>

      {loading ? (
        <div class="obs-empty-state">Loading attribution...</div>
      ) : err ? (
        <div class="obs-empty-state" style="color:var(--obs-error,#ef4444);">Failed to load attribution: {err}</div>
      ) : rows.length === 0 ? (
        <div class="obs-empty-state" data-testid="attribution-empty">
          No UTM-tagged traffic in the window. Add utm_source params to your campaign URLs.
        </div>
      ) : (
        <div class="obs-card-static" style={{ padding: 0, overflow: "hidden" }}>
          <table data-testid="attribution-table" style={{ width: "100%", borderCollapse: "collapse", fontSize: "13px" }}>
            <thead>
              <tr style={{ background: "var(--obs-surface)", textAlign: "left" }}>
                <th style={{ padding: "10px 12px", fontSize: "11px", textTransform: "uppercase", letterSpacing: "0.5px", color: "var(--obs-text-muted)" }}>Source</th>
                <th style={{ padding: "10px 12px", fontSize: "11px", textTransform: "uppercase", letterSpacing: "0.5px", color: "var(--obs-text-muted)", textAlign: "right" }}>Sessions</th>
                <th style={{ padding: "10px 12px", fontSize: "11px", textTransform: "uppercase", letterSpacing: "0.5px", color: "var(--obs-text-muted)", textAlign: "right" }}>Conversions</th>
                <th style={{ padding: "10px 12px", fontSize: "11px", textTransform: "uppercase", letterSpacing: "0.5px", color: "var(--obs-text-muted)", textAlign: "right" }}>Conversion %</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((r, i) => (
                <tr key={`${r.source}-${i}`} style={{ borderTop: "1px solid var(--obs-border)" }}>
                  <td style={{ padding: "10px 12px", color: "var(--obs-text)" }}>{r.source || "(not set)"}</td>
                  <td style={{ padding: "10px 12px", color: "var(--obs-text-secondary)", textAlign: "right", fontVariantNumeric: "tabular-nums" }}>{formatCredit(r.sessions)}</td>
                  <td style={{ padding: "10px 12px", color: "var(--obs-text-secondary)", textAlign: "right", fontVariantNumeric: "tabular-nums" }}>{formatCredit(r.conversions)}</td>
                  <td style={{ padding: "10px 12px", color: "var(--obs-text-secondary)", textAlign: "right", fontVariantNumeric: "tabular-nums" }}>{r.conversion_pct.toFixed(1)}%</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

export default function CampaignsPage() {
  const { state: { siteId } } = useFilters();

  const [tab, setTab] = useState<TabId>("overview");
  const [sources, setSources] = useState<UTMStat[]>([]);
  const [mediums, setMediums] = useState<UTMStat[]>([]);
  const [campaigns, setCampaigns] = useState<UTMStat[]>([]);
  const [terms, setTerms] = useState<UTMStat[]>([]);
  const [contents, setContents] = useState<UTMStat[]>([]);
  const [loading, setLoading] = useState(true);

  const now = new Date();
  const from = new Date(now.getTime() - 30 * 86400000).toISOString();
  const to = now.toISOString();

  const fetch = useCallback(async () => {
    setLoading(true);
    try {
      const [src, med, cam, ter, con] = await Promise.all([
        analyticsApi.utm(siteId, from, to, "source", 20).catch(() => []),
        analyticsApi.utm(siteId, from, to, "medium", 20).catch(() => []),
        analyticsApi.utm(siteId, from, to, "campaign", 20).catch(() => []),
        analyticsApi.utm(siteId, from, to, "term", 20).catch(() => []),
        analyticsApi.utm(siteId, from, to, "content", 20).catch(() => []),
      ]);
      setSources(src || []);
      setMediums(med || []);
      setCampaigns(cam || []);
      setTerms(ter || []);
      setContents(con || []);
    } finally { setLoading(false); }
  }, [siteId]);

  useEffect(() => { fetch(); }, [fetch]);

  const totalSources = sources.reduce((s, d) => s + d.visitors, 0);
  const totalMediums = mediums.reduce((s, d) => s + d.visitors, 0);
  const totalCampaigns = campaigns.reduce((s, d) => s + d.visitors, 0);
  const totalTerms = terms.reduce((s, d) => s + d.visitors, 0);
  const totalContents = contents.reduce((s, d) => s + d.visitors, 0);

  const hasData = totalSources + totalMediums + totalCampaigns > 0;

  return (
    <div>
      <div class="obs-page-header">
        <h1 class="obs-page-title">Campaigns</h1>
        <div class="obs-page-actions">
          <span style={{ fontSize: "12px", color: "var(--obs-text-muted)" }}>Last 30 days</span>
        </div>
      </div>

      <Tabs active={tab} onChange={setTab} />

      {tab === "overview" ? (
        loading ? (
          <div class="obs-empty-state">Loading...</div>
        ) : !hasData ? (
          <div class="obs-empty-state">
            No UTM-tagged traffic in the last 30 days. Add utm_source, utm_medium, utm_campaign
            query parameters to your links to track campaigns.
          </div>
        ) : (
          <>
            {/* Summary */}
            <div style={{ display: "grid", gridTemplateColumns: "repeat(3, 1fr)", gap: "12px", marginBottom: "20px" }}>
              {[
                { label: "Unique Sources", value: sources.length, color: "var(--obs-accent)" },
                { label: "Unique Mediums", value: mediums.length, color: "#22c55e" },
                { label: "Unique Campaigns", value: campaigns.length, color: "#f59e0b" },
              ].map((c, i) => (
                <div key={i} style={{ background: "var(--obs-surface)", padding: "16px", borderRadius: "var(--obs-radius-md)", borderLeft: `3px solid ${c.color}` }}>
                  <div style={{ fontSize: "11px", color: "var(--obs-text-muted)", textTransform: "uppercase", letterSpacing: "0.5px", marginBottom: "4px" }}>{c.label}</div>
                  <div style={{ fontSize: "22px", fontWeight: 700, color: c.color, fontVariantNumeric: "tabular-nums" }}>{c.value}</div>
                </div>
              ))}
            </div>

            <div class="obs-grid-2">
              <CampaignGroup title="UTM Source" data={sources} total={totalSources} color="var(--obs-accent)" />
              <CampaignGroup title="UTM Medium" data={mediums} total={totalMediums} color="#22c55e" />
            </div>

            <div style={{ marginTop: "12px" }}>
              <CampaignGroup title="UTM Campaign" data={campaigns} total={totalCampaigns} color="#f59e0b" />
            </div>

            {(terms.length > 0 || contents.length > 0) && (
              <div class="obs-grid-2" style={{ marginTop: "12px" }}>
                <CampaignGroup title="UTM Term" data={terms} total={totalTerms} color="#a78bfa" />
                <CampaignGroup title="UTM Content" data={contents} total={totalContents} color="#ec4899" />
              </div>
            )}
          </>
        )
      ) : (
        <AttributionTab siteId={siteId} from={from} to={to} />
      )}
    </div>
  );
}
