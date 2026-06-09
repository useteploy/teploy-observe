import { useState, useEffect } from "preact/hooks";
import { settingsApi } from "../api/settings.js";
import type { Site, Webhook, User, ShareLink, APIKeyInfo } from "../api/settings.js";
import StatusBadge from "../components/shared/StatusBadge.js";
import Modal from "../components/shared/Modal.js";
import ConfirmDialog from "../components/shared/ConfirmDialog.js";
import "../styles/settings.css";
import { useFilters } from "../hooks/useFilters.js";

export const config = { mode: "app" };

function formatDate(iso: string): string {
  if (!iso) return "--";
  try {
    return new Date(iso).toLocaleDateString("en-US", {
      month: "short", day: "numeric", year: "numeric",
    });
  } catch { return iso; }
}

function SettingsSkeleton() {
  return (
    <div class="settings-loading">
      {Array.from({ length: 3 }).map((_, i) => (
        <div class="settings-skeleton-row" key={i}>
          <div class="settings-skeleton-bar" style={{ width: "120px" }} />
          <div class="settings-skeleton-bar" style={{ flex: 1 }} />
          <div class="settings-skeleton-bar" style={{ width: "80px" }} />
        </div>
      ))}
    </div>
  );
}

// ─── Sites ───

function SitesSection() {
  const [sites, setSites] = useState<Site[]>([]);
  const [loading, setLoading] = useState(true);
  const [showCreate, setShowCreate] = useState(false);
  const [creating, setCreating] = useState(false);
  const [newKey, setNewKey] = useState<{ key: string; siteId: string } | null>(null);
  const [formName, setFormName] = useState("");
  const [formDomain, setFormDomain] = useState("");
  const [deletingSiteId, setDeletingSiteId] = useState<string | null>(null);
  const [deleteLoading, setDeleteLoading] = useState(false);

  // Share links
  const [shareLinks, setShareLinks] = useState<ShareLink[]>([]);
  const [selectedSiteId, setSelectedSiteId] = useState<string | null>(null);

  const refresh = async () => {
    const data = await settingsApi.sites();
    setSites(data || []);
  };

  useEffect(() => {
    setLoading(true);
    refresh().finally(() => setLoading(false));
  }, []);

  const handleCreate = async () => {
    if (!formName.trim()) return;
    setCreating(true);
    try {
      await settingsApi.createSite({ name: formName.trim(), domain: formDomain.trim() || undefined });
      setShowCreate(false);
      setFormName(""); setFormDomain("");
      await refresh();
    } catch (err) { console.error("Failed to create site:", err); }
    finally { setCreating(false); }
  };

  const handleDeleteConfirm = async () => {
    if (!deletingSiteId) return;
    setDeleteLoading(true);
    try {
      await settingsApi.deleteSite(deletingSiteId);
      setSites(prev => prev.filter(s => s.site_id !== deletingSiteId));
      setDeletingSiteId(null);
    } catch (err) { console.error("Failed to delete site:", err); }
    finally { setDeleteLoading(false); }
  };

  const handleGenerateKey = async (siteId: string) => {
    try {
      const result = await settingsApi.createAPIKey(siteId);
      setNewKey({ key: result.api_key, siteId });
    } catch (err) { console.error("Failed to generate API key:", err); }
  };

  const handleShowShareLinks = async (siteId: string) => {
    if (selectedSiteId === siteId) { setSelectedSiteId(null); return; }
    setSelectedSiteId(siteId);
    try {
      const data = await settingsApi.shareLinks(siteId);
      setShareLinks(data || []);
    } catch { setShareLinks([]); }
  };

  const handleCreateShareLink = async (siteId: string) => {
    try {
      await settingsApi.createShareLink(siteId);
      const data = await settingsApi.shareLinks(siteId);
      setShareLinks(data || []);
    } catch (err) { console.error("Failed to create share link:", err); }
  };

  const handleRevokeShareLink = async (token: string) => {
    try {
      await settingsApi.revokeShareLink(token);
      setShareLinks(prev => prev.filter(l => l.token !== token));
    } catch (err) { console.error("Failed to revoke share link:", err); }
  };

  return (
    <div class="settings-section">
      <div class="settings-section-header">
        <h2 class="settings-section-title">Sites</h2>
        <button class="obs-btn obs-btn--primary obs-btn--sm" onClick={() => setShowCreate(true)}>Add Site</button>
      </div>

      {loading ? <SettingsSkeleton /> : sites.length === 0 ? (
        <div class="obs-empty-state">No sites configured</div>
      ) : (
        <div class="settings-list">
          {sites.map(s => (
            <div key={s.site_id}>
              <div class="settings-row">
                <span class="settings-row-name">{s.name}</span>
                <span class="settings-row-value">{s.domain || s.site_id}</span>
                <button class="obs-btn obs-btn--sm" onClick={() => handleGenerateKey(s.site_id)}>API Key</button>
                <button class="obs-btn obs-btn--sm" onClick={() => handleShowShareLinks(s.site_id)}>Share</button>
                <button class="obs-btn obs-btn--sm obs-btn--danger" onClick={() => setDeletingSiteId(s.site_id)}>Delete</button>
                <span class="settings-row-date">{formatDate(s.created_at)}</span>
              </div>
              {selectedSiteId === s.site_id && (
                <div style={{ padding: "8px 16px 16px", borderBottom: "1px solid var(--obs-border-subtle)" }}>
                  <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", marginBottom: "8px" }}>
                    <span style={{ fontSize: "12px", fontWeight: 600, color: "var(--obs-text-secondary)" }}>
                      Share Links ({shareLinks.length})
                    </span>
                    <button class="obs-btn obs-btn--sm" onClick={() => handleCreateShareLink(s.site_id)}>Create Link</button>
                  </div>
                  {shareLinks.length === 0 ? (
                    <div style={{ fontSize: "12px", color: "var(--obs-text-muted)" }}>No share links</div>
                  ) : (
                    shareLinks.map(l => (
                      <div key={l.token} style={{ display: "flex", alignItems: "center", gap: "8px", padding: "4px 0", fontSize: "12px" }}>
                        <code style={{ flex: 1, color: "var(--obs-text)", fontSize: "11px" }}>/share/{l.token}</code>
                        <span class="settings-row-date">{formatDate(l.created_at)}</span>
                        <button class="obs-btn obs-btn--sm obs-btn--danger" onClick={() => handleRevokeShareLink(l.token)}>Revoke</button>
                      </div>
                    ))
                  )}
                </div>
              )}
            </div>
          ))}
        </div>
      )}

      {newKey && (
        <>
          <div class="settings-key-display">
            <span class="settings-key-value">{newKey.key}</span>
            <button class="obs-btn obs-btn--sm" onClick={() => navigator.clipboard.writeText(newKey.key)}>Copy</button>
          </div>
          <div class="settings-key-note">Save this key now. It will not be shown again.</div>
        </>
      )}

      <Modal open={showCreate} onClose={() => setShowCreate(false)} title="Add Site">
        <div class="obs-form-group">
          <label class="obs-label">Name</label>
          <input class="obs-input" placeholder="My App" value={formName}
            onInput={(e) => setFormName((e.target as HTMLInputElement).value)} />
        </div>
        <div class="obs-form-group">
          <label class="obs-label">Domain (optional)</label>
          <input class="obs-input" placeholder="example.com" value={formDomain}
            onInput={(e) => setFormDomain((e.target as HTMLInputElement).value)} />
        </div>
        <div style={{ display: "flex", justifyContent: "flex-end", gap: "8px", marginTop: "8px" }}>
          <button class="obs-btn" onClick={() => setShowCreate(false)}>Cancel</button>
          <button class="obs-btn obs-btn--primary" onClick={handleCreate} disabled={creating || !formName.trim()}>
            {creating ? "Creating..." : "Create"}
          </button>
        </div>
      </Modal>

      <ConfirmDialog
        open={!!deletingSiteId}
        onClose={() => setDeletingSiteId(null)}
        onConfirm={handleDeleteConfirm}
        title="Delete Site"
        message="This will permanently delete the site and all its data. This cannot be undone."
        loading={deleteLoading}
      />
    </div>
  );
}

