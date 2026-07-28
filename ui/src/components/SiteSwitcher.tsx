import { useEffect, useRef, useState } from "preact/hooks";
import { useFilters } from "../hooks/useFilters.js";
import { settingsApi } from "../api/settings.js";
import type { Site } from "../api/settings.js";

/**
 * Sites resolved once per page load. Navigating between routes remounts this
 * component, and without a cache each mount restarted from an empty list — so
 * the label fell back to the raw site id (a 32-char hash) and visibly flipped
 * to the real name a moment later on every page change.
 */
let sitesCache: Site[] = [];

const NAME_STORE = "obs_site_names";

/** Remembered id -> name, so a cold load resolves the label before the fetch. */
function readNames(): Record<string, string> {
  try {
    return JSON.parse(localStorage.getItem(NAME_STORE) ?? "{}") as Record<string, string>;
  } catch {
    return {};
  }
}

function rememberNames(sites: Site[]): void {
  try {
    const names = readNames();
    for (const s of sites) {
      const label = s.name || s.domain;
      if (label) names[s.site_id] = label;
    }
    localStorage.setItem(NAME_STORE, JSON.stringify(names));
  } catch { /* storage disabled — the in-memory cache still covers navigation */ }
}

/** A generated site id is opaque hex; showing it reads as a bug, so prefer a
 *  neutral placeholder until the real name arrives. Human ids ("default") are
 *  fine to show as-is. */
function placeholderFor(siteId: string): string {
  return /^[0-9a-f]{16,}$/i.test(siteId) ? "Loading site" : siteId;
}

interface Props {
  /** Render a compact pill suitable for the sidebar header. */
}

/**
 * Header dropdown that switches the active site for every route consuming
 * `useFilters().state.siteId`. Persists the choice via the reducer's
 * `SET_SITE` action — `RouteFilterProvider` handles URL + localStorage sync.
 */
export default function SiteSwitcher({}: Props) {
  const { state, dispatch } = useFilters();
  const [open, setOpen] = useState(false);
  const [sites, setSites] = useState<Site[]>(sitesCache);
  const [loading, setLoading] = useState(false);
  const [highlight, setHighlight] = useState(0);
  const wrapRef = useRef<HTMLDivElement>(null);

  // Lazy-load sites on first open so the sidebar render is cheap.
  useEffect(() => {
    if (!open || sites.length > 0 || loading) return;
    setLoading(true);
    settingsApi.sites()
      .then(d => { sitesCache = d || []; rememberNames(sitesCache); setSites(sitesCache); })
      .catch(() => setSites([]))
      .finally(() => setLoading(false));
  }, [open]);

  // Pre-fetch once on mount so the header label can resolve to a friendly name
  // even before the user clicks the switcher.
  useEffect(() => {
    settingsApi.sites()
      .then(d => { sitesCache = d || []; rememberNames(sitesCache); setSites(sitesCache); })
      .catch(() => { /* ignore */ });
  }, []);

  // Click-outside close.
  useEffect(() => {
    if (!open) return;
    const onDoc = (e: MouseEvent) => {
      if (wrapRef.current && !wrapRef.current.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener("mousedown", onDoc);
    return () => document.removeEventListener("mousedown", onDoc);
  }, [open]);

  // Reset highlight whenever menu opens or list changes.
  useEffect(() => {
    if (!open) return;
    const idx = sites.findIndex(s => s.site_id === state.siteId);
    setHighlight(idx >= 0 ? idx : 0);
  }, [open, sites, state.siteId]);

  const current = sites.find(s => s.site_id === state.siteId);
  const label =
    current?.name || current?.domain || readNames()[state.siteId] || placeholderFor(state.siteId);

  const select = (siteId: string) => {
    if (siteId !== state.siteId) dispatch({ type: "SET_SITE", siteId });
    setOpen(false);
  };

  const onKey = (e: KeyboardEvent) => {
    if (!open) {
      if (e.key === "Enter" || e.key === " " || e.key === "ArrowDown") {
        e.preventDefault();
        setOpen(true);
      }
      return;
    }
    if (e.key === "Escape") { e.preventDefault(); setOpen(false); return; }
    if (e.key === "ArrowDown") { e.preventDefault(); setHighlight(h => Math.min(h + 1, sites.length - 1)); return; }
    if (e.key === "ArrowUp")   { e.preventDefault(); setHighlight(h => Math.max(h - 1, 0)); return; }
    if (e.key === "Enter" && sites[highlight]) {
      e.preventDefault();
      select(sites[highlight].site_id);
    }
  };

  return (
    <div
      ref={wrapRef}
      class="obs-site-switcher"
      data-testid="site-switcher"
      data-open={open ? "true" : "false"}
    >
      <button
        type="button"
        class="obs-site-switcher-btn"
        onClick={() => setOpen(o => !o)}
        onKeyDown={onKey}
        aria-haspopup="listbox"
        aria-expanded={open}
        title={current?.domain || label}
        data-testid="site-switcher-trigger"
      >
        <span class="obs-site-switcher-dot" aria-hidden="true" />
        <span class="obs-site-switcher-label">{label}</span>
        {(
          <svg width="12" height="12" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true">
            <path d="M7 10l5 5 5-5z" />
          </svg>
        )}
      </button>

      {open && (
        <div class="obs-site-switcher-menu" role="listbox" data-testid="site-switcher-menu">
          {loading && sites.length === 0 ? (
            <div class="obs-site-switcher-loading">Loading…</div>
          ) : sites.length === 0 ? (
            <div class="obs-site-switcher-empty">No sites</div>
          ) : (
            sites.map((s, i) => {
              const active = s.site_id === state.siteId;
              const hl = i === highlight;
              return (
                <button
                  type="button"
                  key={s.site_id}
                  role="option"
                  aria-selected={active}
                  class={`obs-site-switcher-item ${active ? "obs-site-switcher-item--active" : ""} ${hl ? "obs-site-switcher-item--hl" : ""}`}
                  onMouseEnter={() => setHighlight(i)}
                  onClick={() => select(s.site_id)}
                  data-testid="site-switcher-option"
                  data-site-id={s.site_id}
                >
                  <span class="obs-site-switcher-item-name">{s.name || s.site_id}</span>
                  {s.domain && <span class="obs-site-switcher-item-domain">{s.domain}</span>}
                </button>
              );
            })
          )}
          <a
            class="obs-site-switcher-create"
            href="/settings"
            onClick={(e) => {
              e.preventDefault();
              setOpen(false);
              history.pushState(null, "", "/settings");
              window.dispatchEvent(new PopStateEvent("popstate"));
            }}
            data-testid="site-switcher-create"
          >
            <svg width="12" height="12" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true">
              <path d="M19 13h-6v6h-2v-6H5v-2h6V5h2v6h6v2z" />
            </svg>
            <span>Create new site</span>
          </a>
        </div>
      )}
    </div>
  );
}
