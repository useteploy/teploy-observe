import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { stripTypeScriptTypes } from "node:module";
import vm from "node:vm";

// Execute the real Settings handler bodies and API definitions without a UI
// package install. These are source contract tests, not rendered-browser or
// Go/Nucleus end-to-end tests; those acceptance gates remain separate.
const source = readFileSync(new URL("../routes/settings.tsx", import.meta.url), "utf8");
const apiSource = readFileSync(new URL("../api/settings.ts", import.meta.url), "utf8");
const backend = readFileSync(new URL("../../../cmd/observe/main.go", import.meta.url), "utf8");
const raw = "0123456789abcdef0123456789abcdef";
const masked = raw.slice(0, 8) + "…";
const created = { id: "3eb1bd439947", token: raw, site_id: "site-a", created_at: 1, expires_at: 9999999999999, status: "active" };
const listed = { ...created, token: masked };
const revoked = { ...listed, status: "revoked", revoked_at: 123 };

function handler(name: string) {
  const start = source.indexOf(`  const ${name} =`);
  const end = source.indexOf("\n  };", start);
  assert.ok(start >= 0 && end > start, `handler ${name} exists`);
  return stripTypeScriptTypes(source.slice(start, end + 5));
}

function harness(overrides: Record<string, unknown> = {}) {
  const calls: unknown[][] = [];
  const state: any = {
    shareLinks: [], newShareLink: null, busy: false, loading: false, error: "",
    siteId: created.site_id, shareTtlDays: 30,
    pending: { current: false }, mounted: { current: true },
    settingsApi: {
      async shareLinks(site: string) { calls.push(["list", site]); return [listed]; },
      async createShareLink(site: string, ttl: number) { calls.push(["create", site, ttl]); return created; },
      async revokeShareLink(site: string, id: string) { calls.push(["revoke", site, id]); return revoked; },
      ...overrides,
    },
  };
  for (const key of ["shareLinks", "newShareLink", "busy", "loading", "error"]) {
    state[`set${key[0].toUpperCase()}${key.slice(1)}`] = (next: any) => {
      state[key] = typeof next === "function" ? next(state[key]) : next;
    };
  }
  vm.createContext(state);
  vm.runInContext(["loadShareLinks", "handleCreateShareLink", "handleRevokeShareLink"].map(handler).join("\n"), state);
  const run = (name: string, ...args: unknown[]) => vm.runInContext(`${name}(...${JSON.stringify(args)})`, state);
  return { state, calls, run };
}

function deferred() {
  let resolve!: (value: any) => void;
  const promise = new Promise<any>(r => { resolve = r; });
  return { promise, resolve };
}

test("create preserves the one-time raw response without replacing it with List", async () => {
  const h = harness();
  await h.run("handleCreateShareLink");
  assert.equal(h.state.newShareLink.token, raw);
  assert.equal(h.state.shareLinks[0].token, masked);
  assert.equal(h.state.shareLinks[0].id, created.id);
  assert.deepEqual(h.calls, [["create", "site-a", 30]]);
  assert.equal(h.state.busy, false);
  assert.equal(h.state.error, "");
  await h.run("handleCreateShareLink");
  assert.equal(h.calls.length, 1, "must preserve unsaved reveal instead of overwriting it");
});

test("list after refresh never reconstructs a raw token", async () => {
  const h = harness();
  await h.run("loadShareLinks");
  assert.equal(h.state.newShareLink, null);
  assert.equal(h.state.shareLinks[0].token, masked);
});

test("refresh discards a revealed token that the server now marks revoked", async () => {
  const h = harness({ shareLinks: async () => [revoked] });
  h.state.newShareLink = created;
  await h.run("loadShareLinks");
  assert.equal(h.state.newShareLink, null);
  assert.equal(h.state.shareLinks[0].status, "revoked");
});

test("actual copy handlers use the complete creation token and share URL", () => {
  const urlFunction = source.match(/function shareUrl\([^]*?\n}/)?.[0];
  assert.ok(urlFunction);
  const copies: string[] = [];
  const context = vm.createContext({
    instanceOrigin: () => "https://observe.example.test",
    newShareLink: created,
    copyToClipboard: (value: string) => copies.push(value),
  });
  vm.runInContext(stripTypeScriptTypes(urlFunction), context);
  for (const label of ["Link", "Token"]) {
    const expression = source.match(new RegExp(`onClick=\\{(\\(\\) => copyToClipboard\\([^\\n]+?\\))\\}>Copy ${label}`))?.[1];
    assert.ok(expression, `actual Copy ${label} handler exists`);
    vm.runInContext(`(${expression})()`, context);
  }
  assert.deepEqual(copies, [`https://observe.example.test/share/${raw}`, raw]);
});

test("failed creation is visible and does not invent a credential", async () => {
  const h = harness({ createShareLink: async () => { throw new Error("offline"); } });
  await h.run("handleCreateShareLink");
  assert.equal(h.state.newShareLink, null);
  assert.equal(h.state.shareLinks.length, 0);
  assert.match(h.state.error, /may have been created/);
  assert.equal(h.state.pending.current, false);
});

test("a masked or wrong-site creation response cannot become copyable", async () => {
  for (const link of [listed, { ...created, site_id: "other" }, undefined]) {
    const h = harness({ createShareLink: async () => link });
    await h.run("handleCreateShareLink");
    assert.equal(h.state.newShareLink, null);
    assert.notEqual(h.state.error, "");
  }
});

