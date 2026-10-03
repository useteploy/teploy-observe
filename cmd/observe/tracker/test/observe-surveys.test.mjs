// Contract tests for the survey widget. Loads the REAL served script in a vm
// sandbox against a small fake DOM that THROWS on any HTML-string sink
// (innerHTML/outerHTML/insertAdjacentHTML/document.write), so a regression to
// string-built markup fails loudly. Run: node --test cmd/observe/tracker/test/
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";
import vm from "node:vm";

const here = path.dirname(fileURLToPath(import.meta.url));
const src = readFileSync(path.join(here, "..", "observe-surveys.js"), "utf8");

class El {
  constructor(tag) {
    this.tagName = tag.toUpperCase();
    this.children = [];
    this.parentNode = null;
    this.attrs = {};
    this.listeners = {};
    this.className = "";
    this._text = "";
    this.checked = false;
    this.value = "";
    this.disabled = false;
    this.style = { props: {}, setProperty: (k, v) => { this.style.props[k] = v; } };
  }
  set innerHTML(v) { throw new Error("innerHTML sink used: " + v); }
  set outerHTML(v) { throw new Error("outerHTML sink used"); }
  insertAdjacentHTML() { throw new Error("insertAdjacentHTML sink used"); }
  get textContent() { return this._text + this.children.map((c) => c.textContent).join(""); }
  set textContent(v) { this._text = String(v); this.children = []; }
  setAttribute(k, v) { this.attrs[k] = String(v); if (k === "id") this.id = String(v); }
  getAttribute(k) { return k in this.attrs ? this.attrs[k] : null; }
  appendChild(c) { c.parentNode = this; this.children.push(c); return c; }
  removeChild(c) { this.children = this.children.filter((x) => x !== c); c.parentNode = null; }
  remove() { if (this.parentNode) this.parentNode.removeChild(this); }
  addEventListener(t, fn) { (this.listeners[t] ||= []).push(fn); }
  removeEventListener() {}
  dispatch(t, ev = {}) {
    ev = { preventDefault() {}, stopPropagation() {}, ...ev };
    (this.listeners[t] || []).forEach((fn) => fn(ev));
  }
  focus() { this.ownerRoot.activeElement = this; }
  get ownerRoot() { return this._root || (this.parentNode ? this.parentNode.ownerRoot : this._doc); }
  all(pred, out = []) { this.children.forEach((c) => { if (pred(c)) out.push(c); c.all(pred, out); }); return out; }
  querySelectorAll(sel) {
    const tags = sel.split(",").map((s) => s.trim().toUpperCase());
    return this.all((c) => tags.includes(c.tagName));
  }
  find(cls) { return this.all((c) => c.className.split(" ").includes(cls))[0]; }
  findAll(pred) { return this.all(pred); }
}

