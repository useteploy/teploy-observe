// O06 sanitizer fixtures: synthetic rrweb 2.x event streams with seeded
// secrets, asserting nothing sensitive survives the pure fold. The event
// shapes mirror rrweb 2.1.6's emitted payloads (NodeType Document=0,
// DocumentType=1, Element=2, Text=3; EventType FullSnapshot=2,
// IncrementalSnapshot=3, Meta=4; IncrementalSource Mutation=0, Input=5).
//
// Run: node --test test/ (from cmd/observe/tracker/replay-delta).
import assert from 'node:assert/strict';
import { test } from 'node:test';
import {
  sanitizeEventWith,
  createEventSanitizer,
  sanitizerPolicy,
} from '../src/sanitizer.mjs';

const SECRETS = [
  'SECRET-TOKEN-abc123',
  'csrf-xyz-987',
  'hunter2',
  'sk_live_payme',
  'jwt.eyJzdWIiOiIx',
];

// Deep scan: JSON.stringify the sanitized stream and refuse any seeded
// secret anywhere in it.
function assertNoSecrets(events, label) {
  const blob = JSON.stringify(events);
  for (const s of SECRETS) {
    assert.ok(!blob.includes(s), `${label}: secret ${s} survived sanitization`);
  }
}

// ── serialized node helpers (rrweb shapes) ──
function el(id, tagName, attributes, childNodes) {
  const n = { type: 2, tagName, attributes: attributes || {}, childNodes: childNodes || [] };
  if (id !== undefined) n.id = id;
  return n;
}
function text(id, value) {
  const n = { type: 3, textContent: value };
  if (id !== undefined) n.id = id;
  return n;
}
function doc(...childNodes) {
  return { type: 0, childNodes };
}

function fullSnapshotEvent(node) {
  return { type: 2, timestamp: 1000, data: { node, initialOffset: { left: 0, top: 0 } } };
}

// A realistic sensitive document: head with CSRF meta, a signed href, an
// image with a tokened src, a password input, a textarea with prefilled
// private message, a contenteditable region, a data-observe-block subtree,
// and ordinary visible text that the product DOES record.
function sensitiveDocument() {
  return doc(
    { type: 1, name: 'html' },
    el(2, 'html', {}, [
      el(3, 'head', {}, [
        el(4, 'meta', { name: 'csrf', content: 'csrf-xyz-987' }, []),
        el(5, 'script', { src: 'https://cdn.example.com/app.js?sig=jwt.eyJzdWIiOiIx' }, [
          text(6, 'alert("SECRET-TOKEN-abc123")'),
        ]),
        el(7, 'style', {}, [text(8, 'body { background: url(https://x/p?k=hunter2) }')]),
      ]),
      el(9, 'body', { class: 'page' }, [
        text(10, 'Welcome to the store'),
        el(11, 'a', { href: 'https://bank.example.com/transfer?token=SECRET-TOKEN-abc123' }, [
          text(12, 'Pay now'),
        ]),
        el(13, 'img', { src: '/img/photo.jpg?sig=sk_live_payme', alt: 'Product', width: '40' }, []),
        el(14, 'img', { src: 'data:image/png;base64,SECRET-TOKEN-abc123' }, []),
        el(15, 'input', { type: 'password', value: 'hunter2', id: 'pw' }, []),
        el(16, 'textarea', { id: 'note' }, [text(17, 'SECRET-TOKEN-abc123')]),
        el(18, 'div', { contenteditable: 'true' }, [text(19, 'draft with sk_live_payme inside')]),
        el(20, 'div', { 'data-observe-block': 'true' }, [
          el(21, 'span', { class: 's' }, [text(22, 'jwt.eyJzdWIiOiIx')]),
        ]),
        el(23, 'p', { class: 'lead' }, [text(24, 'Ordinary visible copy the product records')]),
      ]),
    ])
  );
}

function findTag(node, tagName) {
  if (!node || typeof node !== 'object') return undefined;
  if (node.tagName === tagName) return node;
  for (const c of node.childNodes || []) {
    const hit = findTag(c, tagName);
    if (hit) return hit;
  }
  return undefined;
}

