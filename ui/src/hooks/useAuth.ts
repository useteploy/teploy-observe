import { useState, useEffect } from "preact/hooks";
import { authApi } from "../api/auth.js";

export function useAuth() {
  const [authenticated, setAuthenticated] = useState(authApi.isAuthenticated());

  useEffect(() => {
    if (authenticated || typeof window === "undefined") return;
    const path = window.location.pathname;
    if (path === "/login" || path === "/setup") return;
    authApi.checkSetup()
      .then(({ needs_setup }) => {
        window.location.href = needs_setup ? "/setup" : "/login";
      })
      .catch(() => {
        window.location.href = "/login";
      });
  }, [authenticated]);

  const login = async (username: string, password: string) => {
    const res = await authApi.login(username, password);
    localStorage.setItem("obs_token", res.token);
    setAuthenticated(true);
    window.location.href = "/";
  };

  const logout = () => {
    authApi.logout();
    setAuthenticated(false);
  };

  return { authenticated, login, logout };
}
