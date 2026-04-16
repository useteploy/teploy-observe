// Settings API — sites, API keys, webhooks, users, share links.

import { get, post, del } from "./helpers.js";

const BASE = "/api/v1";
const PLATFORM = "/api/v1/platform";

export interface Site {
  site_id: string; name: string; domain: string; created_at: string;
}

export interface APIKey {
  key_id: string; site_id: string; key_prefix: string; created_at: string;
}

export interface APIKeyInfo {
  key_id: string; site_id: string; label: string; created_at: string; revoked: boolean;
}

export interface Webhook {
  webhook_id: string; site_id: string; name: string;
  webhook_type: string; url: string; enabled: string; created_at: string;
}

export interface User {
  user_id: string; username: string; email: string; role: string; created_at: string;
}

export interface ShareLink {
  token: string; site_id: string; created_at: string;
}

export const settingsApi = {
  // Sites
  sites: () => get<Site[]>(`${BASE}/sites`),
  createSite: (data: { name: string; domain?: string }) =>
    post<Site>(`${BASE}/sites`, data),
  deleteSite: (siteId: string) =>
    del(`${BASE}/sites/${siteId}`),
  createAPIKey: (siteId: string) =>
    post<{ api_key: string }>(`${BASE}/sites/${siteId}/keys`, {}),
  listAPIKeys: (siteId: string) =>
    get<APIKeyInfo[]>(`${BASE}/sites/${siteId}/keys`),
  revokeAPIKey: (keyId: string) =>
    del(`${BASE}/keys/${keyId}`),

  // Share links
  shareLinks: (siteId: string) =>
    get<ShareLink[]>(`${BASE}/sites/${siteId}/share`),
  createShareLink: (siteId: string) =>
    post<ShareLink>(`${BASE}/sites/${siteId}/share`, {}),
  revokeShareLink: (token: string) =>
    del(`${BASE}/share/${token}`),

  // Webhooks
  webhooks: (siteId: string) =>
    get<Webhook[]>(`${PLATFORM}/webhooks?site_id=${siteId}`),
  createWebhook: (data: { site_id: string; name: string; webhook_type: string; url: string }) =>
    post<Webhook>(`${PLATFORM}/webhooks`, data),
  deleteWebhook: (webhookId: string) =>
    del(`${PLATFORM}/webhooks/${webhookId}`),

  // Users
  users: () => get<User[]>(`${PLATFORM}/users`),
  createUser: (data: { username: string; password: string; role?: string }) =>
    post<User>(`${PLATFORM}/users`, data),
  updateRole: (userId: string, role: string) =>
    post<{ ok: boolean }>(`${PLATFORM}/users/${userId}/role`, { role }),
};