// ─── Webhooks ───

function WebhooksSection() {
  const { state: { siteId } } = useFilters();

  const [webhooks, setWebhooks] = useState<Webhook[]>([]);
  const [loading, setLoading] = useState(true);
  const [showCreate, setShowCreate] = useState(false);
  const [creating, setCreating] = useState(false);
  const [formName, setFormName] = useState("");
  const [formType, setFormType] = useState("http");
  const [formUrl, setFormUrl] = useState("");
  const [deletingWebhookId, setDeletingWebhookId] = useState<string | null>(null);
  const [deleteLoading, setDeleteLoading] = useState(false);

  const refresh = async () => {
    const data = await settingsApi.webhooks(siteId);
    setWebhooks(data || []);
  };

  useEffect(() => {
    setLoading(true);
    refresh().finally(() => setLoading(false));
  }, [siteId]);

  const handleCreate = async () => {
    if (!formName.trim() || !formUrl.trim()) return;
    setCreating(true);
    try {
      await settingsApi.createWebhook({ site_id: siteId, name: formName.trim(), webhook_type: formType, url: formUrl.trim() });
      setShowCreate(false);
      setFormName(""); setFormUrl(""); setFormType("http");
      await refresh();
    } catch (err) { console.error("Failed to create webhook:", err); }
    finally { setCreating(false); }
  };

  const handleDeleteConfirm = async () => {
    if (!deletingWebhookId) return;
    setDeleteLoading(true);
    try {
      await settingsApi.deleteWebhook(deletingWebhookId);
      setWebhooks(prev => prev.filter(w => w.webhook_id !== deletingWebhookId));
      setDeletingWebhookId(null);
    } catch (err) { console.error("Failed to delete webhook:", err); }
    finally { setDeleteLoading(false); }
  };

  return (
    <div class="settings-section">
      <div class="settings-section-header">
        <h2 class="settings-section-title">Webhooks</h2>
        <button class="obs-btn obs-btn--primary obs-btn--sm" onClick={() => setShowCreate(true)}>Add Webhook</button>
      </div>

      {loading ? <SettingsSkeleton /> : webhooks.length === 0 ? (
        <div class="obs-empty-state">No webhooks configured</div>
      ) : (
        <div class="settings-list">
          {webhooks.map(w => (
            <div key={w.webhook_id} class="settings-row">
              <StatusBadge status={w.enabled ? "enabled" : "disabled"} size="sm" />
              <span class="settings-row-name">{w.name}</span>
              <span class="settings-row-value">{w.url}</span>
              <span style={{ fontSize: "11px", color: "var(--obs-text-muted)" }}>{w.webhook_type}</span>
              <button class="obs-btn obs-btn--sm obs-btn--danger" onClick={() => setDeletingWebhookId(w.webhook_id)}>Delete</button>
            </div>
          ))}
        </div>
      )}

      <Modal open={showCreate} onClose={() => setShowCreate(false)} title="Add Webhook">
        <div class="obs-form-group">
          <label class="obs-label">Name</label>
          <input class="obs-input" placeholder="Slack Alerts" value={formName}
            onInput={(e) => setFormName((e.target as HTMLInputElement).value)} />
        </div>
        <div class="obs-form-group">
          <label class="obs-label">Type</label>
          <select class="obs-select" value={formType}
            onChange={(e) => setFormType((e.target as HTMLSelectElement).value)}>
            <option value="http">HTTP</option>
            <option value="slack">Slack</option>
          </select>
        </div>
        <div class="obs-form-group">
          <label class="obs-label">URL</label>
          <input class="obs-input" placeholder="https://hooks.slack.com/..." value={formUrl}
            onInput={(e) => setFormUrl((e.target as HTMLInputElement).value)} />
        </div>
        <div style={{ display: "flex", justifyContent: "flex-end", gap: "8px", marginTop: "8px" }}>
          <button class="obs-btn" onClick={() => setShowCreate(false)}>Cancel</button>
          <button class="obs-btn obs-btn--primary" onClick={handleCreate}
            disabled={creating || !formName.trim() || !formUrl.trim()}>
            {creating ? "Creating..." : "Create"}
          </button>
        </div>
      </Modal>

      <ConfirmDialog
        open={!!deletingWebhookId}
        onClose={() => setDeletingWebhookId(null)}
        onConfirm={handleDeleteConfirm}
        title="Delete Webhook"
        message="This webhook will stop receiving notifications. This cannot be undone."
        loading={deleteLoading}
      />
    </div>
  );
}

