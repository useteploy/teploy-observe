import { useState, useEffect, useCallback } from "preact/hooks";
import { errorsApi } from "../api/errors.js";
import type { Issue, ErrorEvent, ReleaseHealth } from "../api/errors.js";
import SearchInput from "../components/shared/SearchInput.js";
import StatusBadge from "../components/shared/StatusBadge.js";
import CodeBlock from "../components/shared/CodeBlock.js";
import Tabs from "../components/shared/Tabs.js";
import Pagination from "../components/shared/Pagination.js";
import "../styles/errors.css";

export const config = { mode: "app" };

const STATUSES = ["ALL", "OPEN", "RESOLVED", "IGNORED"] as const;
const LEVELS = ["ALL", "ERROR", "WARNING", "INFO"] as const;

function timeAgo(iso: string): string {
  try {
    const diff = Date.now() - new Date(iso).getTime();
    const mins = Math.floor(diff / 60000);
    if (mins < 1) return "just now";
    if (mins < 60) return `${mins}m ago`;
    const hrs = Math.floor(mins / 60);
    if (hrs < 24) return `${hrs}h ago`;
    const days = Math.floor(hrs / 24);
    if (days < 30) return `${days}d ago`;
    return new Date(iso).toLocaleDateString();
  } catch { return iso; }
}

function formatDate(iso: string): string {
  try {
    return new Date(iso).toLocaleString("en-US", {
      month: "short", day: "numeric", year: "numeric",
      hour: "2-digit", minute: "2-digit", hour12: false,
    });
  } catch { return iso; }
}

function tryParseJson(raw: string): object | null {
  if (!raw || raw === "{}" || raw === "null") return null;
  try {
    const p = JSON.parse(raw);
    return typeof p === "object" && p !== null && Object.keys(p).length > 0 ? p : null;
  } catch { return null; }
}

interface StackFrame {
  function?: string;
  filename?: string;
  lineno?: number;
  colno?: number;
  in_app?: boolean;
}

function parseStackTrace(raw: string): StackFrame[] {
  if (!raw) return [];
  try {
    const parsed = JSON.parse(raw);
    if (Array.isArray(parsed)) return parsed;
    if (parsed?.frames && Array.isArray(parsed.frames)) return parsed.frames;
    return [];
  } catch {
    return raw.split("\n").filter(Boolean).map(line => ({ function: line, in_app: true }));
  }
}

interface BreadcrumbEntry {
  timestamp?: string;
  category?: string;
  message?: string;
  level?: string;
  type?: string;
}

function parseBreadcrumbs(raw: string): BreadcrumbEntry[] {
  if (!raw) return [];
  try {
    const parsed = JSON.parse(raw);
    return Array.isArray(parsed) ? parsed : [];
  } catch { return []; }
}

function IssueListSkeleton() {
  return (
    <div class="errors-loading">
      {Array.from({ length: 6 }).map((_, i) => (
        <div class="errors-skeleton-row" key={i}>
          <div class="errors-skeleton-bar" style={{ width: "48px" }} />
          <div class="errors-skeleton-bar" style={{ flex: 1 }} />
          <div class="errors-skeleton-bar" style={{ width: "60px" }} />
        </div>
      ))}
    </div>
  );
}

// ─── Release Health ───

function ReleaseHealthBar({ siteId }: { siteId: string }) {
  const [releases, setReleases] = useState<ReleaseHealth[]>([]);

  useEffect(() => {
    errorsApi.releases(siteId)
      .then(d => setReleases((d || []).slice(0, 5)))
      .catch(() => setReleases([]));
  }, [siteId]);

  if (!releases.length) return null;

  return (
    <div style={{ display: "flex", gap: "8px", flexWrap: "wrap", marginBottom: "16px" }}>
      {releases.map(r => (
        <div key={r.release_tag} style={{
          padding: "6px 12px", background: "var(--obs-card)", border: "1px solid var(--obs-border)",
          borderRadius: "var(--obs-radius-md)", fontSize: "12px", display: "flex", gap: "8px", alignItems: "center"
        }}>
          <span style={{ fontWeight: 600, color: "var(--obs-text)" }}>{r.release_tag}</span>
          <span style={{ color: r.error_count > 0 ? "var(--obs-danger)" : "var(--obs-text-muted)" }}>
            {r.error_count} errors
          </span>
        </div>
      ))}
    </div>
  );
}

