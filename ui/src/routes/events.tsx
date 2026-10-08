import QueryFailure from "../components/shared/QueryFailure.js";
import { useState, useEffect, useCallback, useMemo } from "preact/hooks";
import { analyticsApi } from "../api/analytics.js";
import type { CustomEventStat, PropertyStat, TimeSeriesPoint } from "../api/analytics.js";
import SearchInput from "../components/shared/SearchInput.js";
import ExportButton from "../components/shared/ExportButton.js";
import { useRequestGuard } from "../hooks/useRequestGuard.js";
import { useFilters } from "../hooks/useFilters.js";

export const config = { mode: "app" };

function formatNumber(n: number): string {
  return n.toLocaleString();
}

export default function EventsPage() {
  const { state: { siteId } } = useFilters();

  const [events, setEvents] = useState<CustomEventStat[]>([]);
  const [loading, setLoading] = useState(true);
  const [query, setQuery] = useState("");
  const [selected, setSelected] = useState<string | null>(null);

  const requestList = useRequestGuard(siteId);
  const [error, setError] = useState("");
  const { from, to } = useMemo(() => {
    const now = new Date();
    return { from: new Date(now.getTime() - 7 * 86400000).toISOString(), to: now.toISOString() };
  }, [siteId]);

  const fetch = useCallback(async () => {
    const current = requestList();
    setLoading(true); setEvents([]); setError("");
    try {
      const data = await analyticsApi.customEvents(siteId, from, to, 50);
      if (current()) setEvents(data || []);
    } catch { if (current()) setError("Unable to load events."); }
    finally { if (current()) setLoading(false); }
  }, [siteId, from, to, requestList]);

  useEffect(() => { setSelected(null); setQuery(""); fetch(); }, [fetch]);

  const filtered = query.trim()
    ? events.filter(e => e.event_type.toLowerCase().includes(query.toLowerCase()))
    : events;

  const maxCount = Math.max(...events.map(e => e.count), 1);

  if (selected) {
    return <EventDetailView key={`${siteId}:${selected}`} eventType={selected} siteId={siteId} from={from} to={to} onBack={() => setSelected(null)} />;
  }

  return (
    <div>
      <div class="obs-page-header">
        <h1 class="obs-page-title">Events</h1>
      </div>

      <div style={{ marginBottom: "16px", display: "flex", gap: "8px", alignItems: "center" }}>
        <div style={{ flex: 1 }}>
          <SearchInput value={query} onInput={setQuery} placeholder="Search events..." />
        </div>
        <ExportButton
          filename={`events-${siteId}-${Date.now()}.csv`}
          rows={filtered}
          columns={[
            { key: "event_type", label: "event" },
            { key: "count", label: "count" },
            { key: "visitors", label: "visitors" },
          ]}
        />
      </div>

      {loading ? (
        <div class="obs-empty-state">Loading...</div>
      ) : error ? <QueryFailure message={error} retry={fetch} /> : filtered.length === 0 ? (
        <div class="obs-empty-state">
          {query ? "No events match" : "No custom events tracked. Use window.observe('event_name', { ...props }) to track events."}
        </div>
      ) : (
        <div style={{ display: "flex", flexDirection: "column", gap: "1px", background: "var(--obs-border-subtle)", borderRadius: "var(--obs-radius-md)", overflow: "hidden" }}>
          {filtered.map(e => {
            const pctBar = (e.count / maxCount) * 100;
            return (
              <div key={e.event_type} onClick={() => setSelected(e.event_type)}
                style={{ background: "var(--obs-surface)", padding: "12px 16px", cursor: "pointer", display: "flex", alignItems: "center", gap: "16px", position: "relative" }}>
                <div style={{ position: "absolute", left: 0, top: 0, bottom: 0, width: `${pctBar}%`, background: "var(--obs-accent)", opacity: 0.08, pointerEvents: "none" }} />
                <div style={{ flex: 1, fontWeight: 500, color: "var(--obs-text)", zIndex: 1 }}>{e.event_type}</div>
                <div style={{ fontSize: "12px", color: "var(--obs-text-secondary)", fontVariantNumeric: "tabular-nums", zIndex: 1 }}>
                  {formatNumber(e.visitors)} visitors
                </div>
                <div style={{ fontSize: "14px", fontWeight: 600, color: "var(--obs-text)", fontVariantNumeric: "tabular-nums", minWidth: "80px", textAlign: "right", zIndex: 1 }}>
                  {formatNumber(e.count)}
                </div>
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}

// ─── Event Detail ───

function EventDetailView({ eventType, siteId, from, to, onBack }: {
  eventType: string; siteId: string; from: string; to: string; onBack: () => void;
}) {
  const [props, setProps] = useState<PropertyStat[]>([]);
  const [timeseries, setTimeseries] = useState<TimeSeriesPoint[]>([]);
  const [loading, setLoading] = useState(true);

  const [propsError, setPropsError] = useState("");
  const [trendError, setTrendError] = useState("");
  const [retry, setRetry] = useState(0);
  useEffect(() => {
    let active = true;
    setProps([]); setTimeseries([]); setLoading(true);
    setPropsError(""); setTrendError("");
    Promise.all([
      analyticsApi.eventProperties(siteId, from, to, eventType)
        .then(p => { if (active) setProps(p || []); })
        .catch(() => { if (active) setPropsError("Unable to load event properties."); }),
      analyticsApi.timeseries(siteId, from, to, "day", { event_type: eventType })
        .then(ts => { if (active) setTimeseries(ts || []); })
        .catch(() => { if (active) setTrendError("Unable to load event trend."); }),
    ]).finally(() => { if (active) setLoading(false); });
    return () => { active = false; };
  }, [eventType, siteId, from, to, retry]);

  const grouped = new Map<string, PropertyStat[]>();
  for (const p of props) {
    if (!grouped.has(p.key)) grouped.set(p.key, []);
    grouped.get(p.key)!.push(p);
  }

  const totalCount = props.reduce((sum, p) => sum + p.count, 0);

  return (
    <div>
      <button class="errors-back-btn" onClick={onBack} style={{ marginBottom: "16px" }}>
        <svg width="14" height="14" viewBox="0 0 24 24" fill="currentColor">
          <path d="M20 11H7.83l5.59-5.59L12 4l-8 8 8 8 1.41-1.41L7.83 13H20v-2z" />
        </svg>
        Back to events
      </button>

      <div class="obs-page-header">
        <h1 class="obs-page-title">{eventType}</h1>
      </div>

      {trendError && <QueryFailure message={trendError} retry={() => setRetry(n => n + 1)} />}
      {/* Timeline sparkline */}
      {timeseries.length > 0 && (
        <div class="obs-card-static" style={{ marginBottom: "16px" }}>
          <div style={{ fontSize: "12px", color: "var(--obs-text-muted)", textTransform: "uppercase", letterSpacing: "0.5px", marginBottom: "8px" }}>
            Trend for selected range
          </div>
          <MiniChart data={timeseries.map(t => t.pageviews)} />
        </div>
      )}

      {loading ? (
        <div class="obs-empty-state">Loading properties...</div>
      ) : propsError ? <QueryFailure message={propsError} retry={() => setRetry(n => n + 1)} /> : props.length === 0 ? (
        <div class="obs-empty-state">No properties recorded for this event</div>
      ) : (
        <div>
          <h2 style={{ fontSize: "14px", fontWeight: 600, color: "var(--obs-text)", marginBottom: "12px" }}>
            Property Breakdown
          </h2>
          <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fill, minmax(320px, 1fr))", gap: "12px" }}>
            {Array.from(grouped.entries()).map(([key, values]) => {
              const keyTotal = values.reduce((s, v) => s + v.count, 0);
              return (
                <div key={key} class="obs-card-static" style={{ padding: "16px" }}>
                  <div style={{ display: "flex", justifyContent: "space-between", alignItems: "baseline", marginBottom: "8px" }}>
                    <div style={{ fontSize: "13px", fontWeight: 600, color: "var(--obs-text)" }}>{key}</div>
                    <div style={{ fontSize: "11px", color: "var(--obs-text-muted)" }}>{values.length} value{values.length !== 1 ? "s" : ""}</div>
                  </div>
                  {values.slice(0, 8).map((v, i) => {
                    const pct = keyTotal > 0 ? (v.count / keyTotal) * 100 : 0;
                    return (
                      <div key={i} style={{ position: "relative", padding: "4px 0", fontSize: "12px" }}>
                        <div style={{ position: "absolute", left: 0, top: 2, bottom: 2, width: `${pct}%`, background: "var(--obs-accent)", opacity: 0.1, borderRadius: "2px" }} />
                        <div style={{ display: "flex", justifyContent: "space-between", position: "relative", zIndex: 1 }}>
                          <span style={{ color: "var(--obs-text)" }}>{v.value}</span>
                          <span style={{ color: "var(--obs-text-secondary)", fontVariantNumeric: "tabular-nums" }}>
                            {formatNumber(v.count)} ({pct.toFixed(0)}%)
                          </span>
                        </div>
                      </div>
                    );
                  })}
                </div>
              );
            })}
          </div>
        </div>
      )}
    </div>
  );
}

function MiniChart({ data }: { data: number[] }) {
  if (data.length < 2) return null;
  const max = Math.max(...data, 1);
  const w = 100;
  const h = 30;
  const points = data.map((v, i) => {
    const x = (i / (data.length - 1)) * w;
    const y = h - (v / max) * h;
    return `${x},${y}`;
  }).join(" ");

  return (
    <svg viewBox={`0 0 ${w} ${h}`} style={{ width: "100%", height: "60px", display: "block" }} preserveAspectRatio="none">
      <polyline points={points} fill="none" stroke="var(--obs-accent)" strokeWidth="1" />
      <polygon points={`0,${h} ${points} ${w},${h}`} fill="var(--obs-accent)" opacity="0.1" />
    </svg>
  );
}
