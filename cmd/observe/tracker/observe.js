(function(){
  'use strict';

  var script = document.currentScript;
  if (!script) return;

  var origin = script.src ? new URL(script.src).origin : '';
  var endpoint = script.getAttribute('data-endpoint') || origin + '/api/v1/events/batch';
  var siteId = script.getAttribute('data-site-id') || '';
  var autoTrack = script.getAttribute('data-auto-track') !== 'false';
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
  }

  window.observe = {
    track: function(name, props) {
      send(name || 'custom', props);
    },
    pageview: trackPageview
  };

  init();
})();