// ─── Issue Detail ───

function IssueDetail({ issue, siteId, onBack }: { issue: Issue; siteId: string; onBack: () => void }) {
  const [events, setEvents] = useState<ErrorEvent[]>([]);
  const [loadingEvents, setLoadingEvents] = useState(true);
  const [updatingStatus, setUpdatingStatus] = useState(false);
  const [currentStatus, setCurrentStatus] = useState(issue.status);
  const [selectedEvent, setSelectedEvent] = useState<ErrorEvent | null>(null);
  const [session, setSession] = useState<{ session_id: string; events: unknown[] } | null>(null);
  const [loadingSession, setLoadingSession] = useState(false);

  useEffect(() => {
    setLoadingEvents(true);
    errorsApi.issueEvents(issue.issue_id, siteId)
      .then(e => { setEvents(e || []); if (e?.length) setSelectedEvent(e[0]); })
      .catch(() => setEvents([]))
      .finally(() => setLoadingEvents(false));
  }, [issue.issue_id, siteId]);

  const handleStatusChange = async (status: string) => {
    setUpdatingStatus(true);
    try {
      await errorsApi.updateStatus(issue.issue_id, siteId, status);
      setCurrentStatus(status);
    } catch (err) { console.error("Failed to update status:", err); }
    finally { setUpdatingStatus(false); }
  };

  const handleViewSession = async () => {
    setLoadingSession(true);
    try {
      const data = await errorsApi.issueSession(issue.issue_id, siteId);
      setSession(data);
    } catch { setSession(null); }
    finally { setLoadingSession(false); }
  };

  const frames = selectedEvent ? parseStackTrace(selectedEvent.stack_trace) : [];
  const breadcrumbs = selectedEvent ? parseBreadcrumbs(selectedEvent.breadcrumbs) : [];
  const contexts = selectedEvent ? tryParseJson(selectedEvent.contexts) : null;
  const extra = selectedEvent ? tryParseJson(selectedEvent.extra) : null;

  return (
    <div class="errors-detail">
      <button class="errors-back-btn" onClick={onBack}>
        <svg width="14" height="14" viewBox="0 0 24 24" fill="currentColor">
          <path d="M20 11H7.83l5.59-5.59L12 4l-8 8 8 8 1.41-1.41L7.83 13H20v-2z" />
        </svg>
        Back to issues
      </button>

      <div class="errors-detail-header">
        <div>
          <h2 class="errors-detail-title">{issue.title}</h2>
          <div class="errors-detail-culprit">{issue.culprit}</div>
        </div>
        <div class="errors-detail-actions">
          <StatusBadge status={currentStatus} size="md" />
          {currentStatus !== "resolved" && (
            <button class="obs-btn obs-btn--sm" disabled={updatingStatus}
              onClick={() => handleStatusChange("resolved")}>Resolve</button>
          )}
          {currentStatus !== "ignored" && (
            <button class="obs-btn obs-btn--sm" disabled={updatingStatus}
              onClick={() => handleStatusChange("ignored")}>Ignore</button>
          )}
          {currentStatus !== "open" && (
            <button class="obs-btn obs-btn--sm" disabled={updatingStatus}
              onClick={() => handleStatusChange("open")}>Reopen</button>
          )}
        </div>
      </div>

      <div class="errors-detail-stats">
        <div class="errors-detail-stat">
          <span class="errors-detail-stat-label">Events</span>
          <span class="errors-detail-stat-value">{Number(issue.event_count).toLocaleString()}</span>
        </div>
        <div class="errors-detail-stat">
          <span class="errors-detail-stat-label">Users</span>
          <span class="errors-detail-stat-value">{Number(issue.user_count).toLocaleString()}</span>
        </div>
        <div class="errors-detail-stat">
          <span class="errors-detail-stat-label">First seen</span>
          <span class="errors-detail-stat-value">{formatDate(issue.first_seen)}</span>
        </div>
        <div class="errors-detail-stat">
          <span class="errors-detail-stat-label">Last seen</span>
          <span class="errors-detail-stat-value">{formatDate(issue.last_seen)}</span>
        </div>
        {issue.release_tag && (
          <div class="errors-detail-stat">
            <span class="errors-detail-stat-label">Release</span>
            <span class="errors-detail-stat-value">{issue.release_tag}</span>
          </div>
        )}
        <div class="errors-detail-stat">
          <button class="obs-btn obs-btn--sm" onClick={handleViewSession} disabled={loadingSession}>
            {loadingSession ? "Loading..." : "View Session"}
          </button>
        </div>
      </div>

      {/* Selected event environment info */}
      {selectedEvent && (
        <div style={{ display: "flex", gap: "12px", flexWrap: "wrap", fontSize: "11px", color: "var(--obs-text-muted)", marginBottom: "12px" }}>
          {selectedEvent.browser && <span>Browser: {selectedEvent.browser}</span>}
          {selectedEvent.os && <span>OS: {selectedEvent.os}</span>}
          {selectedEvent.device && <span>Device: {selectedEvent.device}</span>}
          {selectedEvent.url && <span>URL: {selectedEvent.url}</span>}
          {selectedEvent.environment && <span>Env: {selectedEvent.environment}</span>}
        </div>
      )}

      {loadingEvents ? (
        <IssueListSkeleton />
      ) : selectedEvent ? (
        <Tabs tabs={[
          {
            key: "stacktrace",
            label: "Stack Trace",
            content: frames.length > 0 ? (
              <div class="errors-stack-list">
                {frames.map((f, i) => (
                  <div key={i} class={`errors-stack-frame ${f.in_app !== false ? "errors-stack-frame--app" : "errors-stack-frame--vendor"}`}>
                    <span class="errors-stack-fn">{f.function || "(anonymous)"}</span>
                    {f.filename && (
                      <span class="errors-stack-file">
                        {f.filename}{f.lineno ? `:${f.lineno}` : ""}{f.colno ? `:${f.colno}` : ""}
                      </span>
                    )}
                  </div>
                ))}
              </div>
            ) : <div class="obs-empty-state">No stack trace available</div>,
          },
          {
            key: "breadcrumbs",
            label: `Breadcrumbs (${breadcrumbs.length})`,
            content: breadcrumbs.length > 0 ? (
              <div class="errors-breadcrumb-list">
                {breadcrumbs.map((b, i) => (
                  <div key={i} class="errors-breadcrumb">
                    <span class="errors-breadcrumb-ts">
                      {b.timestamp ? new Date(b.timestamp).toLocaleTimeString("en-US", { hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false }) : "--"}
                    </span>
                    <span class="errors-breadcrumb-category">{b.category || b.type || "--"}</span>
                    <span class="errors-breadcrumb-message">{b.message || "--"}</span>
                  </div>
                ))}
              </div>
            ) : <div class="obs-empty-state">No breadcrumbs</div>,
          },
          {
            key: "events",
            label: `Events (${events.length})`,
            content: events.length > 0 ? (
              <div class="errors-breadcrumb-list">
                {events.map(ev => (
                  <div key={ev.error_id}
                    class={`errors-event-row ${selectedEvent?.error_id === ev.error_id ? "errors-stack-frame--app" : ""}`}
                    onClick={() => setSelectedEvent(ev)}>
                    <span class="errors-event-ts">{formatDate(ev.timestamp)}</span>
                    <StatusBadge status={ev.level.toLowerCase()} size="sm" />
                    <span class="errors-event-type">{ev.error_type}: {ev.error_value}</span>
                    <span class="errors-event-env">{ev.environment || "--"}</span>
                  </div>
                ))}
              </div>
            ) : <div class="obs-empty-state">No events</div>,
          },
          ...(contexts ? [{
            key: "context",
            label: "Context",
            content: <CodeBlock code={JSON.stringify(contexts, null, 2)} maxHeight="400px" />,
          }] : []),
          ...(extra ? [{
            key: "extra",
            label: "Extra",
            content: <CodeBlock code={JSON.stringify(extra, null, 2)} maxHeight="400px" />,
          }] : []),
          ...(session ? [{
            key: "session",
            label: "Session",
            content: <CodeBlock code={JSON.stringify(session, null, 2)} maxHeight="400px" />,
          }] : []),
        ]} />
      ) : (
        <div class="obs-empty-state">No events for this issue</div>
      )}
    </div>
  );
}

