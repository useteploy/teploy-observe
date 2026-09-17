(function(){
  'use strict';

  var script = document.currentScript;
  if (!script) return;

  var origin = script.src ? new URL(script.src).origin : '';
  var endpoint = script.getAttribute('data-endpoint') || origin + '/api/v1/replays';
  var errorsEndpoint = script.getAttribute('data-errors-endpoint') || origin + '/api/v1/errors';
  var siteId = script.getAttribute('data-site-id') || '';
  // Required once a site has any API key registered — APIKeyAuthMiddleware's
  // no-keys grace period closes permanently the moment one key exists
  // anywhere on the instance, and /api/v1/replays + /api/v1/errors both sit
  // behind it. Without this, every send() call below silently 401s and no
  // replay or rage-click data ever lands, on any site with a key configured.
  var apiKey = script.getAttribute('data-api-key') || '';
  var maxEvents = parseInt(script.getAttribute('data-max-events') || '5000', 10);
  var flushInterval = parseInt(script.getAttribute('data-flush-interval') || '10000', 10);
  // Rage click: N clicks on the same target within W ms.
  var rageThreshold = parseInt(script.getAttribute('data-rage-threshold') || '4', 10);
  var rageWindowMs = parseInt(script.getAttribute('data-rage-window') || '1000', 10);

  var events = [];
  var flushTimer = null;
  var sessionId = '';
  var hasError = false;

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

  var PRIVATE_TAG = /^(input|textarea|select|option|script|noscript|iframe|object|embed|style)$/;

  function isPrivateElement(el) {
    var tag = el.tagName.toLowerCase();
    if (PRIVATE_TAG.test(tag)) return true;
    if (el.isContentEditable) return true;
    if (el.hasAttribute && el.hasAttribute('data-observe-block')) return true;
    return false;
  }

  function serializeNode(node) {
    if (node.nodeType === 3) {
      return { type: 'text', value: node.textContent };
    }
    if (node.nodeType !== 1) return null;

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
      var child = serializeNode(node.childNodes[j]);
      if (child) children.push(child);
    }

    return { type: 'element', tag: tag, attrs: attrs, children: children };
  }

  function takeSnapshot() {
    return {
      doctype: document.doctype ? '<!DOCTYPE ' + document.doctype.name + '>' : '',
      html: serializeNode(document.documentElement)
    };
  }

  // --- Event Recording ---

  function record(type, data) {
    if (events.length >= maxEvents) return;
    events.push({
      type: type,
      timestamp: Date.now(),
      data: data
    });
  }

  // Full snapshot on start
  function init() {
    record('snapshot', takeSnapshot());

    // Mouse moves (throttled)
    var lastMove = 0;
    document.addEventListener('mousemove', function(e) {
      var now = Date.now();
      if (now - lastMove < 50) return;
      lastMove = now;
      record('mouse', { x: e.clientX, y: e.clientY });
    }, { passive: true });

    // Clicks (with rage-click detection: N clicks on same target within W ms).
    var rageState = { selector: '', clicks: [], reported: false };
    document.addEventListener('click', function(e) {
      var target = e.target;
      var tag = target.tagName ? target.tagName.toLowerCase() : '';
      var id = target.id ? '#' + target.id : '';
      var cls = target.className ? '.' + String(target.className).split(' ')[0] : '';
      var sel = tag + id + cls;
      record('click', { x: e.clientX, y: e.clientY, target: sel });

      var now = Date.now();
      if (rageState.selector !== sel) {
        rageState.selector = sel;
        rageState.clicks = [now];
        rageState.reported = false;
        return;
      }
      // Drop clicks outside the rage window.
      rageState.clicks.push(now);
      var cutoff = now - rageWindowMs;
      while (rageState.clicks.length && rageState.clicks[0] < cutoff) {
        rageState.clicks.shift();
      }
      if (rageState.clicks.length >= rageThreshold && !rageState.reported) {
        rageState.reported = true;
        record('rage_click', { target: sel, count: rageState.clicks.length });
        reportRageClick(sel, target, rageState.clicks.length);
      }
    }, true);

    // Scrolls (throttled)
    var lastScroll = 0;
    document.addEventListener('scroll', function() {
      var now = Date.now();
      if (now - lastScroll < 100) return;
      lastScroll = now;
      record('scroll', { x: window.scrollX, y: window.scrollY });
    }, { passive: true });

    // Input changes (mask values for privacy)
    document.addEventListener('input', function(e) {
      var target = e.target;
      if (!target || !target.tagName) return;
      var tag = target.tagName.toLowerCase();
      if (tag !== 'input' && tag !== 'textarea' && tag !== 'select') return;
      var id = target.id ? '#' + target.id : '';
      record('input', { target: tag + id, masked: true });
    }, true);

    // Viewport resize
    window.addEventListener('resize', function() {
      record('resize', { w: window.innerWidth, h: window.innerHeight });
    });

    // Navigation
    var origPush = history.pushState;
    var origReplace = history.replaceState;
    history.pushState = function() {
      origPush.apply(this, arguments);
      record('navigation', { url: location.href });
    };
    history.replaceState = function() {
      origReplace.apply(this, arguments);
      record('navigation', { url: location.href });
    };
    window.addEventListener('popstate', function() {
      record('navigation', { url: location.href });
    });

    // DOM mutations (simplified)
    if (typeof MutationObserver !== 'undefined') {
      var observer = new MutationObserver(function(mutations) {
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

    // Error detection
    window.addEventListener('error', function() { hasError = true; });
    window.addEventListener('unhandledrejection', function() { hasError = true; });

    // Periodic flush
    flushTimer = setInterval(flush, flushInterval);

    // Final flush on page hide
    document.addEventListener('visibilitychange', function() {
      if (document.visibilityState === 'hidden') flush();
    });
  }

  // sendBeacon can't carry custom headers, so it can never attach
  // X-API-Key — on any site with a key configured (the normal case once a
  // site is past the anonymous grace period) a beacon-only send would
  // silently 401 and the data would just vanish. fetch's keepalive flag is
  // the modern replacement: same "survives page unload" guarantee as
  // sendBeacon, but supports headers. Only fall back to sendBeacon when no
  // key is configured, matching the original behavior for grace-period
  // (single-tenant, no-keys-yet) installs.
  //
  // Audit F32: keepalive is only set for small bodies (the browser fails
  // any keepalive request over its 64 KiB in-flight budget, which large
  // snapshots used to hit), beacon queuing refusals fall through to fetch,
  // and fetch rejections are handled so they never surface as unhandled
  // promise rejections.
  function send(url, payload) {
    var body = JSON.stringify(payload);
    var small = body.length <= 48 * 1024;
    if (apiKey) {
      if (typeof fetch === 'function') {
        try {
          fetch(url, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json', 'X-API-Key': apiKey },
            body: body,
            keepalive: small
          }).catch(function() { /* best effort; replay batches are not idempotent */ });
          return;
        } catch (e) {
          // fall through to the no-key paths below on very old browsers
          // without fetch/keepalive support
        }
      }
    }
    if (navigator.sendBeacon) {
      try {
        if (navigator.sendBeacon(url, new Blob([body], { type: 'application/json' }))) {
          return;
        }
      } catch (e) { /* fall through to XHR */ }
    }
    var xhr = new XMLHttpRequest();
    xhr.open('POST', url, true);
    xhr.setRequestHeader('Content-Type', 'application/json');
    if (apiKey) xhr.setRequestHeader('X-API-Key', apiKey);
    xhr.send(body);
  }

  function flush() {
    if (events.length === 0) return;
    // Audit F32: the ingest route caps request bodies at 2 MiB; a long
    // session's batch (up to maxEvents records with DOM snapshots) could
    // exceed it and be rejected whole. Flush in bounded chunks.
    var MAX_FLUSH_EVENTS = 1000;
    var batch = events.splice(0, MAX_FLUSH_EVENTS);

    var makePayload = function(chunk) {
      var payload = {
        site_id: siteId,
        session_id: sessionId,
        replay_id: replayId,
        url: location.href,
        browser: navigator.userAgent.substring(0, 128),
        os: '',
        device: '',
        has_error: hasError,
        viewport_width: window.innerWidth || 0,
        events: chunk
      };
      var distinctId = readDistinctID();
      if (distinctId) payload.distinct_id = distinctId;
      return payload;
    };

    send(endpoint, makePayload(batch));
    // Any remainder rides the regular interval tick set up in init().
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
      url: location.href,
      browser: (navigator.userAgent || '').substring(0, 256),
      os: '',
      device: '',
      stack_trace: ancestorPath(targetNode),
      breadcrumbs: [],
      selector: selector
    };
    var distinctId = readDistinctID();
    if (distinctId) payload.distinct_id = distinctId;
    send(errorsEndpoint, payload);
  }

  // --- Public API ---
  window.observeReplay = {
    start: init,
    stop: function() {
      if (flushTimer) clearInterval(flushTimer);
      flush();
    },
    setSessionId: function(id) { sessionId = id; },
    getReplayId: function() { return replayId; }
  };

  init();
})();
