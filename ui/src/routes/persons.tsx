import { useState, useEffect, useCallback, useMemo } from "preact/hooks";
import { personsApi } from "../api/persons.js";
import type { Person, PersonDetail } from "../api/persons.js";
import EmptyState from "../components/shared/EmptyState.js";
import Pagination from "../components/shared/Pagination.js";
import { useFilters } from "../hooks/useFilters.js";

export const config = { mode: "app" };

const PAGE_SIZE = 25;

function fmtDate(ms: number): string {
  if (!ms) return "—";
  try {
    return new Date(ms).toLocaleString("en-US", {
      month: "short", day: "numeric", year: "numeric",
      hour: "2-digit", minute: "2-digit", hour12: false,
    });
  } catch { return String(ms); }
}

function fmtRelative(ms: number): string {
  if (!ms) return "—";
  const delta = Date.now() - ms;
  if (delta < 0) return fmtDate(ms);
  const s = Math.floor(delta / 1000);
  if (s < 60) return `${s}s ago`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ago`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ago`;
  const d = Math.floor(h / 24);
  return `${d}d ago`;
}

function truncateID(id: string): string {
  if (!id) return "(anonymous)";
  if (id.length <= 18) return id;
  return id.slice(0, 8) + "…" + id.slice(-6);
}

// ---------------------------------------------------------------------------
// Detail panel
// ---------------------------------------------------------------------------

