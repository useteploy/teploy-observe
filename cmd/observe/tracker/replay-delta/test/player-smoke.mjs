// O06 player smoke: takes REAL events recorded by the delta recorder in
// a system-Chrome session, then replays them through the ACTUAL shipped
// player bundles (ui/public/rrweb/replayer.js + sanitize.js) in a fresh
// page, asserting:
//   - the rrweb Replayer mounts its wrapper + iframe,
//   - the iframe carries sandbox="allow-same-origin" and NO allow-scripts,
//   - the rebuilt document gets the CSP meta injected (same rule the
//     component applies on fullsnapshot-rebuilded),
//   - recorded images load exclusively through /api/v1/replay-assets,
//   - the rebuilt DOM shows the recorded page text and none of the seeded
//     secrets.
//
// The harness page mirrors the component's wiring (sanitize -> rewrite ->
// Replayer -> CSP inject) because the component itself is TSX; the bundles
// under test are the exact bytes the dashboard serves.
//
// Run: npm run smoke:player
import { chromium } from 'playwright-core';
import http from 'node:http';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import assert from 'node:assert/strict';

const here = path.dirname(fileURLToPath(import.meta.url));
const root = path.resolve(here, '..', '..', '..', '..', '..');
const recorderBundle = path.join(root, 'cmd', 'observe', 'tracker', 'observe-replay-delta.js');
const bundlesDir = path.join(root, 'ui', 'public', 'rrweb');
const pageDir = path.join(root, 'fixtures', 'x05-app', 'page');
const REC_PORT = 4601;
const PLAY_PORT = 4602;

const SECRETS = ['Ada Lovelace', 'ada-player@example.com', 'typed-in-contenteditable', 'typed-in-blocked'];

// ── phase 1: record a session with the real recorder bundle ──
function recordServer(events) {
  const srv = http.createServer(async (req, res) => {
    if (req.method === 'POST' && req.url.startsWith('/api/v1/replays')) {
      let body = '';
      for await (const c of req) body += c;
      for (const e of JSON.parse(body).events) events.push(e);
      res.writeHead(200);
      res.end('{"ok":true}');
      return;
    }
    try {
      const u = new URL(req.url, 'http://x');
      const file = path.join(pageDir, u.pathname === '/' ? 'index.html' : u.pathname.replace(/^\//, ''));
      let data = await readFile(file);
      if (file.endsWith('.html')) {
        const tag = `<script src="/recorder.js" data-site-id="o6-player-smoke" data-endpoint="http://127.0.0.1:${REC_PORT}/api/v1/replays" data-flush-interval="2000"></script>`;
        data = Buffer.from(String(data).replace('<!--X05-RECORDER-->', tag));
      }
      res.writeHead(200, { 'content-type': file.endsWith('.html') ? 'text/html' : 'text/javascript' });
      res.end(data);
      return;
    } catch { /* fall through */ }
    if (req.url === '/recorder.js') {
      res.writeHead(200, { 'content-type': 'text/javascript' });
      res.end(await readFile(recorderBundle));
      return;
    }
    res.writeHead(404);
    res.end('not found');
  });
  return new Promise((ok) => srv.listen(REC_PORT, () => ok(srv)));
}

const recorded = [];
const recSrv = await recordServer(recorded);
const browser = await chromium.launch({ channel: 'chrome', headless: true });
const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 } });
const page = await ctx.newPage();
await page.goto(`http://127.0.0.1:${REC_PORT}/`);
await page.fill('#f-name', 'Ada Lovelace');
await page.fill('#f-email', 'ada-player@example.com');
await page.evaluate(() => {
  const img = document.createElement('img');
  img.src = 'https://cdn.example.com/img/product.png';
  img.alt = 'product';
  img.width = 40;
  document.body.appendChild(img);
  const ce = document.createElement('div');
  ce.setAttribute('contenteditable', 'true');
  ce.textContent = 'typed-in-contenteditable';
  document.body.appendChild(ce);
  const bl = document.createElement('div');
  bl.setAttribute('data-observe-block', 'true');
  bl.textContent = 'typed-in-blocked';
  document.body.appendChild(bl);
});
await page.locator('.card').first().click();
await page.waitForTimeout(500);
await page.locator('#detail-close').click();
await page.mouse.move(500, 350);
await page.mouse.wheel(0, 400);
await page.waitForTimeout(6000);
await ctx.close();
recSrv.close();

