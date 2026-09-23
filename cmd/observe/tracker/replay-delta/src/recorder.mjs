// O06 recorder: rrweb 2.1.6 wrapped in Observe's privacy stack
// (DELEGATED_DECISIONS 2026-09-23 section 2, the adopted integration
// boundary). rrweb.record's emit feeds the pure sanitizer
// (./sanitizer.mjs) BEFORE anything is buffered; sanitized events ride the
// SAME v2 producer/batch/chunk transport the structural recorder uses
// (envelope, byte-chunking, retry requeue, delivery generation, and
// consent-withdrawal discard are ported from cmd/observe/tracker/
// observe-replay.js — transport and storage are unchanged).
//
// Event stream shape: each sanitized rrweb event is wrapped as an Observe
// event {type: 'rrweb', timestamp, data: <sanitized rrweb event>}. The
// Observe-native events the server's aggregates consume are still recorded
// natively and ride the same batches: 'click' (heatmap attribution with
// page_url captured at click time, plus rage-click detection),
// 'navigation' (page_count), 'resize' (viewport bucket), and the
// has_error envelope flag. No Go-side change is needed for ingest.
//
// Bounding (contract point 4): rrweb sampling mousemove 50 ms / scroll
// 100 ms / input 'last' (the measured arm-B configuration), 30 s
// keyframes via checkoutEveryNms, the byte-chunk caps of the existing
// transport, and a max-events cap that STOPS recording when reached — a
// delta chain with silently dropped events replays a lie, so the honest
// bound is a full stop plus a terminal marker event. Captured script tags
// stay inert: rrweb serializes script text as a placeholder upstream and
// the sanitizer replaces script elements with opaque divs before
// anything leaves the browser; the player's sandboxed iframe (no
// allow-scripts) is the third layer.
//
// Built with esbuild (see ../package.json) into ../observe-replay-delta.js.
// rrweb is bundled — the ui package's dependencies are NOT touched.
import { record } from 'rrweb';
import { createEventSanitizer } from './sanitizer.mjs';

