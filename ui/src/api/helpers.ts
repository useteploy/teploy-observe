import { withProgress } from "../lib/progress.js";

// Shared API helpers used by all feature modules.

const enc = encodeURIComponent;

// Public "share link" mode: shareViewHandler injects these meta tags. In shared
// mode we have no JWT, so reads carry the share token instead and a 401 must NOT
// bounce the anonymous viewer to /login.
function metaContent(name: string): string {
  if (typeof document === "undefined") return "";
  const el = document.querySelector(`meta[name="${name}"]`);
  return el?.getAttribute("content") || "";
}

const shareToken = (): string => metaContent("observe-share-token");
export const isShared = (): boolean => metaContent("observe-shared") === "true";

// withShare appends the share token to a request path in shared mode so the
// server's jwt-or-share middleware authorizes the read and pins the site_id.
function withShare(path: string): string {
  const tok = shareToken();
  if (!tok) return path;
  return path + (path.includes("?") ? "&" : "?") + "share_token=" + enc(tok);
}

// activeCohortID reads ?cohort_id= from the page URL so every analytics
// query auto-includes the active cohort filter without each call site
// having to plumb it. C2 (Wave 4): the chip on /insights writes the
// param; the panels read here. Returns "" when not set.
function activeCohortID(): string {
  if (typeof window === "undefined") return "";
  return new URLSearchParams(window.location.search).get("cohort_id") || "";
}

export function qs(siteId: string, from: string, to: string, opts?: {
  limit?: number;
  compare?: string;
  filters?: Record<string, string>;
  interval?: string;
  type?: string;
}): string {
  let q = `site_id=${enc(siteId)}&from=${enc(from)}&to=${enc(to)}`;
  if (opts?.limit) q += `&limit=${opts.limit}`;
  if (opts?.compare) q += `&compare=${enc(opts.compare)}`;
  if (opts?.interval) q += `&interval=${enc(opts.interval)}`;
  if (opts?.type) q += `&type=${enc(opts.type)}`;
  if (opts?.filters) {
    for (const [k, v] of Object.entries(opts.filters)) {
      if (v) q += `&${enc(k)}=${enc(v)}`;
    }
  }
  // Cohort filter — auto-include from URL unless the caller already
  // provided one in filters (filters wins on explicit override).
  if (!opts?.filters?.cohort_id) {
    const cohort = activeCohortID();
    if (cohort) q += `&cohort_id=${enc(cohort)}`;
  }
  return q;
}

export async function get<T>(path: string): Promise<T> {
  const token = localStorage.getItem("obs_token");
  const headers: Record<string, string> = {};
  if (token) headers["Authorization"] = `Bearer ${token}`;
  const res = await withProgress(() => fetch(withShare(path), { headers }));
  if (res.status === 401) {
    if (!isShared()) {
      localStorage.removeItem("obs_token");
      window.location.href = "/login";
    }
    throw new Error("Unauthorized");
  }
  if (!res.ok) throw new Error(`API ${res.status}: ${res.statusText}`);
  return res.json();
}

export async function post<T>(path: string, body: unknown): Promise<T> {
  const token = localStorage.getItem("obs_token");
  const headers: Record<string, string> = { "Content-Type": "application/json" };
  if (token) headers["Authorization"] = `Bearer ${token}`;
  const res = await withProgress(() => fetch(path, { method: "POST", headers, body: JSON.stringify(body) }));
  if (res.status === 401) {
    if (!isShared()) {
      localStorage.removeItem("obs_token");
      window.location.href = "/login";
    }
    throw new Error("Unauthorized");
  }
  if (!res.ok) throw new Error(`API ${res.status}: ${res.statusText}`);
  return res.json();
}

export async function put<T>(path: string, body: unknown): Promise<T> {
  const token = localStorage.getItem("obs_token");
  const headers: Record<string, string> = { "Content-Type": "application/json" };
  if (token) headers["Authorization"] = `Bearer ${token}`;
  const res = await withProgress(() => fetch(path, { method: "PUT", headers, body: JSON.stringify(body) }));
  if (res.status === 401) {
    if (!isShared()) {
      localStorage.removeItem("obs_token");
      window.location.href = "/login";
    }
    throw new Error("Unauthorized");
  }
  if (!res.ok) throw new Error(`API ${res.status}: ${res.statusText}`);
  return res.json();
}

export async function del<T = { ok: boolean }>(path: string): Promise<T> {
  const token = localStorage.getItem("obs_token");
  const headers: Record<string, string> = {};
  if (token) headers["Authorization"] = `Bearer ${token}`;
  const res = await withProgress(() => fetch(path, { method: "DELETE", headers }));
  if (res.status === 401) {
    if (!isShared()) {
      localStorage.removeItem("obs_token");
      window.location.href = "/login";
    }
    throw new Error("Unauthorized");
  }
  if (!res.ok) throw new Error(`API ${res.status}: ${res.statusText}`);
  return res.json();
}
