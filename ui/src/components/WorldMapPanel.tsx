import { useEffect, useState } from "preact/hooks";
import { api } from "../api.js";
import { useFilters } from "../hooks/useFilters.js";
import WorldMap from "./WorldMap.js";
import LoadError from "./shared/LoadError.js";

export default function WorldMapPanel() {
  const { state } = useFilters();
  const { siteId, from, to, filters } = state;

  const [data, setData] = useState<Array<{ country: string; visitors: number }>>([]);
  const [loading, setLoading] = useState(true);
  // O13 chart states: a failed request renders an error, not the
  // "No country data" empty state.
  const [error, setError] = useState<string | null>(null);
  const [reloadKey, setReloadKey] = useState(0);

  useEffect(() => {
    setLoading(true);
    setError(null);
    api.countries(siteId, from, to, 100, filters)
      .then(d => { setData(d || []); })
      .catch((err) => { setError(err instanceof Error ? err.message : String(err)); })
      .finally(() => setLoading(false));
  }, [siteId, from, to, JSON.stringify(filters), reloadKey]);

  return (
    <div class="obs-card-static">
      <h3 class="obs-section-title" style="margin-bottom:12px;">Visitors by Country</h3>
      {loading ? (
        <div class="obs-skeleton">
          <div class="obs-skeleton-bar" style="width:100%;height:300px;border-radius:8px;" />
        </div>
      ) : error !== null ? (
        <LoadError what="country data" detail={error} onRetry={() => setReloadKey((k) => k + 1)} />
      ) : data.length === 0 ? (
        <div class="obs-empty" style="height:200px;">
          <div class="obs-empty-icon">--</div>
          <span>No country data</span>
        </div>
      ) : (
        <WorldMap data={data} />
      )}
    </div>
  );
}
