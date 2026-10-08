import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { stripTypeScriptTypes } from "node:module";
import vm from "node:vm";
import { readAIConfig } from "./aiConfig.ts";

const source = (path: string) => readFileSync(new URL(path, import.meta.url), "utf8");
function callback(path: string, name: string) {
  const text = source(path); const start = text.indexOf(`  const ${name} =`);
  const callback = text.slice(start, start + 80).includes("useCallback(");
  const end = callback ? text.indexOf("\n  }, [", start) : text.indexOf("\n  };", start);
  assert.ok(start >= 0 && end > start, name);
  const fullEnd = callback ? text.indexOf("]);", end) + 3 : end + 5;
  return stripTypeScriptTypes(text.slice(start, fullEnd));
}
function scope(values: Record<string, any>) {
  const state: any = { useCallback: (fn: any) => fn, console: { error() {} }, ...values };
  for (const key of Object.keys(values)) state[`set${key[0].toUpperCase()}${key.slice(1)}`] ??= (next: any) => { state[key] = typeof next === "function" ? next(state[key]) : next; };
  return vm.createContext(state);
}
const json = (x: any) => JSON.parse(JSON.stringify(x));
function defer() { let resolve!: (x: any) => void; let reject!: (e: any) => void; const promise = new Promise((r, j) => { resolve = r; reject = j; }); return { promise, resolve, reject }; }
function guard() { let generation = 0; return () => { const selected = ++generation; return () => selected === generation; }; }

function api(path: string, name: string, extra: Record<string, any> = {}) {
  const calls: any[] = [];
  const ctx = scope({ get: (url: string) => { calls.push({ url }); return Promise.resolve([]); }, post: (url: string, body: unknown) => { calls.push({ url, body }); return Promise.resolve({}); }, put() {}, del() {}, qs: (site: string, from: string, to: string) => new URLSearchParams({ site_id: site, from, to, cohort_id: "paid cohort" }).toString(), activeCohortID: () => "paid cohort", ...extra });
  const text = source(path).replace(/^import .*;\n/gm, "").replace(/export /g, "");
  vm.runInContext(stripTypeScriptTypes(text) + `\nglobalThis.api = ${name};`, ctx);
  return { calls, api: ctx.api };
}

test("OBS26-16/48 explicit encoded site accompanies experiment and host detail reads", async () => {
  const experiments = api("../api/flags.ts", "experimentsApi"); await experiments.api.results("exp/id", "site & B");
  assert.equal(experiments.calls[0].url, "/api/v1/experiments/exp%2Fid/results?site_id=site%20%26%20B");
  const hosts = api("../api/monitoring.ts", "monitoringApi"); await hosts.api.infraHistory("host/a", "site & B", "from +", "to /");
  const url = new URL(hosts.calls[0].url, "https://example.test"); assert.equal(url.searchParams.get("site_id"), "site & B"); assert.equal(url.searchParams.get("from"), "from +");
});

test("OBS26-17 real creation handler preserves sample, goal value and keyed allocation", async () => {
  const calls: any[] = []; const ctx = scope({ formName: "Test", formFlagKey: "checkout", formGoal: "purchase", formGoalValue: "paid", formMinSample: "10000", formVariants: "baseline,treatment", siteId: "B", creating: false, showCreate: true, createError: null, experimentsApi: { create: async (x: any) => calls.push(x) }, fetchExperiments() {} });
  vm.runInContext(callback("../routes/experiments.tsx", "handleCreate"), ctx); await vm.runInContext("handleCreate()", ctx);
  assert.equal(calls[0].min_sample, 10000); assert.equal(calls[0].goal_value, "paid"); assert.deepEqual(JSON.parse(calls[0].variants), [{ key: "baseline", weight: 1 }, { key: "treatment", weight: 1 }]);
  ctx.formName = "Test"; ctx.formFlagKey = "checkout"; ctx.formGoal = "purchase"; ctx.formVariants = "x,x"; await vm.runInContext("handleCreate()", ctx); assert.equal(calls.length, 1); assert.match(ctx.createError, /distinct/);
});

