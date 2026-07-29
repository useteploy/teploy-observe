import { useState, useEffect } from "preact/hooks";
import { useAuth } from "../hooks/useAuth.js";

export const config = { mode: "app" };

export default function LoginPage() {
  const { login } = useAuth();
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);
  const [sso, setSso] = useState<{ label: string } | null>(null);

  useEffect(() => {
    // Surface an SSO failure bounced back via ?error=.
    if (typeof window !== "undefined") {
      const e = new URLSearchParams(window.location.search).get("error");
      if (e) setError(e);
    }
    // Ask the server whether SSO is configured, to show the button.
    fetch("/api/v1/auth/methods")
      .then((r) => (r.ok ? r.json() : null))
      .then((m) => {
        if (m && m.oidc) setSso({ label: m.oidc_label || "Single sign-on" });
      })
      .catch(() => {});
  }, []);

  const handleSubmit = async (e: Event) => {
    e.preventDefault();
    setError("");
    setLoading(true);
    try {
      await login(username, password);
    } catch {
      setError("Invalid credentials");
      setLoading(false);
    }
  };

  return (
    <div class="obs-login-page">
      <form class="obs-login-form" onSubmit={handleSubmit}>
        {/* Wordmark only, matching the other two sign-in pages ("Teploy Ship",
            "TEPLOY DASH"). The gradient letter-tile that used to sit here read
            as a consumer app icon, which this is not. */}
        <h1 class="obs-login-title">Teploy Observe</h1>
        <p class="obs-login-subtitle">Sign in to your dashboard</p>

        {error && <div class="obs-login-error">{error}</div>}

        {sso && (
          <>
            <a class="obs-sso-button" href="/api/v1/auth/oidc/login">{sso.label}</a>
            <div class="obs-login-divider">or</div>
          </>
        )}

        <label class="obs-login-label">
          Username
          <input
            type="text"
            class="obs-login-input"
            value={username}
            onInput={(e) => setUsername((e.target as HTMLInputElement).value)}
            autoFocus
            required
          />
        </label>

        <label class="obs-login-label">
          Password
          <input
            type="password"
            class="obs-login-input"
            value={password}
            onInput={(e) => setPassword((e.target as HTMLInputElement).value)}
            required
          />
        </label>

        <button type="submit" class="obs-login-button" disabled={loading}>
          {loading ? (
            <span style={{ display: "flex", alignItems: "center", justifyContent: "center", gap: "8px" }}>
              <svg width="16" height="16" viewBox="0 0 24 24" fill="currentColor" style={{ animation: "spin 1s linear infinite" }}>
                <path d="M12 4V2A10 10 0 0 0 2 12h2a8 8 0 0 1 8-8z" />
              </svg>
              Signing in...
            </span>
          ) : "Sign in"}
        </button>
      </form>

      <style>{`
        @keyframes spin { to { transform: rotate(360deg); } }
        .obs-login-page {
          display: flex; align-items: center; justify-content: center;
          min-height: 100vh;
          background: var(--obs-bg);
          background-image: radial-gradient(ellipse at 50% 0%, rgba(99, 102, 241, 0.08) 0%, transparent 60%);
        }
        .obs-login-form {
          width: 100%; max-width: 360px; padding: 32px;
          background: var(--obs-surface); border: 1px solid var(--obs-border-subtle);
          border-radius: var(--obs-radius-lg);
        }
        .obs-login-title {
          font-size: 24px; font-weight: 700; margin: 0 0 4px;
          color: var(--obs-text); text-align: center;
        }
        .obs-login-subtitle {
          font-size: 13px; color: var(--obs-text-muted);
          margin: 0 0 24px; text-align: center;
        }
        .obs-login-label {
          display: block; font-size: 12px; font-weight: 500;
          color: var(--obs-text-secondary); margin-bottom: 16px;
        }
        .obs-login-input {
          display: block; width: 100%; margin-top: 6px; padding: 10px 12px;
          background: var(--obs-bg); border: 1px solid var(--obs-border);
          border-radius: var(--obs-radius); color: var(--obs-text);
          font-size: 14px; font-family: var(--obs-font);
          transition: border-color var(--obs-transition);
          outline: none;
        }
        .obs-login-input:focus { border-color: var(--obs-accent); }
        .obs-login-button {
          display: block; width: 100%; padding: 10px; margin-top: 8px;
          background: var(--obs-accent); color: white; border: none;
          border-radius: var(--obs-radius); font-size: 14px; font-weight: 600;
          font-family: var(--obs-font); cursor: pointer;
          transition: background var(--obs-transition);
        }
        .obs-login-button:hover { background: var(--obs-accent-hover); }
        .obs-login-button:disabled { opacity: 0.6; cursor: not-allowed; }
        .obs-sso-button {
          display: block; width: 100%; padding: 10px; margin-bottom: 8px;
          background: transparent; color: var(--obs-text);
          border: 1px solid var(--obs-border); border-radius: var(--obs-radius);
          font-size: 14px; font-weight: 600; font-family: var(--obs-font);
          text-align: center; text-decoration: none; cursor: pointer;
          transition: border-color var(--obs-transition);
        }
        .obs-sso-button:hover { border-color: var(--obs-accent); }
        .obs-login-divider {
          display: flex; align-items: center; gap: 10px;
          margin: 4px 0 16px; color: var(--obs-text-muted); font-size: 12px;
        }
        .obs-login-divider::before, .obs-login-divider::after {
          content: ""; flex: 1; height: 1px; background: var(--obs-border-subtle);
        }
        .obs-login-error {
          background: var(--obs-danger-bg); color: var(--obs-danger);
          padding: 8px 12px; border-radius: var(--obs-radius);
          font-size: 13px; margin-bottom: 16px; text-align: center;
        }
      `}</style>
    </div>
  );
}
