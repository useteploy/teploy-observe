import { useState, useEffect } from "preact/hooks";
import { errorsApi } from "../api/errors.js";
import type {
  ReleaseHealth,
  ReleaseStat,
  ReleaseSparklinePoint,
} from "../api/errors.js";
import ExportButton from "../components/shared/ExportButton.js";
import EmptyState from "../components/shared/EmptyState.js";
import "../styles/errors.css";
import { useFilters } from "../hooks/useFilters.js";

export const config = { mode: "app" };

function formatDate(iso: string): string {
  if (!iso) return "--";
  try {
    return new Date(iso).toLocaleString("en-US", {
      month: "short", day: "numeric", year: "numeric",
      hour: "2-digit", minute: "2-digit", hour12: false,
    });
  } catch { return iso; }
}

function healthClass(errorCount: number, issueCount: number): string {
  if (errorCount === 0) return "releases-health--ok";
  if (issueCount > 20 || errorCount > 500) return "releases-health--bad";
  if (issueCount > 5 || errorCount > 50) return "releases-health--warn";
  return "releases-health--ok";
}

// Color buckets for crash-free %. Sentry uses 99.5%+ as healthy.
function crashFreeClass(pct: number): string {
  if (pct >= 99.5) return "releases-health--ok";
  if (pct >= 95) return "releases-health--warn";
  return "releases-health--bad";
}

// Inline SVG sparkline. ~120x24 px so it fits inside a stat card.
function Sparkline({ points }: { points: ReleaseSparklinePoint[] }) {
  if (points.length === 0) return null;
  const W = 120, H = 24, P = 2;
  // Map crash-free % to y. Treat days with zero sessions as 100%
  // (no signal == "no crashes today") so the line doesn't dip to 0
  // on quiet days.
  const ys = points.map((p) => (p.sessions === 0 ? 100 : p.crash_free_session_pct));
  const min = Math.min(...ys, 95); // anchor scale at 95%+ so we see real movement
  const max = 100;
  const span = Math.max(0.01, max - min);
  const step = (W - 2 * P) / Math.max(1, points.length - 1);
  const path = ys
    .map((y, i) => {
      const x = P + i * step;
      const ny = P + (1 - (y - min) / span) * (H - 2 * P);
      return `${i === 0 ? "M" : "L"}${x.toFixed(1)},${ny.toFixed(1)}`;
    })
    .join(" ");
  // Choose color from the most recent value.
  const last = ys[ys.length - 1];
  const stroke =
    last >= 99.5 ? "var(--obs-success, #22c55e)"
    : last >= 95 ? "var(--obs-warn, #eab308)"
    : "var(--obs-danger, #ef4444)";
  return (
    <svg
      class="release-sparkline"
      width={W}
      height={H}
      viewBox={`0 0 ${W} ${H}`}
      role="img"
      aria-label={`crash-free trend, latest ${last.toFixed(2)}%`}
    >
      <path d={path} fill="none" stroke={stroke} stroke-width={1.5} />
    </svg>
  );
}

