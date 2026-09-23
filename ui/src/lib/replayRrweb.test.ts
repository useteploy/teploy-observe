// O06 replay mode detection: the seam that decides whether a stored
// session renders through the rrweb Replayer (delta recording) or the
// structural keyframe player. A flipped detection fails silent in both
// directions, so the rules are pinned here.
import assert from "node:assert/strict";
import { test } from "node:test";
import { detectRrwebEvents, isRrwebSession } from "./replayRrweb.ts";

const wrap = (rrwebType: number, timestamp = 1) => ({
  type: "rrweb",
  timestamp,
  data: { type: rrwebType, data: {}, timestamp },
});

test("wrapped rrweb events are detected with their inner type and timestamp", () => {
  const out = detectRrwebEvents([
    { type: "rrweb", timestamp: 10, data: { type: 4, data: { href: "https://x/" }, timestamp: 10 } },
    { type: "rrweb", timestamp: 11, data: { type: 2, data: { node: {} }, timestamp: 11 } },
  ]);
  assert.equal(out.length, 2);
  assert.equal(out[0].type, 4);
  assert.equal(out[1].type, 2);
  assert.equal(out[1].timestamp, 11);
});

test("non-rrweb events and malformed wrappers are ignored, never throw", () => {
  const out = detectRrwebEvents([
    { type: "click", timestamp: 1, data: { x: 1, y: 2 } },
    { type: "snapshot", timestamp: 2, data: { html: {} } },
    { type: "rrweb", timestamp: 3, data: null },
    { type: "rrweb", timestamp: 4, data: "not an object" },
    { type: "rrweb", timestamp: 5, data: { noType: true } },
    { type: "rrweb", timestamp: 6, data: { type: "2-as-string" } },
  ]);
  assert.equal(out.length, 0);
  assert.deepEqual(detectRrwebEvents([]), []);
});

test("a delta session needs two events AND a full snapshot", () => {
  const session = (...raws: { type: string; timestamp: number; data: any }[]) =>
    isRrwebSession(detectRrwebEvents(raws));
  // one event only, even with a snapshot: not a session
  assert.equal(session(wrap(2)), false);
  // two events but no full snapshot (e.g. truncated stream): keyframes
  assert.equal(session(wrap(4), wrap(3)), false);
  // the real shape: meta + full snapshot (+ anything after)
  assert.equal(session(wrap(4), wrap(2)), true);
  assert.equal(session(wrap(4), wrap(2), wrap(3), wrap(3)), true);
});
