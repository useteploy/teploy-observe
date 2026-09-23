// O06 player-side runtime entry: exposes the rrweb Replayer CLASS (never
// the rrweb-player svelte UI) as a same-origin static bundle under
// ui/public/rrweb/, so the dashboard ships it without a new ui package
// dependency. The bundle injects rrweb's own replayer stylesheet once
// (cursor + wrapper); everything else about the envelope — the
// allow-same-origin sandbox rrweb itself creates for its iframe, the CSP
// meta the player injects into every rebuilt document, and image loads
// through /api/v1/replay-assets — stays in Observe's player component.
import { Replayer } from 'rrweb';
import replayerCSS from 'rrweb/dist/style.css';

(function () {
  var runtime = (window.__observeReplayRuntime = window.__observeReplayRuntime || {});
  runtime.Replayer = Replayer;
  if (!runtime.styleInjected) {
    runtime.styleInjected = true;
    var style = document.createElement('style');
    style.textContent = replayerCSS;
    document.head.appendChild(style);
  }
})();