test("OBS26-18/19 real flag handler emits multivariate payload and preserves zero", async () => {
  const calls: any[] = []; const ctx = scope({ formKey: "color", formName: "Color", formDesc: "", formType: "multivariate", formRollout: "0", formTargeting: [], formVariants: [{ key: "blue", name: "Blue", payload: '"blue"', rollout_pct: 50 }, { key: "red", name: "Red", payload: '{"color":"red"}', rollout_pct: 50 }], creating: false, createError: null, showCreate: true, siteId: "B", flagsApi: { create: async (x: any) => calls.push(x) }, resetForm() {}, fetchFlags() {} });
  vm.runInContext(callback("../routes/flags.tsx", "handleCreate"), ctx); await vm.runInContext("handleCreate()", ctx);
  assert.equal(calls[0].rollout_pct, 0); assert.equal(calls[0].flag_type, "multivariate"); assert.deepEqual(JSON.parse(calls[0].variants)[0], { key: "blue", name: "Blue", payload: "blue", rollout_pct: 50 });
  for (const invalid of ["", "-1", "101", "1.5", "NaN"]) { ctx.formRollout = invalid; await vm.runInContext("handleCreate()", ctx); assert.ok(ctx.createError); }
  assert.equal(calls.length, 1);
  ctx.formRollout = "1"; ctx.formVariants[0].payload = "invalid JSON"; await vm.runInContext("handleCreate()", ctx); assert.equal(calls.length, 1);
});

test("OBS26-20 repeated returned cursors keep identity and incoming fetch dependency stable", async () => {
  const calls: any[] = []; const ctx = scope({ PAGE_SIZE: 50, siteId: "A", query: "level:error", activeLevel: "ALL", service: "", page: 1, incomingCursor: undefined, cursors: [], loading: false, error: null, syntaxError: null, truncated: false, logs: [], stats: [], histogram: [], requestGeneration: { current: 0 }, queryTooLong: () => false, logsApi: { search: async (...args: any[]) => { calls.push(args); return { logs: [], truncated: true, nextCursor: "same" }; }, stats: async () => [], histogram: async () => [] } });
  const body = callback("../routes/logs.tsx", "fetchLogs"); vm.runInContext(body, ctx);
  await vm.runInContext("fetchLogs()", ctx); const cursors = ctx.cursors;
  await vm.runInContext("fetchLogs()", ctx); assert.equal(ctx.cursors, cursors); assert.equal(ctx.incomingCursor, undefined); assert.equal(calls[0][3].cursor, undefined);
  assert.ok(body.endsWith("service, page, incomingCursor]);"));
  ctx.page = 2; ctx.incomingCursor = "same"; await vm.runInContext("fetchLogs()", ctx); assert.equal(calls[2][3].cursor, "same");
});

test("OBS26-21 cursor overrides row count for sparse, empty and terminal pagination", () => {
  const text = source("../components/shared/Pagination.tsx"); const prefix = text.slice(0, text.indexOf("\n  return ("));
  const ctx = scope({}); vm.runInContext(stripTypeScriptTypes(prefix.replace("export default ", "") + "\n return hasMore; }"), ctx);
  for (const count of [0, 1, 49]) assert.equal(vm.runInContext(`Pagination({ page:1, pageSize:50, resultCount:${count}, hasMore:true })`, ctx), true);
  assert.equal(vm.runInContext("Pagination({ page:1, pageSize:50, resultCount:50, hasMore:false })", ctx), null);
});

test("OBS26-23 actual person list handler ignores old success and finalizer", async () => {
  const old = defer(), latest = defer(); let n = 0; const ctx = scope({ request: guard(), siteId: "A", from: "from", to: "to", page: 1, includeAnonymous: false, PAGE_SIZE: 25, persons: [], total: 0, loading: false, personsApi: { list: () => ++n === 1 ? old.promise : latest.promise } });
  vm.runInContext(callback("../routes/persons.tsx", "fetchPersons"), ctx);
  const first = vm.runInContext("fetchPersons()", ctx); ctx.siteId = "B"; const second = vm.runInContext("fetchPersons()", ctx);
  latest.resolve({ persons: [{ distinct_id: "B" }], total: 1 }); await second; old.resolve({ persons: [{ distinct_id: "A" }], total: 999 }); await first;
  assert.equal(ctx.persons[0].distinct_id, "B"); assert.equal(ctx.total, 1); assert.equal(ctx.loading, false);
});

test("OBS26-42/47 real dashboard loader accepts exact epoch bounds and error series", async () => {
  const posts: any[] = []; const panels = [{ panel_id: "e", title: "Errors", panel_type: "timeseries", query_type: "errors" }, { panel_id: "p", title: "Views", panel_type: "metric", query_type: "pageviews" }];
  const ctx = scope({ requestfetchDetail: guard(), dashboardId: "d", siteId: "B", BASE: "/api/v1/dashboards", detail: null, loading: false, detailError: "", trendError: "", panelValues: {}, panelSparklines: {}, chartLabels: [], metricSeriesByPanel: {}, errorSeriesByPanel: {}, panelErrors: {}, get: async () => ({ panels }), post: async (url: string, body: any) => { posts.push({ url, body }); return url.includes("/e/") ? [{ bucket: 1000, errors: 9 }, { bucket: 2000, errors: 0 }] : { value: "22" }; }, analyticsApi: { timeseries: async () => [{ bucket: 1000, pageviews: 5, visitors: 3 }] } });
  vm.runInContext(callback("../routes/dashboards.tsx", "fetchDetail"), ctx); await vm.runInContext("fetchDetail()", ctx);
  assert.deepEqual(json(ctx.errorSeriesByPanel.e), [{ bucket: 1000, errors: 9 }, { bucket: 2000, errors: 0 }]); assert.equal(ctx.panelValues.p, "22"); assert.deepEqual(json(ctx.panelErrors), {});
  for (const p of posts) { assert.match(p.body.from, /^\d+$/); assert.match(p.body.to, /^\d+$/); assert.ok(Number(p.body.to) > Number(p.body.from)); }
});

