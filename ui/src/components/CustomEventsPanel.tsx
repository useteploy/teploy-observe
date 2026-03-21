import { useEffect, useState } from "preact/hooks";
import type { CustomEventStat } from "../api.js";
import { api } from "../api.js";
import { useFilters } from "../hooks/useFilters.js";
import { formatNumber } from "../utils/format.js";

function CustomEventsPanel() {
  const { state } = useFilters();
  const { siteId, from, to, filters } = state;

  const [data, setData] = useState<CustomEventStat[]>([]);
  const [loading, setLoading] = useState(true);

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
              <tr key={row.event_type}>
                <td>{row.event_type}</td>
                <td>{formatNumber(row.count)}</td>
                <td>{formatNumber(row.visitors)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}

CustomEventsPanel.displayName = "CustomEventsPanel";
export default CustomEventsPanel;
