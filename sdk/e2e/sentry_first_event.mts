/**
 * O11 migration example fixture for the sentry shim: capture -> bounded
 * flush -> visible counters. Exercised end-to-end by
 * sdk/e2e/first_event_test.sh (run with tsx; env: OBSERVE_E2E_URL,
 * OBSERVE_E2E_KEY).
 */

import * as Sentry from "../sentry-shim/src/index.js";

const endpoint = process.env.OBSERVE_E2E_URL;
const apiKey = process.env.OBSERVE_E2E_KEY;
if (!endpoint || !apiKey) {
  console.error("sentry-first-event: OBSERVE_E2E_URL and OBSERVE_E2E_KEY required");
  process.exit(2);
}

Sentry.init({ endpoint, siteId: "default", apiKey, release: "o11-e2e" });
Sentry.captureMessage("o11 sentry-shim first event");
const ok = await Sentry.flush(5000);

const stats = Sentry.getStats();
if (!ok || stats.delivered !== 1 || Object.keys(stats.lost).length !== 0) {
  console.error(`sentry-first-event: unexpected stats ${JSON.stringify(stats)}`);
  process.exit(1);
}
console.log("sentry-first-event: delivered=1 losses=0");