export default function ReleasesPage() {
  const { state: { siteId } } = useFilters();

  const [releases, setReleases] = useState<ReleaseHealth[]>([]);
  const [loading, setLoading] = useState(true);

  // Phase 1 health: crash-free, adoption, error rate per release.
  const [stats, setStats] = useState<ReleaseStat[]>([]);
  const [sparks, setSparks] = useState<Record<string, ReleaseSparklinePoint[]>>({});

  useEffect(() => {
    let alive = true;
    setLoading(true);

    // 14-day window for the health card grid + sparkline.
    const to = new Date();
    const from = new Date(to.getTime() - 14 * 24 * 60 * 60 * 1000);
    const fromIso = from.toISOString();
    const toIso = to.toISOString();

    Promise.all([
      errorsApi.releases(siteId).catch(() => [] as ReleaseHealth[]),
      errorsApi.releaseHealth(siteId, fromIso, toIso).catch(() => [] as ReleaseStat[]),
    ])
      .then(async ([rels, st]) => {
        if (!alive) return;
        setReleases(rels || []);
        setStats(st || []);
        // Fetch one sparkline per release (capped at 8 to keep the
        // request count sane).
        const spMap: Record<string, ReleaseSparklinePoint[]> = {};
        const top = (st || []).slice(0, 8).filter((s) => s.release_tag);
        await Promise.all(top.map(async (s) => {
          try {
            spMap[s.release_tag] = await errorsApi.releaseSparkline(siteId, s.release_tag, 14);
          } catch {
            spMap[s.release_tag] = [];
          }
        }));
        if (alive) setSparks(spMap);
      })
      .finally(() => { if (alive) setLoading(false); });

    return () => { alive = false; };
  }, [siteId]);

  const maxErrors = Math.max(1, ...releases.map((r) => r.error_count));

  return (
    <div>
      <div class="obs-page-header">
        <h1 class="obs-page-title">Releases</h1>
        <ExportButton
          filename={`releases-${siteId}-${Date.now()}.csv`}
          rows={releases}
          columns={[
            { key: "release_tag", label: "release" },
            { key: "error_count", label: "errors" },
            { key: "issue_count", label: "issues" },
            { key: "first_seen", label: "first_seen" },
            { key: "last_seen", label: "last_seen" },
          ]}
        />
      </div>

      {/* Phase 1: per-release health card grid (sessions, crash-free %,
          adoption %, error rate, 14-day sparkline). Renders only when
          we have at least one release with sessions in the window. */}
      {stats.length > 0 && (
        <div class="release-health-grid" data-testid="release-health-grid">
          {stats.map((s) => (
            <div key={s.release_tag} class="release-health-card">
              <div class="release-health-tag">{s.release_tag || "(no release)"}</div>
              <div class="release-health-row">
                <span class="release-health-label">Crash-free</span>
                <span class={`release-health-value ${crashFreeClass(s.crash_free_session_pct)}`}>
                  {s.crash_free_session_pct.toFixed(2)}%
                </span>
              </div>
              <div class="release-health-row">
                <span class="release-health-label">Adoption</span>
                <span class="release-health-value">{s.adoption_pct.toFixed(1)}%</span>
              </div>
              <div class="release-health-row">
                <span class="release-health-label">Sessions</span>
                <span class="release-health-value">{s.sessions.toLocaleString()}</span>
              </div>
              <div class="release-health-row">
                <span class="release-health-label">Error rate</span>
                <span class="release-health-value">{s.error_rate.toFixed(3)}</span>
              </div>
              {sparks[s.release_tag] && sparks[s.release_tag].length > 0 && (
                <Sparkline points={sparks[s.release_tag]} />
              )}
            </div>
          ))}
        </div>
      )}

      {loading ? (
        <div class="obs-empty-state">Loading releases…</div>
      ) : releases.length === 0 ? (
        <EmptyState
          title="No releases tracked yet"
          description="When you send error events with a release_tag field, Observe groups them here with first/last seen and error counts. Source maps can also be scoped per release."
          icon="package"
          actions={[
            { label: "View errors", href: `/errors?site_id=${siteId}` },
            { label: "Read the docs", href: "/docs#releases" },
          ]}
        />
      ) : (
        <div class="releases-list">
          {releases.map((r) => {
            const barPct = (r.error_count / maxErrors) * 100;
            return (
              <a
                key={r.release_tag}
                class="releases-row"
                href={`/errors?site_id=${siteId}&release=${encodeURIComponent(r.release_tag)}`}
              >
                <div class="releases-tag">{r.release_tag}</div>
                <div class="releases-stats">
                  <span class={`releases-health ${healthClass(r.error_count, r.issue_count)}`}>
                    {r.error_count.toLocaleString()} errors
                  </span>
                  <span class="releases-issues">{r.issue_count} issues</span>
                </div>
                <div class="releases-bar">
                  <div class="releases-bar-fill" style={{ width: `${Math.max(barPct, 4)}%` }} />
                </div>
                <div class="releases-times">
                  <div><span class="releases-time-label">First seen</span>{formatDate(r.first_seen)}</div>
                  <div><span class="releases-time-label">Last seen</span>{formatDate(r.last_seen)}</div>
                </div>
              </a>
            );
          })}
        </div>
      )}
    </div>
  );
}
