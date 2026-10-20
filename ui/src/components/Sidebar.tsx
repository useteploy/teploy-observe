import { useState, useEffect } from "preact/hooks";
import SiteSwitcher from "./SiteSwitcher.js";
import ProductSwitcher from "./ProductSwitcher.js";
import { NAV_ITEMS, navKeyFor } from "../lib/navItems.js";

function NavIcon({ d }: { d: string }) {
  return (
    <svg width="18" height="18" viewBox="0 0 24 24" fill="currentColor">
      <path d={d} />
    </svg>
  );
}

export default function Sidebar() {
  // Resolved from the URL at first render. Hardcoding a default meant every
  // route change remounted the sidebar showing Dashboard selected for a frame
  // before the effect corrected it — a visible flash on the wrong item.
  const [active, setActive] = useState(() =>
    typeof window === "undefined" ? "analytics" : navKeyFor(window.location.pathname) ?? "analytics"
  );
  // Below 768px the sidebar is an off-canvas drawer opened from the top bar.
  const [open, setOpen] = useState(false);

  // Keep in step with back/forward and any programmatic navigation.
  useEffect(() => {
    const sync = () => {
      setActive(navKeyFor(window.location.pathname) ?? "analytics");
      setOpen(false);
    };
    sync();
    window.addEventListener("popstate", sync);
    return () => window.removeEventListener("popstate", sync);
  }, []);

  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => { if (e.key === "Escape") setOpen(false); };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [open]);

  // Routes outside the nav (incidents, meta) are titled from their path
  // rather than inheriting the Dashboard fallback `active` uses.
  const path = typeof window === "undefined" ? "/" : window.location.pathname;
  const segment = path.split("/")[1] ?? "";
  const activeLabel = navKeyFor(path)
    ? NAV_ITEMS.find(item => item.key === active)?.label ?? ""
    : segment.charAt(0).toUpperCase() + segment.slice(1);

  return (
    <>
    <header class="obs-mobile-bar">
      <button
        class="obs-mobile-nav-toggle"
        aria-label="Open navigation"
        aria-controls="obs-sidebar"
        aria-expanded={open}
        onClick={() => setOpen(true)}
      >
        <svg width="20" height="20" viewBox="0 0 24 24" fill="currentColor">
          <path d="M3 6h18v2H3V6zm0 5h18v2H3zm0 5h18v2H3v-2z" />
        </svg>
      </button>
      <span class="obs-sidebar-logo">Teploy</span>
      <span class="obs-mobile-bar-title">{activeLabel}</span>
    </header>
    {open && <div class="obs-sidebar-backdrop" onClick={() => setOpen(false)} />}
    <nav id="obs-sidebar" class={`obs-sidebar ${open ? "obs-sidebar--open" : ""}`}>
      {/* Wordmark and product switcher share the top row, matching the
          `Teploy [Product]` header Dash and Ship use. */}
      <div class="obs-sidebar-header">
        <span class="obs-sidebar-logo">Teploy</span>
        <ProductSwitcher />
        {/* Rides the header's bottom rule while requests are in flight. */}
        <span class="obs-load-bar" id="obs-load-bar" />
        <button class="obs-sidebar-close" aria-label="Close navigation" onClick={() => setOpen(false)}>
          <svg width="18" height="18" viewBox="0 0 24 24" fill="currentColor">
            <path d="M19 6.41L17.59 5 12 10.59 6.41 5 5 6.41 10.59 12 5 17.59 6.41 19 12 13.41 17.59 19 19 17.59 13.41 12z" />
          </svg>
        </button>
      </div>

      <SiteSwitcher />

      <ul class="obs-sidebar-nav">
        {NAV_ITEMS.map(item => (
          <li key={item.key}>
            <a
              href={item.href}
              class={`obs-sidebar-link ${active === item.key ? "obs-sidebar-link--active" : ""}`}
              onClick={(e) => {
                e.preventDefault();
                setActive(item.key);
                setOpen(false);
                history.pushState(null, "", item.href);
                window.dispatchEvent(new PopStateEvent("popstate"));
              }}
            >
              <NavIcon d={item.icon} />
              <span>{item.label}</span>
            </a>
          </li>
        ))}
      </ul>
    </nav>
    </>
  );
}
