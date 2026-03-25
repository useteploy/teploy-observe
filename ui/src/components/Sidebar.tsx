import { useState, useEffect } from "preact/hooks";

const NAV_ITEMS = [
  { key: "analytics", label: "Analytics", icon: "M3 13h4v8H3v-8zm7-4h4v12h-4V9zm7-6h4v18h-4V3z", href: "/" },
  { key: "errors", label: "Errors", icon: "M12 2C6.48 2 2 6.48 2 12s4.48 10 10 10 10-4.48 10-10S17.52 2 12 2zm-2 15l-5-5 1.41-1.41L10 14.17l7.59-7.59L19 8l-9 9z", href: "/errors" },
  { key: "traces", label: "Traces", icon: "M3 4h18v2H3V4zm0 7h18v2H3v-2zm0 7h18v2H3v-2z", href: "/traces" },
  { key: "logs", label: "Logs", icon: "M3 3h18v18H3V3zm2 2v14h14V5H5zm2 2h10v2H7V7zm0 4h10v2H7v-2zm0 4h7v2H7v-2z", href: "/logs" },
  { key: "flags", label: "Flags", icon: "M14.4 6L14 4H5v17h2v-7h5.6l.4 2h7V6h-5.6z", href: "/flags" },
  { key: "experiments", label: "Experiments", icon: "M7 2v2h1v14c0 1.1.9 2 2 2h4c1.1 0 2-.9 2-2V4h1V2H7zm4 14h-1V4h1v12zm2 0h-1V4h1v12z", href: "/experiments" },
  { key: "monitoring", label: "Monitoring", icon: "M12 2C6.48 2 2 6.48 2 12s4.48 10 10 10 10-4.48 10-10S17.52 2 12 2zm0 18c-4.42 0-8-3.58-8-8s3.58-8 8-8 8 3.58 8 8-3.58 8-8 8zm.5-13H11v6l5.25 3.15.75-1.23-4.5-2.67V7z", href: "/monitoring" },
  { key: "alerts", label: "Alerts", icon: "M12 22c1.1 0 2-.9 2-2h-4c0 1.1.89 2 2 2zm6-6v-5c0-3.07-1.64-5.64-4.5-6.32V4c0-.83-.67-1.5-1.5-1.5s-1.5.67-1.5 1.5v.68C7.63 5.36 6 7.92 6 11v5l-2 2v1h16v-1l-2-2z", href: "/alerts" },
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
  const [active, setActive] = useState("analytics");
  const [collapsed, setCollapsed] = useState(false);

  useEffect(() => {
    const path = window.location.pathname;
    const match = NAV_ITEMS.find(item =>
      item.href === "/" ? path === "/" : path.startsWith(item.href)
    );
    if (match) setActive(match.key);
  }, []);

  return (
    <nav class={`obs-sidebar ${collapsed ? "obs-sidebar--collapsed" : ""}`}>
      <div class="obs-sidebar-header">
        {!collapsed && <span class="obs-sidebar-logo">Observe</span>}
        <button class="obs-sidebar-toggle" onClick={() => setCollapsed(!collapsed)}
          title={collapsed ? "Expand" : "Collapse"}>
          <svg width="16" height="16" viewBox="0 0 24 24" fill="currentColor">
            <path d={collapsed ? "M10 6L8.59 7.41 13.17 12l-4.58 4.59L10 18l6-6z" : "M15.41 7.41L14 6l-6 6 6 6 1.41-1.41L10.83 12z"} />
          </svg>
        </button>
      </div>

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
              {!collapsed && <span>{item.label}</span>}
            </a>
          </li>
        ))}
      </ul>
    </nav>
  );
}