test("repeated Create clicks send only one request", async () => {
  const d = deferred();
  let count = 0;
  const h = harness({ createShareLink: () => { count++; return d.promise; } });
  const first = h.run("handleCreateShareLink");
  await h.run("handleCreateShareLink");
  assert.equal(count, 1);
  d.resolve(created);
  await first;
  assert.equal(h.state.newShareLink.token, raw);
});

test("closing or switching sites ignores late creation and list responses", async () => {
  for (const name of ["handleCreateShareLink", "loadShareLinks"]) {
    const d = deferred();
    const h = harness({ createShareLink: () => d.promise, shareLinks: () => d.promise });
    const pending = h.run(name);
    h.state.mounted.current = false;
    d.resolve(name === "loadShareLinks" ? [listed] : created);
    await pending;
    assert.equal(h.state.newShareLink, null);
    assert.equal(h.state.shareLinks.length, 0);
  }
  assert.match(source, /<ShareLinksPanel key=\{s\.site_id\} siteId=\{s\.site_id\}/);
  assert.match(source, /return \(\) => \{ mounted\.current = false; \}/);
});

test("revoke sends the ID and retains confirmed revoked history", async () => {
  const h = harness();
  h.state.shareLinks = [listed];
  h.state.newShareLink = created;
  await h.run("handleRevokeShareLink", created.id);
  assert.deepEqual(h.calls, [["revoke", "site-a", created.id]]);
  assert.equal(h.state.shareLinks.length, 1);
  assert.equal(h.state.shareLinks[0].status, "revoked");
  assert.equal(h.state.newShareLink, null);
});

test("failed or unconfirmed revocations leave the row and warn it may work", async () => {
  for (const response of [undefined, listed, { ...revoked, id: "other" }, { ...revoked, site_id: "other" }, { ...revoked, revoked_at: 0 }, "throw"]) {
    const h = harness({ revokeShareLink: async () => {
      if (response === "throw") throw new Error("not found");
      return response;
    } });
    h.state.shareLinks = [listed];
    h.state.newShareLink = created;
    await h.run("handleRevokeShareLink", created.id);
    assert.equal(h.state.shareLinks[0].status, "active");
    assert.equal(h.state.newShareLink.token, raw);
    assert.match(h.state.error, /may still work/);
    assert.equal(h.state.busy, false);
  }
});

test("repeated Revoke clicks are guarded and late completion is ignored", async () => {
  const d = deferred();
  let count = 0;
  const h = harness({ revokeShareLink: () => { count++; return d.promise; } });
  h.state.shareLinks = [listed];
  const first = h.run("handleRevokeShareLink", created.id);
  await h.run("handleRevokeShareLink", created.id);
  assert.equal(count, 1);
  h.state.mounted.current = false;
  d.resolve(revoked);
  await first;
  assert.equal(h.state.shareLinks[0].status, "active");
});

test("load errors are not presented as an empty successful list", async () => {
  const h = harness({ shareLinks: async () => { throw new Error("offline"); } });
  h.state.shareLinks = [listed];
  await h.run("loadShareLinks");
  assert.equal(h.state.shareLinks.length, 1);
  assert.match(h.state.error, /Could not load/);
  assert.equal(h.state.loading, false);
});

test("only the one-time reveal has copy actions; listed rows revoke by ID", () => {
  assert.match(source, /copyToClipboard\(shareUrl\(newShareLink\.token\)\)/);
  assert.match(source, /copyToClipboard\(newShareLink\.token\)/);
  assert.doesNotMatch(source, /copyToClipboard\((?:shareUrl\()?l(?:ink)?\.token/);
  assert.match(source, /onClick=\{\(\) => handleRevokeShareLink\(link\.id\)\}/);
  assert.match(source, /onClick=\{\(\) => setNewShareLink\(null\)\}/);
  assert.doesNotMatch(source.slice(source.indexOf("function ShareLinksPanel"), source.indexOf("function SitesSection")), /(?:localStorage|sessionStorage|console\.)/);
});

test("real Settings API uses the matching editor-only backend ID route", async () => {
  const requests: unknown[][] = [];
  const context = vm.createContext({
    get: async (path: string) => requests.push(["GET", path]),
    post: async (path: string, body: unknown) => requests.push(["POST", path, body]),
    del: async (path: string) => { requests.push(["DELETE", path]); return revoked; },
  });
  const code = stripTypeScriptTypes(apiSource.replace(/^import .*;\n/m, "").replace(/export /g, ""));
  vm.runInContext(code, context);
  await vm.runInContext(`settingsApi.createShareLink('site-a', 30)`, context);
  await vm.runInContext(`settingsApi.shareLinks('site-a')`, context);
  const result = await vm.runInContext(`settingsApi.revokeShareLink('site/a', 'id?x')`, context);
  assert.equal(result, revoked);
  assert.deepEqual(JSON.parse(JSON.stringify(requests)), [
    ["POST", "/api/v1/sites/site-a/share", { ttl_days: 30 }],
    ["GET", "/api/v1/sites/site-a/share"],
    ["DELETE", "/api/v1/sites/site%2Fa/share/id%3Fx"],
  ]);
  assert.match(backend, /shareGroup := r\.Group\("\/api\/v1", jwtMW\)/);
  assert.match(backend, /shareEditor := shareGroup\.Group\("", requireEditor\)/);
  assert.match(backend, /neutron\.Delete\(shareEditor, "\/sites\/\{site_id\}\/share\/\{id\}", revokeShareByIDHandler\(shareSvc\)/);
  assert.match(backend, /shareSvc\.RevokeByID\(ctx, input\.SiteID, input\.ID\)/);
  assert.match(backend, /neutron\.Delete\(shareEditor, "\/share\/\{token\}", revokeShareHandler\(shareSvc\)/);
});