test('full snapshot: private subtrees become opaque placeholders, ids preserved', () => {
  const s = createEventSanitizer({ baseURL: 'https://shop.example.com/products' });
  const out = s.sanitize(fullSnapshotEvent(sensitiveDocument()));
  assert.ok(out, 'event kept');
  const body = findTag(out.data.node, 'body');
  assert.ok(body, 'body survives');

  const byId = new Map();
  const walk = (n) => {
    if (n && typeof n === 'object') {
      if (typeof n.id === 'number') byId.set(n.id, n);
      (n.childNodes || []).forEach(walk);
    }
  };
  walk(out.data.node);

  // head subtree: opaque placeholder, no meta/script/style children
  const head = byId.get(3);
  assert.ok(head, 'head node kept');
  assert.equal(head.tagName, 'div');
  assert.deepEqual(head.childNodes, []);
  assert.deepEqual(head.attributes, {});

  // password input / textarea: placeholder divs
  for (const id of [15, 16]) {
    assert.equal(byId.get(id).tagName, 'div', `node ${id} placeholdered`);
    assert.deepEqual(byId.get(id).childNodes, []);
  }

  // contenteditable + data-observe-block subtrees: placeholders
  for (const id of [18, 20]) {
    assert.equal(byId.get(id).tagName, 'div', `node ${id} placeholdered`);
    assert.deepEqual(byId.get(id).childNodes, []);
  }

  // ordinary text survives (product behavior)
  assert.equal(byId.get(10).textContent, 'Welcome to the store');
  assert.equal(byId.get(24).textContent, 'Ordinary visible copy the product records');

  assertNoSecrets([out], 'full snapshot');
});

test('full snapshot: attribute allowlist drops everything URL/token-bearing except sanitized img src', () => {
  const s = createEventSanitizer({ baseURL: 'https://shop.example.com/products' });
  const out = s.sanitize(fullSnapshotEvent(sensitiveDocument()));
  const blob = JSON.stringify(out);

  // attribute names that must never ride
  for (const forbidden of ['data-observe-block', 'contenteditable', 'value', 'href', '"content"']) {
    assert.ok(!blob.includes(forbidden), `attribute/key ${forbidden} survived: ${blob.slice(0, 400)}`);
  }

  const byId = new Map();
  const walk = (n) => {
    if (n && typeof n === 'object') {
      if (typeof n.id === 'number') byId.set(n.id, n);
      (n.childNodes || []).forEach(walk);
    }
  };
  walk(out.data.node);

  // img 13: relative src resolved against the recorded base, query stripped
  const img = byId.get(13);
  assert.equal(img.attributes.src, 'https://shop.example.com/img/photo.jpg');
  assert.equal(img.attributes.alt, 'Product');
  assert.equal(img.attributes.width, '40');

  // img 14: data: URL refused
  assert.equal(byId.get(14).attributes.src, undefined);

  // link 11: href dropped entirely (only the sanitized-text children stay)
  assert.equal(byId.get(11).attributes.href, undefined);
});

test('meta event: href reduced to origin+path, dimensions kept', () => {
  const s = createEventSanitizer();
  const out = s.sanitize({
    type: 4,
    timestamp: 5,
    data: { href: 'https://app.example.com/reset?token=SECRET-TOKEN-abc123#frag', width: 1440, height: 900 },
  });
  assert.deepEqual(out.data, { href: 'https://app.example.com/reset', width: 1440, height: 900 });
  assert.ok(!JSON.stringify(out).includes('SECRET-TOKEN-abc123'));

  // non-http and garbage hrefs become ''
  assert.equal(s.sanitize({ type: 4, timestamp: 6, data: { href: 'javascript:alert(1)', width: 1, height: 1 } }).data.href, '');
  assert.equal(s.sanitize({ type: 4, timestamp: 7, data: { href: 'not a url', width: 1, height: 1 } }).data.href, '');
});

function mutationEvent(data) {
  return { type: 3, timestamp: 2000, data: Object.assign({ source: 0 }, data) };
}

test('incremental mutation: characterData masked on private ids, bounded on ordinary ids', () => {
  const s = createEventSanitizer();
  // Establish the private set from a snapshot: ids 15..22 live under
  // private subtrees; 10 and 24 are ordinary.
  s.sanitize(fullSnapshotEvent(sensitiveDocument()));

  const out = s.sanitize(mutationEvent({
    texts: [
      { id: 24, value: 'updated ordinary text' },
      { id: 19, value: 'SECRET-TOKEN-abc123 typed into contenteditable' },
      { id: 22, value: 'sk_live_payme in blocked region' },
      { id: 10, value: 'x'.repeat(5000) },
    ],
  }));
  const texts = out.data.texts;
  const byId = new Map(texts.map((t) => [t.id, t]));
  assert.equal(byId.get(24).value, 'updated ordinary text');
  assert.equal(byId.get(19).value, '', 'contenteditable id masked');
  assert.equal(byId.get(22).value, '', 'blocked id masked');
  assert.equal(byId.get(10).value.length, sanitizerPolicy.MAX_TEXT_LENGTH, 'text bounded');
  assertNoSecrets([out], 'characterData');
});

