import { useState, useEffect, useRef } from "preact/hooks";

interface NavApp {
  key: string;
  label: string;
  url: string;
}

/**
 * Cross-product dashboard switcher (top-left). Lets an operator jump between the
 * deployed Teploy dashboards — Dash, Observe, Ship. The entries come from
 * /api/v1/config (server reads TEPLOY_NAV_{DASH,OBSERVE,SHIP}_URL), so it only
 * appears once at least one sibling URL is configured. Fetched on mount, so it
 * adds nothing to SSR and can't cause a hydration mismatch.
 */
export default function ProductSwitcher({ collapsed = false }: { collapsed?: boolean }) {
  const [apps, setApps] = useState<NavApp[]>([]);
  const [current, setCurrent] = useState("");
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    fetch("/api/v1/config")
      .then((r) => (r.ok ? r.json() : null))
      .then((c) => {
        if (c?.nav?.apps) {
          setApps(c.nav.apps as NavApp[]);
          setCurrent(String(c.nav.current ?? ""));
        }
      })
      .catch(() => {});
  }, []);

  useEffect(() => {
    if (!open) return;
    const onDoc = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener("mousedown", onDoc);
    return () => document.removeEventListener("mousedown", onDoc);
  }, [open]);

  // Only show the switcher when there's somewhere else to switch to.
  if (apps.filter((a) => a.url !== "").length === 0) return null;
  const currentLabel = apps.find((a) => a.key === current)?.label ?? "Teploy";

  return (
    <div ref={ref} class="obs-product-switcher">
      <button
        class="obs-product-switcher-btn"
        onClick={() => setOpen(!open)}
        title="Switch dashboard"
      >
        <span class="obs-product-dot" />
        {!collapsed && <span class="obs-product-current">{currentLabel}</span>}
        {!collapsed && (
          <svg width="12" height="12" viewBox="0 0 24 24" fill="currentColor" style={{ marginLeft: "auto", opacity: 0.6 }}>
            <path d="M7 10l5 5 5-5z" />
          </svg>
        )}
      </button>
      {open && (
        <div class="obs-product-menu">
          {apps.map((a) =>
            a.key === current ? (
              <div key={a.key} class="obs-product-item obs-product-item--current">
                {a.label}
                <svg width="14" height="14" viewBox="0 0 24 24" fill="currentColor" style={{ marginLeft: "auto" }}>
                  <path d="M9 16.17L4.83 12l-1.42 1.41L9 19 21 7l-1.41-1.41z" />
                </svg>
              </div>
            ) : (
              <a key={a.key} href={a.url} class="obs-product-item">{a.label}</a>
            ),
          )}
        </div>
      )}
      <style>{`
        .obs-product-switcher { position: relative; padding: 8px 12px 4px; }
        .obs-product-switcher-btn {
          display: flex; align-items: center; gap: 8px; width: 100%;
          padding: 7px 9px; background: var(--obs-surface); color: var(--obs-text);
          border: 1px solid var(--obs-border-subtle); border-radius: var(--obs-radius);
          font-size: 13px; font-weight: 600; font-family: var(--obs-font); cursor: pointer;
        }
        .obs-product-switcher-btn:hover { border-color: var(--obs-border); }
        .obs-product-dot { width: 8px; height: 8px; border-radius: 50%;
          background: linear-gradient(135deg, var(--obs-accent), #a78bfa); flex: none; }
        .obs-product-current { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
        .obs-product-menu {
          position: absolute; left: 12px; right: 12px; top: 100%; z-index: 50;
          margin-top: 4px; background: var(--obs-surface);
          border: 1px solid var(--obs-border); border-radius: var(--obs-radius);
          box-shadow: 0 6px 24px rgba(0,0,0,0.35); overflow: hidden;
        }
        .obs-product-item {
          display: flex; align-items: center; gap: 8px; padding: 9px 12px;
          font-size: 13px; color: var(--obs-text); text-decoration: none; cursor: pointer;
        }
        a.obs-product-item:hover { background: var(--obs-bg); }
        .obs-product-item--current { color: var(--obs-text-muted); cursor: default; font-weight: 600; }
      `}</style>
    </div>
  );
}
