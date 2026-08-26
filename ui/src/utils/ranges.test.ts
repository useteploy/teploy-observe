import assert from "node:assert/strict";
import { test, beforeEach } from "node:test";
import {
  loadRange,
  saveRange,
  resolvePreset,
  RANGE_STORAGE_KEY,
  DEFAULT_RANGE_LABEL,
  CUSTOM_LABEL,
} from "./ranges.ts";

/**
 * A minimal localStorage. `mode` lets a test reproduce the two ways a real
 * browser refuses: Safari's private mode throws on read, and a user who has
 * blocked site data throws on write.
 */
let store = new Map<string, string>();
let mode: "ok" | "throw-read" | "throw-write" = "ok";

(globalThis as any).window = {
  localStorage: {
    getItem(k: string) {
      if (mode === "throw-read") throw new Error("blocked");
      return store.has(k) ? store.get(k)! : null;
    },
    setItem(k: string, v: string) {
      if (mode === "throw-write") throw new Error("blocked");
      store.set(k, v);
    },
  },
};

beforeEach(() => {
  store = new Map();
  mode = "ok";
});

function put(value: unknown) {
  store.set(RANGE_STORAGE_KEY, typeof value === "string" ? value : JSON.stringify(value));
}

test("nothing stored yields the default range", () => {
  const r = loadRange();
  assert.equal(r.label, DEFAULT_RANGE_LABEL);
  assert.equal(r.rolling, true);
  assert.ok(Date.parse(r.from) < Date.parse(r.to));
});

test("a round trip through storage survives navigation", () => {
  const picked = resolvePreset("Last 30 days")!;
  saveRange({ ...picked, label: "Last 30 days", rolling: true });
  assert.equal(loadRange().label, "Last 30 days");
});

test("a rolling preset is recomputed, not restored frozen", () => {
  // Stored a year ago. Restoring these instants verbatim would pin the
  // dashboard to last year and it would silently stop showing today.
  put({
    from: "2025-01-01T00:00:00.000Z",
    to: "2025-01-08T00:00:00.000Z",
    label: "Last 7 days",
    rolling: true,
  });
  const r = loadRange();
  assert.equal(r.label, "Last 7 days");
  assert.ok(
    Date.now() - Date.parse(r.to) < 60_000,
    `restored a stale window ending ${r.to}; a rolling preset must be recomputed from now`,
  );
});

test("a hand-picked range keeps its instants", () => {
  put({
    from: "2025-03-01T00:00:00.000Z",
    to: "2025-03-08T00:00:00.000Z",
    label: CUSTOM_LABEL,
    rolling: false,
  });
  const r = loadRange();
  assert.deepEqual(
    { from: r.from, to: r.to, label: r.label },
    { from: "2025-03-01T00:00:00.000Z", to: "2025-03-08T00:00:00.000Z", label: CUSTOM_LABEL },
  );
});

test("a preset this build no longer offers falls back rather than blanking", () => {
  // "This week", "This month", "This year" and "Yesterday" were removed on
  // 2026-08-25. A browser that stored one of them must not render an empty
  // range on the next visit.
  put({ from: "", to: "", label: "This week", rolling: true });
  const rolling = loadRange();
  assert.equal(rolling.label, DEFAULT_RANGE_LABEL);
  assert.ok(Date.parse(rolling.from) < Date.parse(rolling.to));

  // Pinned to a removed label: the dates are still real, so keep them and
  // relabel, rather than throwing away a deliberate selection.
  put({
    from: "2025-03-01T00:00:00.000Z",
    to: "2025-03-08T00:00:00.000Z",
    label: "This month",
    rolling: false,
  });
  const pinned = loadRange();
  assert.equal(pinned.label, CUSTOM_LABEL);
  assert.equal(pinned.from, "2025-03-01T00:00:00.000Z");
});

test("a stale or hostile stored value never breaks the page", () => {
  const bad: unknown[] = [
    "not json at all",
    "null",
    '"a string"',
    "[]",
    "42",
    {},                                                   // no label
    { label: "" },                                        // empty label
    { label: 7, rolling: true },                          // label not a string
    { label: CUSTOM_LABEL, rolling: false },              // pinned, no instants
    { from: 1, to: 2, label: CUSTOM_LABEL, rolling: false },     // instants not strings
    { from: "banana", to: "kiwi", label: CUSTOM_LABEL, rolling: false },
    { from: "2025-03-08T00:00:00.000Z", to: "2025-03-01T00:00:00.000Z", label: CUSTOM_LABEL, rolling: false }, // reversed
    { from: "2025-03-01T00:00:00.000Z", to: "2025-03-01T00:00:00.000Z", label: CUSTOM_LABEL, rolling: false }, // empty
  ];
  for (const value of bad) {
    put(value);
    const r = loadRange();
    assert.equal(r.label, DEFAULT_RANGE_LABEL, `stored ${JSON.stringify(value)}`);
    assert.ok(
      Date.parse(r.from) < Date.parse(r.to),
      `stored ${JSON.stringify(value)} produced an unusable range`,
    );
  }
});

test("a browser that refuses storage still renders", () => {
  mode = "throw-read";
  assert.equal(loadRange().label, DEFAULT_RANGE_LABEL);
  mode = "throw-write";
  assert.doesNotThrow(() => saveRange({ from: "a", to: "b", label: CUSTOM_LABEL, rolling: false }));
});

test("every preset produces a forward range", () => {
  for (const label of ["Today", "Last 24 hours", "Last 7 days", "Last 30 days", "Last 90 days", "Last 12 months", "All time"]) {
    const r = resolvePreset(label);
    assert.ok(r, `${label} no longer resolves`);
    assert.ok(Date.parse(r!.from) < Date.parse(r!.to), `${label} is inverted`);
  }
  assert.equal(resolvePreset(CUSTOM_LABEL), null);
  assert.equal(resolvePreset("This week"), null);
});