// ─── Users ───

function UsersSection() {
  const [users, setUsers] = useState<User[]>([]);
  const [loading, setLoading] = useState(true);
  const [showCreate, setShowCreate] = useState(false);
  const [creating, setCreating] = useState(false);
  const [formUsername, setFormUsername] = useState("");
  const [formPassword, setFormPassword] = useState("");
  const [formRole, setFormRole] = useState("viewer");

  const refresh = async () => {
    const data = await settingsApi.users();
    setUsers(data || []);
  };

  useEffect(() => {
    setLoading(true);
    refresh().finally(() => setLoading(false));
  }, []);

  const handleCreate = async () => {
    if (!formUsername.trim() || !formPassword.trim()) return;
    setCreating(true);
    try {
      await settingsApi.createUser({ username: formUsername.trim(), password: formPassword.trim(), role: formRole });
      setShowCreate(false);
      setFormUsername(""); setFormPassword(""); setFormRole("viewer");
      await refresh();
    } catch (err) { console.error("Failed to create user:", err); }
    finally { setCreating(false); }
  };

  const handleRoleChange = async (userId: string, newRole: string) => {
    try {
      await settingsApi.updateRole(userId, newRole);
      setUsers(prev => prev.map(u => u.user_id === userId ? { ...u, role: newRole } : u));
    } catch (err) { console.error("Failed to update role:", err); }
  };

  return (
    <div class="settings-section">
      <div class="settings-section-header">
        <h2 class="settings-section-title">Users</h2>
        <button class="obs-btn obs-btn--primary obs-btn--sm" onClick={() => setShowCreate(true)}>Add User</button>
      </div>

      {loading ? <SettingsSkeleton /> : users.length === 0 ? (
        <div class="obs-empty-state">No users found</div>
      ) : (
        <div class="settings-list">
          {users.map(u => (
            <div key={u.user_id} class="settings-row">
              <span class="settings-row-name">{u.username}</span>
              {u.email && <span class="settings-row-value">{u.email}</span>}
              <select class="obs-select" value={u.role} style={{ width: "100px", padding: "4px 8px", fontSize: "12px" }}
                onChange={(e) => handleRoleChange(u.user_id, (e.target as HTMLSelectElement).value)}>
                <option value="viewer">Viewer</option>
                <option value="editor">Editor</option>
                <option value="admin">Admin</option>
              </select>
              <span class="settings-row-date">{formatDate(u.created_at)}</span>
            </div>
          ))}
        </div>
      )}

      <Modal open={showCreate} onClose={() => setShowCreate(false)} title="Add User">
        <div class="obs-form-group">
          <label class="obs-label">Username</label>
          <input class="obs-input" placeholder="jane" value={formUsername}
            onInput={(e) => setFormUsername((e.target as HTMLInputElement).value)} />
        </div>
        <div class="obs-form-group">
          <label class="obs-label">Password</label>
          <input class="obs-input" type="password" placeholder="Password" value={formPassword}
            onInput={(e) => setFormPassword((e.target as HTMLInputElement).value)} />
        </div>
        <div class="obs-form-group">
          <label class="obs-label">Role</label>
          <select class="obs-select" value={formRole}
            onChange={(e) => setFormRole((e.target as HTMLSelectElement).value)}>
            <option value="viewer">Viewer</option>
            <option value="editor">Editor</option>
            <option value="admin">Admin</option>
          </select>
        </div>
        <div style={{ display: "flex", justifyContent: "flex-end", gap: "8px", marginTop: "8px" }}>
          <button class="obs-btn" onClick={() => setShowCreate(false)}>Cancel</button>
          <button class="obs-btn obs-btn--primary" onClick={handleCreate}
            disabled={creating || !formUsername.trim() || !formPassword.trim()}>
            {creating ? "Creating..." : "Create"}
          </button>
        </div>
      </Modal>
    </div>
  );
}

