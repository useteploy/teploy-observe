/**
 * Top loading bar, matching teploy-dash: a light sweeps left to right along the
 * sidebar header's bottom rule while any request is in flight, and completes at
 * the right edge when the last one settles.
 *
 * Counts in-flight requests rather than tracking a single one — a page that
 * fires six panels in parallel should show one continuous sweep, not six
 * restarts. The element is looked up per call because the sidebar mounts after
 * the first requests may already have started.
 */
let inflight = 0;
let fadeTimer: ReturnType<typeof setTimeout> | undefined;
let resetTimer: ReturnType<typeof setTimeout> | undefined;

function bar(): HTMLElement | null {
  if (typeof document === "undefined") return null;
  return document.getElementById("obs-load-bar");
}

export function progressStart(): void {
  inflight++;
  if (inflight !== 1) return; // already sweeping
  const el = bar();
  if (!el) return;
  clearTimeout(fadeTimer);
  clearTimeout(resetTimer);
  el.style.transition = "none";
  el.style.width = "0%";
  el.style.opacity = "1";
  void el.offsetWidth; // reflow, so the reset lands before the sweep animates
  el.style.transition = "";
  el.style.width = "90%"; // race toward the right, easing as it goes
}

export function progressDone(): void {
  if (inflight > 0) inflight--;
  if (inflight !== 0) return; // wait for every request to settle
  const el = bar();
  if (!el) return;
  el.style.width = "100%";
  fadeTimer = setTimeout(() => {
    el.style.opacity = "0";
    resetTimer = setTimeout(() => {
      el.style.width = "0%";
    }, 300);
  }, 220);
}

/** Wraps a request so the bar always settles, including on failure. */
export async function withProgress<T>(run: () => Promise<T>): Promise<T> {
  progressStart();
  try {
    return await run();
  } finally {
    progressDone();
  }
}
