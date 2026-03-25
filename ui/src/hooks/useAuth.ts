import { useState, useEffect } from "preact/hooks";
import { authApi } from "../api/auth.js";

export function useAuth() {
  const [authenticated, setAuthenticated] = useState(authApi.isAuthenticated());

  useEffect(() => {
    // Redirect to login if not authenticated (except on login page)
    if (!authenticated && typeof window !== "undefined" && window.location.pathname !== "/login") {
      window.location.href = "/login";
    }
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
