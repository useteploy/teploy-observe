import { useState, useEffect, useCallback } from "preact/hooks";
import { replaysApi } from "../api/replays.js";
import type { ReplaySession, ReplayEvent } from "../api/replays.js";
import { get } from "../api/helpers.js";
import StatusBadge from "../components/shared/StatusBadge.js";
import CodeBlock from "../components/shared/CodeBlock.js";
import Pagination from "../components/shared/Pagination.js";
import "../styles/sessions.css";

interface SessionEvent {
  event_id: string;
  event_type: string;
  url: string;
  pathname: string;
  title: string;
  timestamp: number;
}

export const config = { mode: "app" };

function formatDuration(ms: number): string {
  if (ms < 1000) return `${ms}ms`;
  const s = Math.floor(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  const rem = s % 60;
  return `${m}m ${rem}s`;
}

function formatTime(iso: string): string {
  try {
    return new Date(iso).toLocaleTimeString("en-US", {
      hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false,
    });
  } catch { return iso; }
}

function formatDate(iso: string): string {
  try {
    return new Date(iso).toLocaleString("en-US", {
      month: "short", day: "numeric", hour: "2-digit", minute: "2-digit", hour12: false,
    });
  } catch { return iso; }
}

function SessionsSkeleton() {
  return (
    <div class="sessions-loading">
      {Array.from({ length: 6 }).map((_, i) => (
        <div class="sessions-skeleton-row" key={i}>
          <div class="sessions-skeleton-bar" style={{ width: "200px" }} />
          <div style={{ flex: 1 }} />
          <div class="sessions-skeleton-bar" style={{ width: "80px" }} />
          <div class="sessions-skeleton-bar" style={{ width: "60px" }} />
          <div class="sessions-skeleton-bar" style={{ width: "40px" }} />
        </div>
      ))}
    </div>
  );
}

// ─── Session Detail ───

function SessionDetail({ session, onBack }: { session: ReplaySession; onBack: () => void }) {
  const [events, setEvents] = useState<ReplayEvent[]>([]);
  const [pageviews, setPageviews] = useState<SessionEvent[]>([]);
  const [loading, setLoading] = useState(true);
  const [expandedId, setExpandedId] = useState<string | null>(null);

  useEffect(() => {
    setLoading(true);
    Promise.all([
      replaysApi.events(session.replay_id).catch(() => []),
      get<SessionEvent[]>(`/api/v1/stats/sessions/${session.session_id}?site_id=${session.site_id}`).catch(() => []),
    ]).then(([replayEvents, sessionEvents]) => {
      setEvents(replayEvents || []);
      setPageviews(sessionEvents || []);
    }).finally(() => setLoading(false));
  }, [session.replay_id, session.session_id, session.site_id]);

  const tryParseData = (raw: string): string | null => {
    if (!raw || raw === "{}" || raw === "null") return null;
    try {
      const p = JSON.parse(raw);
      if (typeof p === "object" && p !== null && Object.keys(p).length > 0)
        return JSON.stringify(p, null, 2);
      return null;
    } catch { return raw.length > 10 ? raw : null; }
  };

  const eventClass = (type: string): string => {
    switch (type) {
      case "click": return "sessions-event--click";
      case "mutation": return "sessions-event--mutation";
      case "scroll": return "sessions-event--scroll";
      case "snapshot": return "sessions-event--snapshot";
      case "error": return "sessions-event--error";
      default: return "";
    }
  };

  return (
    <div>
      <button class="errors-back-btn" onClick={onBack} style={{ marginBottom: "16px" }}>
        <svg width="14" height="14" viewBox="0 0 24 24" fill="currentColor">
          <path d="M20 11H7.83l5.59-5.59L12 4l-8 8 8 8 1.41-1.41L7.83 13H20v-2z" />
        </svg>
        Back to sessions
      </button>

      <div class="sessions-detail-header">
        <div>
          <div class="sessions-detail-url">{session.url || "/"}</div>
          <div class="sessions-detail-meta">
            <span>{session.browser}</span>
            <span>{session.os}</span>
            {session.device && <span>{session.device}</span>}
            <span>{formatDuration(session.duration_ms)}</span>
            <span>{session.page_count} pages</span>
            <span>{formatDate(session.start_time)}</span>
            {session.has_error && <StatusBadge status="error" size="sm" />}
          </div>
        </div>
      </div>

      {loading ? (
        <SessionsSkeleton />
      ) : events.length === 0 && pageviews.length === 0 ? (
        <div class="obs-empty-state">No events recorded for this session</div>
      ) : (
        <>
          {/* Pageview journey — human-readable navigation flow */}
          {pageviews.length > 0 && (
            <div style={{ marginBottom: "20px" }}>
              <h2 style={{ fontSize: "13px", fontWeight: 600, color: "var(--obs-text)", marginBottom: "12px" }}>
                Journey ({pageviews.length} event{pageviews.length !== 1 ? "s" : ""})
              </h2>
              <div class="sessions-timeline">
                {pageviews.map((pv, i) => {
                  const prev = i > 0 ? pageviews[i - 1].timestamp : pv.timestamp;
                  const delta = pv.timestamp - prev;
                  const deltaStr = i === 0 ? "start" : delta < 1000 ? `+${delta}ms` : delta < 60000 ? `+${Math.round(delta / 1000)}s` : `+${Math.round(delta / 60000)}m`;
                  return (
                    <div key={pv.event_id} class={`sessions-event sessions-event--${pv.event_type}`}>
                      <div class="sessions-event-header">
                        <span class="sessions-event-time">{formatTime(new Date(pv.timestamp).toISOString())}</span>
                        <span class="sessions-event-type">{pv.event_type}</span>
                        <span style={{ fontSize: "10px", color: "var(--obs-text-muted)" }}>{deltaStr}</span>
                      </div>
                      <div style={{ marginTop: "4px", fontSize: "13px", color: "var(--obs-text)" }}>
                        {pv.title && <div style={{ fontWeight: 500 }}>{pv.title}</div>}
                        <div style={{ fontSize: "11px", color: "var(--obs-text-muted)", fontFamily: "var(--obs-font-mono, monospace)" }}>
                          {pv.pathname || pv.url}
                        </div>
                      </div>
                    </div>
                  );
                })}
              </div>
            </div>
          )}

          {/* Replay events (clicks, scrolls, mutations) */}
          {events.length > 0 && (
            <div>
              <h2 style={{ fontSize: "13px", fontWeight: 600, color: "var(--obs-text)", marginBottom: "12px" }}>
                Replay Events ({events.length})
              </h2>
              <div class="sessions-timeline">
                {events.map(ev => {
                  const parsed = tryParseData(ev.data);
                  const isExpanded = expandedId === ev.event_id;
                  return (
                    <div key={ev.event_id} class={`sessions-event ${eventClass(ev.event_type)}`}>
                      <div class="sessions-event-header"
                        style={{ cursor: parsed ? "pointer" : "default" }}
                        onClick={() => parsed && setExpandedId(isExpanded ? null : ev.event_id)}>
                        <span class="sessions-event-time">{formatTime(ev.timestamp)}</span>
                        <span class="sessions-event-type">{ev.event_type}</span>
                      </div>
                      {isExpanded && parsed && (
                        <div class="sessions-event-data">
                          <CodeBlock code={parsed} maxHeight="200px" />
                        </div>
                      )}
                    </div>
                  );
                })}
              </div>
            </div>
          )}
        </>
      )}
    </div>
  );
}

// ─── Main Page ───

export default function SessionsPage() {
  const siteId = typeof window !== "undefined"
    ? new URLSearchParams(window.location.search).get("site_id") || "default"
    : "default";

  const PAGE_SIZE = 20;
  const [sessions, setSessions] = useState<ReplaySession[]>([]);
  const [loading, setLoading] = useState(true);
  const [page, setPage] = useState(1);
  const [errorOnly, setErrorOnly] = useState(false);
  const [selected, setSelected] = useState<ReplaySession | null>(null);

  const now = new Date();
  const from = new Date(now.getTime() - 86400000).toISOString();
  const to = now.toISOString();

  const fetchSessions = useCallback(async () => {
    setLoading(true);
    try {
      let data = await replaysApi.list(siteId, from, to, {
        limit: PAGE_SIZE,
        offset: (page - 1) * PAGE_SIZE,
      });
      if (errorOnly) {
        data = (data || []).filter(s => s.has_error);
      }
      setSessions(data || []);
    } catch { setSessions([]); }
    finally { setLoading(false); }
  }, [siteId, page, errorOnly]);

  useEffect(() => { fetchSessions(); }, [fetchSessions]);

  if (selected) {
    return <SessionDetail session={selected} onBack={() => setSelected(null)} />;
  }

  return (
    <div>
      <div class="obs-page-header">
        <h1 class="obs-page-title">Sessions</h1>
      </div>

      <div class="sessions-toolbar">
        <button
          class={`sessions-filter-btn ${errorOnly ? "sessions-filter-btn--active" : ""}`}
          onClick={() => { setErrorOnly(!errorOnly); setPage(1); }}
        >
          Errors only
        </button>
        <span style={{ fontSize: "12px", color: "var(--obs-text-muted)" }}>
          Last 24 hours
        </span>
      </div>

      {loading ? (
        <SessionsSkeleton />
      ) : sessions.length === 0 ? (
        <div class="obs-empty-state">
          {errorOnly ? "No sessions with errors" : "No session replays recorded"}
        </div>
      ) : (
        <>
          <div class="sessions-list obs-stagger">
            {sessions.map(s => (
              <div key={s.replay_id} class="sessions-card" onClick={() => setSelected(s)}>
                <div class="sessions-card-url">{s.url || "/"}</div>
                <div class="sessions-card-meta">
                  <span class="sessions-card-detail">{s.browser}</span>
                  <span class="sessions-card-detail">{s.os}</span>
                  <span class="sessions-card-detail">{formatDuration(s.duration_ms)}</span>
                  <span class="sessions-card-detail">{s.page_count} pg</span>
                  <span class="sessions-card-detail">{formatDate(s.start_time)}</span>
                  {s.has_error && <StatusBadge status="error" size="sm" />}
                </div>
              </div>
            ))}
          </div>
          <Pagination page={page} pageSize={PAGE_SIZE} resultCount={sessions.length}
            onPageChange={(p) => { setPage(p); window.scrollTo(0, 0); }} />
        </>
      )}
    </div>
  );
}
