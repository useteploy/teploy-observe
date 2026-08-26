import { useEffect, useMemo, useRef, useState } from "preact/hooks";
import { copyToClipboard } from "../lib/clipboard.js";
import { ROUTES, fuzzyScore, routeKeywords } from "../lib/paletteRoutes.js";

interface PaletteItem {
  label: string;
  description?: string;
  action: () => void;
  keywords?: string;
  group: string;
}

export default function CommandPalette() {
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");
  const [sel, setSel] = useState(0);
  const inputRef = useRef<HTMLInputElement>(null);

  // Global hotkey: Cmd/Ctrl+K
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        setOpen((v) => !v);
      } else if (e.key === "Escape") {
        setOpen(false);
      }
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, []);

  useEffect(() => {
    if (open) {
      setQuery("");
      setSel(0);
      // defer so the DOM exists
      queueMicrotask(() => inputRef.current?.focus());
    }
  }, [open]);

  const items = useMemo<PaletteItem[]>(() => {
    const navItems = ROUTES.map((r) => ({
      label: r.label,
      description: r.description,
      group: "Navigate",
      keywords: routeKeywords(r),
      action: () => {
        window.history.pushState(null, "", r.path);
        window.dispatchEvent(new PopStateEvent("popstate"));
      },
    }));
    const actions: PaletteItem[] = [
      {
        label: "Copy site ID",
        description: "Copy the current site_id to clipboard",
        group: "Actions",
        keywords: "copy site id",
        action: () => {
          const siteId = new URLSearchParams(window.location.search).get("site_id") || "default";
          void copyToClipboard(siteId);
        },
      },
      {
        label: "Sign out",
        description: "Clear token and return to login",
        group: "Actions",
        keywords: "logout sign out",
        action: () => {
          localStorage.removeItem("obs_token");
          window.location.href = "/login";
        },
      },
    ];
    return [...navItems, ...actions];
  }, []);

  const filtered = useMemo(() => {
    const scored = items
      .map((i) => ({ item: i, score: fuzzyScore(query, i.keywords || i.label) }))
      .filter((x) => x.score > 0)
      .sort((a, b) => b.score - a.score);
    return scored.map((x) => x.item);
  }, [items, query]);

  const runSelected = () => {
    const item = filtered[sel];
    if (item) {
      item.action();
      setOpen(false);
    }
  };

  const onKey = (e: KeyboardEvent) => {
    if (e.key === "ArrowDown") {
      e.preventDefault();
      setSel((s) => Math.min(s + 1, filtered.length - 1));
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      setSel((s) => Math.max(s - 1, 0));
    } else if (e.key === "Enter") {
      e.preventDefault();
      runSelected();
    }
  };

  if (!open) return null;

  // Group items by group name while preserving ranked order.
  const groups: Record<string, PaletteItem[]> = {};
  const groupOrder: string[] = [];
  for (const i of filtered) {
    if (!groups[i.group]) { groups[i.group] = []; groupOrder.push(i.group); }
    groups[i.group].push(i);
  }

  let idx = -1;
  return (
    <div class="cmdk-overlay" onClick={() => setOpen(false)}>
      <div class="cmdk-modal" onClick={(e) => e.stopPropagation()}>
        <div class="cmdk-search">
          <svg class="cmdk-icon" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
            <circle cx="11" cy="11" r="8" />
            <path d="M21 21l-4.35-4.35" />
          </svg>
          <input
            ref={inputRef}
            class="cmdk-input"
            type="text"
            placeholder="Jump to... (type a route or action)"
            value={query}
            onInput={(e) => { setQuery((e.target as HTMLInputElement).value); setSel(0); }}
            onKeyDown={onKey}
          />
          <span class="cmdk-hint">Esc to close</span>
        </div>
        <div class="cmdk-results">
          {filtered.length === 0 ? (
            <div class="cmdk-empty">No matches</div>
          ) : (
            groupOrder.map((g) => (
              <div key={g} class="cmdk-group">
                <div class="cmdk-group-label">{g}</div>
                {groups[g].map((item) => {
                  idx++;
                  const active = idx === sel;
                  return (
                    <div
                      key={item.label}
                      class={`cmdk-item ${active ? "cmdk-item--active" : ""}`}
                      onMouseEnter={() => setSel(idx)}
                      onClick={runSelected}
                    >
                      <div class="cmdk-item-label">{item.label}</div>
                      {item.description && <div class="cmdk-item-desc">{item.description}</div>}
                    </div>
                  );
                })}
              </div>
            ))
          )}
        </div>
        <div class="cmdk-footer">
          <span><kbd>↑</kbd><kbd>↓</kbd> navigate</span>
          <span><kbd>enter</kbd> select</span>
          <span><kbd>⌘</kbd><kbd>K</kbd> toggle</span>
        </div>
      </div>
    </div>
  );
}
