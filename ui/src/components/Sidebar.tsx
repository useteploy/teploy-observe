import { useState, useEffect } from "preact/hooks";
import SiteSwitcher from "./SiteSwitcher.js";
import ProductSwitcher from "./ProductSwitcher.js";

/** The nav item owning a path. Longest href wins so "/dashboards" is not
 *  shadowed by a shorter prefix. */
function navKeyFor(path: string): string | null {
  let best: { key: string; len: number } | null = null;
  for (const item of NAV_ITEMS) {
    const hit = item.href === "/" ? path === "/" : path === item.href || path.startsWith(item.href + "/");
    if (hit && (best === null || item.href.length > best.len)) best = { key: item.key, len: item.href.length };
  }
  return best?.key ?? null;
}

const NAV_ITEMS = [
  { key: "analytics", label: "Dashboard", icon: "M3 13h4v8H3v-8zm7-4h4v12h-4V9zm7-6h4v18h-4V3z", href: "/" },
  { key: "dashboards", label: "Dashboards", icon: "M4 6h18V4H4c-1.1 0-2 .9-2 2v11H0v3h14v-3H4V6zm19 2h-6c-.55 0-1 .45-1 1v10c0 .55.45 1 1 1h6c.55 0 1-.45 1-1V9c0-.55-.45-1-1-1zm-1 9h-4v-7h4v7z", href: "/dashboards" },
  { key: "boards", label: "Boards", icon: "M3 5h8v6H3V5zm10 0h8v10h-8V5zM3 13h8v6H3v-6zm10 4h8v2h-8v-2z", href: "/boards" },
  { key: "events", label: "Events", icon: "M19 9h-4V3H9v6H5l7 7 7-7zM5 18v2h14v-2H5z", href: "/events" },
  { key: "campaigns", label: "Campaigns", icon: "M17.5 2h-11C5.67 2 5 2.67 5 3.5V22l7-3 7 3V3.5c0-.83-.67-1.5-1.5-1.5zM17 19l-5-2.18L7 19V4h10v15z", href: "/campaigns" },
  { key: "insights", label: "Insights", icon: "M19 3H5c-1.1 0-2 .9-2 2v14c0 1.1.9 2 2 2h14c1.1 0 2-.9 2-2V5c0-1.1-.9-2-2-2zm-7 14H5v-2h7v2zm5-4H5v-2h12v2zm0-4H5V7h12v2z", href: "/insights" },
  { key: "errors", label: "Errors", icon: "M12 2C6.48 2 2 6.48 2 12s4.48 10 10 10 10-4.48 10-10S17.52 2 12 2zm-2 15l-5-5 1.41-1.41L10 14.17l7.59-7.59L19 8l-9 9z", href: "/errors" },
  { key: "releases", label: "Releases", icon: "M7 14l5-5 5 5z", href: "/releases" },
  { key: "traces", label: "Traces", icon: "M3 4h18v2H3V4zm0 7h18v2H3v-2zm0 7h18v2H3v-2z", href: "/traces" },
  { key: "logs", label: "Logs", icon: "M3 3h18v18H3V3zm2 2v14h14V5H5zm2 2h10v2H7V7zm0 4h10v2H7v-2zm0 4h7v2H7v-2z", href: "/logs" },
  { key: "metrics", label: "Metrics", icon: "M3 3h2v18H3V3zm4 12h2v6H7v-6zm4-8h2v14h-2V7zm4 4h2v10h-2V11zm4-6h2v16h-2V5z", href: "/metrics" },
  { key: "flags", label: "Flags", icon: "M14.4 6L14 4H5v17h2v-7h5.6l.4 2h7V6h-5.6z", href: "/flags" },
  { key: "experiments", label: "Experiments", icon: "M7 2v2h1v14c0 1.1.9 2 2 2h4c1.1 0 2-.9 2-2V4h1V2H7zm4 14h-1V4h1v12zm2 0h-1V4h1v12z", href: "/experiments" },
  { key: "sessions", label: "Sessions", icon: "M12 12c2.21 0 4-1.79 4-4s-1.79-4-4-4-4 1.79-4 4 1.79 4 4 4zm0 2c-2.67 0-8 1.34-8 4v2h16v-2c0-2.66-5.33-4-8-4z", href: "/sessions" },
  { key: "persons", label: "Persons", icon: "M16 11c1.66 0 2.99-1.34 2.99-3S17.66 5 16 5c-1.66 0-3 1.34-3 3s1.34 3 3 3zm-8 0c1.66 0 2.99-1.34 2.99-3S9.66 5 8 5C6.34 5 5 6.34 5 8s1.34 3 3 3zm0 2c-2.33 0-7 1.17-7 3.5V19h14v-2.5c0-2.33-4.67-3.5-7-3.5zm8 0c-.29 0-.62.02-.97.05 1.16.84 1.97 1.97 1.97 3.45V19h6v-2.5c0-2.33-4.67-3.5-7-3.5z", href: "/persons" },
  { key: "cohorts", label: "Cohorts", icon: "M12 12.75c1.63 0 3.07.39 4.24.9 1.08.48 1.76 1.56 1.76 2.73L18 18H6v-1.61c0-1.18.68-2.26 1.76-2.73 1.17-.52 2.61-.91 4.24-.91zM4 13c1.1 0 2-.9 2-2s-.9-2-2-2-2 .9-2 2 .9 2 2 2zm1.13 1.1c-.37-.06-.74-.1-1.13-.1-.99 0-1.93.21-2.78.58C.48 14.9 0 15.62 0 16.43V18h4.5v-1.61c0-.83.23-1.61.63-2.29zM20 13c1.1 0 2-.9 2-2s-.9-2-2-2-2 .9-2 2 .9 2 2 2zm4 3.43c0-.81-.48-1.53-1.22-1.85-.85-.37-1.79-.58-2.78-.58-.39 0-.76.04-1.13.1.4.68.63 1.46.63 2.29V18H24v-1.57zM12 6c1.66 0 3 1.34 3 3s-1.34 3-3 3-3-1.34-3-3 1.34-3 3-3z", href: "/cohorts" },
  { key: "monitoring", label: "Monitoring", icon: "M12 2C6.48 2 2 6.48 2 12s4.48 10 10 10 10-4.48 10-10S17.52 2 12 2zm0 18c-4.42 0-8-3.58-8-8s3.58-8 8-8 8 3.58 8 8-3.58 8-8 8zm.5-13H11v6l5.25 3.15.75-1.23-4.5-2.67V7z", href: "/monitoring" },
  { key: "alerts", label: "Alerts", icon: "M12 22c1.1 0 2-.9 2-2h-4c0 1.1.89 2 2 2zm6-6v-5c0-3.07-1.64-5.64-4.5-6.32V4c0-.83-.67-1.5-1.5-1.5s-1.5.67-1.5 1.5v.68C7.63 5.36 6 7.92 6 11v5l-2 2v1h16v-1l-2-2z", href: "/alerts" },
  { key: "llm", label: "LLM", icon: "M20 4H4c-1.1 0-1.99.9-1.99 2L2 18c0 1.1.9 2 2 2h16c1.1 0 2-.9 2-2V6c0-1.1-.9-2-2-2zm-9 3h2v2h-2V7zm0 3h2v2h-2v-2zM8 7h2v2H8V7zm0 3h2v2H8v-2zm-1 2H5v-2h2v2zm0-3H5V7h2v2zm9 7H8v-2h8v2zm0-4h-2v-2h2v2zm0-3h-2V7h2v2zm3 3h-2v-2h2v2zm0-3h-2V7h2v2z", href: "/llm" },
  { key: "surveys", label: "Surveys", icon: "M9 11H7v2h2v-2zm4 0h-2v2h2v-2zm4 0h-2v2h2v-2zm2-7h-1V2h-2v2H8V2H6v2H5c-1.11 0-1.99.9-1.99 2L3 20c0 1.1.89 2 2 2h14c1.1 0 2-.9 2-2V6c0-1.1-.9-2-2-2zm0 16H5V9h14v11z", href: "/surveys" },
  { key: "integrations", label: "Integrations", icon: "M11 8c0-1.66-1.34-3-3-3S5 6.34 5 8s1.34 3 3 3 3-1.34 3-3zm8 5c1.66 0 3-1.34 3-3s-1.34-3-3-3-3 1.34-3 3 1.34 3 3 3zM8 13c-2.33 0-7 1.17-7 3.5V19h14v-2.5c0-2.33-4.67-3.5-7-3.5zm11 0c-.29 0-.62.02-.97.05 1.16.84 1.97 1.97 1.97 3.45V19h6v-2.5c0-2.33-4.67-3.5-7-3.5z", href: "/integrations" },
  { key: "reports", label: "Reports", icon: "M19 3H5c-1.1 0-2 .9-2 2v14c0 1.1.9 2 2 2h14c1.1 0 2-.9 2-2V5c0-1.1-.9-2-2-2zm-5 14H7v-2h7v2zm3-4H7v-2h10v2zm0-4H7V7h10v2z", href: "/reports" },
  { key: "audit", label: "Audit", icon: "M12 1L3 5v6c0 5.55 3.84 10.74 9 12 5.16-1.26 9-6.45 9-12V5l-9-4zm-1 6h2v2h-2V7zm0 4h2v6h-2v-6z", href: "/audit" },
  { key: "explorer", label: "Explorer", icon: "M9.4 16.6L4.8 12l4.6-4.6L8 6l-6 6 6 6 1.4-1.4zm5.2 0l4.6-4.6-4.6-4.6L16 6l6 6-6 6-1.4-1.4z", href: "/explorer" },
  { key: "docs", label: "Docs", icon: "M14 2H6c-1.1 0-1.99.9-1.99 2L4 20c0 1.1.89 2 1.99 2H18c1.1 0 2-.9 2-2V8l-6-6zm2 16H8v-2h8v2zm0-4H8v-2h8v2zm-3-5V3.5L18.5 9H13z", href: "/docs" },
  { key: "settings", label: "Settings", icon: "M19.14 12.94c.04-.3.06-.61.06-.94 0-.32-.02-.64-.07-.94l2.03-1.58a.49.49 0 0 0 .12-.61l-1.92-3.32a.49.49 0 0 0-.59-.22l-2.39.96c-.5-.38-1.03-.7-1.62-.94l-.36-2.54a.484.484 0 0 0-.48-.41h-3.84c-.24 0-.43.17-.47.41l-.36 2.54c-.59.24-1.13.57-1.62.94l-2.39-.96c-.22-.08-.47 0-.59.22L2.74 8.87c-.12.21-.08.47.12.61l2.03 1.58c-.05.3-.07.62-.07.94s.02.64.07.94l-2.03 1.58a.49.49 0 0 0-.12.61l1.92 3.32c.12.22.37.29.59.22l2.39-.96c.5.38 1.03.7 1.62.94l.36 2.54c.05.24.24.41.48.41h3.84c.24 0 .44-.17.47-.41l.36-2.54c.59-.24 1.13-.56 1.62-.94l2.39.96c.22.08.47 0 .59-.22l1.92-3.32c.12-.22.07-.47-.12-.61l-2.01-1.58zM12 15.6c-1.98 0-3.6-1.62-3.6-3.6s1.62-3.6 3.6-3.6 3.6 1.62 3.6 3.6-1.62 3.6-3.6 3.6z", href: "/settings" },
];

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
