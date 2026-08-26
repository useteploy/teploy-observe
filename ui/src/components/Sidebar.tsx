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
  const [theme, setTheme] = useState(() => {
    if (typeof window === "undefined") return "dark";
    return localStorage.getItem("obs_theme") || "dark";
  });

  const toggleTheme = () => {
    const next = theme === "dark" ? "light" : "dark";
    setTheme(next);
    localStorage.setItem("obs_theme", next);
    if (next === "light") {
      document.documentElement.setAttribute("data-theme", "light");
    } else {
      document.documentElement.removeAttribute("data-theme");
    }
  };

  useEffect(() => {
    if (theme === "light") {
      document.documentElement.setAttribute("data-theme", "light");
    }
  }, []);

  // Keep in step with back/forward and any programmatic navigation.
  useEffect(() => {
    const sync = () => setActive(navKeyFor(window.location.pathname) ?? "analytics");
    sync();
    window.addEventListener("popstate", sync);
    return () => window.removeEventListener("popstate", sync);
  }, []);

  return (
    <nav class="obs-sidebar">
      {/* Wordmark and product switcher share the top row, matching the
          `Teploy [Product]` header Dash and Ship use. */}
      <div class="obs-sidebar-header">
        <span class="obs-sidebar-logo">Teploy</span>
        <ProductSwitcher />
        {/* Rides the header's bottom rule while requests are in flight. */}
        <span class="obs-load-bar" id="obs-load-bar" />
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

      <div style={{ marginTop: "auto", padding: "12px" }}>
        <button class="obs-sidebar-link" onClick={toggleTheme}
          style={{ width: "100%", border: "none", cursor: "pointer", background: "none" }}>
          <svg width="18" height="18" viewBox="0 0 24 24" fill="currentColor">
            {theme === "dark"
              ? <path d="M12 7c-2.76 0-5 2.24-5 5s2.24 5 5 5 5-2.24 5-5-2.24-5-5-5zM2 13h2c.55 0 1-.45 1-1s-.45-1-1-1H2c-.55 0-1 .45-1 1s.45 1 1 1zm18 0h2c.55 0 1-.45 1-1s-.45-1-1-1h-2c-.55 0-1 .45-1 1s.45 1 1 1zM11 2v2c0 .55.45 1 1 1s1-.45 1-1V2c0-.55-.45-1-1-1s-1 .45-1 1zm0 18v2c0 .55.45 1 1 1s1-.45 1-1v-2c0-.55-.45-1-1-1s-1 .45-1 1zM5.99 4.58c-.39-.39-1.03-.39-1.41 0-.39.39-.39 1.03 0 1.41l1.06 1.06c.39.39 1.03.39 1.41 0s.39-1.03 0-1.41L5.99 4.58zm12.37 12.37c-.39-.39-1.03-.39-1.41 0-.39.39-.39 1.03 0 1.41l1.06 1.06c.39.39 1.03.39 1.41 0 .39-.39.39-1.03 0-1.41l-1.06-1.06zm1.06-10.96c.39-.39.39-1.03 0-1.41-.39-.39-1.03-.39-1.41 0l-1.06 1.06c-.39.39-.39 1.03 0 1.41s1.03.39 1.41 0l1.06-1.06zM7.05 18.36c.39-.39.39-1.03 0-1.41-.39-.39-1.03-.39-1.41 0l-1.06 1.06c-.39.39-.39 1.03 0 1.41s1.03.39 1.41 0l1.06-1.06z" />
              : <path d="M9.37 5.51A7.35 7.35 0 0 0 9.1 7.5c0 4.08 3.32 7.4 7.4 7.4.68 0 1.35-.09 1.99-.27A7.014 7.014 0 0 1 12 19c-3.86 0-7-3.14-7-7 0-2.93 1.81-5.45 4.37-6.49zM12 3a9 9 0 1 0 9 9c0-.46-.04-.92-.1-1.36a5.389 5.389 0 0 1-4.4 2.26 5.403 5.403 0 0 1-3.14-9.8c-.44-.06-.9-.1-1.36-.1z" />
            }
          </svg>
          <span>{theme === "dark" ? "Light Mode" : "Dark Mode"}</span>
        </button>
      </div>
    </nav>
  );
}