function PersonDetailPanel({ distinctID, siteID, onBack }:
  { distinctID: string; siteID: string; onBack: () => void }) {
  const [data, setData] = useState<PersonDetail | null>(null);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    setLoading(true);
    personsApi.detail(distinctID, siteID)
      .then(d => setData(d))
      .catch(() => setData(null))
      .finally(() => setLoading(false));
  }, [distinctID, siteID]);

  return (
    <div>
      <button class="errors-back-btn obs-btn" onClick={onBack} style={{ marginBottom: "16px" }}>
        <svg width="14" height="14" viewBox="0 0 24 24" fill="currentColor" style={{ marginRight: "6px" }}>
          <path d="M20 11H7.83l5.59-5.59L12 4l-8 8 8 8 1.41-1.41L7.83 13H20v-2z" />
        </svg>
        Back to persons
      </button>

      {loading ? (
        <div class="obs-empty-state">Loading…</div>
      ) : !data ? (
        <div class="obs-empty-state">No data for this person</div>
      ) : (
        <>
          <div style={{ marginBottom: "20px" }}>
            <h1 class="obs-page-title" style={{ wordBreak: "break-all", fontSize: "18px" }}>
              {data.person.distinct_id || "(anonymous)"}
            </h1>
            <div style={{ display: "flex", gap: "16px", flexWrap: "wrap", fontSize: "12px",
              color: "var(--obs-text-muted)", marginTop: "8px" }}>
              <span>First seen: {fmtDate(data.person.first_seen_ms)}</span>
              <span>Last seen: {fmtDate(data.person.last_seen_ms)}</span>
              <span>Events: {data.person.event_count.toLocaleString()}</span>
              <span>Sessions: {data.person.session_count.toLocaleString()}</span>
              {data.person.top_country && <span>Country: {data.person.top_country}</span>}
              {data.person.top_browser && <span>Browser: {data.person.top_browser}</span>}
            </div>
          </div>

          <div>
            <h2 style={{ fontSize: "13px", fontWeight: 600, marginBottom: "12px" }}>
              Recent activity ({data.timeline.length})
            </h2>
            {data.timeline.length === 0 ? (
              <div class="obs-empty-state">No events recorded</div>
            ) : (
              <div class="sessions-timeline">
                {data.timeline.map(ev => (
                  <div key={ev.event_id} class="sessions-event">
                    <div class="sessions-event-header">
                      <span class="sessions-event-time">{fmtDate(ev.timestamp)}</span>
                      <span class="sessions-event-type">{ev.event_type}</span>
                    </div>
                    <div style={{ marginTop: "4px", fontSize: "12px",
                      color: "var(--obs-text-muted)",
                      fontFamily: "var(--obs-font-mono, monospace)" }}>
                      {ev.pathname || ev.url}
                    </div>
                  </div>
                ))}
              </div>
            )}
          </div>
        </>
      )}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Main page
// ---------------------------------------------------------------------------

export default function PersonsPage() {
  const { state: { siteId } } = useFilters();

  const [persons, setPersons] = useState<Person[]>([]);
  const [total, setTotal] = useState(0);
  const [loading, setLoading] = useState(true);
  const [page, setPage] = useState(1);
  const [search, setSearch] = useState("");
  const [includeAnonymous, setIncludeAnonymous] = useState(false);
  const [selected, setSelected] = useState<string | null>(null);

  // 30-day window matches PostHog's "Persons" default and the server
  // default in persons.DefaultWindow().
  const [from, to] = useMemo(() => {
    const now = new Date();
    const f = new Date(now.getTime() - 30 * 86400000);
    return [f.toISOString(), now.toISOString()];
  }, []);

  const fetchPersons = useCallback(async () => {
    setLoading(true);
    try {
      const r = await personsApi.list(siteId, {
        from, to,
        limit: PAGE_SIZE,
        offset: (page - 1) * PAGE_SIZE,
        includeAnonymous,
      });
      setPersons(r.persons || []);
      setTotal(r.total || 0);
    } catch {
      setPersons([]);
      setTotal(0);
    } finally {
      setLoading(false);
    }
  }, [siteId, from, to, page, includeAnonymous]);

  useEffect(() => { fetchPersons(); }, [fetchPersons]);

  // Client-side substring filter on the current page. The server doesn't
  // search by distinct_id substring (would force a sequential scan over
  // every event); the search input is a UI-only filter on the visible
  // page. Matches PostHog's pattern.
  const visible = useMemo(() => {
    if (!search.trim()) return persons;
    const q = search.toLowerCase();
    return persons.filter(p => p.distinct_id.toLowerCase().includes(q));
  }, [persons, search]);

  if (selected !== null) {
    return <PersonDetailPanel distinctID={selected} siteID={siteId} onBack={() => setSelected(null)} />;
  }

  return (
    <div>
      <div class="obs-page-header">
        <h1 class="obs-page-title">Persons</h1>
      </div>

      <div style={{ display: "flex", gap: "12px", marginBottom: "16px", alignItems: "center" }}>
        <input
          class="obs-input"
          placeholder="Search distinct_id…"
          value={search}
          onInput={(e) => setSearch((e.target as HTMLInputElement).value)}
          style={{ flex: 1, maxWidth: "400px" }}
          data-testid="persons-search"
        />
        <label style={{ fontSize: "12px", color: "var(--obs-text-muted)",
          display: "flex", gap: "6px", alignItems: "center", cursor: "pointer" }}>
          <input
            type="checkbox"
            checked={includeAnonymous}
            onChange={(e) => { setIncludeAnonymous((e.target as HTMLInputElement).checked); setPage(1); }}
          />
          Include anonymous
        </label>
        <span style={{ marginLeft: "auto", fontSize: "12px", color: "var(--obs-text-muted)" }}>
          {total.toLocaleString()} total
        </span>
      </div>

      {loading ? (
        <div class="obs-empty-state">Loading persons…</div>
      ) : persons.length === 0 ? (
        <EmptyState
          title="No identified users yet"
          description="Persons aggregates events by distinct_id. Call observe.identify(userId) from your SDK after the user logs in to populate this view."
          icon="signal"
          actions={[
            { label: "Read identify() docs", href: "/docs#identify" },
            { label: "Show anonymous traffic", onClick: () => setIncludeAnonymous(true) },
          ]}
        />
      ) : (
        <>
          <div style={{ border: "1px solid var(--obs-border)", borderRadius: "var(--obs-radius-md)",
            overflow: "hidden", background: "var(--obs-card)" }}>
            <table style={{ width: "100%", borderCollapse: "collapse", fontSize: "13px" }}>
              <thead>
                <tr style={{ background: "var(--obs-card-alt, var(--obs-card))",
                  textAlign: "left", color: "var(--obs-text-muted)", fontSize: "11px",
                  textTransform: "uppercase", letterSpacing: "0.04em" }}>
                  <th style={{ padding: "10px 12px" }}>Distinct ID</th>
                  <th style={{ padding: "10px 12px" }}>First seen</th>
                  <th style={{ padding: "10px 12px" }}>Last seen</th>
                  <th style={{ padding: "10px 12px", textAlign: "right" }}>Events</th>
                  <th style={{ padding: "10px 12px", textAlign: "right" }}>Sessions</th>
                  <th style={{ padding: "10px 12px" }}>Country</th>
                </tr>
              </thead>
              <tbody>
                {visible.map(p => (
                  <tr
                    key={p.distinct_id || "anon"}
                    onClick={() => setSelected(p.distinct_id)}
                    style={{ cursor: "pointer", borderTop: "1px solid var(--obs-border)" }}
                    data-testid="person-row"
                  >
                    <td style={{ padding: "10px 12px",
                      fontFamily: "var(--obs-font-mono, monospace)", fontSize: "12px" }}
                      title={p.distinct_id || "(anonymous)"}>
                      {truncateID(p.distinct_id)}
                    </td>
                    <td style={{ padding: "10px 12px", color: "var(--obs-text-muted)" }}>
                      {fmtDate(p.first_seen_ms)}
                    </td>
                    <td style={{ padding: "10px 12px" }}>
                      {fmtRelative(p.last_seen_ms)}
                    </td>
                    <td style={{ padding: "10px 12px", textAlign: "right" }}>
                      {p.event_count.toLocaleString()}
                    </td>
                    <td style={{ padding: "10px 12px", textAlign: "right" }}>
                      {p.session_count.toLocaleString()}
                    </td>
                    <td style={{ padding: "10px 12px", color: "var(--obs-text-muted)" }}>
                      {p.top_country || "—"}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <Pagination page={page} pageSize={PAGE_SIZE} resultCount={persons.length}
            onPageChange={(p) => { setPage(p); window.scrollTo(0, 0); }} />
        </>
      )}
    </div>
  );
}
