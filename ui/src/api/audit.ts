// Compliance/audit log API — admin-only read of the who-did-what trail.

import { get } from "./helpers.js";

const BASE = "/api/v1";

export interface AuditEvent {
  id: string;
  tenant_id: string;
  site_id: string;
  timestamp: number; // unix millis
  actor: string;
  actor_type: string;
  action: string;
  target: string;
  result: string;
  source_ip: string;
  user_agent: string;
  metadata: string;
}

export interface AuditFilter {
  site_id?: string;
  actor?: string;
  action?: string;
  result?: string;
  from?: number;
  to?: number;
  limit?: number;
}

export const auditApi = {
  list(f: AuditFilter = {}): Promise<AuditEvent[]> {
    const p = new URLSearchParams();
    if (f.site_id) p.set("site_id", f.site_id);
    if (f.actor) p.set("actor", f.actor);
    if (f.action) p.set("action", f.action);
    if (f.result) p.set("result", f.result);
    if (f.from) p.set("from", String(f.from));
    if (f.to) p.set("to", String(f.to));
    if (f.limit) p.set("limit", String(f.limit));
    const q = p.toString();
    return get(`${BASE}/audit${q ? "?" + q : ""}`);
  },
};
