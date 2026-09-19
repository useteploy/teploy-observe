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

  var queue = [];
  var flushTimer = null;
  var FLUSH_INTERVAL = 500;
  // Audit F32: the server rejects >100 events per batch request with a 400
  // the tracker treated as delivered. Send in legal chunks, keep a failed
  // chunk for the next flush (bounded, so an outage cannot grow memory
  // forever), and never send keepalive bodies the browser will refuse.
  var MAX_BATCH_EVENTS = 100;
  var MAX_KEPT_ON_FAILURE = 500;
  var currentUrl = null;
  // Set by window.observe.identify(userId). Persisted across page loads
  // via localStorage so SPA navigations and reloads don't lose it.
  var distinctId = null;
  try {
    distinctId = localStorage.getItem('observe_distinct_id') || null;
  } catch (e) { /* localStorage may be disabled */ }

  // F12 (protocol v2): producer identity + per-event stable ids, so a
  // retried chunk re-presents the same event identities and the server can
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
  function campaignFields() {
    var out = {};
    var search = '';
    try { search = location.search; } catch (e) { return out; }
    if (!search || search.charAt(0) !== '?') return out;
    var parts = search.substring(1).split('&');
    for (var i = 0; i < parts.length; i++) {
      var kv = parts[i].split('=');
      var key = decodeURIComponent(kv[0].replace(/\+/g, ' '));
      if (UTM_KEYS.indexOf(key) === -1) continue;
      var val = kv.length > 1 ? decodeURIComponent(kv.slice(1).join('=').replace(/\+/g, ' ')) : '';
      if (val) out[key] = val.substring(0, 256);
    }
    return out;
  }

  // Reduce any href to origin+path for storage-safe wire values.
  function sanitizeURL(href) {
    try {
      var u = new URL(href);
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

  function send(eventType, props) {
    var payload = {
      event_type: eventType || 'pageview',
      event_id: makeId(),
      site_id: siteId,
      url: pageURL(),
      referrer: document.referrer || '',
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

    queue.push(payload);

    if (!flushTimer) {
      flushTimer = setTimeout(flush, FLUSH_INTERVAL);
    }
  }

  function flush() {
    flushTimer = null;
    if (!queue.length) return;

    var batch = queue.splice(0, MAX_BATCH_EVENTS);

    var sendChunk = function(events) {
      // F12 v2 envelope. batch_id is the first event's stable event_id, so
      // a requeued chunk that is retried on a later flush presents the SAME
      // batch id (and the same event ids) to the server.
      var body = JSON.stringify({
        v: PROTOCOL_VERSION,
        producer_id: producerId,
        batch_id: events.length ? events[0].event_id : '',
        events: events
      });

      // sendBeacon cannot set request headers, so a keyed install sends the
      // key via fetch with keepalive — same survives-unload guarantee — and
      // falls back to XHR where fetch is unavailable. An unchecked beacon
      // return used to count a refused queuing as delivered (audit F32);
      // a false return now falls through to fetch.
      if (!apiKey && navigator.sendBeacon) {
        try {
          if (navigator.sendBeacon(endpoint, new Blob([body], { type: 'application/json' }))) {
            return;
          }
        } catch (e) { /* fall through to fetch */ }
      }

      if (typeof fetch === 'function') {
        var headers = { 'Content-Type': 'application/json' };
        if (apiKey) headers['X-API-Key'] = apiKey;
        fetch(endpoint, {
          method: 'POST',
          headers: headers,
          body: body,
          keepalive: body.length <= 48 * 1024,
          mode: 'cors',
          credentials: 'omit'
        }).then(function(res) {
          if (!res.ok && queue.length < MAX_KEPT_ON_FAILURE) {
            // Server rejected the chunk — retain for the next flush tick
            // rather than silently erasing it. (Without producer-side
            // idempotency a retried chunk may double-count; the server-side
            // batch admission contract is tracked as audit F12 follow-up.)
            queue = events.concat(queue);
          }
        }).catch(function() {
          if (queue.length < MAX_KEPT_ON_FAILURE) queue = events.concat(queue);
        });
        return;
      }

      var xhr = new XMLHttpRequest();
      xhr.open('POST', endpoint, true);
      xhr.setRequestHeader('Content-Type', 'application/json');
      if (apiKey) xhr.setRequestHeader('X-API-Key', apiKey);
      xhr.send(body);
    };

    // Chunk to the server's per-request event cap; one flush may need
    // several requests when the queue grew past it.
    for (var i = 0; i < batch.length; i += MAX_BATCH_EVENTS) {
      sendChunk(batch.slice(i, i + MAX_BATCH_EVENTS));
    }
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
        var action = form.getAttribute('action') || '';
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
    revenue: function(amount, currency, props) {
      var p = props || {};
      p.amount = amount;
      p.currency = currency || 'USD';
      send('revenue', p);
    },
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