// ─── Main Page ───

export default function ErrorsPage() {
  const siteId = typeof window !== "undefined"
    ? new URLSearchParams(window.location.search).get("site_id") || "default"
    : "default";

  const PAGE_SIZE = 20;
  const [issues, setIssues] = useState<Issue[]>([]);
  const [loading, setLoading] = useState(true);
  const [query, setQuery] = useState("");
  const [activeStatus, setActiveStatus] = useState<string>("ALL");
  const [activeLevel, setActiveLevel] = useState<string>("ALL");
  const [selectedIssue, setSelectedIssue] = useState<Issue | null>(null);
  const [page, setPage] = useState(1);

  const fetchIssues = useCallback(async () => {
    setLoading(true);
    try {
      let data: Issue[];
      if (query.trim()) {
        data = await errorsApi.search(siteId, query.trim());
      } else {
        const statusFilter = activeStatus === "ALL" ? undefined : activeStatus.toLowerCase();
        data = await errorsApi.issues(siteId, statusFilter, PAGE_SIZE, (page - 1) * PAGE_SIZE);
      }
      if (activeLevel !== "ALL") {
        data = data.filter(i => i.level?.toLowerCase() === activeLevel.toLowerCase());
      }
      setIssues(data || []);
    } catch (err) {
      console.error("Failed to fetch issues:", err);
      setIssues([]);
    } finally {
      setLoading(false);
    }
  }, [siteId, query, activeStatus, activeLevel, page]);

  useEffect(() => { fetchIssues(); }, [fetchIssues]);

  if (selectedIssue) {
    return <IssueDetail issue={selectedIssue} siteId={siteId} onBack={() => setSelectedIssue(null)} />;
  }

  return (
    <div>
      <div class="obs-page-header">
        <h1 class="obs-page-title">Errors</h1>
      </div>

      <ReleaseHealthBar siteId={siteId} />

      <div class="errors-toolbar">
        <SearchInput value={query} onInput={setQuery} placeholder="Search errors..." onSubmit={fetchIssues} />
      </div>

      <div style={{ display: "flex", gap: "8px", flexWrap: "wrap", marginBottom: "16px" }}>
        <div class="errors-status-filters">
          {STATUSES.map(status => (
            <button
              key={status}
              class={`errors-status-btn errors-status-btn--${status.toLowerCase()} ${activeStatus === status ? "errors-status-btn--active" : ""}`}
              onClick={() => setActiveStatus(status)}
            >
              {status}
            </button>
          ))}
        </div>
        <div class="errors-status-filters">
          {LEVELS.map(level => (
            <button
              key={level}
              class={`errors-status-btn ${activeLevel === level ? "errors-status-btn--active errors-status-btn--all" : ""}`}
              onClick={() => setActiveLevel(level)}
            >
              {level}
            </button>
          ))}
        </div>
      </div>

      {loading ? (
        <IssueListSkeleton />
      ) : issues.length === 0 ? (
        <div class="obs-empty-state">
          {query || activeStatus !== "ALL" || activeLevel !== "ALL" ? "No issues match the current filters" : "No errors captured yet"}
        </div>
      ) : (
        <>
          <div class="errors-issue-list obs-stagger">
            {issues.map(issue => {
              const firstMs = new Date(issue.first_seen).getTime();
              const lastMs = new Date(issue.last_seen).getTime();
              const nowMs = Date.now();
              const dayMs = 86400000;
              return (
                <div key={issue.issue_id} class="errors-issue-row" onClick={() => setSelectedIssue(issue)}>
                  <StatusBadge status={issue.status} size="sm" />
                  <div class="errors-issue-info">
                    <div class="errors-issue-title">{issue.title}</div>
                    <div class="errors-issue-culprit">{issue.culprit}</div>
                  </div>
                  <div class="errors-issue-activity">
                    {Array.from({ length: 14 }).map((_, i) => {
                      const dayStart = nowMs - (13 - i) * dayMs;
                      const dayEnd = dayStart + dayMs;
                      const active = lastMs >= dayStart && firstMs <= dayEnd;
                      return <div key={i} class={`errors-activity-dot ${active ? "errors-activity-dot--active" : ""}`} />;
                    })}
                  </div>
                  <div class="errors-issue-meta">
                    <span class="errors-issue-count">{Number(issue.event_count).toLocaleString()}</span>
                    <span class="errors-issue-time">{timeAgo(issue.last_seen)}</span>
                  </div>
                </div>
              );
            })}
          </div>
          <Pagination page={page} pageSize={PAGE_SIZE} resultCount={issues.length} onPageChange={(p) => { setPage(p); window.scrollTo(0, 0); }} />
        </>
      )}
    </div>
  );
}