test("OBS26-55 every funnel/goal/journey/correlation request carries selected cohort", async () => {
  const h = api("../api/analytics.ts", "analyticsApi");
  await h.api.funnel("B", "from", "to", []); await h.api.funnelBreakdown("B", "from", "to", [], "country"); await h.api.goals("B", "from", "to"); await h.api.journeys("B", "from", "to"); await h.api.correlations("B", "from", "to");
  for (const call of h.calls) assert.equal(call.body ? call.body.cohort_id : new URL(call.url, "https://example.test").searchParams.get("cohort_id"), "paid cohort");
});

test("OBS26-23 request guard invalidates new selections, repeated requests and unmount", () => {
  const ref = { current: null as any }; let cleanup: (() => void) | undefined;
  const ctx = scope({ useRef: (initial: any) => { ref.current ??= initial; return ref; }, useEffect: (fn: any) => { cleanup ??= fn(); }, useCallback: (fn: any) => fn });
  vm.runInContext(stripTypeScriptTypes(source("../hooks/useRequestGuard.ts").replace(/^import .*;\n/gm, "").replace("export function", "function")), ctx);
  const begin = vm.runInContext('useRequestGuard("A")', ctx); const a = begin(); assert.equal(a(), true);
  vm.runInContext('useRequestGuard("B")', ctx); assert.equal(a(), false);
  const b = begin(); const c = begin(); assert.equal(b(), false); assert.equal(c(), true);
  cleanup!(); assert.equal(c(), false); assert.equal(begin()(), false);
});

test("OBS26-24 actual custom range reopen/apply roundtrip across local DST", () => {
  const text = source("../components/DatePicker.tsx");
  const helpers = text.slice(text.indexOf("function toDateInput"), text.indexOf("function DatePicker"));
  const functionBody = (name: string) => { const a = text.indexOf(`  function ${name}()`); return text.slice(a, text.indexOf("\n  }", a) + 4); };
  const prior = process.env.TZ;
  try {
    for (const zone of ["UTC", "America/Vancouver", "Europe/Berlin"]) {
      process.env.TZ = zone;
      for (const [start, end] of [["2026-10-01", "2026-10-02"], ["2026-03-07", "2026-03-10"], ["2026-10-31", "2026-11-03"]]) {
        const state = { from: new Date(start + "T00:00:00").toISOString(), to: new Date(end + "T00:00:00").toISOString() };
        const ctx = scope({ state, customFrom: "", customTo: "", customOpen: false, open: true, CUSTOM_LABEL: "Custom", dispatch: (action: any) => Object.assign(state, { from: action.from, to: action.to }) });
        vm.runInContext(stripTypeScriptTypes(helpers + functionBody("openCustom") + functionBody("applyCustom")), ctx);
        const original = { ...state };
        for (let i = 0; i < 3; i++) { vm.runInContext("openCustom(); applyCustom();", ctx); assert.deepEqual(state, original, zone + start); }
      }
    }
  } finally { if (prior === undefined) delete process.env.TZ; else process.env.TZ = prior; }
});

test("OBS26-53 monitoring formatter accepts epoch strings, numbers, ISO and invalid", () => {
  const text = source("../routes/monitoring.tsx"); const a = text.indexOf("function formatDate"); const b = text.indexOf("\nfunction formatInterval", a);
  const ctx = scope({}); vm.runInContext(stripTypeScriptTypes(text.slice(a, b)), ctx);
  const numeric = vm.runInContext("formatDate(1791288000000)", ctx); assert.equal(vm.runInContext('formatDate("1791288000000")', ctx), numeric); assert.notEqual(numeric, "Invalid Date");
  assert.equal(vm.runInContext('formatDate("invalid")', ctx), "--"); assert.notEqual(vm.runInContext('formatDate("2026-10-06T00:00:00Z")', ctx), "--");
});

