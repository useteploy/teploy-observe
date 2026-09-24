/**
 * O11 migration example fixture for the browser SDK: credential -> first
 * event -> flush -> visible counters. Exercised end-to-end by
 * sdk/e2e/first_event_test.sh against a real Observe server (run with
 * tsx; env: OBSERVE_E2E_URL, OBSERVE_E2E_KEY).
 */

import { init, track, flush, getStats } from "../browser/src/index.js";

const endpoint = process.env.OBSERVE_E2E_URL;
const apiKey = process.env.OBSERVE_E2E_KEY;
if (!endpoint || !apiKey) {
  console.error("browser-first-event: OBSERVE_E2E_URL and OBSERVE_E2E_KEY required");
  process.exit(2);
}

init({ endpoint, siteId: "default", apiKey, disableAutoPageview: true });
track("o11_browser_first_event", { source: "sdk-e2e" });
await flush();

const stats = getStats();
if (!stats || stats.deliveredEvents !== 1 || Object.keys(stats.dropped).length !== 0) {
  console.error(`browser-first-event: unexpected stats ${JSON.stringify(stats)}`);
  process.exit(1);
}
console.log(`browser-first-event: delivered=${stats.deliveredEvents} losses=0 producer=${stats.producerId}`);
