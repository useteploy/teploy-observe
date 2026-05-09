(function(){
  'use strict';

  var script = document.currentScript;
  if (!script) return;

  var origin = script.src ? new URL(script.src).origin : '';
  var endpoint = script.getAttribute('data-endpoint') || origin + '/api/v1/errors';
  var siteId = script.getAttribute('data-site-id') || '';
  var release = script.getAttribute('data-release') || '';
  var environment = script.getAttribute('data-environment') || '';
  var maxBreadcrumbs = parseInt(script.getAttribute('data-max-breadcrumbs') || '30', 10);

  var breadcrumbs = [];
  var reportedHashes = {};

  // --- Breadcrumb Collection ---

  function addBreadcrumb(type, category, message, data) {
    breadcrumbs.push({
      type: type,
      category: category,
      message: message,
      data: data || null,
      timestamp: Date.now(),
      level: 'info'
    });
    if (breadcrumbs.length > maxBreadcrumbs) {
      breadcrumbs = breadcrumbs.slice(-maxBreadcrumbs);
    }
  }

  // Console breadcrumbs
  var origConsole = {};
  ['log', 'warn', 'error', 'info', 'debug'].forEach(function(level) {
    origConsole[level] = console[level];
    console[level] = function() {
      var args = Array.prototype.slice.call(arguments);
      var msg = args.map(function(a) {
        return typeof a === 'string' ? a : JSON.stringify(a);
      }).join(' ');
      addBreadcrumb('console', 'console', msg.substring(0, 256));
      origConsole[level].apply(console, arguments);
    };
  });

  // Click breadcrumbs
  document.addEventListener('click', function(e) {
    var target = e.target;
    if (!target) return;
    var tag = target.tagName || '';
    var text = (target.textContent || '').substring(0, 64).trim();
    var id = target.id ? '#' + target.id : '';
    var cls = target.className ? '.' + String(target.className).split(' ')[0] : '';
    addBreadcrumb('user', 'click', tag.toLowerCase() + id + cls, text ? { text: text } : null);
  }, true);

  // Navigation breadcrumbs
  var lastUrl = location.href;
  function trackNav() {
    var url = location.href;
    if (url !== lastUrl) {
      addBreadcrumb('navigation', 'navigation', url, { from: lastUrl });
      lastUrl = url;
    }
  }

  var origPush = history.pushState;
  var origReplace = history.replaceState;
  history.pushState = function() {
    origPush.apply(this, arguments);
    setTimeout(trackNav, 0);
  };
  history.replaceState = function() {
    origReplace.apply(this, arguments);
    setTimeout(trackNav, 0);
  };
  window.addEventListener('popstate', function() {
    setTimeout(trackNav, 0);
  });

  // Fetch breadcrumbs
  if (typeof fetch === 'function') {
    var origFetch = fetch;
    window.fetch = function(input, init) {
      var url = typeof input === 'string' ? input : (input && input.url) || '';
      var method = (init && init.method) || 'GET';
      addBreadcrumb('http', 'fetch', method.toUpperCase() + ' ' + url.substring(0, 256));
      return origFetch.apply(this, arguments);
    };
  }

  // XHR breadcrumbs
  if (typeof XMLHttpRequest !== 'undefined') {
    var origOpen = XMLHttpRequest.prototype.open;
    XMLHttpRequest.prototype.open = function(method, url) {
      this._obMethod = method;
      this._obUrl = url;
      return origOpen.apply(this, arguments);
    };
    var origSend = XMLHttpRequest.prototype.send;
    XMLHttpRequest.prototype.send = function() {
      if (this._obUrl && this._obUrl.indexOf(endpoint) === -1) {
        addBreadcrumb('http', 'xhr', (this._obMethod || 'GET').toUpperCase() + ' ' + String(this._obUrl).substring(0, 256));
      }
      return origSend.apply(this, arguments);
    };
  }

  // --- Stack Trace Parsing ---

  function parseStack(stack) {
    if (!stack) return [];
    var frames = [];
    var lines = stack.split('\n');
    for (var i = 0; i < lines.length && frames.length < 50; i++) {
      var line = lines[i].trim();
      if (!line || line.indexOf('Error') === 0) continue;

      var fn = '';
      var file = '';
      var lineno = 0;
      var colno = 0;

      // Chrome/Edge: "at funcName (file:line:col)" or "at file:line:col"
      var chromeMatch = line.match(/^\s*at\s+(?:(.+?)\s+\()?(.*?):(\d+):(\d+)\)?$/);
      if (chromeMatch) {
        fn = chromeMatch[1] || '';
        file = chromeMatch[2] || '';
        lineno = parseInt(chromeMatch[3], 10) || 0;
        colno = parseInt(chromeMatch[4], 10) || 0;
      } else {
        // Firefox/Safari: "funcName@file:line:col"
        var ffMatch = line.match(/^(.+?)@(.*?):(\d+):(\d+)$/);
        if (ffMatch) {
          fn = ffMatch[1] || '';
          file = ffMatch[2] || '';
          lineno = parseInt(ffMatch[3], 10) || 0;
          colno = parseInt(ffMatch[4], 10) || 0;
        }
      }

      if (!file) continue;

      var inApp = file.indexOf('node_modules') === -1 &&
                  file.indexOf('extensions/') === -1 &&
                  file.indexOf('chrome-extension') === -1 &&
                  file.indexOf('<anonymous>') === -1;

      frames.push({
        filename: file,
        function: fn || '<anonymous>',
        lineno: lineno,
        colno: colno,
        in_app: inApp
      });
    }
    return frames;
  }

  // --- Error Reporting ---

  function dedupeKey(type, msg, file, line) {
    return type + '|' + msg + '|' + (file || '') + '|' + (line || 0);
  }

  function reportError(type, value, stack, handled, mechanism) {
    var frames = parseStack(stack);
    var key = dedupeKey(type, value, frames.length ? frames[0].filename : '', frames.length ? frames[0].lineno : 0);

    if (reportedHashes[key]) return;
    reportedHashes[key] = true;
    setTimeout(function() { delete reportedHashes[key]; }, 5000);

    var ua = navigator.userAgent || '';
    var replayId = '';
    try {
      if (window.observeReplay && typeof window.observeReplay.getReplayId === 'function') {
        replayId = window.observeReplay.getReplayId() || '';
      }
    } catch (e) { /* ignore */ }
    var payload = {
      site_id: siteId,
      session_id: '',
      replay_id: replayId,
      error_type: type || 'Error',
      error_value: (value || '').substring(0, 2048),
      mechanism: mechanism || 'onerror',
      handled: !!handled,
      level: 'error',
      release: release,
      environment: environment,
      url: location.href,
      browser: ua.substring(0, 256),
      os: '',
      device: '',
      stack_trace: frames,
      breadcrumbs: breadcrumbs.slice()
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

  // --- Global Error Handlers ---

  window.addEventListener('error', function(event) {
    if (!event.error && !event.message) return;
    var err = event.error;
    var type = err ? (err.name || 'Error') : 'Error';
    var value = err ? (err.message || event.message) : event.message;
    var stack = err ? err.stack : '';
    reportError(type, value, stack, false, 'onerror');
  });

  window.addEventListener('unhandledrejection', function(event) {
    var reason = event.reason;
    if (!reason) return;
    var type, value, stack;
    if (reason instanceof Error) {
      type = reason.name || 'UnhandledRejection';
      value = reason.message || String(reason);
      stack = reason.stack || '';
    } else {
      type = 'UnhandledRejection';
      value = typeof reason === 'string' ? reason : JSON.stringify(reason);
      stack = '';
    }
    reportError(type, value, stack, false, 'unhandledrejection');
  });

  // --- Public API ---

  window.observeErrors = {
    captureException: function(err, extra) {
      if (!err) return;
      var type = err.name || 'Error';
      var value = err.message || String(err);
      var stack = err.stack || '';
      reportError(type, value, stack, true, 'captureException');
    },
    captureMessage: function(msg, level) {
      reportError(level === 'warning' ? 'Warning' : 'Error', msg, '', true, 'captureMessage');
    },
    addBreadcrumb: function(crumb) {
      addBreadcrumb(crumb.type || 'manual', crumb.category || 'manual', crumb.message || '', crumb.data);
    },
    setRelease: function(r) { release = r; },
    setEnvironment: function(e) { environment = e; }
  };
})();
