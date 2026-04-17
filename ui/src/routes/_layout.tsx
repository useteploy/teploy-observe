import { useState, useEffect } from "preact/hooks";
import Sidebar from "../components/Sidebar.js";
import { ToastProvider } from "../components/shared/Toast.js";
import CommandPalette from "../components/CommandPalette.js";
import "../styles/dashboard.css";
import "../styles/sidebar.css";
import "../styles/shared.css";
import "../styles/cmdk.css";

function DemoBanner() {
  const [demo, setDemo] = useState(false);
  useEffect(() => {
    fetch("/api/v1/config")
      .then((r) => r.json())
      .then((r) => setDemo(!!r?.demo_mode))
      .catch(() => {});
  }, []);
  if (!demo) return null;
  return (
    <div class="obs-demo-banner" role="status">
      <span>
        <strong>Public demo</strong> — writes are disabled; data may reset.
      </span>
      <a href="https://github.com/teploy/observe" target="_blank" rel="noopener noreferrer">
        Deploy your own →
      </a>
    </div>
  );
}

export default function Layout({ children }: { children: preact.ComponentChildren }) {
  const path = typeof window !== "undefined" ? window.location.pathname : "";
  const isBareLayout = path === "/login" || path === "/onboard";

  if (isBareLayout) {
    return (
      <ToastProvider>
        <div style="font-family:var(--obs-font);background:var(--obs-bg);color:var(--obs-text);min-height:100vh;margin:0;">
          {children}
        </div>
      </ToastProvider>
    );
  }

  return (
    <ToastProvider>
      <div style="font-family:var(--obs-font);background:var(--obs-bg);color:var(--obs-text);min-height:100vh;margin:0;">
        <DemoBanner />
        <div class="obs-accent-line" />
        <Sidebar />
        <div class="obs-main-content">
          <div style="max-width:1280px;margin:0 auto;padding:24px 24px 48px;">
            {children}
          </div>
        </div>
        <CommandPalette />
      </div>
    </ToastProvider>
  );
}