(function () {
  var script = document.currentScript;
  if (!script) return;

  function boundedInteger(raw, fallback, min, max) {
    var n = parseInt(raw, 10);
    if (!isFinite(n) || n < min) return fallback;
    if (n > max) return max;
    return n;
  }

  var origin = script.src ? new URL(script.src).origin : '';
  var endpoint = script.getAttribute('data-endpoint') || origin + '/api/v1/replays';
  var errorsEndpoint = script.getAttribute('data-errors-endpoint') || origin + '/api/v1/errors';
  var siteId = script.getAttribute('data-site-id') || '';
  // AUD-034 parity: every ingest request needs a valid key once the site
  // has any registered (APIKeyAuthMiddleware).
  var apiKey = script.getAttribute('data-api-key') || '';
  // Delta events are ~100x more numerous than structural snapshots, so the
  // default retention cap rides the ceiling of the old bound.
  var maxEvents = boundedInteger(script.getAttribute('data-max-events'), 50000, 1, 200000);
  var flushInterval = boundedInteger(script.getAttribute('data-flush-interval'), 10000, 500, 600000);
  var rageThreshold = boundedInteger(script.getAttribute('data-rage-threshold'), 4, 1, 100);
  var rageWindowMs = boundedInteger(script.getAttribute('data-rage-window'), 1000, 100, 60000);
  // Contract point 4: keyframe interval (rrweb checkout). 30 s matches the
  // structural recorder's re-snapshot cadence — keyframe parity is what
  // made the X05 session-bytes comparison honest.
  var checkoutEveryNms = boundedInteger(script.getAttribute('data-checkout-interval'), 30000, 5000, 600000);

  var events = [];
  var flushTimer = null;
  var sessionId = '';
  var hasError = false;

  var PROTOCOL_VERSION = 2;
  var producerId = makeReplayId();
  var chunkSeq = 0;
  var pendingChunks = [];

  var active = false;
  var capped = false;
  var stopRecording = null;
  var listeners = [];
  var origPush = null, wrappedPush = null;
  var origReplace = null, wrappedReplace = null;

  var sanitizer = createEventSanitizer({ baseURL: location.origin });

  function addListener(target, type, fn, opts) {
    var capture = typeof opts === 'boolean' ? opts : !!(opts && opts.capture);
    function guarded(event) { if (active) fn(event); }
    target.addEventListener(type, guarded, opts);
    listeners.push([target, type, guarded, capture]);
  }

  function makeReplayId() {
    var bytes = new Uint8Array(16);
    var c = window.crypto || window.msCrypto;
    if (c && c.getRandomValues) {
      c.getRandomValues(bytes);
    } else {
      for (var i = 0; i < 16; i++) bytes[i] = Math.floor(Math.random() * 256);
    }
    var hex = '';
    for (var j = 0; j < bytes.length; j++) {
      hex += (bytes[j] < 16 ? '0' : '') + bytes[j].toString(16);
    }
    return hex;
  }
  var replayId = makeReplayId();

  function track(type, data) {
    if (!active) return;
    if (events.length >= maxEvents) return;
    events.push({ type: type, timestamp: Date.now(), data: data });
  }

  function pageContext() {
    try {
      return {
        page_url: location.origin + location.pathname,
        viewport_width: window.innerWidth || 0
      };
    } catch (e) {
      return { page_url: '', viewport_width: 0 };
    }
  }

  // Called when the retention cap is reached: stop producing events (a
  // delta chain with holes replays corrupted DOM), mark the cap in the
  // stream so the truncation is visible, and let the transport drain.
  function capReached() {
    if (capped || !active) return;
    capped = true;
    if (stopRecording) {
      try { stopRecording(); } catch (e) { /* already stopped */ }
      stopRecording = null;
    }
    events.push({ type: 'recorder_capped', timestamp: Date.now(), data: { max_events: maxEvents } });
  }

  function init() {
    if (active) return;
    active = true;

    stopRecording = record({
      emit: function (event) {
        if (!active || capped) return;
        var sanitized = sanitizer.sanitize(event);
        if (!sanitized) return;
        if (events.length >= maxEvents) { capReached(); return; }
        events.push({ type: 'rrweb', timestamp: sanitized.timestamp || Date.now(), data: sanitized });
      },
      sampling: { mousemove: 50, scroll: 100, input: 'last' },
      checkoutEveryNms: checkoutEveryNms,
      // Belt-and-suspenders with the sanitizer (which drops input text on
      // its own): never serialize an input value even before the fold runs.
      maskAllInputs: true,
      recordCanvas: false,
      recordCrossOriginIframes: false
    }) || null;

    // Clicks (native: identical heatmap attribution + rage-click semantics
    // to the structural recorder, including page_url at click time).
    var rageState = { selector: '', clicks: [], reported: false };
    addListener(document, 'click', function (e) {
      var target = e.target;
      var tag = target.tagName ? target.tagName.toLowerCase() : '';
      var id = target.id ? '#' + target.id : '';
      var cls = target.className ? '.' + String(target.className).split(' ')[0] : '';
      var sel = tag + id + cls;
      var ctx = pageContext();
      track('click', { x: e.clientX, y: e.clientY, target: sel, page_url: ctx.page_url, viewport_width: ctx.viewport_width });

      var now = Date.now();
      if (rageState.selector !== sel) {
        rageState.selector = sel;
        rageState.clicks = [];
        rageState.reported = false;
      }
      var cutoff = now - rageWindowMs;
      while (rageState.clicks.length && rageState.clicks[0] < cutoff) {
        rageState.clicks.shift();
      }
      if (rageState.clicks.length === 0) {
        rageState.reported = false;
      }
      rageState.clicks.push(now);
      if (rageState.clicks.length >= rageThreshold && !rageState.reported) {
        rageState.reported = true;
        track('rage_click', { target: sel, count: rageState.clicks.length });
        reportRageClick(sel, target, rageState.clicks.length);
      }
    }, true);

    // Viewport resize (feeds the heatmap vw_bucket on the server).
    addListener(window, 'resize', function () {
      track('resize', { w: window.innerWidth, h: window.innerHeight });
    });

    // Navigation (feeds page_count). rrweb 2.x core emits no per-navigation
    // event, so the history wrappers stay.
    origPush = history.pushState;
    origReplace = history.replaceState;
    wrappedPush = function () {
      origPush.apply(this, arguments);
      track('navigation', { url: location.origin + location.pathname });
    };
    wrappedReplace = function () {
      origReplace.apply(this, arguments);
      track('navigation', { url: location.origin + location.pathname });
    };
    history.pushState = wrappedPush;
    history.replaceState = wrappedReplace;
    addListener(window, 'popstate', function () {
      track('navigation', { url: location.origin + location.pathname });
    });

    // Error flag (envelope has_error, error correlation).
    addListener(window, 'error', function () { hasError = true; });
    addListener(window, 'unhandledrejection', function () { hasError = true; });

    flushTimer = setInterval(flush, flushInterval);
    addListener(document, 'visibilitychange', function () {
      if (document.visibilityState === 'hidden') flush();
    });
  }

  // --- Transport (ported from observe-replay.js; audit comments there) ---

  var onErrorHook = function (err) {
    try { console.warn('observe-replay delivery failed:', err); } catch (e) { /* noop */ }
  };

  function byteLength(s) {
    if (typeof TextEncoder !== 'undefined') {
      return new TextEncoder().encode(s).length;
    }
    try {
      return new Blob([s]).size;
    } catch (e) {
      return s.length * 2;
    }
  }

  var MAX_REQUEST_BYTES = 1536 * 1024;
  var MAX_FLUSH_EVENTS = 1000;

  function deliver(url, payload, cb) {
    var body = JSON.stringify(payload);
    var small = byteLength(body) <= 48 * 1024;
    var headers = { 'Content-Type': 'application/json' };
    if (apiKey) headers['X-API-Key'] = apiKey;
    var settled = false;
    var done = function (err) {
      if (settled) return;
      settled = true;
      cb(err || null);
    };

    if (typeof fetch === 'function') {
      try {
        var controller = typeof AbortController !== 'undefined' ? new AbortController() : null;
        var abortFetch = controller
          ? function () { try { controller.abort(); } catch (e) { /* already settled */ } }
          : null;
        if (abortFetch) inFlightRequests.add(abortFetch);
        fetch(url, {
          method: 'POST',
          credentials: 'omit',
          redirect: 'error',
          headers: headers,
          body: body,
          keepalive: small,
          signal: controller ? controller.signal : undefined
        }).then(function (res) {
          if (abortFetch) inFlightRequests.delete(abortFetch);
          if (!res.ok) done(new Error('observe-replay: ingest returned ' + res.status));
          else done(null);
        }, function (err) {
          if (abortFetch) inFlightRequests.delete(abortFetch);
          done(err instanceof Error ? err : new Error(String(err)));
        });
        return;
      } catch (e) {
        // fall through to the beacon/XHR paths on very old browsers
      }
    }
    if (!apiKey && navigator.sendBeacon) {
      try {
        if (navigator.sendBeacon(url, new Blob([body], { type: 'application/json' }))) {
          cb(null);
          return;
        }
      } catch (e) { /* fall through to XHR */ }
    }
    var xhr = new XMLHttpRequest();
    var xhrAbort = function () { try { xhr.abort(); } catch (e) { /* noop */ } };
    inFlightRequests.add(xhrAbort);
    xhr.open('POST', url, true);
    for (var h in headers) {
      if (Object.prototype.hasOwnProperty.call(headers, h)) {
        try { xhr.setRequestHeader(h, headers[h]); } catch (e) { /* forbidden header name */ }
      }
    }
    var xhrDone = function (err) { inFlightRequests.delete(xhrAbort); done(err); };
    xhr.onload = function () {
      if (xhr.status >= 200 && xhr.status < 300) xhrDone(null);
      else xhrDone(new Error('observe-replay: ingest returned ' + xhr.status));
    };
    xhr.onerror = function () { xhrDone(new Error('observe-replay: network error')); };
    xhr.onabort = function () { xhrDone(new Error('observe-replay: aborted')); };
    try { xhr.send(body); } catch (e) { xhrDone(e); }
  }

  var flushInFlight = false;
  var deliveryGeneration = 0;
  var inFlightRequests = new Set();

  function discardDelivery() {
    deliveryGeneration++;
    inFlightRequests.forEach(function (abort) {
      try { abort(); } catch (e) { /* already settled */ }
    });
    inFlightRequests.clear();
    flushInFlight = false;
  }

  function makePayload(chunk) {
    var ctx = pageContext();
    var payload = {
      v: PROTOCOL_VERSION,
      producer_id: producerId,
      batch_id: chunk.id,
      site_id: siteId,
      session_id: sessionId,
      replay_id: replayId,
      url: ctx.page_url,
      browser: navigator.userAgent.substring(0, 128),
      os: '',
      device: '',
      has_error: hasError,
      viewport_width: ctx.viewport_width,
      events: chunk.events
    };
    var distinctId = readDistinctID();
    if (distinctId) payload.distinct_id = distinctId;
    return payload;
  }

  function chunkBatch(batch) {
    var chunks = [];
    var cur = [];
    var curBytes = 512;
    for (var i = 0; i < batch.length; i++) {
      var size = byteLength(JSON.stringify(batch[i])) + 1;
      if (cur.length >= MAX_FLUSH_EVENTS ||
          (curBytes + size > MAX_REQUEST_BYTES && cur.length > 0)) {
        chunks.push({ id: replayId + '-' + (++chunkSeq), events: cur });
        cur = [];
        curBytes = 512;
      }
      curBytes += size;
      cur.push(batch[i]);
    }
    if (cur.length) chunks.push({ id: replayId + '-' + (++chunkSeq), events: cur });
    return chunks;
  }

  function countChunkEvents(chunks, from) {
    var n = 0;
    for (var i = from; i < chunks.length; i++) n += chunks[i].events.length;
    return n;
  }

  function sendChunks(chunks, idx, gen, done) {
    if (gen !== deliveryGeneration) { done(null); return; }
    if (idx >= chunks.length) { done(null); return; }
    deliver(endpoint, makePayload(chunks[idx]), function (err) {
      if (gen !== deliveryGeneration) { done(null); return; }
      if (err) {
        for (var k = idx; k < chunks.length; k++) pendingChunks.push(chunks[k]);
        while (countChunkEvents(pendingChunks, 0) > maxEvents && pendingChunks.length > 1) {
          var dropped = pendingChunks.shift();
          onErrorHook(new Error('observe-replay: dropped ' + dropped.events.length +
            ' queued event(s) - retention cap reached'));
        }
        onErrorHook(err);
        done(err);
        return;
      }
      sendChunks(chunks, idx + 1, gen, done);
    });
  }

  function flush() {
    if (events.length === 0 && pendingChunks.length === 0) return;
    if (flushInFlight) return;
    var chunks = pendingChunks;
    pendingChunks = [];
    var batch = events.splice(0, MAX_FLUSH_EVENTS);
    if (batch.length) {
      var legal = [];
      for (var i = 0; i < batch.length; i++) {
        if (soloEventFits(batch[i])) {
          legal.push(batch[i]);
        } else {
          onErrorHook(new Error('observe-replay: dropped an event exceeding the request byte budget at flush time'));
        }
      }
      batch = legal;
      if (batch.length) chunks = chunks.concat(chunkBatch(batch));
    }
    if (!chunks.length) return;
    flushInFlight = true;
    var gen = deliveryGeneration;
    sendChunks(chunks, 0, gen, function () {
      if (gen === deliveryGeneration) flushInFlight = false;
    });
  }

  function soloEventFits(event) {
    var probe = { id: replayId + '-probe', events: [event] };
    try {
      return byteLength(JSON.stringify(makePayload(probe))) <= MAX_REQUEST_BYTES;
    } catch (e) {
      return false;
    }
  }

  function readDistinctID() {
    try { return localStorage.getItem('observe_distinct_id') || ''; }
    catch (e) { return ''; }
  }

  function ancestorPath(node) {
    var frames = [];
    var n = node;
    var depth = 0;
    while (n && n.nodeType === 1 && depth < 10) {
      var tag = n.tagName ? n.tagName.toLowerCase() : '';
      var id = n.id ? '#' + n.id : '';
      var cls = n.className ? '.' + String(n.className).split(' ').filter(Boolean).slice(0, 2).join('.') : '';
      frames.push({ filename: location.pathname, function: tag + id + cls, in_app: true });
      n = n.parentNode;
      depth++;
    }
    return frames;
  }

  function reportRageClick(selector, targetNode, count) {
    var payload = {
      site_id: siteId,
      session_id: sessionId,
      replay_id: replayId,
      error_type: 'RageClick',
      error_value: 'User clicked ' + count + ' times on ' + selector + ' without progress',
      mechanism: 'rage_click',
      handled: false,
      level: 'warning',
      url: location.origin + location.pathname,
      browser: (navigator.userAgent || '').substring(0, 256),
      os: '',
      device: '',
      stack_trace: ancestorPath(targetNode),
      breadcrumbs: [],
      selector: selector
    };
    var distinctId = readDistinctID();
    if (distinctId) payload.distinct_id = distinctId;
    deliver(errorsEndpoint, payload, function (err) { if (err) onErrorHook(err); });
  }

  // --- Public API (same surface as observe-replay.js so observe-errors.js
  // replay linking keeps working; a page runs ONE of the two recorders). ---

  var api = {
    start: init,
    stop: function (options) {
      if (!active) return;
      active = false;
      if (stopRecording) {
        try { stopRecording(); } catch (e) { /* already stopped */ }
        stopRecording = null;
      }
      for (var i = 0; i < listeners.length; i++) {
        try { listeners[i][0].removeEventListener(listeners[i][1], listeners[i][2], listeners[i][3]); }
        catch (e) { /* already gone */ }
      }
      listeners = [];
      if (flushTimer !== null) clearInterval(flushTimer);
      flushTimer = null;
      if (history.pushState === wrappedPush) history.pushState = origPush;
      if (history.replaceState === wrappedReplace) history.replaceState = origReplace;
      if (options && options.discard) {
        discardDelivery();
        events.length = 0;
        pendingChunks = [];
      } else {
        flush();
      }
    },
    setSessionId: function (id) { sessionId = id; },
    getReplayId: function () { return replayId; },
    onError: function (fn) { if (typeof fn === 'function') onErrorHook = fn; },
    mode: function () { return 'rrweb-delta'; }
  };
  window.observeReplay = api;
  window.observeReplayDelta = api;

  init();
})();