// ─── Password ───

// ─── API Keys ───

function APIKeysSection() {
  const { state: { siteId } } = useFilters();

  const [keys, setKeys] = useState<APIKeyInfo[]>([]);
  const [loading, setLoading] = useState(true);
  const [revokingId, setRevokingId] = useState<string | null>(null);
  const [revokeLoading, setRevokeLoading] = useState(false);

  useEffect(() => {
    setLoading(true);
    settingsApi.listAPIKeys(siteId)
      .then(d => setKeys(d || []))
      .catch(() => setKeys([]))
      .finally(() => setLoading(false));
  }, [siteId]);

  const handleRevoke = async () => {
    if (!revokingId) return;
    setRevokeLoading(true);
    try {
      await settingsApi.revokeAPIKey(revokingId);
      setKeys(prev => prev.map(k => k.key_id === revokingId ? { ...k, revoked: true } : k));
      setRevokingId(null);
    } catch (err) { console.error("Failed to revoke key:", err); }
    finally { setRevokeLoading(false); }
  };

  return (
    <div class="settings-section">
      <div class="settings-section-header">
        <h2 class="settings-section-title">API Keys</h2>
      </div>

      {loading ? <SettingsSkeleton /> : keys.length === 0 ? (
        <div class="obs-empty-state">No API keys. Generate one from the Sites section above.</div>
      ) : (
        <div class="settings-list">
          {keys.map(k => (
            <div key={k.key_id} class="settings-row">
              <StatusBadge status={k.revoked ? "disabled" : "enabled"} size="sm" />
              <span class="settings-row-name">{k.label || "default"}</span>
              <span class="settings-row-value" style={{ fontFamily: "var(--obs-font-mono, 'SF Mono', monospace)", fontSize: "11px" }}>
                {k.key_id.slice(0, 8)}...
              </span>
              <span class="settings-row-date">{k.created_at}</span>
              {!k.revoked && (
                <button class="obs-btn obs-btn--sm obs-btn--danger" onClick={() => setRevokingId(k.key_id)}>
                  Revoke
                </button>
              )}
            </div>
          ))}
        </div>
      )}

      <ConfirmDialog
        open={!!revokingId}
        onClose={() => setRevokingId(null)}
        onConfirm={handleRevoke}
        title="Revoke API Key"
        message="This key will immediately stop working. Any services using it will lose access."
        confirmLabel="Revoke"
        loading={revokeLoading}
      />
    </div>
  );
}

