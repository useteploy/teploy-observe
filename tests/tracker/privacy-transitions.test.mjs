import test from 'node:test';
import assert from 'node:assert/strict';
import vm from 'node:vm';
import { readFileSync } from 'node:fs';
import { createEventSanitizer } from '../../cmd/observe/tracker/replay-delta/src/sanitizer.mjs';

const element = (id, children = [], attributes = {}) => ({ type: 2, id, tagName: 'div', attributes, childNodes: children });
const text = (id, value = 'public') => ({ type: 3, id, textContent: value });
const snapshot = () => ({ type: 2, timestamp: 1, data: { node: { type: 0, id: 1, childNodes: [element(2, [element(3, [text(4)])]), element(5)] } } });
const mutation = (data) => ({ type: 3, timestamp: 2, data: { source: 0, ...data } });
const built = { window: {} };
vm.runInNewContext(readFileSync(process.env.OBSERVE_SANITIZER_BUNDLE || new URL('../../ui/public/rrweb/sanitize.js', import.meta.url), 'utf8'), built);
const factories = { source: createEventSanitizer, built: built.window.__observeReplayRuntime.createEventSanitizer };
for (const [name, factory] of Object.entries(factories)) {
  for (const marker of ['data-observe-block', 'contenteditable']) {
    test(`${name}: ${marker} masks grandchildren before same-batch text, stays private until checkout`, () => {
      const recorder = factory();
      const player = factory();
      player.sanitize(recorder.sanitize(snapshot()));
      const event = recorder.sanitize(mutation({ attributes: [{ id: 2, attributes: { [marker]: 'true' } }], texts: [{ id: 4, value: 'PRIVATE_SENTINEL' }] }));
      assert.ok(!JSON.stringify(event).includes('PRIVATE_SENTINEL'));
      assert.equal(event.data.texts.find(t => t.id === 4).value, '');
      const played = player.sanitize(event);
      assert.equal(played.data.texts.find(t => t.id === 4).value, '');
      const removed = recorder.sanitize(mutation({ attributes: [{ id: 2, attributes: { [marker]: null } }], texts: [{ id: 4, value: 'PRIVATE_SENTINEL' }] }));
      assert.ok(!JSON.stringify(removed).includes('PRIVATE_SENTINEL'));
      recorder.sanitize(snapshot());
      assert.equal(recorder.sanitize(mutation({ texts: [{ id: 4, value: 'public-again' }] })).data.texts[0].value, 'public-again');
    });
  }
  test(`${name}: privacy clears existing text without characterData and follows reparenting`, () => {
    const s = factory(); s.sanitize(snapshot());
    const marked = s.sanitize(mutation({ attributes: [{ id: 5, attributes: { 'data-observe-block': '' } }] }));
    assert.ok(marked);
    const moved = s.sanitize(mutation({ removes: [{ id: 3, parentId: 2 }], adds: [{ parentId: 5, node: element(3, [text(4, 'PRIVATE_SENTINEL')]) }], texts: [{ id: 4, value: 'PRIVATE_SENTINEL' }] }));
    assert.ok(!JSON.stringify(moved).includes('PRIVATE_SENTINEL'));
    const out = s.sanitize(mutation({ adds: [{ parentId: 2, node: element(3, [text(4, 'PRIVATE_SENTINEL')]) }] }));
    assert.ok(!JSON.stringify(out).includes('PRIVATE_SENTINEL'));
    const s2 = factory(); s2.sanitize(snapshot());
    const cleared = s2.sanitize(mutation({ attributes: [{ id: 2, attributes: { contenteditable: '' } }] }));
    assert.equal(cleared.data.texts.find(t => t.id === 4).value, '');
  });
  test(`${name}: reverse-ordered additions inherit privacy before text emission`, () => {
    const s = factory(); s.sanitize(snapshot());
    const out = s.sanitize(mutation({ adds: [{ parentId: 7, node: text(8, 'PRIVATE_SENTINEL') }, { parentId: 2, node: element(7, [], { 'data-observe-block': 'true' }) }], texts: [{ id: 8, value: 'PRIVATE_SENTINEL' }] }));
    assert.ok(!JSON.stringify(out).includes('PRIVATE_SENTINEL'));
  });
  test(`${name}: all allowed incremental sources survive second sanitizer`, () => {
    for (const source of [1, 2, 3, 4, 5, 6, 7, 12, 14]) {
      const event = { type: 3, timestamp: 3, data: { source, type: 2, id: 4, x: 1, y: 2, positions: [{ x: 1, y: 2, id: 4 }], ranges: [] } };
      const once = factory().sanitize(event);
      assert.equal(once.data.source, source);
      assert.equal(JSON.stringify(factory().sanitize(once)), JSON.stringify(once));
    }
  });
}
