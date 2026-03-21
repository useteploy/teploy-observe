import { useEffect, useState } from "preact/hooks";
import { useFilters } from "../hooks/useFilters.js";
import { formatNumber } from "../utils/format.js";

interface Props {
  fetchFn: (siteId: string, from: string, to: string, limit?: number, filters?: Record<string, string>) => Promise<Record<string, any>[]>;
  labelKey: string;
  valueKey: string;
  filterKey: string;
  limit?: number;
}

function SkeletonRows() {
  return (
    <div class="obs-skeleton">
      {[80, 65, 50, 40, 30].map((w, i) => (
        <div key={i} class="obs-skeleton-row">
          <div class="obs-skeleton-bar" style={`width:${w}%;flex:1;`} />
          <div class="obs-skeleton-bar" style="width:40px;" />
        </div>
      ))}
    </div>
  );
}

function BreakdownTable({ fetchFn, labelKey, valueKey, filterKey, limit: initialLimit = 10 }: Props) {
  const { state, dispatch } = useFilters();
  const { siteId, from, to, filters } = state;

  const [data, setData] = useState<Record<string, any>[]>([]);
  const [loading, setLoading] = useState(true);
  const [limit, setLimit] = useState(initialLimit);
  const [sortAsc, setSortAsc] = useState(false);

  useEffect(() => {
    setLoading(true);
    fetchFn(siteId, from, to, limit, filters).then((d) => {
      setData(d || []);
      setLoading(false);
    }).catch(() => setLoading(false));
  }, [siteId, from, to, limit, JSON.stringify(filters)]);

  const sorted = sortAsc ? [...data].sort((a, b) => (Number(a[valueKey]) || 0) - (Number(b[valueKey]) || 0)) : data;
  const total = data.reduce((sum, r) => sum + (Number(r[valueKey]) || 0), 0);

  function handleRowClick(row: Record<string, any>) {
    const value = String(row[labelKey] || "");
    if (value && value !== "(unknown)") {
      dispatch({ type: "SET_FILTER", key: filterKey, value });
    }
  }

  function handleViewAll() {
    setLimit(100);
  }

  if (loading) {
    return <SkeletonRows />;
  }

  if (data.length === 0) {
    return (
      <div class="obs-empty">
        <div class="obs-empty-icon">--</div>
        <span>No data for this period</span>
      </div>
    );
  }

  return (
    <div>
      <div style="display:flex;justify-content:flex-end;margin-bottom:4px;">
        <button
          class="obs-sort-btn"
          onClick={() => setSortAsc(!sortAsc)}
          title="Toggle sort order"
        >
          {sortAsc ? "\u2191" : "\u2193"}
        </button>
      </div>
      <div class="obs-scrollable">
        {sorted.map((row, idx) => {
          const label = String(row[labelKey] || "(unknown)");
          const value = Number(row[valueKey]) || 0;
          const pct = total > 0 ? (value / total) * 100 : 0;
          return (
            <div
              key={label}
              class="obs-table-row"
              onClick={() => handleRowClick(row)}
              title={`Filter by ${filterKey}: ${label}`}
            >
              <span class="obs-table-row-rank">{idx + 1}</span>
              <div style="flex:1;min-width:0;">
                <div style="display:flex;justify-content:space-between;align-items:baseline;">
                  <span class="obs-table-row-label">{label}</span>
                  <span class="obs-table-row-value">{formatNumber(value)}</span>
                </div>
                <div class="obs-bar">
                  <div class="obs-bar-fill" style={`width:${pct}%`} />
                </div>
              </div>
            </div>
          );
        })}
      </div>
      {limit < 100 && data.length >= limit && (
        <div class="obs-view-all">
          <button onClick={handleViewAll}>
            View all
          </button>
        </div>
      )}
    </div>
  );
}

BreakdownTable.displayName = "BreakdownTable";
export default BreakdownTable;
