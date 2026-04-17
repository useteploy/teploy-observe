import { useState, useEffect } from "preact/hooks";
import { errorsApi } from "../api/errors.js";
import type { ReleaseHealth } from "../api/errors.js";
import ExportButton from "../components/shared/ExportButton.js";
import EmptyState from "../components/shared/EmptyState.js";
import "../styles/errors.css";

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

export default function ReleasesPage() {
  const siteId = typeof window !== "undefined"
    ? new URLSearchParams(window.location.search).get("site_id") || "default"
    : "default";

  const [releases, setReleases] = useState<ReleaseHealth[]>([]);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    let alive = true;
    setLoading(true);
    errorsApi.releases(siteId)
      .then((r) => { if (alive) setReleases(r || []); })
      .catch(() => { if (alive) setReleases([]); })
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
