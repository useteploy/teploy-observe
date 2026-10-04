(function(){
  'use strict';

  // Opt-in console and network capture for session replay. Loaded as its own
  // script next to a replay recorder; records ride the recorder's existing v2
  // batch transport as custom 'console' / 'network' events through
  // window.observeReplay.pushEvent (the recorder owns delivery, caps on the
  // wire, consent withdrawal and discard).
  //
  // Privacy boundary (audited allowlist policy): both captures are OFF unless
  // the script tag sets data-console="true" / data-network="true". Console
  // args are stringified with depth/size limits, scrubbed against a secret
  // denylist and cut to 1 KiB. Network records carry method, URL with query,
  // fragment and credentials removed, status, duration and size - never
  // bodies and never headers. Patches are reversible (stop()) and never throw
  // into the host page. The server re-validates and re-scrubs every record
  // (internal/replays/capture.go keeps the denylist in lockstep).

  var script = document.currentScript;
  if (!script) return;

  var consoleOn = script.getAttribute('data-console') === 'true';
  var networkOn = script.getAttribute('data-network') === 'true';
  if (!consoleOn && !networkOn) return; // default OFF: install nothing

  function boundedInteger(raw, fallback, min, max) {
    var n = parseInt(raw, 10);
    if (!isFinite(n) || n < min) return fallback;
    if (n > max) return max;
    return n;
  }

  var MAX_MESSAGE_BYTES = 1024;
  var MAX_URL_BYTES = 2048;
  var MAX_ARG_DEPTH = 3;
  var MAX_ARG_KEYS = 20;
  var MAX_ARG_ITEMS = 20;
  var MAX_ARG_CHARS = 2048; // working size per arg before the final cut
  // Server batch caps are 200 / 500 per batch; these are the per-session caps.
  var maxConsole = boundedInteger(script.getAttribute('data-max-console'), 200, 1, 200);
  var maxNetwork = boundedInteger(script.getAttribute('data-max-network'), 500, 1, 500);

  var origin = script.src ? new URL(script.src).origin : '';

  var active = true;
  var consoleCount = 0;
  var networkCount = 0;
  var busy = false; // reentrancy guard: never capture our own work
  var LEVELS = ['log', 'info', 'warn', 'error'];

  // --- Scrubbing (mirror of internal/replays/capture.go scrubPatterns) ---
  var SCRUBS = [
    [/\b(Bearer|Basic)\s+[A-Za-z0-9._~+\/=-]{8,}/gi, '$1 [redacted]'],
    [/\beyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]*/g, '[redacted-jwt]'],
    [/((?:pass(?:word|wd)?|secret|token|api[_-]?key|authorization|auth|credential|session(?:id)?|cookie|private[_-]?key)["']?\s*[:=]\s*)("[^"]*"|'[^']*'|[^\s,;&}]+)/gi, '$1[redacted]'],
    [/\b[A-Fa-f0-9]{32,}\b/g, '[redacted-hex]'],
    [/\b[A-Za-z0-9_-]{40,}\b/g, '[redacted-token]']
  ];

  function scrub(s) {
    for (var i = 0; i < SCRUBS.length; i++) s = s.replace(SCRUBS[i][0], SCRUBS[i][1]);
    return s;
  }

  function utf8Length(s) {
    var n = 0;
    for (var i = 0; i < s.length; i++) {
      var c = s.charCodeAt(i);
      if (c < 0x80) n += 1;
      else if (c < 0x800) n += 2;
      else if (c >= 0xd800 && c <= 0xdbff) { n += 4; i++; }
      else n += 3;
    }
    return n;
  }

  // Cut to at most maxBytes of UTF-8 without splitting a code point.
  function cutBytes(s, maxBytes) {
    if (utf8Length(s) <= maxBytes) return s;
    var out = '';
    var n = 0;
    for (var i = 0; i < s.length; i++) {
      var c = s.charCodeAt(i);
      var step = 1;
      var w = c < 0x80 ? 1 : c < 0x800 ? 2 : 3;
      if (c >= 0xd800 && c <= 0xdbff) { w = 4; step = 2; }
      if (n + w > maxBytes) break;
      out += s.substr(i, step);
      n += w;
      i += step - 1;
    }
    return out;
  }

  // --- Console argument stringification (depth/size bounded, cycle safe) ---
  function str(v, depth, seen) {
    try {
      if (v === null) return 'null';
      var t = typeof v;
      if (t === 'undefined') return 'undefined';
      if (t === 'string') return v;
      if (t === 'number' || t === 'boolean' || t === 'bigint') return String(v);
      if (t === 'symbol') return String(v.description || 'symbol');
      if (t === 'function') return '[Function ' + (v.name || 'anonymous') + ']';
      if (v instanceof Error) return (v.name || 'Error') + ': ' + (v.message || '');
      if (typeof Node !== 'undefined' && v instanceof Node) return '[Node ' + (v.nodeName || '') + ']';
      if (depth >= MAX_ARG_DEPTH) return Array.isArray(v) ? '[Array]' : '[Object]';
      if (seen.indexOf(v) !== -1) return '[Circular]';
      seen.push(v);
      var parts = [];
      var i;
      if (Array.isArray(v)) {
        for (i = 0; i < v.length && i < MAX_ARG_ITEMS; i++) parts.push(str(v[i], depth + 1, seen));
        if (v.length > MAX_ARG_ITEMS) parts.push('...');
        seen.pop();
        return '[' + parts.join(', ') + ']';
      }
      var keys = Object.keys(v);
      for (i = 0; i < keys.length && i < MAX_ARG_KEYS; i++) {
        var val;
        try { val = v[keys[i]]; } catch (e) { val = '[Unreadable]'; }
        parts.push(keys[i] + ': ' + str(val, depth + 1, seen));
      }
      if (keys.length > MAX_ARG_KEYS) parts.push('...');
      seen.pop();
      return '{' + parts.join(', ') + '}';
    } catch (e) {
      return '[Unserializable]';
    }
  }

  function formatArgs(args) {
    var out = '';
    for (var i = 0; i < args.length && i < 10; i++) {
      var s = str(args[i], 0, []);
      if (s.length > MAX_ARG_CHARS) s = s.substring(0, MAX_ARG_CHARS);
      out += (i ? ' ' : '') + s;
      if (out.length > MAX_ARG_CHARS * 2) break;
    }
    return out;
  }

  // --- Sink: the recorder's pushEvent. Absent recorder => the record drops. ---
  function emit(type, data) {
    try {
      var r = window.observeReplay;
      if (r && typeof r.pushEvent === 'function') return r.pushEvent(type, data) !== false;
    } catch (e) { /* the host page must never see capture failures */ }
    return false;
  }

  // --- Console patch ---
  var consoleOrig = {};
  var consoleWrap = {};

  function recordConsole(level, args) {
    if (!active || busy || consoleCount >= maxConsole) return;
    busy = true;
    try {
      var raw = formatArgs(args);
      // The recorder's own diagnostics must not feed back into the stream.
      if (raw.indexOf('observe-replay') === 0) return;
      var scrubbed = scrub(raw);
      var message = cutBytes(scrubbed, MAX_MESSAGE_BYTES);
      var data = { level: level, message: message };
      if (message.length !== scrubbed.length || raw.length >= MAX_ARG_CHARS) data.truncated = true;
      if (emit('console', data)) consoleCount++;
    } catch (e) { /* never throw into the page */ } finally { busy = false; }
  }

  function patchConsole() {
    if (typeof console === 'undefined' || !console) return;
    LEVELS.forEach(function (level) {
      var orig = console[level];
      if (typeof orig !== 'function') return;
      var wrapped = function () {
        try { recordConsole(level, arguments); } catch (e) { /* noop */ }
        return orig.apply(this, arguments);
      };
      consoleOrig[level] = orig;
      consoleWrap[level] = wrapped;
      try { console[level] = wrapped; } catch (e) { delete consoleOrig[level]; delete consoleWrap[level]; }
    });
  }

  // --- Network capture ---
  var OPAQUE = /^[A-Za-z0-9_-]{24,}$/;

  // Absolute http(s) URL, no credentials/query/fragment, opaque path
  // segments masked. Returns '' when the URL cannot be reported.
  function cleanURL(raw) {
    try {
      var u = new URL(String(raw), location.href);
      if (u.protocol !== 'http:' && u.protocol !== 'https:') return '';
      var segs = u.pathname.split('/');
      for (var i = 0; i < segs.length; i++) {
        if (OPAQUE.test(segs[i]) && /[0-9]/.test(segs[i])) segs[i] = ':redacted';
      }
      var out = u.protocol + '//' + u.host + segs.join('/');
      return utf8Length(out) > MAX_URL_BYTES ? '' : out;
    } catch (e) {
      return '';
    }
  }

  function isOwnTraffic(url) {
    // Observe's own ingest calls (replay batches, errors) are not host traffic.
    if (origin && url.indexOf(origin + '/api/v1/') === 0) return true;
    var custom = cleanURL(script.getAttribute('data-endpoint') || '');
    return !!custom && url.indexOf(custom) === 0;
  }

  function recordNetwork(kind, method, rawURL, status, startedAt, size) {
    if (!active || busy || networkCount >= maxNetwork) return;
    busy = true;
    try {
      var url = cleanURL(rawURL);
      if (!url || isOwnTraffic(url)) return;
      method = String(method || 'GET').toUpperCase();
      if (!/^[A-Z]{1,16}$/.test(method)) return;
      var dur = Math.max(0, Math.min(3600000, Math.round(Date.now() - startedAt)));
      var data = { method: method, url: url, status: status > 0 && status < 1000 ? Math.floor(status) : 0, duration_ms: dur, kind: kind };
      if (typeof size === 'number' && isFinite(size) && size >= 0) data.size = Math.floor(size);
      if (emit('network', data)) networkCount++;
    } catch (e) { /* noop */ } finally { busy = false; }
  }

  function lengthHeader(h) {
    var n = parseInt(h, 10);
    return isFinite(n) && n >= 0 ? n : undefined;
  }

  var origFetch = null, wrappedFetch = null;
  function patchFetch() {
    if (typeof window.fetch !== 'function') return;
    origFetch = window.fetch;
    wrappedFetch = function (input, init) {
      var started = Date.now();
      var meta = null;
      try {
        var isReq = typeof Request !== 'undefined' && input instanceof Request;
        meta = {
          url: isReq ? input.url : String(input),
          method: (init && init.method) || (isReq && input.method) || 'GET'
        };
      } catch (e) { meta = null; }
      var p = origFetch.apply(this, arguments);
      if (meta && p && typeof p.then === 'function') {
        try {
          p.then(function (res) {
            var size;
            try { size = lengthHeader(res.headers && res.headers.get('content-length')); } catch (e) { /* noop */ }
            recordNetwork('fetch', meta.method, meta.url, res && res.status, started, size);
          }, function () {
            recordNetwork('fetch', meta.method, meta.url, 0, started);
          });
        } catch (e) { /* noop */ }
      }
      return p;
    };
    window.fetch = wrappedFetch;
  }

  var xhrProto = null;
  var origOpen = null, origSend = null, wrappedOpen = null, wrappedSend = null;
  var XHR_META = typeof WeakMap === 'function' ? new WeakMap() : null;
  function patchXHR() {
    if (typeof XMLHttpRequest === 'undefined' || !XHR_META) return;
    xhrProto = XMLHttpRequest.prototype;
    origOpen = xhrProto.open;
    origSend = xhrProto.send;
    wrappedOpen = function (method, url) {
      try { XHR_META.set(this, { method: method, url: String(url) }); } catch (e) { /* noop */ }
      return origOpen.apply(this, arguments);
    };
    wrappedSend = function () {
      try {
        var meta = XHR_META.get(this);
        if (meta) {
          var xhr = this;
          var started = Date.now();
          xhr.addEventListener('loadend', function () {
            var size;
            try { size = lengthHeader(xhr.getResponseHeader('content-length')); } catch (e) { /* noop */ }
            recordNetwork('xhr', meta.method, meta.url, xhr.status, started, size);
          });
        }
      } catch (e) { /* noop */ }
      return origSend.apply(this, arguments);
    };
    xhrProto.open = wrappedOpen;
    xhrProto.send = wrappedSend;
  }

  // Restore every patch we still own (a later wrapper layered on top of ours
  // is left alone - it still calls through, and `active` makes ours inert).
  function stop() {
    active = false;
    try {
      LEVELS.forEach(function (level) {
        if (consoleWrap[level] && console[level] === consoleWrap[level]) console[level] = consoleOrig[level];
      });
      if (wrappedFetch && window.fetch === wrappedFetch) window.fetch = origFetch;
      if (xhrProto) {
        if (xhrProto.open === wrappedOpen) xhrProto.open = origOpen;
        if (xhrProto.send === wrappedSend) xhrProto.send = origSend;
      }
    } catch (e) { /* noop */ }
  }

  try {
    if (consoleOn) patchConsole();
    if (networkOn) { patchFetch(); patchXHR(); }
  } catch (e) { stop(); }

  window.observeReplayCapture = {
    stop: stop,
    stats: function () { return { console: consoleCount, network: networkCount }; }
  };
})();
