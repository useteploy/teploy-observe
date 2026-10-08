import test from 'node:test';
import assert from 'node:assert/strict';
import vm from 'node:vm';
import { readFileSync } from 'node:fs';
const src = readFileSync(new URL('../../cmd/observe/tracker/observe-errors.js', import.meta.url), 'utf8');
function setup({ key = 'obs_key', transport = 'fetch', statuses = [200] } = {}) {
  const calls = [], native = [], listeners = {}, timers = [];
  const sandbox = { document: { currentScript: { src: 'https://observe.test/t/errors.js', getAttribute: n => n === 'data-api-key' ? key : null }, addEventListener() {} },
    console: Object.fromEntries(['log','info','warn','error','debug'].map(n => [n, (...args) => native.push(args)])),
    location: { origin: 'https://app.test', pathname: '/page' }, history: { pushState() {}, replaceState() {} },
    navigator: { userAgent: 'test', sendBeacon() { throw new Error('beacon cannot authenticate'); } },
    localStorage: { getItem() {} }, URL, Uint8Array, Error,
    setTimeout(fn, delay) { if (delay === 1000) timers.push(fn); return 1; },
    addEventListener(n, fn) { listeners[n] = fn; },
    XMLHttpRequest: function() { this.open = () => {}; this.setRequestHeader = (n,v) => (this.headers ||= {})[n] = v; this.send = body => { calls.push({ headers: this.headers, body: JSON.parse(body) }); this.status = statuses.shift() || 200; this.onload(); }; }
  };
  if (transport === 'fetch') sandbox.fetch = (url, opts) => { calls.push({ url, headers: opts.headers, body: JSON.parse(opts.body) }); return Promise.resolve({ status: statuses.shift() || 200 }); };
  sandbox.window = sandbox; vm.runInNewContext(src, sandbox);
  return { sandbox, calls, native, listeners, timers };
}
for (const transport of ['fetch', 'xhr']) {
  test(`${transport}: all reporting APIs carry telemetry key despite available beacon`, async () => {
    const h = setup({ transport });
    h.listeners.error({ message: 'global' }); h.listeners.unhandledrejection({ reason: 'rejection' });
    h.sandbox.observeErrors.captureException(new Error('exception')); h.sandbox.observeErrors.captureMessage('message');
    await Promise.resolve();
    assert.equal(h.calls.length, 4);
    for (const c of h.calls) assert.equal(c.headers['X-API-Key'], 'obs_key');
    assert.equal(h.sandbox.observeErrors.getDeliveryStatus().status, 'sent');
  });
  for (const status of [401, 403, 429, 503]) {
    test(`${transport}: HTTP ${status} diagnostic and bounded transient retry`, async () => {
      const h = setup({ transport, statuses: [status, status] }); h.sandbox.observeErrors.captureMessage('test');
      await Promise.resolve();
      assert.equal(h.sandbox.observeErrors.getDeliveryStatus().httpStatus, status);
      assert.equal(h.timers.length, status >= 429 ? 1 : 0);
      h.timers.shift()?.(); await Promise.resolve();
      assert.equal(h.timers.length, 0);
      if (h.calls.length === 2) assert.equal(h.calls[0].body.event_id, h.calls[1].body.event_id);
    });
  }
}
test('missing telemetry key emits no request and exposes diagnostic', () => {
  const h = setup({ key: '' }); h.sandbox.observeErrors.captureMessage('test');
  assert.equal(h.calls.length, 0); assert.equal(h.sandbox.observeErrors.getDeliveryStatus().status, 'missing-key');
});
test('console and rejection formatting never interrupts app on circular/BigInt/hooks/getters/Symbol', () => {
  const h = setup(); const circular = {}; circular.self = circular;
  const getter = Object.defineProperty({}, 'bad', { enumerable: true, get() { throw new Error('getter'); } });
  const values = [circular, 1n, { toJSON() { throw new Error('hook'); } }, getter, Symbol('symbol')];
  for (const value of values) {
    assert.doesNotThrow(() => h.sandbox.console.log(value));
    assert.doesNotThrow(() => h.listeners.unhandledrejection({ reason: value }));
  }
  assert.equal(h.native.length, values.length);
  values.forEach((value, i) => assert.equal(h.native[i][0], value));
  // Several unsupported values intentionally share the same bounded fallback
  // and are deduplicated. Each one independently remains reportable.
  for (const value of values) {
    const isolated = setup(); isolated.listeners.unhandledrejection({ reason: value });
    assert.equal(isolated.calls.length, 1);
  }
});
test('console formatting bounds traversal without invoking application getters or toJSON', () => {
  const h = setup(); let invoked = 0;
  const value = { toJSON() { invoked++; throw new Error('hook'); } };
  Object.defineProperty(value, 'getter', { enumerable: true, get() { invoked++; throw new Error('getter'); } });
  for (let i = 0; i < 100; i++) value[`field${i}`] = 'x'.repeat(10000);
  h.sandbox.console.warn(value); h.sandbox.observeErrors.captureMessage('inspect');
  assert.equal(invoked, 0); assert.equal(h.native.length, 1);
  assert.ok(h.calls[0].body.breadcrumbs[0].message.length <= 256);
});
test('network failures retry once and expose final diagnostic', async () => {
  const h = setup();
  // origFetch is captured at initialization, so initialize a fresh actual IIFE
  // with a rejected transport while preserving the harness globals.
  h.sandbox.fetch = () => Promise.reject(new Error('network'));
  vm.runInNewContext(src, h.sandbox);
  h.sandbox.observeErrors.captureMessage('network-test'); await Promise.resolve();
  assert.equal(h.sandbox.observeErrors.getDeliveryStatus().status, 'retrying');
  h.timers.shift()(); await Promise.resolve();
  assert.equal(h.sandbox.observeErrors.getDeliveryStatus().status, 'rejected');
  assert.equal(h.timers.length, 0);
});
