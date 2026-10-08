import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { stripTypeScriptTypes } from "node:module";
import vm from "node:vm";
const read = (file: string) => readFileSync(new URL(file, import.meta.url), "utf8");
const settle = () => new Promise(resolve => setImmediate(resolve));
function effect(file: string, marker: string) {
  const source = read(file), start = source.indexOf("  useEffect(() => {", source.indexOf(marker));
  const end = source.indexOf("]);", source.indexOf("\n  }, [", start)) + 3;
  assert.ok(start >= 0 && end > start);
  return stripTypeScriptTypes(source.slice(start, end));
}
function context(values: Record<string, any>) {
  const state: any = { useCallback: (f: any) => f, ...values };
  for (const key of Object.keys(values)) state[`set${key[0].toUpperCase()}${key.slice(1)}`] ??= (next: any) => { state[key] = typeof next === "function" ? next(state[key]) : next; };
  return vm.createContext(state);
}
function deferred() { let resolve!: (v: any) => void, reject!: (e: any) => void; const promise = new Promise((r,j) => { resolve = r; reject = j; }); return { promise, resolve, reject }; }

test("OBS26-45 analyses reject distinctly, retry empty success, ignore cleaned-up replies", async () => {
  for (const [panel, api, field, empty] of [["RetentionPanel", "retention", "cohorts", []], ["JourneysPanel", "journeys", "data", null], ["CorrelationsPanel", "correlations", "data", []]] as const) {
    const pending = deferred(); let cleanup: any;
    const ctx = context({ siteId: "A", from: "0", to: "1", retry: 0, error: "", loading: true, [field]: empty, useEffect: (f: any) => { cleanup = f(); }, analyticsApi: { [api]: () => Promise.reject(Error("503")) } });
    const body = effect("../routes/insights.tsx", `function ${panel}`);
    vm.runInContext(body, ctx); await settle();
    assert.match(ctx.error, /Unable to load/); assert.equal(ctx.loading, false);
    ctx.analyticsApi[api] = () => Promise.resolve(empty);
    vm.runInContext(body, ctx); await settle(); assert.equal(ctx.error, "");
    ctx.analyticsApi[api] = () => pending.promise;
    vm.runInContext(body, ctx); cleanup(); pending.reject(Error("late A")); await settle();
    assert.equal(ctx.error, ""); assert.equal(ctx.loading, true);
    const source = read("../routes/insights.tsx").slice(read("../routes/insights.tsx").indexOf(`function ${panel}`));
    assert.ok(source.indexOf("if (error)") < source.indexOf("<EmptyState"));
  }
});

test("OBS26-23 metric catalogue A/B/C fetches and modal close invalidate late data/error", async () => {
  const requests: any[] = []; let cleanup: any;
  const ctx = context({ siteId: "A", showAddPanel: true, catalogueRetry: 0, availableMetrics: [], catalogueError: "", metricName: "old", metricLabelRows: [{ k: "old", v: "old" }], metricGroupBy: "old", useEffect: (f: any) => { cleanup = f(); }, metricsApi: { list: (site: string) => { const request = deferred(); requests.push({ site, ...request }); return request.promise; } } });
  const body = effect("../routes/dashboards.tsx", "// Lazy-load the metric autocomplete");
  vm.runInContext(body, ctx); cleanup(); ctx.siteId = "B"; vm.runInContext(body, ctx);
  requests[1].resolve([{ name: "B" }]); await settle();
  requests[0].resolve([{ name: "A" }]); await settle(); assert.equal(ctx.availableMetrics[0].name, "B");
  cleanup(); ctx.siteId = "C"; vm.runInContext(body, ctx);
  assert.equal(ctx.availableMetrics.length, 0); assert.equal(ctx.metricName, ""); assert.equal(ctx.metricLabelRows.length, 0);
  assert.deepEqual(requests.map(r => r.site), ["A", "B", "C"]);
  cleanup(); ctx.showAddPanel = false; vm.runInContext(body, ctx);
  requests[2].reject(Error("late C")); await settle(); assert.equal(ctx.catalogueError, ""); assert.equal(ctx.availableMetrics.length, 0);
});

test("OBS26-45 event properties and trend keep independent failure availability", async () => {
  let cleanup: any;
  const ctx = context({ siteId: "A", from: "0", to: "1", eventType: "purchase", retry: 0, props: [], timeseries: [], propsError: "", trendError: "", loading: true, useEffect: (f: any) => { cleanup = f(); }, analyticsApi: { eventProperties: () => Promise.reject(Error("503")), timeseries: () => Promise.resolve([{ pageviews: 7 }]) } });
  vm.runInContext(effect("../routes/events.tsx", "function EventDetailView"), ctx); await settle();
  assert.match(ctx.propsError, /Unable to load/); assert.equal(ctx.trendError, ""); assert.equal(ctx.timeseries[0].pageviews, 7); assert.equal(ctx.loading, false); cleanup();
});

test("Failure UI exposes an alert and keyboard-operable retry button", () => {
  const source = read("../components/shared/QueryFailure.tsx");
  assert.match(source, /role="alert"/); assert.match(source, /<button type="button"/); assert.match(source, /onClick={retry}/);
});

