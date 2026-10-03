(function(){
  'use strict';

  // Observe survey widget. Dependency-free; renders ONE active survey at a time
  // in a Shadow DOM dialog. Every server-supplied string is written with
  // textContent / setAttribute on fixed attribute names - never innerHTML - so
  // survey text cannot inject markup or script. Nothing here evals, builds
  // functions from strings, or uses inline event-handler attributes, and all
  // styling is a constructed stylesheet (or a <style> element) plus CSSOM
  // custom properties, so a CSP without 'unsafe-inline'/'unsafe-eval' works.
  // The whole widget is wrapped so it can never throw into the host page.
  try {
    var script = document.currentScript;
    if (!script) return;

    var siteId = script.getAttribute('data-site-id') || '';
    if (!siteId) return;

    // Same DNT contract as observe.js: honoured unless data-respect-dnt="false".
    var respectDNT = script.getAttribute('data-respect-dnt') !== 'false';
    if (respectDNT && navigator.doNotTrack === '1') return;

    var origin = '';
    try { origin = script.src ? new URL(script.src).origin : ''; } catch (e) { origin = ''; }
    var base = (script.getAttribute('data-endpoint') || (origin + '/api/v1/surveys')).replace(/\/+$/, '');

    var MAX_QUESTIONS = 10;
    var MAX_CHOICES = 20;
    var MAX_TEXT = 500;      // rendered question / choice text
    var MAX_ANSWER = 2000;   // free-text answer, matches the server cap
    var DISMISS_DAYS = 30;
    var STORE_PREFIX = 'observe_sv:' + siteId + ':';
    var ID_RE = /^[A-Za-z0-9_-]{1,64}$/;
    var COLOR_RE = /^#[0-9a-fA-F]{3,8}$/;

    var active = null;       // { close() } for the survey currently on screen
    var running = false;
    var exposed = {};        // survey_id -> true, once per page load

    // ---- storage (always try/catch; the widget works without it) ----------
    function read(id) {
      try {
        var raw = localStorage.getItem(STORE_PREFIX + id);
        if (!raw) return null;
        var v = JSON.parse(raw);
        return v && typeof v === 'object' ? v : null;
      } catch (e) { return null; }
    }
    function write(id, state) {
      try { localStorage.setItem(STORE_PREFIX + id, JSON.stringify({ s: state, t: Date.now() })); } catch (e) {}
    }
    // 'done' and 'shown'(once) are permanent; a dismissal lapses after 30 days.
    function suppressed(sv) {
      var v = read(sv.survey_id);
      if (!v) return false;
      if (v.s === 'done') return true;
      if (v.s === 'shown') return !!sv.once;
      if (v.s === 'dismissed') return Date.now() - (v.t || 0) < DISMISS_DAYS * 86400000;
      return false;
    }

    // ---- network ---------------------------------------------------------
    function sleep(ms, fn) { setTimeout(fn, ms); }

    function randomId() {
      var chars = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789';
      var out = '';
      try {
        var a = new Uint8Array(24);
        crypto.getRandomValues(a);
        for (var i = 0; i < a.length; i++) out += chars.charAt(a[i] % chars.length);
        return out;
      } catch (e) {
        for (var j = 0; j < 24; j++) out += chars.charAt(Math.floor(Math.random() * chars.length));
        return out;
      }
    }

    // POST with bounded retries. 2xx -> done; 4xx other than 429 -> give up
    // (the server refused it, retrying cannot help); 429 -> honour
    // Retry-After (capped); network/5xx -> exponential backoff. Max 3 retries.
    function post(path, body, cb) {
      var attempt = 0;
      function go() {
        var delay = 0;
        var p;
        try {
          p = fetch(base + path, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(body),
            keepalive: true,
            credentials: 'omit'
          });
        } catch (e) { p = Promise.reject(e); }
        p.then(function(res) {
          if (res && res.ok) { if (cb) cb(true); return; }
          var st = res ? res.status : 0;
          if (st === 429) {
            var ra = 0;
            try { ra = parseInt(res.headers.get('Retry-After'), 10) || 0; } catch (e) {}
            delay = Math.min(Math.max(ra, 1), 30) * 1000;
          } else if (st >= 400 && st < 500) { if (cb) cb(false); return; }
          retry(delay);
        }, function() { retry(0); }).catch(function() {});
      }
      function retry(d) {
        attempt++;
        if (attempt > 3) { if (cb) cb(false); return; }
        sleep(d || Math.pow(2, attempt) * 1000, go);
      }
      go();
    }

    function fetchActive(cb) {
      var attempt = 0;
      function go() {
        var p;
        try {
          var q = '?site_id=' + encodeURIComponent(siteId) +
            '&path=' + encodeURIComponent(location.pathname || '/') +
            '&device_type=' + deviceType();
          var rh = referrerHost();
          if (rh) q += '&referrer_host=' + encodeURIComponent(rh);
          p = fetch(base + '/active' + q, { credentials: 'omit' });
        } catch (e) { p = Promise.reject(e); }
        p.then(function(res) {
          if (res && res.ok) return res.json().then(function(j) { cb(Array.isArray(j) ? j : []); });
          // 429: stop quietly (next page view tries again); 5xx: one more go.
          if (res && res.status >= 500 && attempt < 2) return again();
          cb([]);
        }, function() { again(); }).catch(function() { cb([]); });
      }
      function again() {
        attempt++;
        if (attempt > 2) { cb([]); return; }
        sleep(attempt * 3000, go);
      }
      go();
    }

    function deviceType() {
      var w = 1024;
      try { w = window.innerWidth || document.documentElement.clientWidth || 1024; } catch (e) {}
      return w < 600 ? 'mobile' : (w < 1024 ? 'tablet' : 'desktop');
    }
    function referrerHost() {
      try {
        if (!document.referrer) return '';
        var h = new URL(document.referrer).hostname.toLowerCase();
        return h === location.hostname ? '' : h;
      } catch (e) { return ''; }
    }

    // ---- model: sanitise server data before it touches the DOM ------------
    function clip(s, n) {
      s = typeof s === 'string' ? s : (typeof s === 'number' ? String(s) : '');
      return s.length > n ? s.slice(0, n) : s;
    }
    function parseJSON(v, fallback) {
      if (typeof v === 'string') { try { return JSON.parse(v); } catch (e) { return fallback; } }
      return v == null ? fallback : v;
    }
    function normalise(sv) {
      var list = parseJSON(sv.questions, []);
      if (!Array.isArray(list)) list = [];
      var out = [];
      for (var i = 0; i < list.length && out.length < MAX_QUESTIONS; i++) {
        var q = list[i];
        if (!q || typeof q !== 'object') continue;
        var type = q.type;
        if (type !== 'text' && type !== 'rating' && type !== 'nps' && type !== 'choice') continue;
        var nq = {
          id: typeof q.id === 'string' && ID_RE.test(q.id) ? q.id : 'q' + (i + 1),
          type: type,
          text: clip(q.text, MAX_TEXT) || 'Question ' + (out.length + 1),
          required: q.required === true,
          placeholder: clip(q.placeholder, 200),
          choices: []
        };
        if (type === 'choice') {
          var cs = Array.isArray(q.choices) ? q.choices : [];
          for (var c = 0; c < cs.length && nq.choices.length < MAX_CHOICES; c++) {
            var t = clip(cs[c], 200);
            if (t) nq.choices.push(t);
          }
          if (!nq.choices.length) continue;
        }
        out.push(nq);
      }
      var ap = parseJSON(sv.appearance, {});
      if (!ap || typeof ap !== 'object') ap = {};
      return {
        id: sv.survey_id,
        name: clip(sv.name, 200),
        questions: out,
        position: ap.position === 'center' ? 'center' : 'bottom',
        accent: typeof ap.accent === 'string' && COLOR_RE.test(ap.accent) ? ap.accent : ''
      };
    }

    // ---- view ------------------------------------------------------------
    var CSS =
      ':host{all:initial}' +
      '*{box-sizing:border-box}' +
      '.wrap{--accent:#4f46e5;--bg:#fff;--fg:#18181b;--muted:#52525b;--line:#d4d4d8;--err:#b91c1c;' +
      'position:fixed;z-index:2147483000;font:15px/1.45 system-ui,-apple-system,"Segoe UI",Roboto,sans-serif;color:var(--fg)}' +
      '.wrap.bottom{left:0;right:0;bottom:0;display:flex;justify-content:flex-end;padding:12px;pointer-events:none}' +
      '.wrap.center{inset:0;display:flex;align-items:center;justify-content:center;padding:12px;background:rgba(0,0,0,.45)}' +
      '.card{pointer-events:auto;background:var(--bg);color:var(--fg);border:1px solid var(--line);border-radius:12px;' +
      'box-shadow:0 8px 32px rgba(0,0,0,.25);width:100%;max-width:420px;max-height:calc(100vh - 24px);overflow:auto;padding:16px}' +
      '@media (prefers-color-scheme:dark){.wrap{--bg:#18181b;--fg:#fafafa;--muted:#a1a1aa;--line:#3f3f46;--err:#f87171}}' +
      '@media (max-width:480px){.wrap.bottom{padding:0}.wrap.bottom .card{max-width:none;border-radius:12px 12px 0 0;border-bottom:0}}' +
      '@media (prefers-reduced-motion:no-preference){.card{animation:rise .2s ease-out}' +
      '@keyframes rise{from{opacity:0;transform:translateY(12px)}to{opacity:1;transform:none}}}' +
      '.head{display:flex;align-items:flex-start;gap:8px;margin-bottom:12px}' +
      '.title{flex:1;margin:0;font-size:16px;font-weight:600;outline:none}' +
      '.x{flex:none;min-width:44px;min-height:44px;margin:-10px -10px 0 0;border:0;background:none;color:var(--muted);font-size:22px;line-height:1;cursor:pointer;border-radius:8px}' +
      'button:focus-visible,input:focus-visible,textarea:focus-visible,.title:focus-visible{outline:2px solid var(--accent);outline-offset:2px}' +
      'fieldset{border:0;margin:0 0 14px;padding:0;min-width:0}' +
      'legend{padding:0;margin:0 0 6px;font-weight:500}' +
      '.req{color:var(--err)}' +
      '.opts{display:flex;flex-wrap:wrap;gap:6px}' +
      '.opt{position:relative}' +
      '.opt input{position:absolute;opacity:0;width:100%;height:100%;margin:0;cursor:pointer}' +
      '.opt span{display:flex;align-items:center;justify-content:center;min-width:44px;min-height:44px;padding:0 10px;border:1px solid var(--line);border-radius:8px;cursor:pointer}' +
      '.opt input:checked+span{background:var(--accent);border-color:var(--accent);color:#fff}' +
      '.opt input:focus-visible+span{outline:2px solid var(--accent);outline-offset:2px}' +
      '.list .opt span{justify-content:flex-start;width:100%}.list .opt{width:100%}' +
      '.scale{display:flex;justify-content:space-between;font-size:12px;color:var(--muted);margin-top:4px}' +
      'textarea{width:100%;min-height:80px;padding:8px;font:inherit;font-size:16px;color:inherit;background:transparent;border:1px solid var(--line);border-radius:8px;resize:vertical}' +
      '.err{min-height:1.2em;margin:0 0 8px;color:var(--err);font-size:13px}' +
      '.row{display:flex;gap:8px;justify-content:flex-end}' +
      '.btn{min-height:44px;padding:0 16px;font:inherit;font-weight:500;border-radius:8px;border:1px solid var(--line);background:transparent;color:inherit;cursor:pointer}' +
      '.btn.primary{background:var(--accent);border-color:var(--accent);color:#fff}' +
      '.btn[disabled]{opacity:.6;cursor:default}' +
      '.thanks{margin:0 0 14px}';

    function el(tag, cls, text) {
      var n = document.createElement(tag);
      if (cls) n.className = cls;
      if (text != null) n.textContent = text;
      return n;
    }

    function applyStyles(root) {
      try {
        if (typeof CSSStyleSheet === 'function' && 'adoptedStyleSheets' in root) {
          var sheet = new CSSStyleSheet();
          sheet.replaceSync(CSS);
          root.adoptedStyleSheets = [sheet];
          return;
        }
      } catch (e) {}
      var st = document.createElement('style');
      st.textContent = CSS;
      root.appendChild(st);
    }

    function show(model, raw) {
      var restoreTo = null;
      try { restoreTo = document.activeElement; } catch (e) {}

      var host = document.createElement('div');
      host.setAttribute('data-observe-survey', '');
      var root = host.attachShadow({ mode: 'closed' });
      applyStyles(root);

      var uid = 'obs' + randomId().slice(0, 8);
      var modal = model.position === 'center';
      var wrap = el('div', 'wrap ' + (modal ? 'center' : 'bottom'));
      if (model.accent) wrap.style.setProperty('--accent', model.accent);
      var card = el('div', 'card');
      card.setAttribute('role', 'dialog');
      card.setAttribute('aria-labelledby', uid + '-t');
      if (modal) card.setAttribute('aria-modal', 'true');

      var head = el('div', 'head');
      var title = el('h2', 'title', model.name || 'Quick survey');
      title.id = uid + '-t';
      title.setAttribute('tabindex', '-1');
      var xbtn = el('button', 'x', '×');
      xbtn.type = 'button';
      xbtn.setAttribute('aria-label', 'Dismiss survey');
      head.appendChild(title);
      head.appendChild(xbtn);
      card.appendChild(head);

      var form = el('form');
      form.setAttribute('novalidate', '');
      var getters = [];

      model.questions.forEach(function(q, qi) {
        var fs = el('fieldset');
        var lg = el('legend', null, q.text);
        if (q.required) {
          var star = el('span', 'req', ' *');
          star.setAttribute('aria-hidden', 'true');
          lg.appendChild(star);
          fs.setAttribute('aria-required', 'true');
        }
        fs.appendChild(lg);
        var name = uid + '-q' + qi;

        if (q.type === 'text') {
          var ta = el('textarea');
          ta.id = name;
          ta.name = name;
          ta.maxLength = MAX_ANSWER;
          ta.setAttribute('aria-labelledby', uid + '-l' + qi);
          lg.id = uid + '-l' + qi;
          if (q.placeholder) ta.setAttribute('placeholder', q.placeholder);
          fs.appendChild(ta);
          getters.push({ q: q, node: ta, get: function() { var v = ta.value.trim(); return v ? v.slice(0, MAX_ANSWER) : null; } });
        } else {
          var values = [];
          var wrapOpts = el('div', q.type === 'choice' ? 'opts list' : 'opts');
          var inputs = [];
          if (q.type === 'rating') for (var r = 1; r <= 5; r++) values.push(r);
          else if (q.type === 'nps') for (var n = 0; n <= 10; n++) values.push(n);
          else q.choices.forEach(function(c) { values.push(c); });
          values.forEach(function(val) {
            var lab = el('label', 'opt');
            var inp = el('input');
            inp.type = 'radio';
            inp.name = name;
            inp.value = String(val);
            var face = el('span', null, String(val));
            lab.appendChild(inp);
            lab.appendChild(face);
            wrapOpts.appendChild(lab);
            inputs.push({ input: inp, val: val });
          });
          fs.appendChild(wrapOpts);
          if (q.type === 'rating' || q.type === 'nps') {
            var sc = el('div', 'scale');
            sc.setAttribute('aria-hidden', 'true');
            sc.appendChild(el('span', null, q.type === 'nps' ? 'Not at all likely' : 'Poor'));
            sc.appendChild(el('span', null, q.type === 'nps' ? 'Extremely likely' : 'Excellent'));
            fs.appendChild(sc);
          }
          getters.push({ q: q, node: inputs[0].input, get: function() {
            for (var k = 0; k < inputs.length; k++) if (inputs[k].input.checked) return inputs[k].val;
            return null;
          } });
        }
        form.appendChild(fs);
      });

      var errBox = el('p', 'err');
      errBox.setAttribute('role', 'alert');
      form.appendChild(errBox);
      var row = el('div', 'row');
      var later = el('button', 'btn', 'Not now');
      later.type = 'button';
      var submit = el('button', 'btn primary', 'Submit');
      submit.type = 'submit';
      row.appendChild(later);
      row.appendChild(submit);
      form.appendChild(row);
      card.appendChild(form);

      wrap.appendChild(card);
      root.appendChild(wrap);
      document.body.appendChild(host);

      var closed = false;
      var finished = false;
      var responseId = randomId();
      var onKey = null;

      function close(state) {
        if (closed) return;
        closed = true;
        if (state && !finished) write(model.id, state);
        try { document.removeEventListener('keydown', onKey, true); } catch (e) {}
        try { host.remove ? host.remove() : host.parentNode.removeChild(host); } catch (e) {}
        try { if (restoreTo && restoreTo.focus) restoreTo.focus(); } catch (e) {}
        active = null;
        running = false;
      }

      function focusables() {
        return Array.prototype.slice.call(card.querySelectorAll('button,input,textarea')).filter(function(n) { return !n.disabled; });
      }
      onKey = function(ev) {
        try {
          if (ev.key === 'Escape' || ev.key === 'Esc') {
            ev.stopPropagation();
            close('dismissed');
          } else if (ev.key === 'Tab' && modal) {
            var f = focusables();
            if (!f.length) return;
            var cur = root.activeElement;
            if (ev.shiftKey && (cur === f[0] || cur === title)) { ev.preventDefault(); f[f.length - 1].focus(); }
            else if (!ev.shiftKey && cur === f[f.length - 1]) { ev.preventDefault(); f[0].focus(); }
          }
        } catch (e) {}
      };
      document.addEventListener('keydown', onKey, true);

      xbtn.addEventListener('click', function() { close('dismissed'); });
      later.addEventListener('click', function() { close('dismissed'); });

      form.addEventListener('submit', function(ev) {
        try { ev.preventDefault(); } catch (e) {}
        var answers = {};
        var count = 0;
        for (var i = 0; i < getters.length; i++) {
          var v = getters[i].get();
          if (v == null) {
            if (getters[i].q.required) {
              errBox.textContent = 'Please answer: ' + getters[i].q.text;
              getters[i].node.setAttribute('aria-invalid', 'true');
              try { getters[i].node.focus(); } catch (e) {}
              return;
            }
            continue;
          }
          answers[getters[i].q.id] = v;
          count++;
        }
        if (!count) { errBox.textContent = 'Please answer at least one question.'; return; }
        errBox.textContent = '';

        // Optimistic: the reader is thanked at once; the stable response_id
        // makes the server dedupe any retry of this same submission.
        finished = true;
        write(model.id, 'done');
        post('/respond', { survey_id: model.id, site_id: siteId, response_id: responseId, answers: answers });

        form.remove ? form.remove() : card.removeChild(form);
        var thanks = el('p', 'thanks', 'Thank you for your feedback.');
        thanks.setAttribute('role', 'status');
        var done = el('button', 'btn primary', 'Close');
        done.type = 'button';
        done.addEventListener('click', function() { close(null); });
        var r2 = el('div', 'row');
        r2.appendChild(done);
        card.appendChild(thanks);
        card.appendChild(r2);
        try { done.focus(); } catch (e) {}
      });

      active = { close: function() { close(null); } };

      // Focus management: land on the title (announces the dialog name).
      try { title.focus(); } catch (e) {}

      // One exposure per survey per page load; "shown" also feeds `once`.
      if (!exposed[model.id]) {
        exposed[model.id] = true;
        write(model.id, 'shown');
        post('/expose', { survey_id: model.id, site_id: siteId });
      }
    }

    // ---- orchestration ---------------------------------------------------
    function run() {
      if (running) return;
      running = true;
      fetchActive(function(list) {
        try {
          for (var i = 0; i < list.length; i++) {
            var sv = list[i];
            if (!sv || typeof sv.survey_id !== 'string' || !ID_RE.test(sv.survey_id)) continue;
            if (suppressed(sv)) continue;
            var model = normalise(sv);
            if (!model.questions.length) continue;
            show(model, sv);
            return;
          }
        } catch (e) {}
        running = false;
      });
    }

    function start() {
      if (document.readyState === 'loading') {
        document.addEventListener('DOMContentLoaded', run, { once: true });
      } else {
        run();
      }
    }

    // SPAs can call window.observeSurveys.check() after a route change.
    try { window.observeSurveys = { check: function() { try { if (!active) run(); } catch (e) {} } }; } catch (e) {}

    start();
  } catch (e) { /* never throw into the host page */ }
})();
