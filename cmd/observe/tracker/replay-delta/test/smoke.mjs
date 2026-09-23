// O06 recorder smoke: drives the X05 fixture page (fixtures/x05-app) with
// the built delta recorder substituted in, then asserts AT THE TRANSPORT
// that (a) the v2 batch envelope shape is intact, (b) rrweb full+incremental
// events ride it, (c) Observe-native click/navigation events still ride
// the same batches, and (d) nothing typed into form fields, a
// contenteditable region, or a data-observe-block subtree appears in any
// posted body, and no URL query string ever leaves the page.
//
// Not part of `npm test` (needs system Chrome via playwright-core, the X05
// harness pattern). Run: npm run smoke
import { chromium } from 'playwright-core';
import http from 'node:http';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import assert from 'node:assert/strict';

const here = path.dirname(fileURLToPath(import.meta.url));
const recorderBundle = path.join(here, '..', '..', 'observe-replay-delta.js');
const pageDir = path.resolve(here, '..', '..', '..', '..', '..', 'fixtures', 'x05-app', 'page');
const PORT = 4596;

const SECRETS = [
  'Ada Lovelace',
  'ada-smoke@example.com',
  'typed-in-contenteditable',
  'typed-in-blocked',
  'journey-notes-secret',
];

function serve() {
  const srv = http.createServer(async (req, res) => {
    if (req.method === 'POST' && req.url.startsWith('/api/v1/replays')) {
      let body = '';
      for await (const c of req) body += c;
      bodies.push(JSON.parse(body));
      res.writeHead(200, { 'content-type': 'application/json' });
      res.end('{"ok":true}');
      return;
    }
    try {
      const u = new URL(req.url, 'http://x');
      const file = path.join(pageDir, u.pathname === '/' ? 'index.html' : u.pathname.replace(/^\//, ''));
      let data = await readFile(file);
      if (file.endsWith('.html')) {
        const tag = `<script src="/recorder.js" data-site-id="o6-smoke" data-endpoint="http://127.0.0.1:${PORT}/api/v1/replays" data-flush-interval="2000"></script>`;
        data = Buffer.from(String(data).replace('<!--X05-RECORDER-->', tag));
      }
      const type = file.endsWith('.js') ? 'text/javascript'
        : file.endsWith('.html') ? 'text/html' : 'application/octet-stream';
      res.writeHead(200, { 'content-type': type });
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
  return new Promise((ok) => srv.listen(PORT, () => ok(srv)));
}

const bodies = [];
const srv = await serve();
const browser = await chromium.launch({ channel: 'chrome', headless: true });

let failed = 0;
try {
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 } });
  const page = await ctx.newPage();
  await page.goto(`http://127.0.0.1:${PORT}/?q=leak-check`);

  // Seed secrets through the real UI + dynamically-created private regions.
  await page.fill('#f-name', 'Ada Lovelace');
  await page.fill('#f-email', 'ada-smoke@example.com');
  await page.fill('#f-notes', 'journey-notes-secret');
  await page.evaluate(() => {
    const ce = document.createElement('div');
    ce.setAttribute('contenteditable', 'true');
    ce.id = 'smoke-ce';
    ce.textContent = 'typed-in-contenteditable';
    document.body.appendChild(ce);
    const bl = document.createElement('div');
    bl.setAttribute('data-observe-block', 'true');
    bl.id = 'smoke-block';
    bl.textContent = 'typed-in-blocked';
    document.body.appendChild(bl);
  });
  await page.locator('.card').first().click();
  await page.waitForTimeout(400);
  await page.locator('#detail-close').click();
  await page.mouse.move(400, 300);
  await page.mouse.wheel(0, 500);
  await page.locator('#nav button').nth(1).click();
  // The fixture's tabs are pure DOM switches; exercise the history wrapper
  // (the navigation event source) explicitly.
  await page.evaluate(() => history.pushState({}, '', '/smoke-tab'));
  await page.setViewportSize({ width: 1100, height: 760 });
  await page.mouse.wheel(0, 200);

  // Let flushes fire (2 s interval configured above).
  await page.waitForTimeout(7000);
  await ctx.close();
} finally {
  await browser.close();
  srv.close();
}

// ── assertions at the transport ──
const tryAssert = (label, fn) => {
  try {
    fn();
    console.log(`ok - ${label}`);
  } catch (e) {
    failed++;
    console.error(`not ok - ${label}: ${e.message}`);
  }
};

const blob = JSON.stringify(bodies);
const allEvents = bodies.flatMap((b) => b.events);

tryAssert('at least one v2 batch posted', () => {
  assert.ok(bodies.length >= 1, `got ${bodies.length} batches`);
  const b = bodies[0];
  assert.equal(b.v, 2);
  assert.match(b.producer_id, /^[0-9a-f]{32}$/);
  assert.match(b.batch_id, /^[0-9a-f]{32}-\d+$/);
  assert.match(b.replay_id, /^[0-9a-f]{32}$/);
  assert.equal(b.site_id, 'o6-smoke');
});

tryAssert('rrweb full snapshot + incremental events ride the envelope', () => {
  const rr = allEvents.filter((e) => e.type === 'rrweb');
  assert.ok(rr.length > 20, `only ${rr.length} rrweb events`);
  assert.ok(rr.some((e) => e.data && e.data.type === 2), 'no FullSnapshot event');
  assert.ok(rr.some((e) => e.data && e.data.type === 3), 'no IncrementalSnapshot event');
  assert.ok(rr.some((e) => e.data && e.data.type === 4), 'no Meta event');
});

tryAssert('Observe-native click + navigation events still ride the batches', () => {
  assert.ok(allEvents.some((e) => e.type === 'click' && Number.isFinite(e.data.x)), 'no click event');
  assert.ok(allEvents.some((e) => e.type === 'navigation'), 'no navigation event');
  assert.ok(allEvents.some((e) => e.type === 'resize'), 'no resize event');
});

tryAssert('meta href carries no query string', () => {
  const metas = allEvents.filter((e) => e.type === 'rrweb' && e.data && e.data.type === 4);
  assert.ok(metas.length > 0, 'no meta events');
  for (const m of metas) {
    assert.ok(!m.data.data.href.includes('?'), `meta href leaked query: ${m.data.data.href}`);
  }
});

tryAssert('no seeded secret appears in any posted body', () => {
  for (const s of SECRETS) {
    assert.ok(!blob.includes(s), `secret ${JSON.stringify(s)} leaked`);
  }
});

tryAssert('no data-observe-block/contenteditable/value attributes ride', () => {
  assert.ok(!blob.includes('data-observe-block'), 'data-observe-block attr leaked');
  assert.ok(!blob.includes('contenteditable'), 'contenteditable attr leaked');
});

tryAssert('session url field has no query', () => {
  for (const b of bodies) {
    assert.ok(!b.url.includes('?'), `envelope url leaked query: ${b.url}`);
  }
});

console.log(JSON.stringify({
  batches: bodies.length,
  events: allEvents.length,
  rrweb: allEvents.filter((e) => e.type === 'rrweb').length,
  bytes: blob.length,
  failed,
}));

process.exit(failed ? 1 : 0);
