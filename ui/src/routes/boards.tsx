import { useState, useEffect, useCallback } from "preact/hooks";
import { get, post, del } from "../api/helpers.js";
import Modal from "../components/shared/Modal.js";
import EmptyState from "../components/shared/EmptyState.js";
import "../styles/boards.css";

export const config = { mode: "app" };

// Mirrors internal/query.SiteRow + sites.Site shapes.
interface SiteRow {
  site_id: string;
  site_name: string;
  domain: string;
  pageviews: number;
  visitors: number;
  sessions: number;
  errors: number;
  uptime_pct: number;
  replay_count: number;
  last_activity_ms: number;
}
interface SiteOption {
  site_id: string;
  domain: string;
  name: string;
}
interface SavedBoard {
  board_id: string;
  name: string;
  payload: string;
  created_at: string;
  created_by: string;
}
interface BoardPayload {
  site_ids: string[];
  metrics?: string[];
  window?: string;
}

const WINDOWS: Array<{ key: string; label: string; ms: number }> = [
  { key: "1h", label: "Last hour", ms: 3600_000 },
  { key: "24h", label: "Last 24 hours", ms: 86_400_000 },
  { key: "7d", label: "Last 7 days", ms: 7 * 86_400_000 },
  { key: "30d", label: "Last 30 days", ms: 30 * 86_400_000 },
];

function rangeFor(windowKey: string): { from: string; to: string; key: string } {
  const w = WINDOWS.find(x => x.key === windowKey) || WINDOWS[1];
  const to = new Date();
  const from = new Date(to.getTime() - w.ms);
  return { from: from.toISOString(), to: to.toISOString(), key: w.key };
}

function formatPct(v: number): string {
  if (v <= 0) return "—";
  if (v >= 99.95) return "100%";
  return v.toFixed(1) + "%";
}

function formatNum(n: number): string {
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(1) + "M";
  if (n >= 1_000) return (n / 1_000).toFixed(1) + "k";
  return String(n);
}

function formatRelative(ms: number): string {
  if (!ms) return "—";
  const diff = Date.now() - ms;
  if (diff < 60_000) return "just now";
  if (diff < 3_600_000) return Math.floor(diff / 60_000) + "m ago";
  if (diff < 86_400_000) return Math.floor(diff / 3_600_000) + "h ago";
  return Math.floor(diff / 86_400_000) + "d ago";
}

// ---------------------------------------------------------------------------
// Builder modal
// ---------------------------------------------------------------------------

function BoardBuilder({
  open,
  sites,
  initialName,
  initialIDs,
  onClose,
  onSave,
}: {
  open: boolean;
  sites: SiteOption[];
  initialName: string;
  initialIDs: string[];
  onClose: () => void;
  onSave: (name: string, ids: string[], windowKey: string) => Promise<void>;
}) {
  const [name, setName] = useState(initialName);
  const [selected, setSelected] = useState<Record<string, boolean>>({});
  const [windowKey, setWindowKey] = useState("24h");
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");

  useEffect(() => {
    if (!open) return;
    setName(initialName);
    const seed: Record<string, boolean> = {};
    initialIDs.forEach(id => { seed[id] = true; });
    setSelected(seed);
    setError("");
  }, [open, initialName, initialIDs.join(",")]);

  const toggleSite = (id: string) => {
    setSelected(prev => ({ ...prev, [id]: !prev[id] }));
  };

  const selectAll = () => {
    const all: Record<string, boolean> = {};
    sites.forEach(s => { all[s.site_id] = true; });
    setSelected(all);
  };

  const clearAll = () => setSelected({});

  const ids = sites.filter(s => selected[s.site_id]).map(s => s.site_id);

  const submit = async () => {
    setError("");
    if (!name.trim()) {
      setError("Name required");
      return;
    }
    if (ids.length === 0) {
      setError("Pick at least one site");
      return;
    }
    setSaving(true);
    try {
      await onSave(name.trim(), ids, windowKey);
      onClose();
    } catch (e: any) {
      setError(e?.message || "Save failed");
    } finally {
      setSaving(false);
    }
  };

  if (!open) return null;

  return (
    <Modal open={open} title="New board" onClose={onClose}>
      <div class="boards-builder">
        <label class="boards-field">
          <span>Name</span>
          <input
            class="obs-input"
            placeholder="All Customers"
            value={name}
            onInput={(e) => setName((e.target as HTMLInputElement).value)}
          />
        </label>
        <label class="boards-field">
          <span>Default time window</span>
          <select
            class="obs-select"
            value={windowKey}
            onChange={(e) => setWindowKey((e.target as HTMLSelectElement).value)}
          >
            {WINDOWS.map(w => (
              <option key={w.key} value={w.key}>{w.label}</option>
            ))}
          </select>
        </label>
        <div class="boards-field">
          <div class="boards-builder-header">
            <span>Sites ({ids.length} selected)</span>
            <div class="boards-builder-toolbar">
              <button class="obs-btn obs-btn--sm" type="button" onClick={selectAll}>Select all</button>
              <button class="obs-btn obs-btn--sm" type="button" onClick={clearAll}>Clear</button>
            </div>
          </div>
          <div class="boards-site-list">
            {sites.length === 0 && (
              <p class="boards-muted">No sites yet — create one from Settings.</p>
            )}
            {sites.map(s => (
              <label key={s.site_id} class="boards-site-row">
                <input
                  type="checkbox"
                  checked={!!selected[s.site_id]}
                  onChange={() => toggleSite(s.site_id)}
                />
                <span class="boards-site-name">{s.name || s.site_id}</span>
                <span class="boards-site-domain">{s.domain}</span>
              </label>
            ))}
          </div>
        </div>
        {error && <p class="boards-error">{error}</p>}
        <div class="boards-builder-actions">
          <button class="obs-btn" type="button" onClick={onClose} disabled={saving}>Cancel</button>
          <button class="obs-btn obs-btn--primary" type="button" onClick={submit} disabled={saving || !ids.length}>
            {saving ? "Saving..." : "Create"}
          </button>
        </div>
      </div>
    </Modal>
  );
}

