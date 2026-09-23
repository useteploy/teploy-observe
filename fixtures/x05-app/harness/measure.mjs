// X05 measurement harness (O06 rrweb experiment; DELEGATED_DECISIONS
// 2026-09-23 section 8.2). Drives the fixture page through a scripted
// journey with playwright-core against system Chrome (channel:'chrome' - no
// browser download), serving the page and fulfilling the ingest endpoint so
// session bytes are counted at the transport, not guessed from buffers.
//
// Phases:
//   lcp     - N page loads per arm; LCP, FCP, long-task time/count.
//   session - one full 5-minute journey per arm; bytes POSTed, event counts.
//
// Output: JSON on stdout (one line per run for lcp, one summary per arm for
// session). The verdict against the pre-declared thresholds (recorder
// <= 100 kB gz effective, <= 100 ms p95 LCP delta, session <= 2 MiB / 5 min
// sampled) is computed by the operator/CI reading these numbers - the
// harness measures, it does not grade itself.
import { chromium } from 'playwright-core';
import http from 'node:http';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const root = path.resolve(here, '..');
const pageDir = path.join(root, 'page');
const PORT = 4590;

const args = new Set(process.argv.slice(2));
const phase = args.has('--phase=session') ? 'session' : 'lcp';
const lcpRuns = 20;

function serve() {
  // Tiny static server that fulfills the ingest endpoint with 200 and
  // counts POST body bytes per arm (site header carries the arm tag).
  const counts = { a: { posts: 0, bytes: 0 }, b: { posts: 0, bytes: 0 } };
  const srv = http.createServer(async (req, res) => {
    if (req.method === 'POST' && req.url.startsWith('/api/v1/replays')) {
      const arm = new URL(req.url, 'http://x').searchParams.get('arm') || 'a';
      let body = '';
      for await (const c of req) body += c;
      if (counts[arm]) { counts[arm].posts++; counts[arm].bytes += body.length; }
      res.writeHead(200, { 'content-type': 'application/json' });
      res.end('{"ok":true}');
      return;
    }
    try {
      const u = new URL(req.url, 'http://x');
      const arm = u.searchParams.get('arm') === 'b' ? 'b' : 'a';
      // Both arm scripts live outside page/ - served explicitly so the
      // recorder tag can be substituted into the HTML BEFORE load (a
      // post-load injection would make every LCP comparison vacuous).
      if (u.pathname.startsWith('/arm-a/') || u.pathname.startsWith('/arm-b/')) {
        const from = u.pathname.startsWith('/arm-a/')
          ? path.join(root, '..', '..', 'cmd', 'observe', 'tracker', 'observe-replay.js')
          : path.join(root, u.pathname);
        const data = await readFile(from);
        res.writeHead(200, { 'content-type': 'text/javascript' });
        res.end(data);
        return;
      }
      const rel = u.pathname === '/' ? '/index.html' : u.pathname;
      const file = path.join(pageDir, rel.replace(/^\//, ''));
      let data = await readFile(file);
      if (file.endsWith('.html')) {
        const tag = `<script src="/arm-${arm}/recorder.js" data-site-id="x05-${arm}" data-endpoint="http://127.0.0.1:${PORT}/api/v1/replays?arm=${arm}"></script>`;
        data = Buffer.from(String(data).replace('<!--X05-RECORDER-->', tag));
      }
      const type = file.endsWith('.js')
        ? 'text/javascript'
        : file.endsWith('.html') ? 'text/html' : 'application/octet-stream';
      res.writeHead(200, { 'content-type': type });
      res.end(data);
    } catch {
      res.writeHead(404); res.end('not found');
    }
  });
  return new Promise((ok) => srv.listen(PORT, () => ok({ srv, counts })));
}

async function runArm(browser, arm, { journeyMs }) {
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 } });
  const page = await ctx.newPage();
  const script = arm === 'a'
    ? `/../cmd/observe/tracker/observe-replay.js` // resolved via route below
    : `/arm-b/recorder.js`;
  const metrics = await page.goto(`http://127.0.0.1:${PORT}/?arm=${arm}`);

  if (phase === 'lcp') {
    const out = await page.evaluate(
      () =>
        new Promise((resolve) => {
          const po = new PerformanceObserver(() => {});
          let lcp = 0, fcp = 0, longTasks = 0, longMs = 0;
          try {
            new PerformanceObserver((l) => {
              const es = l.getEntries();
              if (es.length) lcp = es[es.length - 1].startTime;
            }).observe({ type: 'largest-contentful-paint', buffered: true });
            new PerformanceObserver((l) => {
              const es = l.getEntries();
              if (es.length) fcp = es[es.length - 1].startTime;
            }).observe({ type: 'paint', buffered: true });
            new PerformanceObserver((l) => {
              for (const e of l.getEntries()) { longTasks++; longMs += e.duration; }
            }).observe({ type: 'longtask', buffered: true });
          } catch {}
          po.disconnect();
          setTimeout(() => resolve({ lcp, fcp, longTasks, longMs }), 8000);
        })
    );
    await ctx.close();
    return out;
  }

  // Session phase: scripted journey, ~7.5 s per block.
  const blockMs = 7500;
  const blocks = Math.max(1, Math.round(journeyMs / blockMs));
  const t0 = Date.now();
  for (let i = 0; i < blocks; i++) {
    await page.mouse.move(200 + Math.random() * 800, 150 + Math.random() * 500);
    for (let m = 0; m < 10; m++) {
      await page.mouse.move(200 + Math.random() * 800, 150 + Math.random() * 500, { steps: 2 });
      await page.waitForTimeout(120);
    }
    await page.mouse.wheel(0, 600 + Math.random() * 900);
    await page.waitForTimeout(400);
    await page.mouse.wheel(0, -(300 + Math.random() * 500));
    await page.waitForTimeout(300);
    const card = page.locator('.card').nth(i % 12);
    await card.click();
    await page.waitForTimeout(600);
    await page.locator('#detail-close').click();
    await page.waitForTimeout(200);
    if (i % 2 === 0) {
      await page.fill('#f-name', 'Ada Lovelace');
      await page.fill('#f-email', `ada${i}@example.com`);
      await page.selectOption('#f-channel', ['Webhook']);
      await page.fill('#f-notes', `journey block ${i} notes`);
    } else {
      await page.locator('#nav button').nth(1 + (i % 3)).click();
    }
    await page.waitForTimeout(Math.max(0, blockMs - (Date.now() - t0) % blockMs - 500));
  }
  // Let the final flush interval fire.
  await page.waitForTimeout(11000);
  const stats = await page.evaluate(() => ({
    armB: window.__x05armB ? window.__x05armB.stats() : null,
  }));
  await ctx.close();
  return stats;
}

const { srv, counts } = await serve();
const browser = await chromium.launch({ channel: 'chrome', headless: true });

try {
  if (phase === 'lcp') {
    for (const arm of (process.env.ARMS||'a,b').split(',')) {
      for (let i = 0; i < lcpRuns; i++) {
        const m = await runArm(browser, arm, {});
        console.log(JSON.stringify({ phase, arm, run: i + 1, ...m }));
      }
    }
  } else {
    const journeyMs = 5 * 60 * 1000;
    for (const arm of ['a', 'b']) {
      await runArm(browser, arm, { journeyMs });
      await new Promise((r) => setTimeout(r, 1500));
      console.log(JSON.stringify({
        phase, arm, journeyMs,
        transport: counts[arm],
        armBInPage: null,
      }));
    }
  }
} finally {
  await browser.close();
  srv.close();
}
