import { useEffect, useState } from "preact/hooks";
import type { UniqueCoverage } from "../api/analytics.js";
import { api } from "../api.js";
import { useFilters } from "../hooks/useFilters.js";

/**
 * Says so when the visitor figures on this screen describe a shorter window
 * than the range the user picked.
 *
 * The pageview panel reads the rollups, which are kept for a year and then
 * indefinitely. A unique count cannot be summed out of a rollup, so it has to
 * be counted from a table holding one row per thing counted — raw events, or
 * the session-grain sessions table — and both are pruned sooner. Pick "Last 12
 * months" or "All time" and the two halves of the same screen therefore
 * describe different windows. Returning the smaller number without saying so
 * is the bug this replaces; the server decides the tier and hands back the
 * sentence (GET /api/v1/stats/unique-coverage), which runs no query.
 */
function CoverageNote() {
  const { state } = useFilters();
  const { siteId, from, to, filters } = state;
  const [coverage, setCoverage] = useState<UniqueCoverage | null>(null);

  useEffect(() => {
    let live = true;
    api
      .uniqueCoverage(siteId, from, to, filters)
      .then((c) => {
        if (live) setCoverage(c);
      })
      .catch(() => {
        if (live) setCoverage(null);
      });
    return () => {
      live = false;
    };
  }, [siteId, from, to, JSON.stringify(filters)]);

  if (!coverage || coverage.exact || !coverage.note) return null;

  return (
    <div class="obs-coverage-note" role="status">
      <span class="obs-coverage-badge">Partial</span>
      <span>{coverage.note}</span>
    </div>
  );
}

CoverageNote.displayName = "CoverageNote";
export default CoverageNote;