test('incremental mutation: attribute mutations filtered through the allowlist', () => {
  const s = createEventSanitizer();
  s.sanitize(fullSnapshotEvent(sensitiveDocument()));

  const out = s.sanitize(mutationEvent({
    attributes: [
      { id: 23, attributes: { class: 'highlight', 'data-token': 'SECRET-TOKEN-abc123', onclick: 'steal()' } },
      { id: 13, attributes: { src: 'https://evil.example.com/x.png?u=hunter2' } },
      { id: 25, attributes: { contenteditable: 'true' } },
    ],
  }));
  const attrs = out.data.attributes;
  const a0 = attrs.find((a) => a.id === 23).attributes;
  assert.deepEqual(Object.keys(a0).sort(), ['class']);
  assert.equal(a0.class, 'highlight');

  // attribute mutations carry no tagName context, so img src is dropped
  // (not proxied) — the conservative branch of the allowlist.
  assert.deepEqual(attrs.find((a) => a.id === 13).attributes, {});
  assertNoSecrets([out], 'attribute mutations');

  // a contenteditable flip marks the id private: a later characterData on
  // id 25 masks even though it was ordinary in the snapshot.
  const after = s.sanitize(mutationEvent({ texts: [{ id: 25, value: 'now editable jwt.eyJzdWIiOiIx' }] }));
  assert.equal(after.data.texts[0].value, '');
  assertNoSecrets([after], 'post-flip characterData');
});

test('incremental mutation: addedNodes sanitized, blocked parents absorb children', () => {
  const s = createEventSanitizer();
  s.sanitize(fullSnapshotEvent(sensitiveDocument()));

  const out = s.sanitize(mutationEvent({
    adds: [
      // ordinary add with dirty attributes
      {
        parentId: 9,
        nextId: 23,
        node: el(30, 'div', { class: 'card', 'data-secret': 'SECRET-TOKEN-abc123', style: 'background:url(x)' }, [
          text(31, 'new card title'),
        ]),
      },
      // add under the data-observe-block placeholder (id 20): absorbed
      {
        parentId: 20,
        node: el(32, 'span', {}, [text(33, 'csrf-xyz-987 sneaked under block')]),
      },
      // add that is itself private (a script)
      {
        parentId: 9,
        node: el(34, 'script', { src: 'https://evil/x.js' }, [text(35, 'csrf-xyz-987')]),
      },
    ],
  }));
  const adds = out.data.adds;
  const a0 = adds.find((a) => a.node.id === 30);
  assert.deepEqual(Object.keys(a0.node.attributes), ['class']);
  assert.equal(a0.node.childNodes[0].textContent, 'new card title');

  const a1 = adds.find((a) => a.node.id === 32);
  assert.equal(a1.node.tagName, 'div');
  assert.deepEqual(a1.node.childNodes, []);

  const a2 = adds.find((a) => a.node.id === 34);
  assert.equal(a2.node.tagName, 'div');
  assert.deepEqual(a2.node.childNodes, []);
  assertNoSecrets([out], 'addedNodes');

  // the absorbed subtree's ids are now private: later text on id 33 masks.
  const after = s.sanitize(mutationEvent({ texts: [{ id: 33, value: 'hunter2 later' }] }));
  assert.equal(after.data.texts[0].value, '');
});

test('incremental sources: input text dropped; coordinates and ids pass; style/log/canvas dropped', () => {
  const s = createEventSanitizer();

  const input = s.sanitize({ type: 3, timestamp: 1, data: { source: 5, id: 15, text: 'hunter2', isChecked: false } });
  assert.equal(input.data.text, undefined, 'input text dropped');
  assert.equal(input.data.id, 15);

  const mouse = s.sanitize({ type: 3, timestamp: 2, data: { source: 2, type: 2, id: 9, x: 120, y: 40 } });
  assert.deepEqual(mouse.data, { type: 2, id: 9, x: 120, y: 40 });

  const move = s.sanitize({
    type: 3, timestamp: 3,
    data: { source: 1, positions: [{ x: 1, y: 2, timeOffset: 5, id: 9 }, { x: 3, y: 4, timeOffset: 9 }] },
  });
  assert.deepEqual(move.data.positions[0], { x: 1, y: 2, id: 9, timeOffset: 5 });

  const scroll = s.sanitize({ type: 3, timestamp: 4, data: { source: 3, id: 9, x: 0, y: 400 } });
  assert.deepEqual(scroll.data, { source: 3, id: 9, x: 0, y: 400 });

  for (const source of [8, 9, 10, 11, 13, 15, 16]) {
    const dropped = s.sanitize({ type: 3, timestamp: 5, data: { source, css: 'body{}', orWhatever: 'SECRET-TOKEN-abc123' } });
    assert.equal(dropped, null, `source ${source} dropped`);
  }

  // Custom / Plugin / Asset / unknown event types dropped entirely
  for (const type of [5, 6, 7, 99]) {
    assert.equal(s.sanitize({ type, timestamp: 6, data: { tag: 'x', payload: 'hunter2' } }), null);
  }

  // DomContentLoaded / Load pass as markers
  assert.deepEqual(s.sanitize({ type: 0, timestamp: 7, data: {} }).data, {});
  assert.deepEqual(s.sanitize({ type: 1, timestamp: 8, data: {} }).data, {});
});

