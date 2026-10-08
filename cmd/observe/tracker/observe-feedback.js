(function(){
  'use strict';

  var script = document.currentScript;
  if (!script) return;

  var origin = script.src ? new URL(script.src).origin : '';
  var endpoint = script.getAttribute('data-endpoint') || origin + '/api/v1/feedback';
  var siteId = script.getAttribute('data-site-id') || '';
  var position = script.getAttribute('data-position') || 'bottom-right';
  var color = script.getAttribute('data-color') || '#6366f1';

  var widget = null;
  var generation = 0;
  var pending = false;

  function createWidget() {
    widget = document.createElement('div');
    widget.id = 'obs-feedback-widget';
    var pos = position === 'bottom-left' ? 'left:20px;' : 'right:20px;';
    widget.innerHTML =
      '<button id="obs-fb-btn" style="position:fixed;bottom:20px;' + pos + 'z-index:9999;background:' + color + ';color:#fff;border:none;padding:8px 16px;border-radius:6px;font-size:13px;font-weight:500;cursor:pointer;font-family:-apple-system,sans-serif;box-shadow:0 2px 8px rgba(0,0,0,0.2);">Feedback</button>' +
      '<div id="obs-fb-form" style="display:none;position:fixed;bottom:60px;' + pos + 'z-index:9999;background:#111114;border:1px solid #27272a;border-radius:8px;padding:16px;width:300px;font-family:-apple-system,sans-serif;box-shadow:0 8px 24px rgba(0,0,0,0.4);">' +
        '<div style="font-size:14px;font-weight:600;color:#fafafa;margin-bottom:12px;">Send Feedback</div>' +
        '<textarea id="obs-fb-msg" placeholder="What\'s on your mind?" style="width:100%;height:80px;background:#0f0f12;border:1px solid #27272a;border-radius:6px;color:#fafafa;padding:8px;font-size:13px;font-family:inherit;resize:vertical;"></textarea>' +
        '<input id="obs-fb-email" type="email" placeholder="Email (optional)" style="width:100%;margin-top:8px;background:#0f0f12;border:1px solid #27272a;border-radius:6px;color:#fafafa;padding:6px 8px;font-size:13px;font-family:inherit;">' +
        '<div style="display:flex;gap:8px;margin-top:12px;">' +
          '<button id="obs-fb-cancel" style="flex:1;padding:6px;background:transparent;border:1px solid #27272a;border-radius:6px;color:#a1a1aa;cursor:pointer;font-size:13px;font-family:inherit;">Cancel</button>' +
          '<button id="obs-fb-submit" style="flex:1;padding:6px;background:' + color + ';border:none;border-radius:6px;color:#fff;cursor:pointer;font-size:13px;font-weight:500;font-family:inherit;">Send</button>' +
        '</div>' +
        '<div id="obs-fb-status" style="display:none;margin-top:8px;font-size:12px;color:#22c55e;text-align:center;"></div>' +
      '</div>';

    document.body.appendChild(widget);

    document.getElementById('obs-fb-btn').addEventListener('click', function() {
      var f = document.getElementById('obs-fb-form');
      generation++;
      document.getElementById('obs-fb-status').style.display = 'none';
      f.style.display = f.style.display === 'none' ? 'block' : 'none';
    });

    document.getElementById('obs-fb-cancel').addEventListener('click', function() {
      generation++;
      document.getElementById('obs-fb-form').style.display = 'none';
    });

    document.getElementById('obs-fb-submit').addEventListener('click', function() {
      if (pending) return;
      var messageInput = document.getElementById('obs-fb-msg');
      var emailInput = document.getElementById('obs-fb-email');
      var capturedMessage = messageInput.value;
      var capturedEmail = emailInput.value;
      var msg = capturedMessage.trim();
      if (!msg) return;
      var requestGeneration = generation;
      var submit = document.getElementById('obs-fb-submit');
      var status = document.getElementById('obs-fb-status');
      pending = true;
      submit.disabled = true;
      status.style.display = 'none';
      var payload = {
        site_id: siteId,
        url: (function(){ try { return location.origin + location.pathname; } catch (e) { return ''; } })(),
        message: msg, email: capturedEmail.trim(), category: 'feedback'
      };
      Promise.resolve().then(function() {
        return fetch(endpoint, {
          method: 'POST', redirect: 'error',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(payload)
        });
      }).then(function(response) {
        if (!response.ok) throw new Error('submission refused');
        return response.json();
      }).then(function(ack) {
        if (!ack || ack.ok !== true) throw new Error('submission not confirmed');
        if (generation !== requestGeneration || messageInput.value !== capturedMessage || emailInput.value !== capturedEmail) return;
        messageInput.value = '';
        emailInput.value = '';
        status.textContent = 'Thanks for your feedback!';
        status.style.color = '#22c55e';
        status.style.display = 'block';
        setTimeout(function() {
          if (generation !== requestGeneration || messageInput.value || emailInput.value) return;
          document.getElementById('obs-fb-form').style.display = 'none';
          status.style.display = 'none';
        }, 2000);
      }).catch(function() {
        if (generation !== requestGeneration) return;
        status.textContent = 'Feedback could not be sent. Your draft is saved; please try again.';
        status.style.color = '#ef4444';
        status.style.display = 'block';
      }).then(function() { pending = false; submit.disabled = false; });
    });
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', createWidget);
  } else {
    createWidget();
  }
})();
