(function(){
  'use strict';

  var script = document.currentScript;
  if (!script) return;

  var origin = script.src ? new URL(script.src).origin : '';
  var endpoint = script.getAttribute('data-endpoint') || origin + '/api/v1/events/batch';
  var siteId = script.getAttribute('data-site-id') || '';
  var autoTrack = script.getAttribute('data-auto-track') !== 'false';
  var autoCapture = script.getAttribute('data-autocapture') !== 'false';
  var respectDNT = script.getAttribute('data-respect-dnt') !== 'false';

  if (respectDNT && navigator.doNotTrack === '1') return;

  if (document.visibilityState === 'prerender') {
    document.addEventListener('visibilitychange', function() {
      if (document.visibilityState !== 'prerender') init();
    }, { once: true });
    return;
  }

  var queue = [];
  var flushTimer = null;
  var FLUSH_INTERVAL = 500;
  var currentUrl = null;

  function send(eventType, props) {
    var payload = {
      event_type: eventType || 'pageview',
      site_id: siteId,
      url: location.href,
      referrer: document.referrer || '',
      title: document.title || '',
      language: navigator.language || '',
      screen: screen.width + 'x' + screen.height
    };
    if (props) payload.properties = props;

    queue.push(payload);

    if (!flushTimer) {
      flushTimer = setTimeout(flush, FLUSH_INTERVAL);
    }
  }

  function flush() {
    flushTimer = null;
    if (!queue.length) return;

    var events = queue.splice(0);
    var body = JSON.stringify({ events: events });

    if (navigator.sendBeacon) {
      navigator.sendBeacon(endpoint, new Blob([body], { type: 'application/json' }));
    } else {
      var xhr = new XMLHttpRequest();
      xhr.open('POST', endpoint, true);
      xhr.setRequestHeader('Content-Type', 'application/json');
      xhr.send(body);
    }
  }

  function trackPageview() {
    var url = location.href;
    if (url === currentUrl) return;
    currentUrl = url;
    send('pageview');
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

        var text = (target.textContent || '').trim().substring(0, 32);
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
          send('click', { selector: selector, text: text, href: href });
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
              page_url: startUrl,
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
            send('outbound_click', { href: link.href, text: (link.textContent || '').trim().substring(0, 32) });
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
    trackVitals: trackWebVitals
  };

  init();
})();