test('checkout (second full snapshot) resets the private-id set', () => {
  const s = createEventSanitizer();
  s.sanitize(fullSnapshotEvent(sensitiveDocument()));

  // second keyframe: the same document, but node 18 is no longer
  // contenteditable and the blocked subtree is gone — ids re-mint per
  // segment, so the fold must judge THIS snapshot, not the last one.
  const second = doc(
    { type: 1, name: 'html' },
    el(2, 'html', {}, [
      el(9, 'body', {}, [
        text(18, 'plain text now'),
        el(19, 'div', { 'data-observe-block': '1' }, [text(20, 'fresh secret sk_live_payme')]),
      ]),
    ])
  );
  const out = s.sanitize(fullSnapshotEvent(second));
  const body = findTag(out.data.node, 'body');
  assert.equal(body.childNodes[0].textContent, 'plain text now', 'id 18 ordinary again');

  // and the fold tracks the NEW private id from this snapshot
  const after = s.sanitize(mutationEvent({ texts: [{ id: 20, value: 'hunter2' }] }));
  assert.equal(after.data.texts[0].value, '');
  const ordinary = s.sanitize(mutationEvent({ texts: [{ id: 18, value: 'still ordinary' }] }));
  assert.equal(ordinary.data.texts[0].value, 'still ordinary');
  assertNoSecrets([out], 'checkout');
});

test('pure fold: sanitizeEventWith is a deterministic function of the event sequence', () => {
  const state0 = { privateIds: new Set(), baseURL: '' };
  const r1 = sanitizeEventWith(state0, fullSnapshotEvent(sensitiveDocument()));
  assert.ok(r1.event);
  const r2 = sanitizeEventWith(r1.state, mutationEvent({ texts: [{ id: 19, value: 'hunter2' }] }));
  assert.equal(r2.event.data.texts[0].value, '');

  // replaying the same sequence from the same initial state yields the
  // same outputs (no hidden state, no globals)
  const s2 = sanitizeEventWith(state0, fullSnapshotEvent(sensitiveDocument()));
  const t2 = sanitizeEventWith(s2.state, mutationEvent({ texts: [{ id: 19, value: 'hunter2' }] }));
  assert.deepEqual(t2.event, r2.event);

  // malformed inputs never throw
  for (const bad of [null, 42, 'x', { type: 2 }, { type: 2, data: { node: null } }, { type: 3, data: { source: 0, adds: [{ node: 5 }] } }]) {
    assert.doesNotThrow(() => sanitizeEventWith(state0, bad));
  }
});

test('snapshot budgets: over-depth subtrees truncate to placeholders, node budget bounds the walk', () => {
  // depth chain: 33 nested divs, ordinary text at the bottom
  let deep = text(50, 'deep text');
  for (let i = 0; i < 33; i++) deep = el(50 + i + 1, 'div', {}, [deep]);
  const s = createEventSanitizer();
  const out = s.sanitize(fullSnapshotEvent(doc(el(1, 'html', {}, [deep]))));
  assert.ok(out, 'snapshot survives');
  assertNoSecrets([out], 'deep chain');

  // wide tree beyond the node budget: still well-formed, bounded
  const wide = el(2, 'html', {}, Array.from({ length: 6000 }, (_, i) => el(100 + i, 'p', {}, [text(7000 + i, `p${i}`)])));
  const out2 = s.sanitize(fullSnapshotEvent(doc(wide)));
  let count = 0;
  const walkCount = (n) => { if (n && typeof n === 'object') { count++; (n.childNodes || []).forEach(walkCount); } };
  walkCount(out2.data.node);
  assert.ok(count <= sanitizerPolicy.MAX_SNAPSHOT_NODES + 5, `node budget respected (${count})`);
});