function callback(file: string, name: string, marker = "") {
  const source = read(file), start = source.indexOf(`  const ${name} =`, source.indexOf(marker));
  const hooked = source.slice(start, start + 80).includes("useCallback(");
  const end = source.indexOf(hooked ? "\n  }, [" : "\n  };", start);
  return stripTypeScriptTypes(source.slice(start, hooked ? source.indexOf("]);", end) + 3 : end + 5));
}
test("OBS26-45 actual funnel rejection retains draft and retry succeeds", async () => {
  const ctx = context({ siteId: "A", from: "0", to: "1", steps: [{ type: "page", value: "/" }, { type: "page", value: "/buy" }], breakdownBy: "", error: "", loading: false, analyzed: false, results: [], breakdownResults: [], requestAnalyze: () => () => true, analyticsApi: { funnel: () => Promise.reject(Error("503")) } });
  vm.runInContext(callback("../routes/insights.tsx", "analyze"), ctx); await vm.runInContext("analyze()", ctx);
  assert.match(ctx.error, /Unable to analyze/); assert.equal(ctx.steps[1].value, "/buy"); assert.equal(ctx.loading, false);
  ctx.analyticsApi.funnel = () => Promise.resolve([]); await vm.runInContext("analyze()", ctx); assert.equal(ctx.error, ""); assert.equal(ctx.analyzed, true);
});
test("OBS26-45 UTM failure stays per-dimension with accepted neighbours", async () => {
  const ctx = context({ siteId: "A", from: "0", to: "1", sources: [], mediums: [], campaigns: [], terms: [], contents: [], utmErrors: {}, loading: false, requestfetch: () => () => true, analyticsApi: { utm: (_s: string, _f: string, _t: string, dimension: string) => dimension === "term" ? Promise.reject(Error("503")) : Promise.resolve([{ value: dimension, visitors: 7 }]) } });
  vm.runInContext(callback("../routes/campaigns.tsx", "fetch", "export default function CampaignsPage"), ctx); await vm.runInContext("fetch()", ctx);
  assert.match(ctx.utmErrors.term, /Unable to load/); assert.equal(ctx.utmErrors.source, undefined); assert.equal(ctx.sources[0].visitors, 7); assert.equal(ctx.terms.length, 0);
});
test("OBS26-45 dashboard metric refusal retains other accepted panel values", async () => {
  const ctx = context({ BASE: "/dashboards", dashboardId: "d", siteId: "A", detail: null, detailError: "", trendError: "", loading: false, panelValues: {}, panelSparklines: {}, metricSeriesByPanel: {}, errorSeriesByPanel: {}, panelErrors: {}, chartLabels: [], requestfetchDetail: () => () => true, get: async () => ({ panels: [{ title: "metric", panel_id: "m", panel_type: "metric_series", query_type: "metric_series" }, { title: "value", panel_id: "v", panel_type: "metric", query_type: "pageviews" }] }), post: async (url: string) => { if (url.includes("/m/")) throw Error("503"); return { value: "7" }; }, analyticsApi: { timeseries: async () => [] } });
  vm.runInContext(callback("../routes/dashboards.tsx", "fetchDetail"), ctx); await vm.runInContext("fetchDetail()", ctx);
  assert.match(ctx.panelErrors.m, /Unable to load/); assert.equal(ctx.panelErrors.v, undefined); assert.equal(ctx.panelValues.v, "7");
});


test("Events list and Incidents ignore a stale site response and expose current failures", async () => {
  for (const [file, name, guard] of [["../routes/events.tsx", "fetch", "requestList"], ["../routes/incidents.tsx", "load", "requestList"]]) {
    let generation = 0;
    const pending = deferred();
    const ctx = context({ siteId: "A", from: "0", to: "1", events: [], active: [], recent: [], error: "", loading: false,
      [guard]: () => { const own = ++generation; return () => own === generation; },
      analyticsApi: { customEvents: () => pending.promise }, api: () => pending.promise });
    vm.runInContext(callback(file, name), ctx);
    const old = vm.runInContext(`${name}()`, ctx);
    generation++; ctx.siteId = "B";
    ctx.analyticsApi.customEvents = () => Promise.reject(Error("503")); ctx.api = () => Promise.reject(Error("503"));
    await vm.runInContext(`${name}()`, ctx);
    assert.match(ctx.error, /Unable to load/);
    pending.resolve([{ event_type: "old", ended_at: 1 }]); await old;
    assert.match(ctx.error, /Unable to load/); assert.equal(ctx.events.length, 0); assert.equal(ctx.active.length, 0);
  }
});


test("Goal conversion read failures remain distinct from no goals", async () => {
  const ctx = context({ siteId: "A", from: "0", to: "1", goals: [], loadError: "", loading: false,
    requestGoals: () => () => true, analyticsApi: { goals: () => Promise.reject(Error("503")) } });
  vm.runInContext(callback("../routes/insights.tsx", "load", "function GoalsPanel"), ctx);
  await vm.runInContext("load()", ctx); assert.match(ctx.loadError, /Unable to load goal conversions/);
  ctx.analyticsApi.goals = () => Promise.resolve([]); await vm.runInContext("load()", ctx);
  assert.equal(ctx.loadError, ""); assert.equal(ctx.loading, false);
});