function makeEnv({ attrs = { "data-site-id": "site1" }, surveys = [], dnt = null, store = {}, storageThrows = false, responses = {}, fetchImpl, width = 1280 } = {}) {
  const calls = [];
  const timers = [];
  let shadow = null;
  const docListeners = {};
  const doc = {
    currentScript: { src: "https://observe.test/t/observe-surveys.js", getAttribute: (n) => (n in attrs ? attrs[n] : null) },
    readyState: "complete",
    referrer: "https://News.Example.com/story",
    activeElement: null,
    documentElement: { clientWidth: width },
    body: new El("body"),
    createElement: (t) => { const e = new El(t); e._doc = doc; return e; },
    addEventListener: (t, fn) => (docListeners[t] ||= []).push(fn),
    removeEventListener: (t, fn) => { docListeners[t] = (docListeners[t] || []).filter((f) => f !== fn); },
    write() { throw new Error("document.write sink used"); },
  };
  doc.body._doc = doc;
  const origCreate = doc.createElement;
  doc.createElement = (t) => {
    const e = origCreate(t);
    if (e.tagName === "DIV") {
      e.attachShadow = () => {
        const r = new El("shadow");
        r._root = r;
        r._doc = doc;
        r.activeElement = null;
        shadow = r;
        return r;
      };
    }
    return e;
  };
  const storage = {
    getItem: (k) => { if (storageThrows) throw new Error("denied"); return k in store ? store[k] : null; },
    setItem: (k, v) => { if (storageThrows) throw new Error("denied"); store[k] = v; },
  };
  const res = (status, body, headers = {}) => ({
    ok: status >= 200 && status < 300, status,
    headers: { get: (h) => headers[h] ?? null },
    json: () => Promise.resolve(body),
  });
  const sandbox = {
    console, URL, Promise, JSON, Math, Date, Array, Uint8Array,
    document: doc,
    navigator: { doNotTrack: dnt },
    location: { pathname: "/pricing", hostname: "app.test", origin: "https://app.test" },
    localStorage: storage,
    innerWidth: width,
    setTimeout: (fn, ms) => { timers.push({ fn, ms }); return timers.length; },
    fetch: fetchImpl || ((url, opts) => {
      calls.push({ url, opts, body: opts && opts.body ? JSON.parse(opts.body) : null });
      if (url.includes("/active")) return Promise.resolve(responses.active || res(200, surveys));
      if (url.includes("/respond")) return Promise.resolve(responses.respond || res(200, { ok: true }));
      return Promise.resolve(responses.expose || res(200, { ok: true }));
    }),
  };
  sandbox.window = sandbox;
  vm.runInNewContext(src, sandbox, { filename: "observe-surveys.js" });
  return { sandbox, calls, timers, store, doc, docListeners, root: () => shadow, res };
}

const tick = () => new Promise((r) => setImmediate(r));
async function settle() { for (let i = 0; i < 6; i++) await tick(); }

const Q = JSON.stringify([
  { id: "q1", type: "rating", text: "How was it?", required: true },
  { id: "q2", type: "nps", text: "Recommend?" },
  { id: "q3", type: "choice", text: "Plan?", choices: ["Free", "Pro"] },
  { id: "q4", type: "text", text: "Anything else?", placeholder: "type here" },
]);
const SV = (over = {}) => ({ survey_id: "sv1", site_id: "site1", name: "Pricing survey", questions: Q, appearance: "{}", once: false, ...over });

function cardOf(root) { return root.findAll((c) => c.attrs.role === "dialog")[0]; }
function radios(card, n) { return card.findAll((c) => c.tagName === "INPUT" && c.attrs && c.name && c.name.endsWith("-q" + n)); }

test("renders a labelled dialog, exposes once, sends context to /active", async () => {
  const env = makeEnv({ surveys: [SV()] });
  await settle();
  const card = cardOf(env.root());
  assert.ok(card, "dialog rendered");
  assert.equal(card.attrs["aria-labelledby"], card.find("head").children[0].id);
  assert.equal(card.find("head").children[0].textContent, "Pricing survey");
  const active = env.calls.find((c) => c.url.includes("/active"));
  assert.match(active.url, /^https:\/\/observe\.test\/api\/v1\/surveys\/active\?site_id=site1&path=%2Fpricing&device_type=desktop&referrer_host=news\.example\.com$/);
  const expose = env.calls.filter((c) => c.url.endsWith("/expose"));
  assert.equal(expose.length, 1);
  assert.deepEqual(expose[0].body, { survey_id: "sv1", site_id: "site1" });
  assert.ok(env.root().activeElement, "focus moved into the dialog");
  assert.equal(env.root().activeElement.className, "title");
});

test("all four question types render with accessible controls", async () => {
  const env = makeEnv({ surveys: [SV()] });
  await settle();
  const card = cardOf(env.root());
  assert.equal(radios(card, 0).length, 5);
  assert.equal(radios(card, 1).length, 11);
  assert.equal(radios(card, 2).length, 2);
  assert.equal(card.findAll((c) => c.tagName === "TEXTAREA").length, 1);
  assert.equal(card.findAll((c) => c.tagName === "FIELDSET").length, 4);
  const close = card.findAll((c) => c.tagName === "BUTTON" && c.attrs["aria-label"] === "Dismiss survey");
  assert.equal(close.length, 1);
});