const rrwebEvents = recorded
  .filter((e) => e.type === 'rrweb' && e.data && typeof e.data.type === 'number')
  .map((e) => e.data);

// ── phase 2: replay through the shipped bundles ──
const playerHTML = `<!DOCTYPE html><html><head><style>
  body { margin: 0; }
  #stage { position: relative; width: 1000px; height: 620px; }
  #stage .replayer-wrapper { position: absolute; inset: 0; }
  #stage .replayer-wrapper iframe { width: 100%; height: 100%; border: 0; }
</style></head><body>
<div id="stage"></div>
<script src="/replayer.js"></script>
<script src="/sanitize.js"></script>
<script>
window.__smoke = { status: 'pending', error: null };
window.addEventListener('load', function () {
  try {
    var rt = window.__observeReplayRuntime;
    var REPLAY_CSP = "default-src 'none'; script-src 'none'; connect-src 'none'; img-src 'self'; " +
      "style-src 'none'; media-src 'none'; frame-src 'none'; object-src 'none'; " +
      "base-uri 'none'; form-action 'none'";
    var sanitizer = rt.createEventSanitizer({ baseURL: 'https://shop.example.com/' });
    var events = [];
    for (var i = 0; i < window.__rrwebEvents.length; i++) {
      var s = sanitizer.sanitize(window.__rrwebEvents[i]);
      if (s) events.push(s);
    }
    // rewrite img srcs to the asset proxy (component rule; dummy ticket -
    // the real player mints a stream ticket, this harness only checks the
    // URL shape)
    (function rewrite(events) {
      function walk(node) {
        if (!node || typeof node !== 'object') return;
        if (node.type === 2) {
          var tag = String(node.tagName || '').toLowerCase();
          if (tag === 'img' && node.attributes && typeof node.attributes.src === 'string') {
            node.attributes.src = '/api/v1/replay-assets?u=' + encodeURIComponent(node.attributes.src) + '&ticket=smoke';
          }
        }
        if (Array.isArray(node.childNodes)) node.childNodes.forEach(walk);
      }
      events.forEach(function (ev) {
        if (ev.type === 2 && ev.data && ev.data.node) walk(ev.data.node);
        if (ev.type === 3 && ev.data && ev.data.source === 0 && Array.isArray(ev.data.adds)) {
          ev.data.adds.forEach(function (a) { walk(a && a.node); });
        }
      });
    })(events);
    var replayer = new rt.Replayer(events, {
      root: document.getElementById('stage'),
      triggerFocus: false,
      mouseTail: false,
      showWarning: false,
      insertStyleRules: [],
    });
    function injectCSP() {
      var doc = replayer.iframe && replayer.iframe.contentDocument;
      if (!doc || !doc.documentElement) return;
      if (doc.querySelector('meta[http-equiv="Content-Security-Policy"]')) return;
      var head = doc.head;
      if (!head) {
        head = doc.createElement('head');
        doc.documentElement.insertBefore(head, doc.documentElement.firstChild);
      }
      var meta = doc.createElement('meta');
      meta.setAttribute('http-equiv', 'Content-Security-Policy');
      meta.setAttribute('content', REPLAY_CSP);
      head.insertBefore(meta, head.firstChild);
    }
    replayer.on('fullsnapshot-rebuilded', injectCSP);
    replayer.on('finish', function () { window.__smoke.status = 'finished'; });
    window.__smoke.replayer = replayer;
    window.__smoke.meta = replayer.getMetaData();
    window.__smoke.eventCount = events.length;
    replayer.play(0);
    window.__smoke.status = 'playing';
  } catch (e) {
    window.__smoke.status = 'error';
    window.__smoke.error = String(e && e.message || e);
  }
});
</script></body></html>`;

