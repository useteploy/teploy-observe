(function(){
  'use strict';

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
  // Required once a site has any API key registered — APIKeyAuthMiddleware
  // requires a valid key for every ingest request (the old no-keys grace
  // path is gone, AUD-002), and /api/v1/replays + /api/v1/errors both sit
  // behind it.
  var apiKey = script.getAttribute('data-api-key') || '';
  // AUD-034: numeric attributes are parsed with explicit bounds — a typo'd
  // data-max-events="lots" used to yield NaN, which disabled every cap.
  var maxEvents = boundedInteger(script.getAttribute('data-max-events'), 5000, 1, 50000);
  var flushInterval = boundedInteger(script.getAttribute('data-flush-interval'), 10000, 500, 600000);
  // Rage click: N clicks on the same target within W ms.
  var rageThreshold = boundedInteger(script.getAttribute('data-rage-threshold'), 4, 1, 100);
  var rageWindowMs = boundedInteger(script.getAttribute('data-rage-window'), 1000, 100, 60000);
  // F39 (the smaller step): mid-session re-snapshots. The player replays
  // interactions over the initial snapshot, so a long session on a changing
  // page drifts indefinitely. A fresh snapshot is recorded periodically
  // and early when a mutation burst hits the threshold — both throttled by
  // the minimum gap so a churning page cannot snapshot itself to death.
  // This is NOT a DOM-delta protocol; the full design stays deferred.
  var resnapshotIntervalMs = boundedInteger(script.getAttribute('data-resnapshot-interval'), 30000, 5000, 600000);
  var resnapshotMinGapMs = boundedInteger(script.getAttribute('data-resnapshot-min-gap'), 10000, 1000, 600000);
  var resnapshotBurst = boundedInteger(script.getAttribute('data-resnapshot-burst'), 150, 10, 5000);

  var events = [];
  var flushTimer = null;
  var sessionId = '';
  var hasError = false;

  // F12/F19 (protocol v2): producer identity for this recorder instance and
  // a monotonic sequence numbering the chunks this recorder detaches. A
  // chunk keeps its id when requeued, so a retry after a lost response is
  // recognized by the server's batch ledger instead of double-counted.
  var PROTOCOL_VERSION = 2;
  var producerId = makeReplayId();
  var chunkSeq = 0;
  // Failed chunks awaiting retry, as {id, events} - identity intact.
  var pendingChunks = [];

  // AUD-027: explicit lifecycle. `active` gates every capture path — a
  // stopped recorder's listeners/wrappers become inert instead of quietly
  // continuing to record, and repeated start() cannot stack duplicate
  // listeners, observers, wrappers, and timers.
  var active = false;
  var listeners = [];       // [target, type, fn] triples to remove on stop
  var observer = null;      // MutationObserver to disconnect on stop
  var origPush = null, wrappedPush = null;
  var origReplace = null, wrappedReplace = null;

  function addListener(target, type, fn, opts) {
    target.addEventListener(type, fn, opts);
    listeners.push([target, type, fn]);
  }

  // Local replay id — generated client-side so observe-errors.js can attach
  // it to errors captured before the first replay batch reaches the server.
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

  // --- DOM Snapshot ---
  //
  // Audit F37: the serializer used to copy every text node verbatim and
  // every attribute except on* and input/textarea value. A textarea's
  // INITIAL content is a child text node, so pre-filled private messages
  // were recorded despite the advertised input masking; contenteditable
  // regions and data-* attributes (often tokens) leaked the same way.
  //
  // Policy now: form controls (input/textarea/select/option) and
  // script-bearing elements serialize as opaque placeholders; text inside
  // contenteditable or an element marked data-observe-block is masked;
  // data-* attributes are never copied. Ordinary visible text in the rest
  // of the DOM is still recorded — default-masking ALL text is a product
  // decision recorded in AUDIT_OPEN.md, not something to slip into a bug
  // fix. A subtree can opt into masking with data-observe-block; there is
  // deliberately no opt-OUT from blocking.
  //
  // AUD-031 (round 2): the serializer is bounded — total nodes, depth, and
  // text length — so a huge or pathologically deep page cannot stall the
  // recorder. Bounds are enforced per snapshot, not per element.

  var PRIVATE_TAG = /^(input|textarea|select|option|script|noscript|iframe|object|embed|style)$/;

  var MAX_SNAPSHOT_NODES = 15000;
  var MAX_SNAPSHOT_DEPTH = 32;
  var MAX_TEXT_LENGTH = 2048;

  function isPrivateElement(el) {
    var tag = el.tagName.toLowerCase();
    if (PRIVATE_TAG.test(tag)) return true;
    if (el.isContentEditable) return true;
    if (el.hasAttribute && el.hasAttribute('data-observe-block')) return true;
    return false;
  }

  var snapshotBudget = 0;

  function serializeNode(node, depth) {
    if (snapshotBudget <= 0 || depth > MAX_SNAPSHOT_DEPTH) return null;
    if (node.nodeType === 3) {
      snapshotBudget--;
      var text = node.textContent || '';
      if (text.length > MAX_TEXT_LENGTH) text = text.slice(0, MAX_TEXT_LENGTH);
      return { type: 'text', value: text };
    }
    if (node.nodeType !== 1) return null;

    snapshotBudget--;
    var tag = node.tagName.toLowerCase();
    if (tag === 'script' || tag === 'noscript') return null;

    if (isPrivateElement(node)) {
      return { type: 'element', tag: 'div', attrs: {}, children: [] };
    }

    var attrs = {};
    for (var i = 0; i < node.attributes.length; i++) {
      var attr = node.attributes[i];
      var name = attr.name;
      // Skip event handlers, data-* attributes (token-bearing), and the
      // value attribute anywhere it can hold user input.
      if (name.lastIndexOf('on', 0) === 0) continue;
      if (name.lastIndexOf('data-', 0) === 0) continue;
      if (name === 'value') continue;
      attrs[name] = attr.value;
    }

    var children = [];
    for (var j = 0; j < node.childNodes.length && children.length < 200; j++) {
      var child = serializeNode(node.childNodes[j], depth + 1);
      if (child) children.push(child);
    }

    return { type: 'element', tag: tag, attrs: attrs, children: children };
  }

  function takeSnapshot() {
    snapshotBudget = MAX_SNAPSHOT_NODES;
    return {
      doctype: document.doctype ? '<!DOCTYPE ' + document.doctype.name + '>' : '',
      html: serializeNode(document.documentElement, 0)
    };
  }

  // --- Event Recording ---

  function record(type, data) {
    // AUD-027: capture is gated on `active` — a stopped recorder must not
    // accept events from wrappers still unwinding in the stack.
    if (!active) return;
    if (events.length >= maxEvents) return;
    events.push({
      type: type,
      timestamp: Date.now(),
      data: data
    });
  }

  // AUD-033 (round 2): clicks carry the page URL and viewport captured AT
  // CLICK TIME — the server groups heatmap attribution by this, so a click
  // followed by SPA navigation is no longer credited to the post-navigation
  // page observed at flush time. Sanitized to origin+path (no query or
  // fragment material ever leaves the page).
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

  // --- F39: throttled mid-session re-snapshot ---
  //
  // A fresh snapshot is recorded when the interval elapsed OR a mutation
  // burst hit the threshold, whichever comes first — but never more often
  // than the minimum gap. Snapshots are the heaviest event in a batch
  // (bounded by the serializer's node/depth/text budgets), so the throttle
  // bounds the overhead; the player already selects the most recent
  // keyframe at or before the playhead, so these snapshots are what keeps
  // a long replay from drifting off a single initial DOM.
  var lastSnapshotAt = 0;
  var mutationsSinceSnapshot = 0;
  var resnapshotTimer = null;

  function maybeResnapshot() {
    if (!active) return;
    var now = Date.now();
    var intervalElapsed = now - lastSnapshotAt >= resnapshotIntervalMs;
    var burstHit = mutationsSinceSnapshot >= resnapshotBurst;
    if (!intervalElapsed && !burstHit) return;
    if (now - lastSnapshotAt < resnapshotMinGapMs) return;
    record('snapshot', takeSnapshot());
    lastSnapshotAt = now;
    mutationsSinceSnapshot = 0;
  }

  // Full snapshot on start
  function init() {
    if (active) return; // AUD-027: idempotent start
    active = true;

    record('snapshot', takeSnapshot());
    lastSnapshotAt = Date.now();
    mutationsSinceSnapshot = 0;

    // Mouse moves (throttled)
    var lastMove = 0;
    addListener(document, 'mousemove', function(e) {
      var now = Date.now();
      if (now - lastMove < 50) return;
      lastMove = now;
      record('mouse', { x: e.clientX, y: e.clientY });
    }, { passive: true });

    // Clicks (with rage-click detection: N clicks on same target within W ms).
    var rageState = { selector: '', clicks: [], reported: false };
    addListener(document, 'click', function(e) {
      var target = e.target;
      var tag = target.tagName ? target.tagName.toLowerCase() : '';
      var id = target.id ? '#' + target.id : '';
      var cls = target.className ? '.' + String(target.className).split(' ')[0] : '';
      var sel = tag + id + cls;
      var ctx = pageContext();
      record('click', { x: e.clientX, y: e.clientY, target: sel, page_url: ctx.page_url, viewport_width: ctx.viewport_width });

      // AUD-034 (round 2): an emptied time window starts a NEW burst — the
      // `reported` flag used to reset only on a selector change, so a
      // second independent rage burst on the same element was never
      // reported, and threshold=1 never fired on its first click.
      var now = Date.now();
      if (rageState.selector !== sel) {
        rageState.selector = sel;
        rageState.clicks = [];
        rageState.reported = false;
      }
      rageState.clicks.push(now);
      var cutoff = now - rageWindowMs;
      while (rageState.clicks.length && rageState.clicks[0] < cutoff) {
        rageState.clicks.shift();
      }
      if (!rageState.clicks.length) {
        rageState.reported = false;
      }
      if (rageState.clicks.length >= rageThreshold && !rageState.reported) {
        rageState.reported = true;
        record('rage_click', { target: sel, count: rageState.clicks.length });
        reportRageClick(sel, target, rageState.clicks.length);
      }
    }, true);

    // Scrolls (throttled)
    var lastScroll = 0;
    addListener(document, 'scroll', function() {
      var now = Date.now();
      if (now - lastScroll < 100) return;
      lastScroll = now;
      record('scroll', { x: window.scrollX, y: window.scrollY });
    }, { passive: true });

    // Input changes (mask values for privacy)
    addListener(document, 'input', function(e) {
      var target = e.target;
      if (!target || !target.tagName) return;
      var tag = target.tagName.toLowerCase();
      if (tag !== 'input' && tag !== 'textarea' && tag !== 'select') return;
      var id = target.id ? '#' + target.id : '';
      record('input', { target: tag + id, masked: true });
    }, true);

    // Viewport resize
    addListener(window, 'resize', function() {
      record('resize', { w: window.innerWidth, h: window.innerHeight });
    });

    // Navigation. Wrappers check `active` and restore the originals on
    // stop without clobbering a wrapper another library installed later.
    origPush = history.pushState;
    origReplace = history.replaceState;
    wrappedPush = function() {
      origPush.apply(this, arguments);
      record('navigation', { url: location.origin + location.pathname });
    };
    wrappedReplace = function() {
      origReplace.apply(this, arguments);
      record('navigation', { url: location.origin + location.pathname });
    };
    history.pushState = wrappedPush;
    history.replaceState = wrappedReplace;
    addListener(window, 'popstate', function() {
      record('navigation', { url: location.origin + location.pathname });
    });

    // DOM mutations (simplified)
    if (typeof MutationObserver !== 'undefined') {
      observer = new MutationObserver(function(mutations) {
        // F39: count mutations toward the re-snapshot burst threshold.
        mutationsSinceSnapshot += mutations.length;
        if (mutationsSinceSnapshot >= resnapshotBurst) {
          maybeResnapshot();
        }
        for (var i = 0; i < Math.min(mutations.length, 10); i++) {
          var m = mutations[i];
          if (m.type === 'childList' && m.addedNodes.length > 0) {
            record('mutation', { type: 'add', count: m.addedNodes.length });
          } else if (m.type === 'attributes') {
            var t = m.target;
            record('mutation', {
              type: 'attr',
              target: t.tagName ? t.tagName.toLowerCase() + (t.id ? '#' + t.id : '') : '',
              attr: m.attributeName
            });
          }
        }
      });
      observer.observe(document.body, {
        childList: true, subtree: true, attributes: true,
        attributeFilter: ['class', 'style', 'hidden', 'disabled']
      });
    }

    // F39: periodic re-snapshot check rides the flush timer's cadence —
    // the clock condition is evaluated against wall time, not the tick
    // count, so a custom flush interval never changes the snapshot policy.
    resnapshotTimer = setInterval(maybeResnapshot, 1000);

    // Error detection
    addListener(window, 'error', function() { hasError = true; });
    addListener(window, 'unhandledrejection', function() { hasError = true; });

    // Periodic flush
    flushTimer = setInterval(flush, flushInterval);

    // Final flush on page hide
    addListener(document, 'visibilitychange', function() {
      if (document.visibilityState === 'hidden') flush();
    });
  }

  // --- Transport ---
  //
  // AUD-024 (round 2): batches are detached only when the request they
  // belong to is actually accepted. The fetch branch never checked res.ok,
  // the XHR branch had no completion handling at all, and sendBeacon's
  // "queued" return was treated as server acceptance — 401/413/429/5xx and
  // offline all used to lose the already-spliced records. Failed chunks
  // are requeued (bounded by maxEvents) and reported through the
  // error hook.
  //
  // AUD-025 (round 2): sendBeacon can NEVER carry X-API-Key, so it is only
  // attempted for keyless installs — an authenticated configuration goes
  // straight to fetch/XHR when fetch is unavailable instead of silently
  // 401ing through a beacon.
  //
  // AUD-026 (round 2): chunk boundaries are computed from encoded BYTES,
  // not event counts or UTF-16 code units — a snapshot alone can exceed
  // the route's 2 MiB cap.

  var onErrorHook = function(err) {
    try { console.warn('observe-replay delivery failed:', err); } catch (e) { /* noop */ }
  };

  function byteLength(s) {
    if (typeof TextEncoder !== 'undefined') {
      return new TextEncoder().encode(s).length;
    }
    try {
      return new Blob([s]).size;
    } catch (e) {
      return s.length * 2; // conservative worst case for non-ASCII
    }
  }

  // 1.5 MiB per request: under the ingest route's 2 MiB cap with room for
  // the envelope metadata every chunk carries.
  var MAX_REQUEST_BYTES = 1536 * 1024;
  var MAX_FLUSH_EVENTS = 1000;

  function deliver(url, payload, cb) {
    var body = JSON.stringify(payload);
    var small = byteLength(body) <= 48 * 1024;
    var headers = { 'Content-Type': 'application/json' };
    if (apiKey) headers['X-API-Key'] = apiKey;

    if (typeof fetch === 'function') {
      try {
        fetch(url, {
          method: 'POST',
          credentials: 'omit',
          redirect: 'error',
          headers: headers,
          body: body,
          keepalive: small
        }).then(function(res) {
          if (!res.ok) cb(new Error('observe-replay: ingest returned ' + res.status));
          else cb(null);
        }, function(err) {
          cb(err instanceof Error ? err : new Error(String(err)));
        });
        return;
      } catch (e) {
        // fall through to the beacon/XHR paths on very old browsers
      }
    }
    // Beacon only for keyless installs: it cannot carry the API key
    // header, so "successfully queued" would still be a guaranteed 401.
    if (!apiKey && navigator.sendBeacon) {
      try {
        if (navigator.sendBeacon(url, new Blob([body], { type: 'application/json' }))) {
          cb(null);
          return;
        }
      } catch (e) { /* fall through to XHR */ }
    }
    var xhr = new XMLHttpRequest();
    xhr.open('POST', url, true);
    for (var h in headers) {
      if (Object.prototype.hasOwnProperty.call(headers, h)) {
        try { xhr.setRequestHeader(h, headers[h]); } catch (e) { /* forbidden header name */ }
      }
    }
    xhr.onload = function() {
      if (xhr.status >= 200 && xhr.status < 300) cb(null);
      else cb(new Error('observe-replay: ingest returned ' + xhr.status));
    };
    xhr.onerror = function() { cb(new Error('observe-replay: network error')); };
    try { xhr.send(body); } catch (e) { cb(e); }
  }

  var flushInFlight = false;

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

  // Split a batch into request-legal chunks by encoded bytes AND count.
  // Each chunk is born with a stable id (F12/F19): retries of a failed
  // chunk re-send under the SAME id so the server dedupes them.
  function chunkBatch(batch) {
    var chunks = [];
    var cur = [];
    var curBytes = 512; // envelope overhead floor
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

  function sendChunks(chunks, idx, done) {
    if (idx >= chunks.length) { done(null); return; }
    deliver(endpoint, makePayload(chunks[idx]), function(err) {
      if (err) {
        // Requeue this chunk (identity intact) and every chunk not yet
        // attempted; already accepted chunks stay sent. Bounded by
        // maxEvents: the oldest pending chunk events are dropped and
        // reported when a long outage exceeds the cap.
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
      sendChunks(chunks, idx + 1, done);
    });
  }

  function flush() {
    if (events.length === 0 && pendingChunks.length === 0) return;
    if (flushInFlight) return; // one owned batch at a time
    // Previously-failed chunks retry FIRST and under their original ids -
    // the batch is the unit of idempotency, so its composition must never
    // be reshuffled between attempts.
    var chunks = pendingChunks;
    pendingChunks = [];
    var batch = events.splice(0, MAX_FLUSH_EVENTS);
    if (batch.length) chunks = chunks.concat(chunkBatch(batch));
    flushInFlight = true;
    sendChunks(chunks, 0, function() {
      flushInFlight = false;
    });
    // Any remainder rides the interval tick set up in init().
  }

  // Read the distinct_id set by observe.js's identify(). Lives in
  // localStorage under 'observe_distinct_id'. Empty when no identify
  // has been called.
  function readDistinctID() {
    try { return localStorage.getItem('observe_distinct_id') || ''; }
    catch (e) { return ''; }
  }

  // Build a breadcrumb-like ancestor path for a click target. Used as a
  // pseudo-stack on auto-issued RageClicks (no JS stack exists otherwise).
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

  // POST a synthetic "RageClick" issue to the errors ingest endpoint.
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
    deliver(errorsEndpoint, payload, onErrorHook);
  }

  // --- Public API ---

  window.observeReplay = {
    start: init,
    // AUD-027: stop actually stops collection — listeners removed,
    // observer disconnected, history wrappers restored, timer cleared.
    // stop({discard: true}) withdraws pending capture entirely (consent
    // withdrawal); plain stop() flushes what was recorded.
    stop: function(options) {
      if (!active) return;
      active = false; // first: wrappers/observers still in the stack go inert
      for (var i = 0; i < listeners.length; i++) {
        try { listeners[i][0].removeEventListener(listeners[i][1], listeners[i][2]); }
        catch (e) { /* already gone */ }
      }
      listeners = [];
      if (observer) {
        try { observer.disconnect(); } catch (e) { /* noop */ }
        observer = null;
      }
      if (flushTimer !== null) clearInterval(flushTimer);
      flushTimer = null;
      if (resnapshotTimer !== null) clearInterval(resnapshotTimer);
      resnapshotTimer = null;
      if (history.pushState === wrappedPush) history.pushState = origPush;
      if (history.replaceState === wrappedReplace) history.replaceState = origReplace;
      if (options && options.discard) {
        events.length = 0;
        pendingChunks = [];
      } else {
        flush();
      }
    },
    setSessionId: function(id) { sessionId = id; },
    getReplayId: function() { return replayId; },
    onError: function(fn) { if (typeof fn === 'function') onErrorHook = fn; }
  };

  init();
})();
