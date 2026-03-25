// Authentication API.

export interface LoginResponse {
  token: string;
}

export const authApi = {
  login: async (username: string, password: string): Promise<LoginResponse> => {
    const res = await fetch("/api/v1/auth/login", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ username, password }),
    });
    if (!res.ok) throw new Error("Invalid credentials");
    return res.json();
  },

  logout: () => {
    localStorage.removeItem("obs_token");
    window.location.href = "/login";
  },

  isAuthenticated: () => !!localStorage.getItem("obs_token"),
};