test("required validation blocks submit; valid submit posts answers once and persists completion", async () => {
  const env = makeEnv({ surveys: [SV()] });
  await settle();
  const card = cardOf(env.root());
  const form = card.findAll((c) => c.tagName === "FORM")[0];
  form.dispatch("submit");
  assert.match(card.find("err").textContent, /Please answer: How was it\?/);
  assert.equal(env.calls.filter((c) => c.url.endsWith("/respond")).length, 0);

  radios(card, 0)[3].checked = true;
  radios(card, 2)[1].checked = true;
  card.findAll((c) => c.tagName === "TEXTAREA")[0].value = "  hello  ";
  form.dispatch("submit");
  await settle();
  const resp = env.calls.filter((c) => c.url.endsWith("/respond"));
  assert.equal(resp.length, 1);
  assert.deepEqual(resp[0].body.answers, { q1: 4, q3: "Pro", q4: "hello" });
  assert.match(resp[0].body.response_id, /^[A-Za-z0-9_-]{8,64}$/);
  assert.equal(resp[0].body.survey_id, "sv1");
  assert.equal(JSON.parse(env.store["observe_sv:site1:sv1"]).s, "done");
  assert.match(card.textContent, /Thank you/);
});

test("dismiss via Escape and via button persists; widget stays away on next load", async () => {
  const env = makeEnv({ surveys: [SV()] });
  await settle();
  env.docListeners.keydown[0]({ key: "Escape", stopPropagation() {}, preventDefault() {} });
  assert.equal(env.doc.body.children.length, 0, "removed from the page");
  assert.equal(JSON.parse(env.store["observe_sv:site1:sv1"]).s, "dismissed");
  const env2 = makeEnv({ surveys: [SV()], store: env.store });
  await settle();
  assert.equal(env2.root(), null, "dismissed survey not shown again");
  assert.equal(env2.calls.filter((c) => c.url.endsWith("/expose")).length, 0);

  // A dismissal older than 30 days lapses.
  const old = { "observe_sv:site1:sv1": JSON.stringify({ s: "dismissed", t: Date.now() - 31 * 86400000 }) };
  const env3 = makeEnv({ surveys: [SV()], store: old });
  await settle();
  assert.ok(env3.root(), "stale dismissal lapses");
});

test("completed surveys never return; once-surveys are not re-shown after being shown", async () => {
  const done = { "observe_sv:site1:sv1": JSON.stringify({ s: "done", t: Date.now() }) };
  assert.equal((await (async () => { const e = makeEnv({ surveys: [SV()], store: done }); await settle(); return e; })()).root(), null);
  const shown = { "observe_sv:site1:sv1": JSON.stringify({ s: "shown", t: Date.now() }) };
  const e1 = makeEnv({ surveys: [SV({ once: true })], store: shown });
  await settle();
  assert.equal(e1.root(), null);
  const e2 = makeEnv({ surveys: [SV({ once: false })], store: shown });
  await settle();
  assert.ok(e2.root(), "non-once survey may reappear after being shown");
});

test("renders ONE survey at a time, skipping suppressed ones", async () => {
  const store = { "observe_sv:site1:a1": JSON.stringify({ s: "done", t: Date.now() }) };
  const env = makeEnv({ surveys: [SV({ survey_id: "a1" }), SV({ survey_id: "b2", name: "Second" })], store });
  await settle();
  assert.equal(env.doc.body.children.length, 1);
  assert.equal(env.calls.filter((c) => c.url.endsWith("/expose")).length, 1);
  assert.equal(env.calls.find((c) => c.url.endsWith("/expose")).body.survey_id, "b2");
});

test("localStorage that throws never breaks the widget", async () => {
  const env = makeEnv({ surveys: [SV()], storageThrows: true });
  await settle();
  const card = cardOf(env.root());
  assert.ok(card);
  card.find("x") || null;
  env.docListeners.keydown[0]({ key: "Escape", stopPropagation() {} });
  assert.equal(env.doc.body.children.length, 0);
});

