import { test } from "node:test";
import assert from "node:assert/strict";
import { captureState, trackerSnippet } from "./onboardCapture.ts";

test("a site already capturing is confirmed immediately", () => {
  assert.deepEqual(captureState(5, 5, 0), { kind: "confirmed", sinceSetup: false });
});

test("an event arriving during setup is confirmed", () => {
  assert.deepEqual(captureState(0, 1, 500), { kind: "confirmed", sinceSetup: true });
});

test("zero traffic past the grace period becomes no-capture", () => {
  assert.deepEqual(captureState(0, 0, 31_000), { kind: "no-capture" });
});

test("zero traffic inside the grace period stays waiting", () => {
  assert.deepEqual(captureState(0, 0, 1_000), { kind: "waiting", noneEver: true });
});

test("baseline still loading reads as waiting, not confirmed", () => {
  assert.deepEqual(captureState(null, null, 0), { kind: "waiting", noneEver: true });
});

test("snippet targets the served tracker route and carries the key", () => {
  const s = trackerSnippet("https://observe.example.com/", "site1", "pk_abc");
  assert.match(s, /src="https:\/\/observe\.example\.com\/t\/observe\.js"/);
  assert.match(s, /data-site-id="site1"/);
  assert.match(s, /data-api-key="pk_abc"/);
  // The old broken form — src pointing at the unserved bare /observe.js —
  // must not come back (the served route is /t/observe.js).
  assert.equal(/src="[^"]*(?<!\/t)\/observe\.js"/.test(s), false);
  assert.equal(s.includes("data-api-key"), true);
});
