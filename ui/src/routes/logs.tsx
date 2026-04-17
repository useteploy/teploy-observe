import { useState, useEffect, useCallback } from "preact/hooks";
import { logsApi } from "../api/logs.js";
import type { LogEntry, LogStats } from "../api/logs.js";
import SearchInput from "../components/shared/SearchInput.js";
import StatusBadge from "../components/shared/StatusBadge.js";
import CodeBlock from "../components/shared/CodeBlock.js";
import Pagination from "../components/shared/Pagination.js";
import "../styles/logs.css";

export const config = { mode: "app" };

const LEVELS = ["ALL", "DEBUG", "INFO", "WARN", "ERROR", "FATAL"] as const;

const STAT_COLORS: Record<string, string> = {
  debug: "#52525b",
  info: "#6366f1",
  warn: "#f59e0b",
  error: "#ef4444",
  fatal: "#991b1b",
};

function formatTimestamp(iso: string): string {
  try {
    const d = new Date(iso);
    return d.toLocaleTimeString("en-US", {
      hour: "2-digit",
      minute: "2-digit",
      second: "2-digit",
      hour12: false,
    });
  } catch {
    return iso;
  }
}

function formatFullTimestamp(iso: string): string {
  try {
    const d = new Date(iso);
    return d.toLocaleString("en-US", {
      year: "numeric",
      month: "short",
      day: "numeric",
      hour: "2-digit",
      minute: "2-digit",
      second: "2-digit",
      hour12: false,
    });
  } catch {
    return iso;
  }
}

function tryParseJson(raw: string): string | null {
  if (!raw || raw === "{}" || raw === "null") return null;
  try {
    const parsed = JSON.parse(raw);
    if (typeof parsed === "object" && parsed !== null && Object.keys(parsed).length > 0) {
      return JSON.stringify(parsed, null, 2);
    }
    return null;
  } catch {
    return raw;
  }
}

function LogEntrySkeleton() {
  return (
    <div class="logs-loading">
      {Array.from({ length: 8 }).map((_, i) => (
        <div class="logs-skeleton-row" key={i}>
          <div class="logs-skeleton-bar" style={{ width: "70px" }} />
          <div class="logs-skeleton-bar" style={{ width: "48px" }} />
          <div class="logs-skeleton-bar" style={{ width: "60px" }} />
          <div class="logs-skeleton-bar" style={{ flex: 1 }} />
        </div>
      ))}
    </div>
  );
}

interface LogRowProps {
  entry: LogEntry;
  expanded: boolean;
  onToggle: () => void;
}

function LogRow({ entry, expanded, onToggle }: LogRowProps) {
  const attrs = tryParseJson(entry.attributes);

  return (
    <>
      <div
        class={`logs-entry ${expanded ? "logs-entry--expanded" : ""}`}
        onClick={onToggle}
      >
        <span class="logs-entry-ts">{formatTimestamp(entry.timestamp)}</span>
        <span class="logs-entry-badge">
          <StatusBadge status={entry.level.toLowerCase()} size="sm" />
        </span>
        <span class="logs-entry-service">{entry.service_name || "--"}</span>
        <span class="logs-entry-message">{entry.message}</span>
      </div>
      {expanded && (
        <div class="logs-detail">
          <div class="logs-detail-section">
            <div class="logs-detail-meta">
              <span class="logs-detail-meta-item">
                <span class="logs-detail-meta-key">Time</span>
                {formatFullTimestamp(entry.timestamp)}
              </span>
              <span class="logs-detail-meta-item">
                <span class="logs-detail-meta-key">Level</span>
                {entry.level}
              </span>
              <span class="logs-detail-meta-item">
                <span class="logs-detail-meta-key">Service</span>
                {entry.service_name || "--"}
              </span>
              {entry.trace_id && (
                <span class="logs-detail-meta-item">
                  <span class="logs-detail-meta-key">Trace</span>
                  <a
                    class="logs-trace-link"
                    href={`/traces?trace_id=${entry.trace_id}`}
                    onClick={(e) => e.stopPropagation()}
                  >
                    {entry.trace_id.slice(0, 16)}...
                  </a>
                </span>
              )}
              {entry.span_id && (
                <span class="logs-detail-meta-item">
                  <span class="logs-detail-meta-key">Span</span>
                  {entry.span_id}
                </span>
              )}
            </div>
          </div>
          <div class="logs-detail-section">
            <div class="logs-detail-label">Message</div>
            <div class="logs-detail-message">{entry.message}</div>
          </div>
          {attrs && (
            <div class="logs-detail-section">
              <div class="logs-detail-label">Attributes</div>
              <CodeBlock code={attrs} maxHeight="300px" />
            </div>
          )}
        </div>
      )}
    </>
  );
}