function PasswordSection() {
  const [currentPw, setCurrentPw] = useState("");
  const [newPw, setNewPw] = useState("");
  const [confirmPw, setConfirmPw] = useState("");
  const [saving, setSaving] = useState(false);
  const [message, setMessage] = useState<{ type: "ok" | "error"; text: string } | null>(null);

  const handleChange = async () => {
    if (!currentPw || !newPw) return;
    if (newPw !== confirmPw) {
      setMessage({ type: "error", text: "New passwords do not match" });
      return;
    }
    if (newPw.length < 8) {
      setMessage({ type: "error", text: "New password must be at least 8 characters" });
      return;
    }
    setSaving(true);
    setMessage(null);
    try {
      const token = localStorage.getItem("obs_token");
      const res = await fetch("/api/v1/auth/password", {
        method: "POST",
        headers: { "Content-Type": "application/json", ...(token ? { Authorization: `Bearer ${token}` } : {}) },
        body: JSON.stringify({ current_password: currentPw, new_password: newPw }),
      });
      if (!res.ok) {
        const data = await res.json().catch(() => null);
        throw new Error(data?.error || `Error ${res.status}`);
      }
      setCurrentPw(""); setNewPw(""); setConfirmPw("");
      setMessage({ type: "ok", text: "Password changed successfully" });
    } catch (err: any) {
      setMessage({ type: "error", text: err.message || "Failed to change password" });
    } finally { setSaving(false); }
  };

  return (
    <div class="settings-section">
      <div class="settings-section-header">
        <h2 class="settings-section-title">Change Password</h2>
      </div>
      <div style={{ maxWidth: "400px" }}>
        <div class="obs-form-group">
          <label class="obs-label">Current Password</label>
          <input class="obs-input" type="password" value={currentPw}
            onInput={(e) => setCurrentPw((e.target as HTMLInputElement).value)} />
        </div>
        <div class="obs-form-group">
          <label class="obs-label">New Password</label>
          <input class="obs-input" type="password" value={newPw}
            onInput={(e) => setNewPw((e.target as HTMLInputElement).value)} />
        </div>
        <div class="obs-form-group">
          <label class="obs-label">Confirm New Password</label>
          <input class="obs-input" type="password" value={confirmPw}
            onInput={(e) => setConfirmPw((e.target as HTMLInputElement).value)} />
        </div>
        {message && (
          <div style={{ fontSize: "12px", marginBottom: "12px", color: message.type === "ok" ? "var(--obs-success)" : "var(--obs-danger)" }}>
            {message.text}
          </div>
        )}
        <button class="obs-btn obs-btn--primary" onClick={handleChange}
          disabled={saving || !currentPw || !newPw || !confirmPw}>
          {saving ? "Saving..." : "Change Password"}
        </button>
      </div>
    </div>
  );
}

