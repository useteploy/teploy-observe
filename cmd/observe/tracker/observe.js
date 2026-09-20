(function(){
  'use strict';

  var script = document.currentScript;
  if (!script) return;

  var origin = script.src ? new URL(script.src).origin : '';
  var endpoint = script.getAttribute('data-endpoint') || origin + '/api/v1/events/batch';
  var siteId = script.getAttribute('data-site-id') || '';
  // Site-scoped write key. Required once any API key exists on the instance:
  // the ingest middleware only falls back to the default site while the
  // system has no keys at all, and rejects everything else with 401.
  var apiKey = script.getAttribute('data-api-key') || '';
  var autoTrack = script.getAttribute('data-auto-track') !== 'false';
  var autoCapture = script.getAttribute('data-autocapture') !== 'false';
  var respectDNT = script.getAttribute('data-respect-dnt') !== 'false';
  // F41: element text is sensitive by default. Autocaptured clicks used to
  // carry up to 32 chars of the clicked element's visible text; it is now
  // sent ONLY when the page opts in explicitly.
  var captureText = script.getAttribute('data-capture-text') === 'true';

  if (respectDNT && navigator.doNotTrack === '1') return;

  var prerendering = document.visibilityState === 'prerender';
  if (prerendering) {
    document.addEventListener('visibilitychange', function() {
      if (document.visibilityState !== 'prerender') start();
    }, { once: true });
  }

  var currentUrl = null;
  // Set by window.observe.identify(userId). Persisted across page loads
  // via localStorage so SPA navigations and reloads don't lose it.
  var distinctId = null;
  try {
    distinctId = localStorage.getItem('observe_distinct_id') || null;
  } catch (e) { /* localStorage may be disabled */ }

  // F12 (protocol v2): producer identity + per-event stable ids, so a
  // retried batch re-presents the same event identities and the server can
  // dedupe at admission and at flush.
  var PROTOCOL_VERSION = 2;
  var producerId = makeId();

  function makeId() {
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

  // F41 URL contract: what leaves the page is origin+path only. Query
  // string, fragment, and any credentials the URL might carry never reach
  // the wire from this tracker; campaign attribution rides the explicit
  // utm_* fields extracted from the allowlisted query params below.
  function pageURL() {
    try { return location.origin + location.pathname; } catch (e) { return ''; }
  }

  var UTM_KEYS = ['utm_source', 'utm_medium', 'utm_campaign', 'utm_term', 'utm_content'];

  // Extract the allowlisted campaign params from the CURRENT query string.
  // Manual parse (no URLSearchParams dependency — the classic tracker
  // supports old browsers); values are decoded defensively and capped.
  // R33 (round 4): each pair decodes under its own try/catch. One malformed
  // percent escape anywhere in the query string used to throw out of
  // campaignFields, which ran inside send() — a single bad pair could kill
  // the initial pageview and abort tracker initialization before all
  // listeners were installed. Malformed pairs are skipped; valid allowlisted
  // attribution still ships.
  function campaignFields() {
    var out = {};
    var search = '';
    try { search = location.search; } catch (e) { return out; }
    if (!search || search.charAt(0) !== '?') return out;
    var parts = search.substring(1).split('&');
    for (var i = 0; i < parts.length; i++) {
      var kv = parts[i].split('=');
      var key, val;
      try {
        key = decodeURIComponent(kv[0].replace(/\+/g, ' '));
        if (UTM_KEYS.indexOf(key) === -1) continue;
        val = kv.length > 1 ? decodeURIComponent(kv.slice(1).join('=').replace(/\+/g, ' ')) : '';
      } catch (e) { continue; }
      if (val) out[key] = val.substring(0, 256);
    }
    return out;
  }

  // Reduce any captured href to origin+path for storage-safe wire values.
  // R29 (round 4): relative URLs resolve against the document (the old
  // `new URL(href)` without a base threw away every relative link and form
  // action), and non-http(s) schemes are dropped.
  function sanitizeURL(href, base) {
    if (!href) return '';
    try {
      var u = new URL(href, base || location.href);
      if (u.protocol !== 'http:' && u.protocol !== 'https:') return '';
      return u.origin + u.pathname;
    } catch (e) {
      return '';
    }
  }

  // Element text only when the page opted in (F41).
  function elText(target) {
    if (!captureText) return '';
    return (target.textContent || '').trim().substring(0, 32);
  }

  // ---------------------------------------------------------------------
  // Delivery (R31/R32, round 4): single-flight, self-draining transport.
  //
  // The old flush() took at most the first 100 events and never scheduled
  // the tail (a 101-event burst stranded one event forever), requeued
  // failed batches without scheduling a retry (delivery depended on the
  // next unrelated event), measured keepalive fitness in UTF-16 code units
  // instead of bytes, and had no XHR failure handling at all. Events are
  // now serialized to immutable JSON at CAPTURE (mutation of the caller's
  // properties after track() cannot change what is sent, and a cyclic
  // value is a capture-time rejection instead of a mid-flush throw that
  // detached and lost the batch), budgeted by count AND bytes across
  // queued+in-flight, and delivered by one in-flight request that keeps
  // its frozen batch across bounded retries and reschedules itself until
  // the queue is empty.
  // ---------------------------------------------------------------------
  var MAX_QUEUED_EVENTS = 500;         // retained-event cap (drop-oldest-free: reject-new)
  var MAX_TOTAL_BYTES = 2 * 1024 * 1024;
  var MAX_EVENT_BYTES = 32 * 1024;
  var MAX_BODY_BYTES = 48 * 1024;      // byte budget, not UTF-16 length (R31)
  var MAX_BATCH_EVENTS = 100;          // server per-request cap
  var MAX_RETRY_ATTEMPTS = 5;
  var RETRY_BASE_MS = 1000;
  var RETRY_MAX_MS = 30000;
  var FLUSH_DELAY_MS = 500;

  var queue = [];          // frozen packets: {id, json, cost}
  var pending = null;      // frozen batch: {body, count, cost}
  var inFlight = false;
  var drainScheduled = false;
  var drainHandle = null;
  var retryScheduled = false;
  var retryHandle = null;
  var retryAttempts = 0;
  var usedCount = 0;
  var usedBytes = 0;
  var dropped = 0;

  // Byte length of a string: Blob size when available (all fetch-capable
  // browsers), else a conservative percent-escape estimate that overcounts
  // non-ASCII — overcounting only tightens the budget (R31).
  function byteLen(s) {
    try {
      if (typeof Blob === 'function') return new Blob([s]).size;
    } catch (e) { /* fall through */ }
    return encodeURIComponent(s).replace(/%[0-9A-Fa-f]{2}/g, 'x').length;
  }

  function reportDrop(reason, count) {
    dropped += count;
    try {
      if (typeof window.observeOnDrop === 'function') window.observeOnDrop(reason, count);
    } catch (e) { /* diagnostics must not throw into the page */ }
  }

  // Serialize the event ONCE at capture (R32). Returns null when the value
  // is not JSON-serializable or exceeds the per-event cap.
  function snapshotEvent(event) {
    var json;
    try { json = JSON.stringify(event); } catch (e) { return null; }
    if (typeof json !== 'string') return null;
    if (byteLen(json) > MAX_EVENT_BYTES) return null;
    return json;
  }

  function enqueue(json, id) {
    var cost = byteLen(json) + 128; // payload + envelope allowance
    if (usedCount >= MAX_QUEUED_EVENTS || cost > MAX_TOTAL_BYTES - usedBytes) {
      reportDrop('buffer_full', 1);
      return false;
    }
    queue.push({ id: id, json: json, cost: cost });
    usedCount++;
    usedBytes += cost;
    return true;
  }

  function releasePending() {
    if (pending) {
      usedCount -= pending.count;
      usedBytes -= pending.cost;
      pending = null;
    }
    retryAttempts = 0;
  }

  function dropPending(reason) {
    reportDrop(reason, pending ? pending.count : 0);
    releasePending();
  }

  function encodeBatch(packets) {
    // v2 envelope; batch_id is the first event's stable id, so a retried
    // batch presents the SAME identity (server admission dedupe, F12).
    var parts = [];
    for (var i = 0; i < packets.length; i++) parts.push(packets[i].json);
    return '{"v":' + PROTOCOL_VERSION +
      ',"producer_id":' + JSON.stringify(producerId) +
      ',"batch_id":' + JSON.stringify(packets[0].id) +
      ',"events":[' + parts.join(',') + ']}';
  }

  function makePendingBatch() {
    var packets = [];
    var cost = 0;
    var body = '';
    while (queue.length && packets.length < MAX_BATCH_EVENTS) {
      var candidatePackets = packets.concat([queue[0]]);
      var candidateBody = encodeBatch(candidatePackets);
      var candidateBytes = byteLen(candidateBody);
      if (candidateBytes > MAX_BODY_BYTES) break;
      packets = candidatePackets;
      body = candidateBody;
      cost += queue[0].cost;
      queue.shift();
    }
    if (!packets.length) {
      // A single event that cannot fit any legal request: drop it at the
      // flush boundary instead of a permanently failing retry head (the
      // per-event cap makes this unreachable; kept as a guard).
      var oversize = queue.shift();
      usedCount--;
      usedBytes -= oversize.cost;
      reportDrop('event_exceeds_batch_budget', 1);
      return null;
    }
    return { body: body, count: packets.length, cost: cost };
  }

  function retryDelay() {
    var exp = RETRY_BASE_MS;
    for (var i = 1; i < retryAttempts && exp < RETRY_MAX_MS; i++) exp *= 2;
    if (exp > RETRY_MAX_MS) exp = RETRY_MAX_MS;
    return exp;
  }

  // Scheduling uses booleans as the source of truth, not timer handles: a
  // host whose setTimeout runs the callback synchronously (test sandboxes,
  // some embedded webviews) would otherwise leave a stale handle parked in
  // the timer variable and block every future drain.
  function scheduleDrain(delay) {
    if (inFlight || (!pending && !queue.length)) return;
    if (drainScheduled || retryScheduled) return;
    drainScheduled = true;
    drainHandle = setTimeout(function() {
      drainScheduled = false;
      pump();
    }, delay);
  }

  function scheduleRetry(delay) {
    if (drainScheduled || retryScheduled) return;
    retryScheduled = true;
    retryHandle = setTimeout(function() {
      retryScheduled = false;
      pump();
    }, delay);
  }

  function pump() {
    if (inFlight) return;
    if (!pending && queue.length) pending = makePendingBatch();
    if (!pending) { scheduleDrain(FLUSH_DELAY_MS); return; }
    inFlight = true;
    deliver(pending.body, function(outcome) {
      inFlight = false;
      if (outcome === 'ok') {
        releasePending();
        scheduleDrain(0); // drain the tail without waiting for a new event
      } else if (outcome === 'retry') {
        retryAttempts++;
        if (retryAttempts >= MAX_RETRY_ATTEMPTS) {
          dropPending('retry_budget_exhausted');
          scheduleDrain(0);
        } else {
          scheduleRetry(retryDelay());
        }
      } else {
        dropPermanent();
      }
    });
  }

  function dropPermanent() {
    dropPending('permanent_rejection');
    scheduleDrain(0);
  }

  // One delivery attempt. cb('ok' | 'retry' | 'drop'). Transient outcomes:
  // network error, 408/425/429, 5xx. Permanent: other 4xx (the server
  // rejected the request itself; retrying identical bytes cannot succeed).
  function deliver(body, cb) {
    // Keyless delivery is gone (AUD-002 removed keyless ingest server-side;
    // TO-051 removed the SDK's beacon fallback — this closes the classic
    // tracker's copy). Without a key the batch cannot be accepted; drop it
    // loudly instead of beaconing to a 401.
    if (!apiKey) {
      reportDrop('missing_api_key', pending ? pending.count : 1);
      releasePending();
      cb('drop');
      return;
    }

    if (typeof fetch === 'function') {
      var headers = { 'Content-Type': 'application/json', 'X-API-Key': apiKey };
      fetch(endpoint, {
        method: 'POST',
        headers: headers,
        body: body,
        keepalive: byteLen(body) <= MAX_BODY_BYTES,
        mode: 'cors',
        credentials: 'omit'
      }).then(function(res) {
        if (res.ok) return cb('ok');
        if (res.status === 408 || res.status === 425 || res.status === 429 || res.status >= 500) return cb('retry');
        cb('drop');
      }).catch(function() { cb('retry'); });
      return;
    }

    // XHR fallback WITH failure handling (R31: this branch used to fire and
    // forget — no onload/onerror at all).
    try {
      var xhr = new XMLHttpRequest();
      xhr.open('POST', endpoint, true);
      xhr.setRequestHeader('Content-Type', 'application/json');
      xhr.setRequestHeader('X-API-Key', apiKey);
      xhr.onload = function() {
        if (xhr.status >= 200 && xhr.status < 300) return cb('ok');
        if (xhr.status === 408 || xhr.status === 425 || xhr.status === 429 || xhr.status >= 500) return cb('retry');
        cb('drop');
      };
      xhr.onerror = function() { cb('retry'); };
      xhr.ontimeout = function() { cb('retry'); };
      xhr.send(body);
    } catch (e) {
      cb('retry');
    }
  }

  function send(eventType, props) {
    var payload = {
      event_type: eventType || 'pageview',
      event_id: makeId(),
      site_id: siteId,
      url: pageURL(),
      // R29 (round 4): the referrer is reduced client-side — the raw value
      // (which can carry its own query tokens) never crosses the wire. The
      // server-side cleanReferrer stays as the backstop.
      referrer: sanitizeURL(document.referrer || ''),
      title: document.title || '',
      language: navigator.language || '',
      screen: screen.width + 'x' + screen.height
    };
    var utm = campaignFields();
    for (var k in utm) {
      if (Object.prototype.hasOwnProperty.call(utm, k)) payload[k] = utm[k];
    }
    if (props) payload.properties = props;
    if (distinctId) payload.distinct_id = distinctId;

    // R32 (round 4): snapshot at admission. The queue holds serialized
    // bytes, never the caller's mutable object; a cyclic or BigInt value
    // rejects HERE (diagnostic, contained) instead of throwing out of a
    // later flush after the batch was already detached.
    var json = snapshotEvent(payload);
    if (json === null) {
      reportDrop('not_json_serializable_or_too_large', 1);
      return;
    }
    enqueue(json, payload.event_id);
    scheduleDrain(FLUSH_DELAY_MS);
  }

  // Public flush: called on visibility-hidden. Best effort — a fetch with
  // keepalive survives unload; XHR may not.
  function flush() {
    if (drainScheduled && drainHandle !== null) { clearTimeout(drainHandle); drainHandle = null; }
    if (retryScheduled && retryHandle !== null) { clearTimeout(retryHandle); retryHandle = null; }
    drainScheduled = false;
    retryScheduled = false;
    pump();
  }

  function trackPageview() {
    var url = location.href;
    if (url === currentUrl) return;
    currentUrl = url;
    send('pageview');
  }

  // Auto-tracking is wired exactly once, whether it starts from the script
  // tag, from a prerender becoming visible, or from a programmatic init().
  var started = false;
  function start() {
    if (started) return;
    started = true;
    init();
  }

  function init() {
    if (!autoTrack) return;
    trackPageview();

    var origPush = history.pushState;
    var origReplace = history.replaceState;

    history.pushState = function() {
      origPush.apply(this, arguments);
      setTimeout(trackPageview, 0);
    };

    history.replaceState = function() {
      origReplace.apply(this, arguments);
      setTimeout(trackPageview, 0);
    };

    window.addEventListener('popstate', function() {
      setTimeout(trackPageview, 0);
    });

    document.addEventListener('visibilitychange', function() {
      if (document.visibilityState === 'hidden') flush();
    });

    // Autocapture: clicks, form submissions, rage clicks
    if (autoCapture) {
      var lastClickTime = 0;
      var lastClickTarget = null;
      var clickCount = 0;

      document.addEventListener('click', function(e) {
        var target = e.target;
        if (!target || !target.tagName) return;
        var tag = target.tagName.toLowerCase();
        if (tag === 'html' || tag === 'body') return;

        var selector = tag;
        if (target.id) selector += '#' + target.id;
        else if (target.className && typeof target.className === 'string') selector += '.' + target.className.split(' ')[0];

        // F41: text is sent only under data-capture-text opt-in; hrefs are
        // reduced to origin+path (a query string can carry tokens).
        var text = elText(target);
        var href = target.getAttribute('href') || '';

        // Rage click detection: 3+ clicks on same element within 1 second
        var now = Date.now();
        if (target === lastClickTarget && now - lastClickTime < 1000) {
          clickCount++;
          if (clickCount === 3) {
            send('rage_click', { selector: selector, text: text });
          }
        } else {
          clickCount = 1;
          lastClickTarget = target;
        }
        lastClickTime = now;

        // Track meaningful clicks (links, buttons, inputs)
        if (tag === 'a' || tag === 'button' || tag === 'input' || target.getAttribute('role') === 'button') {
          send('click', { selector: selector, text: text, href: sanitizeURL(href) });
        }

        // Dead click detection: click yielded no DOM mutation, no navigation,
        // and no input focus change within 1.5s. Per-click temporary observer
        // to avoid leaking listeners across many clicks.
        if (typeof MutationObserver !== 'undefined') {
          var deadClickX = (typeof e.clientX === 'number') ? e.clientX : 0;
          var deadClickY = (typeof e.clientY === 'number') ? e.clientY : 0;
          var startUrl = location.href;
          var startActive = document.activeElement;
          var mutated = false;
          var mo = new MutationObserver(function() { mutated = true; });
          mo.observe(document.documentElement, {
            childList: true, subtree: true,
            attributes: true, characterData: true,
          });
          setTimeout(function() {
            mo.disconnect();
            if (mutated) return;
            if (location.href !== startUrl) return;
            if (document.activeElement !== startActive) return;
            send('dead_click', {
              x: deadClickX, y: deadClickY,
              target_selector: selector,
              page_url: sanitizeURL(startUrl),
            });
          }, 1500);
        }
      }, true);

      // Form submissions
      document.addEventListener('submit', function(e) {
        var form = e.target;
        if (!form || !form.tagName) return;
        var id = form.id ? '#' + form.id : '';
        // R29 (round 4): the form action is a captured URL — sanitize it
        // like every other (a relative action resolves against the page; a
        // token-bearing query never leaves). An empty action means the form
        // posts back to the current page.
        var action = sanitizeURL(form.getAttribute('action')) || pageURL();
        send('form_submit', { selector: 'form' + id, action: action });
      }, true);

      // Track outbound link clicks
      document.addEventListener('click', function(e) {
        var link = e.target;
        while (link && link.tagName !== 'A') link = link.parentElement;
        if (!link || !link.href) return;
        try {
          var url = new URL(link.href);
          if (url.hostname !== location.hostname) {
            // F41: origin+path only — the full href (query, fragment) never
            // leaves the page; text under the same opt-in as clicks.
            send('outbound_click', { href: sanitizeURL(link.href), text: elText(link) });
          }
        } catch(err) {}
      }, true);
    }
  }

  // Web vitals tracking
  function trackWebVitals() {
    if (typeof PerformanceObserver === 'undefined') return;

    // LCP
    try {
      new PerformanceObserver(function(list) {
        var entries = list.getEntries();
        if (entries.length) {
          send('web_vital', { metric: 'lcp', value: Math.round(entries[entries.length - 1].startTime) });
        }
      }).observe({ type: 'largest-contentful-paint', buffered: true });
    } catch(e) {}

    // FID
    try {
      new PerformanceObserver(function(list) {
        var entries = list.getEntries();
        if (entries.length) {
          send('web_vital', { metric: 'fid', value: Math.round(entries[0].processingStart - entries[0].startTime) });
        }
      }).observe({ type: 'first-input', buffered: true });
    } catch(e) {}

    // CLS
    try {
      var clsValue = 0;
      new PerformanceObserver(function(list) {
        for (var i = 0; i < list.getEntries().length; i++) {
          if (!list.getEntries()[i].hadRecentInput) clsValue += list.getEntries()[i].value;
        }
        send('web_vital', { metric: 'cls', value: Math.round(clsValue * 1000) });
      }).observe({ type: 'layout-shift', buffered: true });
    } catch(e) {}

    // TTFB
    if (performance.getEntriesByType) {
      var nav = performance.getEntriesByType('navigation');
      if (nav.length) {
        send('web_vital', { metric: 'ttfb', value: Math.round(nav[0].responseStart) });
      }
    }
  }

  window.observe = {
    /**
     * Programmatic setup, for pages that inject the script rather than
     * carrying data-* attributes. Accepts { endpoint, siteId, apiKey } and
     * begins auto-tracking. Attribute-configured installs start on their own
     * and never need this.
     */
    init: function(opts) {
      var o = opts || {};
      if (o.endpoint) {
        // Accept either the bare origin or a full ingest URL.
        endpoint = /\/api\/v1\/events(\/batch)?$/.test(o.endpoint)
          ? o.endpoint
          : String(o.endpoint).replace(/\/+$/, '') + '/api/v1/events/batch';
      }
      if (o.siteId) siteId = o.siteId;
      if (o.apiKey) apiKey = o.apiKey;
      if (!siteId) return;
      start();
    },
    track: function(name, props) {
      send(name || 'custom', props);
    },
    pageview: trackPageview,
    flush: flush,
    revenue: function(amount, currency, props) {
      // R32 (round 4): build a fresh object — do not write amount/currency
      // into the caller's properties.
      var p = {};
      if (props) {
        for (var k in props) {
          if (Object.prototype.hasOwnProperty.call(props, k)) p[k] = props[k];
        }
      }
      p.amount = amount;
      p.currency = currency || 'USD';
      send('revenue', p);
    },
    /**
     * Delivery diagnostics (R31): how many events this tracker dropped
     * locally (buffer full, unserializable, retry budget exhausted, keyless
     * install) — the honest counter for "events you expected but never
     * landed".
     */
    droppedEvents: function() { return dropped; },
    /**
     * Associate subsequent events with a user identifier. The server
     * hashes the value with the per-site session_salt before storage —
     * raw IDs never persist by default. Pass null to clear.
     *
     * Traits are sent as properties of the $identify event, minus any
     * identity-shaped key (audit F33): the raw ID travels ONLY in the
     * top-level distinct_id field the server hashes. properties.user_id
     * used to duplicate it verbatim into stored event properties.
     */
    identify: function(userId, traits) {
      if (userId === null || userId === undefined || userId === '') {
        distinctId = null;
        try { localStorage.removeItem('observe_distinct_id'); } catch (e) {}
        return;
      }
      distinctId = String(userId);
      try { localStorage.setItem('observe_distinct_id', distinctId); } catch (e) {}
      var props = {};
      if (traits) {
        for (var k in traits) {
          if (!Object.prototype.hasOwnProperty.call(traits, k)) continue;
          if (k === 'user_id' || k === 'distinct_id' || k === 'email') continue;
          props[k] = traits[k];
        }
      }
      send('$identify', props);
    },
    reset: function() {
      distinctId = null;
      try { localStorage.removeItem('observe_distinct_id'); } catch (e) {}
    },
    trackVitals: trackWebVitals
  };

  // Without a site id there is nothing valid to send — the server rejects an
  // empty site_id — so an attribute-less install waits for observe.init().
  if (siteId && !prerendering) start();
})();