export default function LogsPage() {
  const siteId =
    typeof window !== "undefined"
      ? new URLSearchParams(window.location.search).get("site_id") || "default"
      : "default";

  const PAGE_SIZE = 50;
  const [logs, setLogs] = useState<LogEntry[]>([]);
  const [stats, setStats] = useState<LogStats[]>([]);
  const [loading, setLoading] = useState(true);
  const [query, setQuery] = useState("");
  const [activeLevel, setActiveLevel] = useState<string>("ALL");
  const [service, setService] = useState("");
  const [expandedId, setExpandedId] = useState<string | null>(null);
  const [page, setPage] = useState(1);

  const fetchLogs = useCallback(async () => {
    setLoading(true);
    const now = new Date();
    const from = new Date(now.getTime() - 86400000);
    const fromStr = from.toISOString();
    const toStr = now.toISOString();

    try {
      const opts: { query?: string; level?: string; service?: string; limit?: number; offset?: number } = {
        limit: PAGE_SIZE,
        offset: (page - 1) * PAGE_SIZE,
      };
      if (query.trim()) opts.query = query.trim();
      if (activeLevel !== "ALL") opts.level = activeLevel;
      if (service.trim()) opts.service = service.trim();

      const [logData, statData] = await Promise.all([
        logsApi.search(siteId, fromStr, toStr, opts),
        logsApi.stats(siteId, fromStr, toStr),
      ]);

      setLogs(logData || []);
      setStats(statData || []);
    } catch (err) {
      console.error("Failed to fetch logs:", err);
      setLogs([]);
      setStats([]);
    } finally {
      setLoading(false);
    }
  }, [siteId, query, activeLevel, service, page]);

  useEffect(() => {
    fetchLogs();
  }, [fetchLogs]);

  const handleSearch = () => {
    fetchLogs();
  };

  const handleLevelToggle = (level: string) => {
    setActiveLevel(level);
  };

  const toggleExpand = (logId: string) => {
    setExpandedId((prev) => (prev === logId ? null : logId));
  };

  const totalCount = stats.reduce((sum, s) => sum + s.count, 0);

  return (
    <div>
      <div class="obs-page-header">
        <h1 class="obs-page-title">Logs</h1>
      </div>

      {/* Toolbar */}
      <div class="logs-toolbar">
        <SearchInput
          value={query}
          onInput={setQuery}
          placeholder="Search logs..."
          onSubmit={handleSearch}
        />
        <input
          type="text"
          class="logs-service-input"
          placeholder="Service..."
          value={service}
          onInput={(e) => setService((e.target as HTMLInputElement).value)}
          onKeyDown={(e) => e.key === "Enter" && handleSearch()}
        />
      </div>

      {/* Level Filters */}
      <div class="logs-level-filters" style={{ marginBottom: "16px" }}>
        {LEVELS.map((level) => (
          <button
            key={level}
            class={`logs-level-btn logs-level-btn--${level.toLowerCase()} ${
              activeLevel === level ? "logs-level-btn--active" : ""
            }`}
            onClick={() => handleLevelToggle(level)}
          >
            {level}
          </button>
        ))}
      </div>

      {/* Stats Bar */}
      {stats.length > 0 && (
        <div class="logs-stats-bar">
          <span class="logs-stat-item">
            <span class="logs-stat-count">{totalCount.toLocaleString()}</span> total
          </span>
          {stats.map((s) => (
            <span class="logs-stat-item" key={s.level}>
              <span
                class="logs-stat-dot"
                style={{ background: STAT_COLORS[s.level.toLowerCase()] || "#52525b" }}
              />
              <span class="logs-stat-count">{s.count.toLocaleString()}</span>
              {s.level.toLowerCase()}
            </span>
          ))}
        </div>
      )}

      {/* Log Entries */}
      {loading ? (
        <LogEntrySkeleton />
      ) : logs.length === 0 ? (
        <div class="obs-empty-state">
          {query || activeLevel !== "ALL" || service
            ? "No logs match the current filters"
            : "No logs in the last 24 hours"}
        </div>
      ) : (
        <>
          <div class="logs-list obs-stagger">
            {logs.map((entry) => (
              <LogRow
                key={entry.log_id}
                entry={entry}
                expanded={expandedId === entry.log_id}
                onToggle={() => toggleExpand(entry.log_id)}
              />
            ))}
          </div>
          <Pagination page={page} pageSize={PAGE_SIZE} resultCount={logs.length} onPageChange={(p) => { setPage(p); window.scrollTo(0, 0); }} />
        </>
      )}
    </div>
  );
}