// ─── Main ───

function AISection() {
  const [cfg, setCfg] = useState<{ provider: string; endpoint: string; model: string; has_key: boolean; api_key?: string } | null>(null);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [message, setMessage] = useState<string | null>(null);

  useEffect(() => {
    fetch("/api/v1/ai/config", { headers: { Authorization: "Bearer " + localStorage.getItem("observe_token") } })
      .then(r => r.json()).then(setCfg).catch(() => setCfg({ provider: "", endpoint: "", model: "", has_key: false }))
      .finally(() => setLoading(false));
  }, []);

  const save = async () => {
    if (!cfg) return;
    setSaving(true);
    setMessage(null);
    try {
      const r = await fetch("/api/v1/ai/config", {
        method: "PUT",
        headers: { "Content-Type": "application/json", Authorization: "Bearer " + localStorage.getItem("observe_token") },
        body: JSON.stringify(cfg),
      });
      const data = await r.json();
      setCfg(data);
      setMessage("Saved");
    } catch (e: any) {
      setMessage("Error: " + (e.message || "save failed"));
    } finally {
      setSaving(false);
    }
  };

  if (loading) return <SettingsSkeleton />;
  return (
    <div class="settings-section">
      <h2>AI query assistant</h2>
      <p style={{ color: "var(--obs-text-muted)", fontSize: "13px" }}>
        Provide an OpenAI-compatible endpoint and API key. Observe sends only the
        schema and your question — no stored data leaves the instance.
      </p>
      <div style={{ display: "grid", gridTemplateColumns: "120px 1fr", gap: "12px 16px", maxWidth: "640px", alignItems: "center" }}>
        <label>Provider</label>
        <input class="obs-input" value={cfg?.provider || ""} onInput={(e) => setCfg({ ...cfg!, provider: (e.target as HTMLInputElement).value })} placeholder="openai / anthropic / ollama" />
        <label>Endpoint</label>
        <input class="obs-input" value={cfg?.endpoint || ""} onInput={(e) => setCfg({ ...cfg!, endpoint: (e.target as HTMLInputElement).value })} placeholder="https://api.openai.com/v1/chat/completions" />
        <label>Model</label>
        <input class="obs-input" value={cfg?.model || ""} onInput={(e) => setCfg({ ...cfg!, model: (e.target as HTMLInputElement).value })} placeholder="gpt-4o-mini" />
        <label>API Key</label>
        <input class="obs-input" type="password"
          value={cfg?.api_key || ""}
          onInput={(e) => setCfg({ ...cfg!, api_key: (e.target as HTMLInputElement).value })}
          placeholder={cfg?.has_key ? "(stored) leave blank to keep" : "sk-..."} />
      </div>
      <div style={{ marginTop: "16px", display: "flex", gap: "12px", alignItems: "center" }}>
        <button class="obs-btn obs-btn--primary" onClick={save} disabled={saving}>
          {saving ? "Saving..." : "Save"}
        </button>
        {message && <span style={{ color: message.startsWith("Error") ? "var(--obs-danger)" : "var(--obs-text-muted)" }}>{message}</span>}
      </div>
    </div>
  );
}

interface ScheduledExport {
  export_id: string;
  name: string;
  sql: string;
  format: string;
  cron: string;
  destination_type: string;
  destination_cfg: string;
  enabled: string;
  last_run_at: number;
  last_status: string;
  last_error: string;
  last_rows: number;
}

