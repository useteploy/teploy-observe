/**
 * Integration smoke for @observe/sentry-shim.
 *
 * Requires a running Observe stack at OBSERVE_URL (defaults to
 * http://localhost:3000) with admin/observe credentials. The test:
 *
 *   1. logs in to fetch a JWT
 *   2. captureException through the shim with a unique error_value
 *   3. polls /api/v1/issues?site_id=default until that issue appears
 *
 * Skipped automatically if Observe is unreachable.
 */

import { test } from "node:test";
import assert from "node:assert/strict";

import { init, captureException, flush } from "../src/index.js";

const OBSERVE_URL = process.env.OBSERVE_URL ?? "http://localhost:3000";
const USERNAME = process.env.OBSERVE_USER ?? "admin";
const PASSWORD = process.env.OBSERVE_PASS ?? "observe";

async function isReachable(): Promise<boolean> {
  try {
    const res = await fetch(`${OBSERVE_URL}/healthz`, {
      signal: AbortSignal.timeout(1500),
    });
    return res.ok || res.status === 404; // root may 404 without a UI bundle
  } catch {
    try {
      const res = await fetch(`${OBSERVE_URL}/`, {
        signal: AbortSignal.timeout(1500),
      });
      return res.status < 500;
    } catch {
      return false;
    }
  }
}

async function login(): Promise<string> {
  const res = await fetch(`${OBSERVE_URL}/api/v1/auth/login`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ username: USERNAME, password: PASSWORD }),
  });
  if (!res.ok) throw new Error(`login failed: ${res.status}`);
  const { token } = (await res.json()) as { token: string };
  if (!token) throw new Error("login: no token in response");
  return token;
}

async function findIssue(
  token: string,
  needle: string,
  attempts = 30,
  delayMs = 500
): Promise<boolean> {
  for (let i = 0; i < attempts; i++) {
    const res = await fetch(
      `${OBSERVE_URL}/api/v1/issues?site_id=default&limit=100`,
      { headers: { Authorization: `Bearer ${token}` } }
    );
    if (res.ok) {
      const issues = (await res.json()) as Array<{ title: string }>;
      if (issues.some((i) => i.title.includes(needle))) return true;
    }
    await new Promise((r) => setTimeout(r, delayMs));
  }
  return false;
}

test("integration: captureException lands as an issue in the running Observe stack", async (t) => {
  if (!(await isReachable())) {
    t.skip(`Observe not reachable at ${OBSERVE_URL}`);
    return;
  }

  const token = await login();
  // Use a fully unique error type so the group_hash is brand-new (won't collide
  // with prior runs' identical stacks/types — Observe groups by error_type +
  // in-app stack, so the title only matches our needle when the issue is fresh).
  const stamp = `${Date.now()}${Math.floor(Math.random() * 1e6)}`;
  const errorType = `ShimSmoke${stamp}Error`;
  const needle = `smoke-${stamp}`;

  init({
    endpoint: OBSERVE_URL,
    siteId: "default",
    release: "shim-test",
    environment: "ci",
    // Disable stack traces so the smoke test stays fast and side-effect-free
    // (full traces work in unit tests, see unit.test.ts).
    attachStacktrace: false,
  });

  const err = new Error(needle);
  err.name = errorType;
  captureException(err);
  await flush();
  await new Promise((r) => setTimeout(r, 200));

  const found = await findIssue(token, needle);
  assert.equal(found, true, `expected issue containing "${needle}" within polling window`);
});
