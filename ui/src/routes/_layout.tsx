import Sidebar from "../components/Sidebar.js";
import "../styles/dashboard.css";
import "../styles/sidebar.css";
import "../styles/shared.css";

export default function Layout({ children }: { children: preact.ComponentChildren }) {
  // Don't show sidebar on login page
  const isLogin = typeof window !== "undefined" && window.location.pathname === "/login";

  if (isLogin) {
    return (
      <div style="font-family:var(--obs-font);background:var(--obs-bg);color:var(--obs-text);min-height:100vh;margin:0;">
        {children}
      </div>
    );
  }

  return (
    <div style="font-family:var(--obs-font);background:var(--obs-bg);color:var(--obs-text);min-height:100vh;margin:0;">
      <div class="obs-accent-line" />
      <Sidebar />
      <div class="obs-main-content">
        <div style="max-width:1280px;margin:0 auto;padding:24px 24px 48px;">
          {children}
        </div>
      </div>
    </div>
  );
}
