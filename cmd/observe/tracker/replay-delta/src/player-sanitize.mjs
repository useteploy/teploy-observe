// O06 player-side sanitizer entry: the SECOND, independent application of
// the same pure fold the recorder ran before anything left the browser
// (F38 two-layer posture). Exposed as a static bundle under
// ui/public/rrweb/ so the dashboard loads it without a new package
// dependency; the player re-sanitizes every stored rrweb event at play
// time before feeding the Replayer.
import { createEventSanitizer } from './sanitizer.mjs';

(function () {
  var runtime = (window.__observeReplayRuntime = window.__observeReplayRuntime || {});
  runtime.createEventSanitizer = createEventSanitizer;
})();