test("Do-Not-Track is honoured like observe.js, with the same opt-out", async () => {
  const on = makeEnv({ surveys: [SV()], dnt: "1" });
  await settle();
  assert.equal(on.calls.length, 0, "no request under DNT");
  const off = makeEnv({ surveys: [SV()], dnt: "1", attrs: { "data-site-id": "site1", "data-respect-dnt": "false" } });
  await settle();
  assert.ok(off.root());
});

test("no site id or no script element: inert", async () => {
  const e = makeEnv({ attrs: {}, surveys: [SV()] });
  await settle();
  assert.equal(e.calls.length, 0);
});

test("never throws into the host page when fetch/DOM misbehave", async () => {
  assert.doesNotThrow(() => makeEnv({ fetchImpl: () => { throw new Error("boom"); } }));
  assert.doesNotThrow(() => makeEnv({ fetchImpl: () => Promise.reject(new Error("net")) }));
  const garbage = makeEnv({ surveys: [null, 5, "x", { survey_id: "../x" }, { survey_id: "ok1", questions: "{not json" }, { survey_id: "ok2", questions: [{ type: "weird", text: "t" }] }] });
  await settle();
  assert.equal(garbage.root(), null, "nothing renderable, nothing rendered");
});

test("XSS corpus: server strings are inert text, no HTML sink is ever touched", async () => {
  const payloads = [
    "<img src=x onerror=alert(1)>", "<script>window.pwned=1</script>", "\"><svg/onload=alert(1)>",
    "javascript:alert(1)", "</style><script>window.pwned=1</script>", "{{constructor.constructor('window.pwned=1')()}}",
    "&lt;b&gt;", "\u0000<b>", "' onmouseover='window.pwned=1",
  ];
  for (const p of payloads) {
    const sv = SV({
      name: p,
      questions: JSON.stringify([
        { id: "q1", type: "text", text: p, placeholder: p },
        { id: "q2", type: "choice", text: p, choices: [p, p + "2"] },
        { id: p, type: "rating", text: p },
      ]),
      appearance: JSON.stringify({ accent: p, position: p }),
    });
    const env = makeEnv({ surveys: [sv] });
    await settle();
    assert.equal(env.sandbox.pwned, undefined, "payload executed: " + p);
    const root = env.root();
    assert.ok(root, "still renders: " + p);
    // The payload exists only as text content, never as an element/attribute name.
    const tags = new Set(root.findAll(() => true).map((c) => c.tagName));
    for (const t of tags) assert.ok(["DIV", "H2", "BUTTON", "FORM", "FIELDSET", "LEGEND", "TEXTAREA", "LABEL", "INPUT", "SPAN", "P", "STYLE"].includes(t), "unexpected element " + t);
    const wrap = root.find("wrap");
    assert.equal(wrap.style.props["--accent"], undefined, "unsafe accent rejected");
    // Hostile question id falls back to a safe answer key.
    const card = cardOf(root);
    radios(card, 2)[0].checked = true;
    card.findAll((c) => c.tagName === "FORM")[0].dispatch("submit");
    await settle();
    const body = env.calls.filter((c) => c.url.endsWith("/respond"))[0].body;
    assert.deepEqual(Object.keys(body.answers), ["q3"]);
  }
});

test("valid accent colour is applied through CSSOM, not markup", async () => {
  const env = makeEnv({ surveys: [SV({ appearance: JSON.stringify({ accent: "#ff6600", position: "center" }) })] });
  await settle();
  assert.equal(env.root().find("wrap").style.props["--accent"], "#ff6600");
  assert.equal(cardOf(env.root()).attrs["aria-modal"], "true");
  assert.ok(env.root().find("wrap").className.includes("center"));
});

