// upgrade.spec.ts — exercise `observe upgrade` end-to-end.
//
// Spins up a private Nucleus + Observe stack on dedicated ports (3001 +
// 5435), pushes 100 events, runs `observe upgrade --target ...` (a copy
// of the same binary), and asserts both that the new binary serves
// /healthz at the same port and that all 100 events landed in the
// events table — the WAL queue must preserve any in-flight events
// across the swap.
//
// Skipped when the prerequisites are missing so the rest of the suite
// stays green on CI runners that lack the nucleus binary.

import { test, expect } from "@playwright/test";
import { spawn, spawnSync, ChildProcess } from "node:child_process";
import { mkdirSync, rmSync, existsSync, copyFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const __filename = fileURLToPath(import.meta.url);
const __dirname = dirname(__filename);
const ROOT = join(__dirname, "..", "..");
const NUCLEUS_BIN =
  process.env.NUCLEUS_BIN ||
  join(ROOT, "..", "..", "Neutron", "nucleus", "target", "release", "nucleus");
const OBS_PORT = 3001;
const NUC_PORT = 5435;
const TEST_DIR = "/tmp/observe-upgrade-e2e";
const DATA_DIR = join(TEST_DIR, "data");

function haveBinary(p: string): boolean {
  try {
    return existsSync(p);
  } catch {
    return false;
  }
}

const skipReason =
  !haveBinary(NUCLEUS_BIN)
    ? `nucleus binary missing at ${NUCLEUS_BIN} (set NUCLEUS_BIN to override)`
    : null;

test.describe("observe upgrade", () => {
  test.skip(skipReason !== null, skipReason ?? "");

  let nucleus: ChildProcess | null = null;
  let observe: ChildProcess | null = null;
  let observeBin = "";
  let observeNew = "";

  test.beforeAll(async () => {
    // Build observe.
    rmSync(TEST_DIR, { recursive: true, force: true });
    mkdirSync(DATA_DIR, { recursive: true });
    observeBin = join(TEST_DIR, "observe");
    observeNew = join(TEST_DIR, "observe-new");
    const build = spawnSync(
      "go",
      ["build", "-o", observeBin, "./cmd/observe"],
      { cwd: ROOT, stdio: "inherit" },
    );
    if (build.status !== 0) throw new Error("go build failed");
    copyFileSync(observeBin, observeNew);

    // Start nucleus (detached so PID-based shutdown isn't blocked by
    // Node's child-process bookkeeping).
    nucleus = spawn(NUCLEUS_BIN, [
      "start",
      "--port",
      String(NUC_PORT),
      "--data",
      join(TEST_DIR, "nucleus"),
    ], { stdio: "ignore", detached: true });
    nucleus.unref();
    await waitForTcp("127.0.0.1", NUC_PORT, 15_000);

    // Start observe — detached so Node doesn't hold the PID as a zombie
    // when the upgrader SIGTERMs it. Without `detached: true`, Node never
    // calls wait(2) on the child, leaving its PID alive in /proc and
    // causing WaitForExit to spin until its 30s deadline.
    observe = spawn(observeBin, [], {
      env: {
        ...process.env,
        OBSERVE_NUCLEUS_URL: `postgres://postgres@localhost:${NUC_PORT}/postgres?sslmode=disable`,
        OBSERVE_ADDR: `:${OBS_PORT}`,
        OBSERVE_DATA_DIR: DATA_DIR,
        OBSERVE_SEED_DEMO: "false",
      },
      stdio: "ignore",
      detached: true,
    });
    observe.unref();
    await waitForHealthz(OBS_PORT, 30_000);
  });

  test.afterAll(async () => {
    // After upgrade the original `observe` child PID no longer matches
    // the running observe — re-read it from the PID file the new binary
    // wrote.
    try {
      const pidPath = join(DATA_DIR, "observe.pid");
      if (existsSync(pidPath)) {
        const pid = parseInt(
          (await import("node:fs")).readFileSync(pidPath, "utf8").trim(),
          10,
        );
        if (pid > 0) process.kill(pid, "SIGTERM");
      }
    } catch {}
    // Belt and braces: kill what we originally spawned too.
    if (observe && observe.pid) {
      try { process.kill(observe.pid, "SIGTERM"); } catch {}
    }
    if (nucleus && nucleus.pid) {
      try { process.kill(nucleus.pid, "SIGTERM"); } catch {}
    }
    if (process.env.OBSERVE_E2E_KEEP !== "1") {
      rmSync(TEST_DIR, { recursive: true, force: true });
    }
  });

  test("preserves events across binary swap", async ({ request }) => {
    // 1. Auth + API key.
    const login = await request.post(`http://localhost:${OBS_PORT}/api/v1/auth/login`, {
      data: { username: "admin", password: "observe" },
    });
    expect(login.ok()).toBeTruthy();
    const { token } = await login.json();
    expect(token).toBeTruthy();

    const sites = await request.get(`http://localhost:${OBS_PORT}/api/v1/sites`, {
      headers: { Authorization: `Bearer ${token}` },
    });
    const sitesList = await sites.json();
    const siteId = (Array.isArray(sitesList) ? sitesList[0] : sitesList.sites?.[0])?.site_id;
    expect(siteId).toBeTruthy();

    const keyRes = await request.post(
      `http://localhost:${OBS_PORT}/api/v1/sites/${siteId}/keys`,
      { headers: { Authorization: `Bearer ${token}` } },
    );
    const keyJson = await keyRes.json();
    const apiKey: string = keyJson.key || keyJson.api_key;
    expect(apiKey).toMatch(/^obs_/);

    // 2. Send 50 events BEFORE upgrade — these should already be in DB.
    const ts = Date.now();
    const tag = `upgrade-e2e-${ts}`;
    for (let i = 0; i < 50; i++) {
      const r = await request.post(`http://localhost:${OBS_PORT}/api/v1/events`, {
        headers: { "X-API-Key": apiKey, "User-Agent": "Mozilla/5.0 (E2E)" },
        data: { event_type: "pageview", url: `https://${tag}/pre-${i}`, timestamp: ts },
      });
      expect(r.status()).toBe(201);
    }
    // Allow flush.
    await sleep(2500);

    // 3. Run `observe upgrade --target observe-new` — old observe will
    // graceful-shutdown, new will spawn, healthz must return 200 within 60s.
    const up = spawnSync(observeBin, ["upgrade", "--target", observeNew], {
      env: { ...process.env, OBSERVE_DATA_DIR: DATA_DIR },
      encoding: "utf8",
      timeout: 90_000,
    });
    if (up.status !== 0) {
      console.error("upgrade stdout:", up.stdout);
      console.error("upgrade stderr:", up.stderr);
    }
    expect(up.status).toBe(0);
    expect(up.stderr + up.stdout).toContain("upgrade: complete");

    // 4. Send 50 MORE events AFTER upgrade — these go to the new binary.
    // Wait briefly for the new binary's HTTP server to be fully ready.
    await waitForHealthz(OBS_PORT, 30_000);
    for (let i = 0; i < 50; i++) {
      const r = await request.post(`http://localhost:${OBS_PORT}/api/v1/events`, {
        headers: { "X-API-Key": apiKey, "User-Agent": "Mozilla/5.0 (E2E)" },
        data: { event_type: "pageview", url: `https://${tag}/post-${i}`, timestamp: ts },
      });
      expect(r.status()).toBe(201);
    }
    await sleep(2500);

    // 5. Login to the NEW binary and verify event count via export.
    // Use psql via the explorer endpoint or a direct count via the
    // events stats endpoint. We'll use a simple SQL count via the
    // /api/v1/explorer/run path if available; fall back to summing
    // returned breakdown rows.
    const login2 = await request.post(`http://localhost:${OBS_PORT}/api/v1/auth/login`, {
      data: { username: "admin", password: "observe" },
    });
    expect(login2.ok()).toBeTruthy();
    const { token: token2 } = await login2.json();

    // Count events via a raw export. Window the query around the test's
    // event timestamp to be deterministic.
    const fromIso = new Date(ts - 60_000).toISOString();
    const toIso = new Date(ts + 60_000).toISOString();
    const exp = await request.get(
      `http://localhost:${OBS_PORT}/api/v1/export?site_id=${siteId}&type=events&format=json&from=${encodeURIComponent(fromIso)}&to=${encodeURIComponent(toIso)}`,
      { headers: { Authorization: `Bearer ${token2}` } },
    );
    expect(exp.ok()).toBeTruthy();
    const body = await exp.text();
    // The export emits a JSON array; count occurrences of the test tag.
    const matches = body.match(new RegExp(tag, "g")) ?? [];
    // Both pre- and post-upgrade events must be present.
    expect(matches.length).toBeGreaterThanOrEqual(100);
  });
});

async function sleep(ms: number) {
  return new Promise((r) => setTimeout(r, ms));
}

async function waitForTcp(host: string, port: number, timeoutMs: number) {
  const { connect } = await import("node:net");
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const ok = await new Promise<boolean>((resolve) => {
      const sock = connect(port, host, () => {
        sock.end();
        resolve(true);
      });
      sock.on("error", () => resolve(false));
    });
    if (ok) return;
    await sleep(200);
  }
  throw new Error(`tcp ${host}:${port} did not become reachable in ${timeoutMs}ms`);
}

async function waitForHealthz(port: number, timeoutMs: number) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try {
      const r = await fetch(`http://localhost:${port}/healthz`);
      if (r.ok) return;
    } catch {}
    await sleep(300);
  }
  throw new Error(`healthz on :${port} did not return 200 in ${timeoutMs}ms`);
}
