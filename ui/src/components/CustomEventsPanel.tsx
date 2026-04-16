import { useEffect, useState } from "preact/hooks";
import type { CustomEventStat, PropertyStat } from "../api.js";
import { api } from "../api.js";
import { useFilters } from "../hooks/useFilters.js";
import { formatNumber } from "../utils/format.js";

function PropertyDrilldown({ eventType, siteId, from, to }: {
  eventType: string; siteId: string; from: string; to: string;
}) {
  const [props, setProps] = useState<PropertyStat[]>([]);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    setLoading(true);
    api.eventProperties(siteId, from, to, eventType)
      .then(d => setProps(d || []))
      .catch(() => setProps([]))
      .finally(() => setLoading(false));
  }, [siteId, from, to, eventType]);

  if (loading) return <div style={{ padding: "8px 0", fontSize: "12px", color: "var(--obs-text-muted)" }}>Loading properties...</div>;
  if (!props.length) return <div style={{ padding: "8px 0", fontSize: "12px", color: "var(--obs-text-muted)" }}>No properties recorded</div>;

  // Group by key
  const grouped = new Map<string, PropertyStat[]>();
  for (const p of props) {
    if (!grouped.has(p.key)) grouped.set(p.key, []);
    grouped.get(p.key)!.push(p);
  }

  return (
    <div style={{ padding: "8px 0 4px 16px", borderLeft: "2px solid var(--obs-accent)" }}>
      {Array.from(grouped.entries()).map(([key, values]) => (
        <div key={key} style={{ marginBottom: "8px" }}>
          <div style={{ fontSize: "11px", fontWeight: 600, color: "var(--obs-text-secondary)", marginBottom: "4px" }}>{key}</div>
          {values.map((v, i) => (
            <div key={i} style={{ display: "flex", alignItems: "center", gap: "8px", padding: "2px 0", fontSize: "12px" }}>
              <span style={{ flex: 1, color: "var(--obs-text)" }}>{v.value}</span>
              <span style={{ color: "var(--obs-text-secondary)", fontVariantNumeric: "tabular-nums", minWidth: "40px", textAlign: "right" }}>{formatNumber(v.count)}</span>
              <span style={{ color: "var(--obs-text-muted)", fontVariantNumeric: "tabular-nums", minWidth: "40px", textAlign: "right" }}>{formatNumber(v.visitors)} vis</span>
            </div>
          ))}
        </div>
      ))}
    </div>
  );
}

function CustomEventsPanel() {
  const { state } = useFilters();
  const { siteId, from, to, filters } = state;

  const [data, setData] = useState<CustomEventStat[]>([]);
  const [loading, setLoading] = useState(true);
  const [expandedEvent, setExpandedEvent] = useState<string | null>(null);

  useEffect(() => {
    setLoading(true);
    api.customEvents(siteId, from, to, 20, filters).then((d) => {
      setData(d || []);
      setLoading(false);
    }).catch(() => setLoading(false));
  }, [siteId, from, to, JSON.stringify(filters)]);

  return (
    <div class="obs-card-static">
      <h3 class="obs-section-title" style="margin-bottom:12px;">Custom Events</h3>
      {loading ? (
        <div class="obs-skeleton">
          {[90, 70, 55].map((w, i) => (
            <div key={i} class="obs-skeleton-row">
              <div class="obs-skeleton-bar" style={`width:${w}%;flex:1;`} />
              <div class="obs-skeleton-bar" style="width:40px;" />
              <div class="obs-skeleton-bar" style="width:40px;" />
            </div>
          ))}
        </div>
      ) : data.length === 0 ? (
        <div class="obs-empty">
          <div class="obs-empty-icon">--</div>
          <span>No events recorded</span>
        </div>
      ) : (
        <div>
          <table class="obs-events-table">
            <thead>
              <tr>
                <th>Event Name</th>
                <th>Count</th>
                <th>Visitors</th>
              </tr>
            </thead>
            <tbody>
              {data.map((row) => (
                <>
                  <tr key={row.event_type}
                    style={{ cursor: "pointer" }}
                    onClick={() => setExpandedEvent(expandedEvent === row.event_type ? null : row.event_type)}>
                    <td style={{ fontWeight: expandedEvent === row.event_type ? 600 : 400 }}>
                      {expandedEvent === row.event_type ? "- " : "+ "}{row.event_type}
                    </td>
                    <td>{formatNumber(row.count)}</td>
                    <td>{formatNumber(row.visitors)}</td>
                  </tr>
                  {expandedEvent === row.event_type && (
                    <tr key={`${row.event_type}-props`}>
                      <td colSpan={3} style={{ padding: "0 8px 8px" }}>
                        <PropertyDrilldown eventType={row.event_type} siteId={siteId} from={from} to={to} />
                      </td>
                    </tr>
                  )}
                </>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

CustomEventsPanel.displayName = "CustomEventsPanel";
export default CustomEventsPanel;