function ExportsSection() {
  const [exports, setExports] = useState<ScheduledExport[]>([]);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState("");
  const [showCreate, setShowCreate] = useState(false);
  const [form, setForm] = useState({
    name: "", sql: "", cron: "@daily", format: "ndjson",
    endpoint: "", region: "us-east-1", bucket: "", prefix: "observe/", access_key_id: "", secret_access_key: "",
  });

  const load = async () => {
    setLoading(true);
    setLoadError("");
    try {
      const r = await fetch("/api/v1/exports/scheduled", { headers: { Authorization: "Bearer " + localStorage.getItem("observe_token") } });
      if (!r.ok) {
        const body = await r.text();
        let msg = `Server error ${r.status}`;
        try { msg = JSON.parse(body).error ?? msg; } catch { /* non-JSON */ }
        setLoadError(msg);
        return;
      }
      const data = await r.json();
      setExports(Array.isArray(data) ? data : []);
    } catch (e) {
      setLoadError(String(e));
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => { load(); }, []);

  const create = async () => {
    const body = {
      name: form.name, sql: form.sql, cron: form.cron, format: form.format,
      destination_type: "s3",
      destination: {
        endpoint: form.endpoint, region: form.region, bucket: form.bucket, prefix: form.prefix,
        access_key_id: form.access_key_id, secret_access_key: form.secret_access_key,
        force_path_style: !!form.endpoint,
      },
    };
    const r = await fetch("/api/v1/exports/scheduled", {
      method: "POST",
      headers: { "Content-Type": "application/json", Authorization: "Bearer " + localStorage.getItem("observe_token") },
      body: JSON.stringify(body),
    });
    if (r.ok) {
      setShowCreate(false);
      setForm({ ...form, name: "", sql: "" });
      load();
    } else {
      alert("Create failed: " + await r.text());
    }
  };

  const runNow = async (id: string) => {
    await fetch(`/api/v1/exports/scheduled/${id}/run`, {
      method: "POST",
      headers: { Authorization: "Bearer " + localStorage.getItem("observe_token") },
    });
    load();
  };

  const remove = async (id: string) => {
    if (!confirm("Delete this export?")) return;
    await fetch(`/api/v1/exports/scheduled/${id}`, {
      method: "DELETE",
      headers: { Authorization: "Bearer " + localStorage.getItem("observe_token") },
    });
    load();
  };

  if (loading) return <SettingsSkeleton />;
  if (loadError) return (
    <div class="settings-section">
      <h2>Scheduled SQL exports</h2>
      <div class="obs-empty-state" style={{ color: "var(--obs-danger, #e54)" }}>
        Failed to load exports: {loadError}
      </div>
      <button class="obs-btn" onClick={load} style={{ marginTop: "8px" }}>Retry</button>
    </div>
  );
  return (
    <div class="settings-section">
      <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center" }}>
        <h2>Scheduled SQL exports</h2>
        <button class="obs-btn obs-btn--primary" onClick={() => setShowCreate(true)}>New export</button>
      </div>
      <p style={{ color: "var(--obs-text-muted)", fontSize: "13px" }}>
        Run a SELECT on a schedule, upload the result to S3 / R2 / any
        S3-compatible target as NDJSON or CSV.
      </p>
      {exports.length === 0 && (
        <div class="obs-empty-state">No scheduled exports yet.</div>
      )}
      {exports.map(e => (
        <div key={e.export_id} style={{ border: "1px solid var(--obs-border)", borderRadius: "6px", padding: "12px", marginBottom: "8px" }}>
          <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center" }}>
            <strong>{e.name}</strong>
            <div style={{ display: "flex", gap: "8px" }}>
              <button class="obs-btn" onClick={() => runNow(e.export_id)}>Run now</button>
              <button class="obs-btn" onClick={() => remove(e.export_id)}>Delete</button>
            </div>
          </div>
          <div style={{ fontSize: "12px", fontFamily: "ui-monospace, monospace", color: "var(--obs-text-muted)", marginTop: "4px" }}>{e.sql}</div>
          <div style={{ fontSize: "12px", marginTop: "6px" }}>
            cron: {e.cron} &middot; format: {e.format} &middot;
            last: {e.last_status || "never"} {e.last_rows ? `(${e.last_rows} rows)` : ""}
            {e.last_error && <span style={{ color: "var(--obs-danger)" }}> — {e.last_error}</span>}
          </div>
        </div>
      ))}

      {showCreate && (
        <Modal open onClose={() => setShowCreate(false)} title="New scheduled export">
          <div style={{ display: "grid", gridTemplateColumns: "120px 1fr", gap: "10px 12px", alignItems: "center" }}>
            <label>Name</label>
            <input class="obs-input" value={form.name} onInput={(e) => setForm({ ...form, name: (e.target as HTMLInputElement).value })} />
            <label>Cron</label>
            <select class="obs-input" value={form.cron} onChange={(e) => setForm({ ...form, cron: (e.target as HTMLSelectElement).value })}>
              <option value="@hourly">Hourly</option>
              <option value="@daily">Daily</option>
              <option value="@weekly">Weekly</option>
              <option value="*/15 * * * *">Every 15 min</option>
            </select>
            <label>Format</label>
            <select class="obs-input" value={form.format} onChange={(e) => setForm({ ...form, format: (e.target as HTMLSelectElement).value })}>
              <option value="ndjson">NDJSON</option>
              <option value="csv">CSV</option>
            </select>
            <label>SQL</label>
            <textarea class="obs-input" rows={4} value={form.sql} onInput={(e) => setForm({ ...form, sql: (e.target as HTMLTextAreaElement).value })} placeholder="SELECT ... FROM events ..." />
            <label>Endpoint</label>
            <input class="obs-input" value={form.endpoint} onInput={(e) => setForm({ ...form, endpoint: (e.target as HTMLInputElement).value })} placeholder="(blank for AWS) or https://<account>.r2.cloudflarestorage.com" />
            <label>Region</label>
            <input class="obs-input" value={form.region} onInput={(e) => setForm({ ...form, region: (e.target as HTMLInputElement).value })} />
            <label>Bucket</label>
            <input class="obs-input" value={form.bucket} onInput={(e) => setForm({ ...form, bucket: (e.target as HTMLInputElement).value })} />
            <label>Prefix</label>
            <input class="obs-input" value={form.prefix} onInput={(e) => setForm({ ...form, prefix: (e.target as HTMLInputElement).value })} />
            <label>Access Key</label>
            <input class="obs-input" value={form.access_key_id} onInput={(e) => setForm({ ...form, access_key_id: (e.target as HTMLInputElement).value })} />
            <label>Secret</label>
            <input class="obs-input" type="password" value={form.secret_access_key} onInput={(e) => setForm({ ...form, secret_access_key: (e.target as HTMLInputElement).value })} />
          </div>
          <div style={{ marginTop: "16px", display: "flex", gap: "12px", justifyContent: "flex-end" }}>
            <button class="obs-btn" onClick={() => setShowCreate(false)}>Cancel</button>
            <button class="obs-btn obs-btn--primary" onClick={create} disabled={!form.name || !form.sql || !form.bucket}>Create</button>
          </div>
        </Modal>
      )}
    </div>
  );
}

export default function SettingsPage() {
  const [tab, setTab] = useState("sites");

  const tabs = [
    { key: "sites", label: "Sites" },
    { key: "webhooks", label: "Webhooks" },
    { key: "users", label: "Users" },
    { key: "keys", label: "API Keys" },
    { key: "password", label: "Password" },
    { key: "ai", label: "AI" },
    { key: "exports", label: "Exports" },
  ];

  return (
    <div>
      <div class="obs-page-header">
        <h1 class="obs-page-title">Settings</h1>
      </div>

      <div class="obs-tabs-bar" style={{ marginBottom: "20px" }}>
        {tabs.map(t => (
          <button key={t.key}
            class={`obs-tab ${tab === t.key ? "obs-tab--active" : ""}`}
            onClick={() => setTab(t.key)}>
            {t.label}
          </button>
        ))}
      </div>

      {tab === "sites" && <SitesSection />}
      {tab === "webhooks" && <WebhooksSection />}
      {tab === "users" && <UsersSection />}
      {tab === "keys" && <APIKeysSection />}
      {tab === "password" && <PasswordSection />}
      {tab === "ai" && <AISection />}
      {tab === "exports" && <ExportsSection />}
    </div>
  );
}