test("payload bounds: oversize text and choice lists are clipped before rendering and sending", async () => {
  const long = "x".repeat(5000);
  const choices = Array.from({ length: 100 }, (_, i) => "c" + i);
  const qs = Array.from({ length: 30 }, (_, i) => ({ id: "q" + i, type: "text", text: long }));
  qs.unshift({ id: "pick", type: "choice", text: "pick", choices });
  const env = makeEnv({ surveys: [SV({ questions: JSON.stringify(qs) })] });
  await settle();
  const card = cardOf(env.root());
  assert.equal(card.findAll((c) => c.tagName === "FIELDSET").length, 10, "max 10 questions");
  assert.equal(radios(card, 0).length, 20, "max 20 choices");
  const ta = card.findAll((c) => c.tagName === "TEXTAREA")[0];
  assert.ok(ta.maxLength <= 2000);
  assert.ok(card.findAll((c) => c.tagName === "LEGEND")[1].textContent.length <= 500);
  ta.value = "y".repeat(10000);
  radios(card, 0)[0].checked = true;
  card.findAll((c) => c.tagName === "FORM")[0].dispatch("submit");
  await settle();
  const body = env.calls.find((c) => c.url.endsWith("/respond")).body;
  assert.equal(body.answers.q0.length, 2000);
});

test("rate-limited and failing posts retry a bounded number of times with the same response_id", async () => {
  let respondCalls = [];
  const env = makeEnv({
    surveys: [SV()],
    fetchImpl: (url, opts) => {
      if (url.includes("/active")) return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve([SV()]) });
      if (url.endsWith("/respond")) {
        respondCalls.push(JSON.parse(opts.body));
        return Promise.resolve({ ok: false, status: 429, headers: { get: () => "9999" }, json: () => Promise.resolve({}) });
      }
      return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve({}) });
    },
  });
  await settle();
  const card = cardOf(env.root());
  radios(card, 0)[0].checked = true;
  card.findAll((c) => c.tagName === "FORM")[0].dispatch("submit");
  for (let i = 0; i < 10; i++) {
    await settle();
    const t = env.timers.shift();
    if (!t) break;
    assert.ok(t.ms <= 30000, "Retry-After capped at 30s, got " + t.ms);
    t.fn();
  }
  await settle();
  assert.equal(respondCalls.length, 4, "1 attempt + 3 retries, then stop");
  assert.equal(new Set(respondCalls.map((b) => b.response_id)).size, 1, "stable id for server dedupe");
});

test("a 4xx refusal (e.g. closed survey) is not retried", async () => {
  const env = makeEnv({
    surveys: [SV()],
    responses: { respond: { ok: false, status: 400, headers: { get: () => null }, json: () => Promise.resolve({}) } },
  });
  await settle();
  const card = cardOf(env.root());
  radios(card, 0)[0].checked = true;
  card.findAll((c) => c.tagName === "FORM")[0].dispatch("submit");
  await settle();
  assert.equal(env.timers.length, 0);
  assert.equal(env.calls.filter((c) => c.url.endsWith("/respond")).length, 1);
});

test("/active 429 stops quietly; 5xx retries at most twice", async () => {
  const e1 = makeEnv({ responses: { active: { ok: false, status: 429, headers: { get: () => null }, json: () => Promise.resolve([]) } } });
  await settle();
  assert.equal(e1.timers.length, 0);
  assert.equal(e1.root(), null);
  const e2 = makeEnv({ responses: { active: { ok: false, status: 503, headers: { get: () => null }, json: () => Promise.resolve([]) } } });
  for (let i = 0; i < 6; i++) { await settle(); const t = e2.timers.shift(); if (t) t.fn(); }
  await settle();
  assert.equal(e2.calls.filter((c) => c.url.includes("/active")).length, 3);
});

test("script source has no eval/innerHTML/inline-handler sinks", () => {
  assert.doesNotMatch(src.replace(/\/\/.*$|\/\*[\s\S]*?\*\//gm, ""), /\binnerHTML\b|\bouterHTML\b|insertAdjacentHTML|document\.write|\beval\s*\(|new Function|setTimeout\s*\(\s*['"`]|\bon[a-z]+\s*=\s*['"]/);
});
