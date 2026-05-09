(function(){
  'use strict';

  var script = document.currentScript;
  if (!script) return;

  var origin = script.src ? new URL(script.src).origin : '';
  var endpoint = script.getAttribute('data-endpoint') || origin + '/api/v1/replays';
  var errorsEndpoint = script.getAttribute('data-errors-endpoint') || origin + '/api/v1/errors';
  var siteId = script.getAttribute('data-site-id') || '';
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

  function serializeNode(node) {
    if (node.nodeType === 3) {
      return { type: 'text', value: node.textContent };
    }
    if (node.nodeType !== 1) return null;

    var tag = node.tagName.toLowerCase();
    // Skip script and style content for privacy
    if (tag === 'script' || tag === 'noscript') return null;

    var attrs = {};
    for (var i = 0; i < node.attributes.length; i++) {
      var attr = node.attributes[i];
      // Skip event handlers and sensitive attrs
      if (attr.name.startsWith('on') || attr.name === 'value' && (tag === 'input' || tag === 'textarea')) continue;
      attrs[attr.name] = attr.value;
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

  function flush() {
    if (events.length === 0) return;
    var batch = events.splice(0);

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
      events: batch
    };

    var body = JSON.stringify(payload);
    if (navigator.sendBeacon) {
      navigator.sendBeacon(endpoint, new Blob([body], { type: 'application/json' }));
    } else {
      var xhr = new XMLHttpRequest();
      xhr.open('POST', endpoint, true);
      xhr.setRequestHeader('Content-Type', 'application/json');
      xhr.send(body);
    }
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
    var body = JSON.stringify(payload);
    if (navigator.sendBeacon) {
      navigator.sendBeacon(errorsEndpoint, new Blob([body], { type: 'application/json' }));
    } else {
      var xhr = new XMLHttpRequest();
      xhr.open('POST', errorsEndpoint, true);
      xhr.setRequestHeader('Content-Type', 'application/json');
      xhr.send(body);
    }
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