const playSrv = http.createServer(async (req, res) => {
  if (req.url === '/') {
    res.writeHead(200, { 'content-type': 'text/html' });
    res.end(playerHTML.replace('__RRWEB_EVENTS_JSON__', ''));
    return;
  }
  try {
    const file = path.join(bundlesDir, req.url.replace(/^\//, ''));
    const data = await readFile(file);
    res.writeHead(200, { 'content-type': 'text/javascript' });
    res.end(data);
    return;
  } catch { /* fall through */ }
  if (req.url.startsWith('/api/v1/replay-assets')) {
    res.writeHead(200, { 'content-type': 'image/svg+xml' });
    res.end('<svg xmlns="http://www.w3.org/2000/svg" width="1" height="1"/>');
    return;
  }
  res.writeHead(404);
  res.end('not found');
});
await new Promise((ok) => playSrv.listen(PLAY_PORT, ok));

const ctx2 = await browser.newContext({ viewport: { width: 1200, height: 800 } });
const ppage = await ctx2.newPage();
ppage.on('pageerror', (e) => console.log('PLAYER PAGEERROR:', String(e).slice(0, 300)));
await ppage.addInitScript((evts) => { window.__rrwebEvents = evts; }, rrwebEvents);
await ppage.goto(`http://127.0.0.1:${PLAY_PORT}/`);
await ppage.waitForTimeout(4000);

let failed = 0;
const tryAssert = (label, fn) => {
  try {
    fn();
    console.log(`ok - ${label}`);
  } catch (e) {
    failed++;
    console.error(`not ok - ${label}: ${e.message}`);
  }
};

const smoke = await ppage.evaluate(() => window.__smoke);
const stageInfo = await ppage.evaluate(() => {
  const stage = document.getElementById('stage');
  const iframe = stage.querySelector('iframe');
  const doc = iframe && iframe.contentDocument;
  return {
    hasWrapper: !!stage.querySelector('.replayer-wrapper'),
    hasMouse: !!stage.querySelector('.replayer-mouse'),
    iframeSandbox: iframe ? iframe.getAttribute('sandbox') : null,
    csp: doc ? (doc.querySelector('meta[http-equiv="Content-Security-Policy"]') || {}).content : null,
    bodyText: doc && doc.body ? doc.body.textContent : '',
    imgSrcs: doc ? Array.from(doc.querySelectorAll('img')).map((i) => i.getAttribute('src')) : [],
    readyState: doc ? doc.readyState : null,
  };
});

tryAssert('recorder produced a replayable rrweb stream', () => {
  assert.ok(rrwebEvents.length >= 2, `only ${rrwebEvents.length} rrweb events`);
  assert.ok(rrwebEvents.some((e) => e.type === 2), 'no full snapshot');
});

tryAssert('replayer mounted without error', () => {
  assert.equal(smoke.status, 'playing', `status=${smoke.status} err=${smoke.error}`);
  assert.ok(smoke.eventCount >= 2);
  assert.ok(smoke.meta && smoke.meta.totalTime > 0, 'no duration metadata');
});

tryAssert('rrweb iframe is sandboxed allow-same-origin WITHOUT allow-scripts', () => {
  assert.equal(stageInfo.iframeSandbox, 'allow-same-origin');
});

tryAssert('rebuilt document carries the trusted CSP meta', () => {
  assert.ok(stageInfo.csp && stageInfo.csp.includes("script-src 'none'"), `csp=${stageInfo.csp}`);
});

tryAssert('rebuilt DOM shows recorded page structure and text', () => {
  assert.ok(stageInfo.bodyText.includes('Live updates'), `body text: ${stageInfo.bodyText.slice(0, 200)}`);
  assert.ok(stageInfo.bodyText.includes('Requests'), 'KPI text missing');
});

tryAssert('recorded images load only through the asset proxy', () => {
  assert.ok(stageInfo.imgSrcs.length >= 1, 'no images rebuilt');
  for (const src of stageInfo.imgSrcs) {
    assert.ok(src && src.startsWith('/api/v1/replay-assets?'), `img src not proxied: ${src}`);
  }
});

tryAssert('no seeded secret appears in the rebuilt DOM', () => {
  for (const s of SECRETS) {
    assert.ok(!stageInfo.bodyText.includes(s), `secret ${s} in replayed DOM`);
  }
});

await ctx2.close();
await browser.close();
playSrv.close();

console.log(JSON.stringify({
  recordedEvents: recorded.length,
  rrwebEvents: rrwebEvents.length,
  replayedEvents: smoke && smoke.eventCount,
  totalTimeMs: smoke && smoke.meta && Math.round(smoke.meta.totalTime),
  failed,
}));
process.exit(failed ? 1 : 0);
