import { useState, useEffect, useCallback } from "preact/hooks";
import { analyticsApi } from "../api/analytics.js";
import type { UTMStat } from "../api/analytics.js";

export const config = { mode: "app" };

function formatNumber(n: number): string {
  return n.toLocaleString();
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

export default function CampaignsPage() {
  const siteId = typeof window !== "undefined"
    ? new URLSearchParams(window.location.search).get("site_id") || "default"
    : "default";

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

      {loading ? (
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
      )}
    </div>
  );
}
