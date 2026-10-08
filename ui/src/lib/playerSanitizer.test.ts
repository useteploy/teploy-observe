import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";
const built = readFileSync(new URL("../../public/rrweb/sanitize.js", import.meta.url), "utf8");
function sanitizer() {
  const context: any = { window: {} }; vm.createContext(context); vm.runInContext(built, context);
  return context.window.__observeReplayRuntime.createEventSanitizer();
}
const el = (id: number, childNodes: unknown[] = []) => ({ type: 2, id, tagName: "div", attributes: {}, childNodes });
const snapshot = () => ({ type: 2, timestamp: 1, data: { node: { type: 0, id: 1, childNodes: [el(2, [el(3, [{ type: 3, id: 4, textContent: "public" }])])] }, initialOffset: { top: 0, left: 0 } } });
const mutation = (data: any) => ({ type: 3, timestamp: 2, data: { source: 0, texts: [], attributes: [], adds: [], removes: [], ...data } });
test("OBS26-118 generated player blocks existing descendants before same-batch text", () => {
  for (const attr of [{ "data-observe-block": "true" }, { contenteditable: "true" }]) {
    const s = sanitizer(); s.sanitize(snapshot());
    const out = s.sanitize(mutation({ attributes: [{ id: 2, attributes: attr }], texts: [{ id: 4, value: "SYNTHETIC_PRIVATE_SENTINEL" }] }));
    assert.ok(!JSON.stringify(out).includes("SYNTHETIC_PRIVATE_SENTINEL"));
    const after = s.sanitize(mutation({ texts: [{ id: 4, value: "SYNTHETIC_PRIVATE_SENTINEL" }] }));
    assert.ok(!JSON.stringify(after).includes("SYNTHETIC_PRIVATE_SENTINEL"));
    s.sanitize(snapshot());
    assert.equal(s.sanitize(mutation({ texts: [{ id: 4, value: "public again" }] })).data.texts[0].value, "public again");
  }
});
test("OBS26-119 generated player preserves incremental interaction discriminators twice", () => {
  for (const type of [0, 2, 5, 7]) {
    const event = { type: 3, timestamp: 3, data: { source: 2, type, id: 4, x: 12, y: 20 } };
    const once = sanitizer().sanitize(event); const twice = sanitizer().sanitize(once);
    assert.equal(once.data.source, 2); assert.equal(twice.data.source, 2);
    assert.equal(twice.data.type, type); assert.equal(twice.data.id, 4);
  }
});
