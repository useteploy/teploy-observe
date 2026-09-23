// X05/O06 experiment arm B: the rrweb 2.x wrapper in the shape the adopted
// integration contract specifies (DELEGATED_DECISIONS 2026-09-23 section 2):
// rrweb.record feeds an Observe sanitizer transform BEFORE batching, and the
// sanitized events ride batch POSTs to the same ingest endpoint shape the
// current recorder uses. For the OVERHEAD EXPERIMENT the sanitizer is a
// pass-through (its cost is not part of the reversal thresholds - Observe's
// own rules run in both futures); the batching, transport and keyframe
// cadence ARE the measured configuration.
//
// Sampling: rrweb defaults for input sampling (mousemove 50 ms, scroll
// 100 ms), checkoutEveryNms 30 s to match arm A's re-snapshot cadence -
// keyframe parity is what makes the session-bytes comparison honest.
import { record } from 'rrweb';

(function () {
  var script = document.currentScript;
  if (!script) return;
  var origin = script.src ? new URL(script.src).origin : '';
  var endpoint = script.getAttribute('data-endpoint') || origin + '/api/v1/replays';
  var siteId = script.getAttribute('data-site-id') || '';
  var apiKey = script.getAttribute('data-api-key') || '';
  var flushIntervalMs = 10000;

  var events = [];
  var stop = null;
  var sessionId = '';
  var chunkSeq = 0;
  var bytesShipped = 0;
  var eventCount = 0;

  function makeId() {
    var b = new Uint8Array(16);
    (self.crypto || {}).getRandomValues ? crypto.getRandomValues(b) : b.fill(0);
    return Array.prototype.map.call(b, function (x) { return x.toString(16).padStart(2, '0'); }).join('');
  }
  sessionId = makeId();
  var producerId = makeId();

  // Sanitizer seam (pass-through for the experiment; the integration slice
  // implements the allowlist here - applied to snapshots AND incremental
  // mutation payloads, characterData included).
  var sanitize = function (e) { return e; };

  function flush() {
    if (!events.length) return;
    var batch = events.splice(0, events.length);
    var body = JSON.stringify({
      protocol_version: 2,
      producer_id: producerId,
      chunk_seq: chunkSeq++,
      session_id: sessionId,
      site_id: siteId,
      events: batch,
    });
    bytesShipped += body.length;
    fetch(endpoint, {
      method: 'POST',
      headers: {
        'content-type': 'application/json',
        'x-observe-site': siteId,
        'x-observe-api-key': apiKey,
      },
      body: body,
      keepalive: true,
    }).catch(function () { /* retry semantics are the integration slice's */ });
  }

  stop = record({
    emit: function (event) {
      event = sanitize(event);
      if (!event) return;
      events.push(event);
      eventCount++;
    },
    sampling: { mousemove: 50, scroll: 100, input: 'last' },
    checkoutEveryNms: 30000,
  });
  setInterval(flush, flushIntervalMs);
  window.addEventListener('pagehide', flush);

  window.__x05armB = {
    sessionId: function () { return sessionId; },
    stats: function () { return { events: eventCount, bytesShipped: bytesShipped }; },
  };
})();
