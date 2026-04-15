// Shared API helpers used by all feature modules.

const enc = encodeURIComponent;

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
  return q;
}

export async function get<T>(path: string): Promise<T> {
  const token = localStorage.getItem("obs_token");
  const headers: Record<string, string> = {};
  if (token) headers["Authorization"] = `Bearer ${token}`;
  const res = await fetch(path, { headers });
  if (res.status === 401) {
    localStorage.removeItem("obs_token");
    window.location.href = "/login";
    throw new Error("Unauthorized");
  }
  if (!res.ok) throw new Error(`API ${res.status}: ${res.statusText}`);
  return res.json();
}

export async function post<T>(path: string, body: unknown): Promise<T> {
  const token = localStorage.getItem("obs_token");
  const headers: Record<string, string> = { "Content-Type": "application/json" };
  if (token) headers["Authorization"] = `Bearer ${token}`;
  const res = await fetch(path, { method: "POST", headers, body: JSON.stringify(body) });
  if (res.status === 401) {
    localStorage.removeItem("obs_token");
    window.location.href = "/login";
    throw new Error("Unauthorized");
  }
  if (!res.ok) throw new Error(`API ${res.status}: ${res.statusText}`);
  return res.json();
}

export async function del<T = { ok: boolean }>(path: string): Promise<T> {
  const token = localStorage.getItem("obs_token");
  const headers: Record<string, string> = {};
  if (token) headers["Authorization"] = `Bearer ${token}`;
  const res = await fetch(path, { method: "DELETE", headers });
  if (res.status === 401) {
    localStorage.removeItem("obs_token");
    window.location.href = "/login";
    throw new Error("Unauthorized");
  }
  if (!res.ok) throw new Error(`API ${res.status}: ${res.statusText}`);
  return res.json();
}
