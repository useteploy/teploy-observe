import { useState } from "preact/hooks";
import { authApi } from "../api/auth.js";

export const config = { mode: "app" };

export default function SetupPage() {
  const [username, setUsername] = useState("admin");
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);

  const handleSubmit = async (e: Event) => {
    e.preventDefault();
    setError("");
    if (password !== confirm) {
      setError("Passwords do not match");
      return;
    }
    if (password.length < 8) {
      setError("Password must be at least 8 characters");
      return;
    }
    setLoading(true);
    try {
      const res = await authApi.createAdmin(username, password);
      localStorage.setItem("obs_token", res.token);
      window.location.href = "/";
    } catch (err: any) {
      setError(err.message || "Setup failed");
      setLoading(false);
    }
  };

  return (
    <div class="obs-login-page">
      <form class="obs-login-form" onSubmit={handleSubmit}>
        {/* Wordmark only — see the note on the sign-in page. */}
        <h1 class="obs-login-title">Set up Teploy Observe</h1>
        <p class="obs-login-subtitle">Create your admin account to get started</p>

        {error && <div class="obs-login-error">{error}</div>}

        <label class="obs-login-label">
          Username
          <input
            type="text"
            class="obs-login-input"
            value={username}
            onInput={(e) => setUsername((e.target as HTMLInputElement).value)}
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
            autoFocus
            required
          />
        </label>

        <label class="obs-login-label">
          Confirm Password
          <input
            type="password"
            class="obs-login-input"
            value={confirm}
            onInput={(e) => setConfirm((e.target as HTMLInputElement).value)}
            required
          />
        </label>

        <button type="submit" class="obs-login-button" disabled={loading}>
          {loading ? (
            <span style={{ display: "flex", alignItems: "center", justifyContent: "center", gap: "8px" }}>
              <svg width="16" height="16" viewBox="0 0 24 24" fill="currentColor" style={{ animation: "spin 1s linear infinite" }}>
                <path d="M12 4V2A10 10 0 0 0 2 12h2a8 8 0 0 1 8-8z" />
              </svg>
              Creating account...
            </span>
          ) : "Create account"}
        </button>
      </form>

      <style>{`
        @keyframes spin { to { transform: rotate(360deg); } }
      `}</style>
    </div>
  );
}