// ---------------------------------------------------------------------------
// Open board (grid view)
// ---------------------------------------------------------------------------

function BoardGrid({
  rows,
  loading,
}: {
  rows: SiteRow[];
  loading: boolean;
}) {
  if (loading) {
    return (
      <div class="boards-loading">
        {Array.from({ length: 4 }).map((_, i) => (
          <div class="boards-skeleton-row" key={i} />
        ))}
      </div>
    );
  }
  if (rows.length === 0) {
    return <p class="boards-muted">No data for the selected sites.</p>;
  }
  return (
    <table class="boards-table">
      <thead>
        <tr>
          <th>Site</th>
          <th class="boards-num">Pageviews</th>
          <th class="boards-num">Visitors</th>
          <th class="boards-num">Errors</th>
          <th class="boards-num">Uptime</th>
          <th class="boards-num">Replays</th>
          <th class="boards-num">Last activity</th>
        </tr>
      </thead>
      <tbody>
        {rows.map(r => (
          <tr
            key={r.site_id}
            class="boards-row"
            onClick={() => { window.location.href = "/?site_id=" + encodeURIComponent(r.site_id); }}
          >
            <td>
              <div class="boards-site-cell">
                <strong>{r.site_name || r.site_id}</strong>
                {r.domain && <small>{r.domain}</small>}
              </div>
            </td>
            <td class="boards-num">{formatNum(r.pageviews)}</td>
            <td class="boards-num">{formatNum(r.visitors)}</td>
            <td class={"boards-num" + (r.errors > 0 ? " boards-warn" : "")}>{formatNum(r.errors)}</td>
            <td class="boards-num">{formatPct(r.uptime_pct)}</td>
            <td class="boards-num">{formatNum(r.replay_count)}</td>
            <td class="boards-num">{formatRelative(r.last_activity_ms)}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

// ---------------------------------------------------------------------------
// Page
// ---------------------------------------------------------------------------

export default function BoardsPage() {
  const [sites, setSites] = useState<SiteOption[]>([]);
  const [boards, setBoards] = useState<SavedBoard[]>([]);
  const [openID, setOpenID] = useState<string>("");
  const [openName, setOpenName] = useState<string>("");
  const [openIDs, setOpenIDs] = useState<string[]>([]);
  const [windowKey, setWindowKey] = useState<string>("24h");
  const [rows, setRows] = useState<SiteRow[]>([]);
  const [loadingRows, setLoadingRows] = useState(false);
  const [showBuilder, setShowBuilder] = useState(false);

  // Initial fetch: sites + saved boards.
  useEffect(() => {
    (async () => {
      try {
        const [s, b] = await Promise.all([
          get<SiteOption[]>("/api/v1/sites"),
          get<SavedBoard[]>("/api/v1/boards/saved"),
        ]);
        setSites(s || []);
        setBoards(b || []);
      } catch {
        // soft-fail; the empty state covers it
      }
    })();
  }, []);

  const loadSummary = useCallback(async (ids: string[], wKey: string) => {
    if (ids.length === 0) {
      setRows([]);
      return;
    }
    setLoadingRows(true);
    try {
      const { from, to } = rangeFor(wKey);
      const r = await get<SiteRow[]>(
        "/api/v1/boards/summary?site_ids=" + encodeURIComponent(ids.join(",")) +
        "&from=" + encodeURIComponent(from) +
        "&to=" + encodeURIComponent(to),
      );
      setRows(r || []);
    } catch {
      setRows([]);
    } finally {
      setLoadingRows(false);
    }
  }, []);

  const openBoard = (b: SavedBoard) => {
    let payload: BoardPayload = { site_ids: [] };
    try { payload = JSON.parse(b.payload || "{}"); } catch {}
    const ids = payload.site_ids || [];
    const w = payload.window || "24h";
    setOpenID(b.board_id);
    setOpenName(b.name);
    setOpenIDs(ids);
    setWindowKey(w);
    loadSummary(ids, w);
  };

  const closeBoard = () => {
    setOpenID("");
    setOpenName("");
    setOpenIDs([]);
    setRows([]);
  };

  const createBoard = async (name: string, ids: string[], wKey: string) => {
    const payload: BoardPayload = { site_ids: ids, window: wKey, metrics: ["pageviews", "errors", "uptime", "replays"] };
    const created = await post<SavedBoard>("/api/v1/boards/saved", { name, payload });
    setBoards(prev => [created, ...prev.filter(b => b.board_id !== created.board_id)]);
    openBoard(created);
  };

  const removeBoard = async (id: string) => {
    if (!confirm("Delete this board?")) return;
    try {
      await del("/api/v1/boards/saved/" + encodeURIComponent(id));
      setBoards(prev => prev.filter(b => b.board_id !== id));
      if (openID === id) closeBoard();
    } catch {
      // swallow — UI still shows the previous list
    }
  };

  const refresh = () => loadSummary(openIDs, windowKey);

  return (
    <div class="boards-page">
      <div class="boards-page-header">
        <div>
          <h1>Boards</h1>
          <p class="boards-muted">Aggregate stats across multiple sites — agency / MSP overview.</p>
        </div>
        <button class="obs-btn obs-btn--primary" type="button" onClick={() => setShowBuilder(true)}>
          + New board
        </button>
      </div>

      {!openID && boards.length === 0 && (
        <EmptyState
          icon="layers"
          title="No boards yet"
          description="Create your first board to monitor multiple sites at a glance."
          actions={[{
            label: "Create your first board",
            primary: true,
            onClick: () => setShowBuilder(true),
          }]}
        />
      )}

      {!openID && boards.length > 0 && (
        <div class="boards-card-grid">
          {boards.map(b => {
            let payload: BoardPayload = { site_ids: [] };
            try { payload = JSON.parse(b.payload || "{}"); } catch {}
            const ids = payload.site_ids || [];
            return (
              <div key={b.board_id} class="boards-card" onClick={() => openBoard(b)}>
                <h3>{b.name}</h3>
                <p class="boards-card-meta">
                  {ids.length} site{ids.length === 1 ? "" : "s"} · {payload.window || "24h"}
                </p>
                <div class="boards-card-actions">
                  <button class="obs-btn obs-btn--sm" type="button"
                    onClick={(e) => { e.stopPropagation(); openBoard(b); }}>Open</button>
                  <button class="obs-btn obs-btn--sm" type="button"
                    onClick={(e) => { e.stopPropagation(); removeBoard(b.board_id); }}>Delete</button>
                </div>
              </div>
            );
          })}
        </div>
      )}

      {openID && (
        <div class="boards-open">
          <div class="boards-open-header">
            <button class="obs-btn obs-btn--sm" type="button" onClick={closeBoard}>← Back</button>
            <h2>{openName}</h2>
            <div class="boards-open-toolbar">
              <select class="obs-select obs-select--sm" value={windowKey}
                onChange={(e) => {
                  const v = (e.target as HTMLSelectElement).value;
                  setWindowKey(v);
                  loadSummary(openIDs, v);
                }}>
                {WINDOWS.map(w => (
                  <option key={w.key} value={w.key}>{w.label}</option>
                ))}
              </select>
              <button class="obs-btn obs-btn--sm" type="button" onClick={refresh}>Refresh</button>
            </div>
          </div>
          <BoardGrid rows={rows} loading={loadingRows} />
        </div>
      )}

      <BoardBuilder
        open={showBuilder}
        sites={sites}
        initialName=""
        initialIDs={[]}
        onClose={() => setShowBuilder(false)}
        onSave={createBoard}
      />
    </div>
  );
}
