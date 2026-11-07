import { useState, useEffect } from "preact/hooks";
import { authApi } from "../api/auth.js";
import { loginReturnPath } from "../lib/serviceTokenLink.js";

export function useAuth() {
  const [authenticated, setAuthenticated] = useState(authApi.isAuthenticated());

  useEffect(() => {
    if (authenticated || typeof window === "undefined") return;
    const path = window.location.pathname;
    if (path === "/login" || path === "/setup") return;
    // Keep the page they asked for, so a link survives signing in first.
    const here = path + window.location.search;
    const loginUrl = path === "/" ? "/login" : `/login?next=${encodeURIComponent(here)}`;
    authApi.checkSetup()
      .then(({ needs_setup }) => {
        window.location.href = needs_setup ? "/setup" : loginUrl;
      })
      .catch(() => {
        window.location.href = loginUrl;
      });
  }, [authenticated]);

  const login = async (username: string, password: string) => {
    const res = await authApi.login(username, password);
    localStorage.setItem("obs_token", res.token);
    setAuthenticated(true);
    window.location.href = loginReturnPath(window.location.search);
  };

  const logout = () => {
    authApi.logout();
    setAuthenticated(false);
  };

  return { authenticated, login, logout };
}