test("OBS26-110 actual row callbacks bind each hovered and clicked item", () => {
  const text = source("../components/CommandPalette.tsx");
  const hover = text.match(/onMouseEnter=\{(.+)\}/)![1];
  const click = text.match(/onClick=\{(\(\) => \{ item\.action\(\); setOpen\(false\); \})\}/)![1];
  const ctx = scope({ sel: -1, open: true, displayed: [], hits: [] });
  vm.runInContext(`globalThis.row = (item) => { const itemIndex = displayed.indexOf(item); return { hover: ${hover}, click: ${click} }; };`, ctx);
  ctx.displayed = [{ action: () => ctx.hits.push("first") }, { action: () => ctx.hits.push("last") }];
  const first = ctx.row(ctx.displayed[0]); const last = ctx.row(ctx.displayed[1]);
  first.hover(); assert.equal(ctx.sel, 0); first.click(); assert.deepEqual(ctx.hits, ["first"]);
  last.hover(); assert.equal(ctx.sel, 1); last.click(); assert.deepEqual(ctx.hits, ["first", "last"]);
});

test("OBS26-111 actual AI save preserves draft on rejection, blocks repeats and ignores unmount", async () => {
  for (const status of [400, 401, 403, 500]) {
    const draft = { provider: "openai", endpoint: "https://example.test", model: "test", has_key: false, api_key: "synthetic" };
    const ctx = scope({ cfg: draft, pending: { current: false }, mounted: { current: true }, saving: false, message: null, localStorage: { getItem: () => "synthetic" }, readAIConfig, fetch: async () => new Response(JSON.stringify({ error: "refused" }), { status }) });
    const text = source("../routes/settings.tsx"); const section = text.slice(text.indexOf("function AISection()")); const a = section.indexOf("  const save ="); const b = section.indexOf("\n  };", a);
    vm.runInContext(stripTypeScriptTypes(section.slice(a, b + 5)), ctx); await vm.runInContext("save()", ctx);
    assert.equal(ctx.cfg, draft); assert.match(ctx.message, /refused/); assert.equal(ctx.saving, false); assert.equal(ctx.pending.current, false);
    const waiting = defer(); let calls = 0; ctx.fetch = () => { calls++; return waiting.promise; };
    const first = vm.runInContext("save()", ctx); await vm.runInContext("save()", ctx); assert.equal(calls, 1);
    ctx.mounted.current = false; waiting.resolve(new Response(JSON.stringify({ ...draft, has_key: true, api_key: undefined }))); await first; assert.equal(ctx.cfg, draft); assert.equal(ctx.message, null);
  }
});

test("OBS26-23 actual trace loader cannot replace newer detail after Back", async () => {
  const a = defer(), b = defer(); let calls = 0; const request = guard();
  const ctx = scope({ traceRequest: request, siteId: "B", loadingTrace: false, traceSpans: [], traceId: null, view: "services", tracesApi: { trace: () => ++calls === 1 ? a.promise : b.promise } });
  vm.runInContext(callback("../routes/traces.tsx", "loadTrace"), ctx);
  const old = vm.runInContext('loadTrace("a")', ctx); request(); const latest = vm.runInContext('loadTrace("b")', ctx);
  b.resolve([{ trace_id: "b" }]); await latest; a.resolve([{ trace_id: "a" }]); await old;
  assert.equal(ctx.traceId, "b"); assert.equal(ctx.traceSpans[0].trace_id, "b"); assert.equal(ctx.loadingTrace, false);
  assert.match(source("../routes/traces.tsx"), /<SiteTracesPage key=\{siteId\}/);
});

test("OBS26-23 old overview rejection cannot replace current result after cleanup", async () => {
  const text = source("../components/StatsCards.tsx"); const a = text.indexOf("  useEffect(() => {"); const b = text.indexOf("\n  }, [", a); const effect = text.slice(a, text.indexOf("]);", b) + 3);
  const old = defer(), latest = defer(); let n = 0; const cleanups: (() => void)[] = [];
  const ctx = scope({ siteId: "A", from: "from", to: "to", compare: null, filters: {}, overview: null, realtime: null, error: null, setInterval: () => 1, clearInterval() {}, useEffect: (fn: any) => cleanups.push(fn()), api: { overview: () => ++n === 1 ? old.promise : latest.promise, realtime: async () => ({ active_visitors: 1 }) } });
  vm.runInContext(stripTypeScriptTypes(effect), ctx); cleanups[0](); ctx.siteId = "B"; vm.runInContext(stripTypeScriptTypes(effect), ctx);
  latest.resolve({ current: { pageviews: 22 } }); await latest.promise; await Promise.resolve(); old.reject(new Error("old failure")); await old.promise.catch(() => {}); await Promise.resolve();
  assert.equal(ctx.overview.current.pageviews, 22); assert.equal(ctx.error, null); cleanups[1]();
});
