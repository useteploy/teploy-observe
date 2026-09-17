import { useEffect, useRef, useState, useCallback } from "preact/hooks";
import type { ReplayEvent } from "../api/replays.js";
import { heatmapsApi, type Click } from "../api/heatmaps.js";
import HeatmapOverlay from "./HeatmapOverlay.js";

type SerializedNode =
  | { type: "text"; value: string }
  | { type: "element"; tag: string; attrs: Record<string, string>; children: SerializedNode[] };

interface Snapshot {
  doctype?: string;
  html?: SerializedNode | string;
}

interface ParsedEvent {
  type: string;
  timestamp: number;
  data: any;
}

function parseEvents(raw: ReplayEvent[]): ParsedEvent[] {
  const parsed: ParsedEvent[] = [];
  for (const e of raw) {
    let data: any = {};
    try { data = e.data ? JSON.parse(e.data) : {}; } catch { /* ignore */ }
    const ts = typeof e.timestamp === "string" ? new Date(e.timestamp).getTime() : (e.timestamp as unknown as number);
    parsed.push({ type: e.event_type, timestamp: ts, data });
  }
  parsed.sort((a, b) => a.timestamp - b.timestamp);
  return parsed;
}

// Audit F38: replay snapshots are untrusted stored data. The player writes
// them into an iframe sandboxed without allow-scripts, so this is a
// resource/privacy boundary, not script execution — but arbitrary tags and
// attributes could still make the operator's browser issue network requests
// (img/style/meta-refresh/base). Only allowlisted structural tags survive,
// carrying only inert attributes; everything URL- or style-bearing is
// dropped, and unknown tags keep their text children.
const SAFE_TAGS = new Set([
  "html", "head", "body", "div", "span", "p", "br", "a",
  "section", "article", "header", "footer", "main", "nav", "aside",
  "ul", "ol", "li", "dl", "dt", "dd",
  "table", "thead", "tbody", "tfoot", "tr", "th", "td", "caption",
  "h1", "h2", "h3", "h4", "h5", "h6", "strong", "em", "b", "i", "u", "s",
  "small", "mark", "abbr", "cite", "blockquote", "pre", "code", "kbd", "samp",
  "sub", "sup", "figure", "figcaption", "details", "summary", "label",
]);
const SAFE_ATTRS = new Set(["class", "id", "colspan", "rowspan", "dir", "lang"]);
const REPLAY_CSP =
  "default-src 'none'; script-src 'none'; connect-src 'none'; img-src 'none'; " +
  "style-src 'none'; media-src 'none'; frame-src 'none'; object-src 'none'; " +
  "base-uri 'none'; form-action 'none'";

function nodeToHTML(node: SerializedNode): string {
  if (node.type === "text") return escapeText(String(node.value ?? ""));
  const tag = String(node.tag).toLowerCase();
  if (!SAFE_TAGS.has(tag)) {
    // Unknown/unsafe tag: keep its textual content, drop the element.
    return (node.children || []).map(nodeToHTML).join("");
  }
  const attrs = Object.entries(node.attrs || {})
    .filter(([k]) => SAFE_ATTRS.has(k))
    .map(([k, v]) => ` ${k}="${escapeAttr(String(v))}"`)
    .join("");
  if (tag === "br") return "<br>";
  const inner = (node.children || []).map(nodeToHTML).join("");
  return `<${tag}${attrs}>${inner}</${tag}>`;
}

function escapeText(s: string): string {
  return s.replace(/[&<>]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;" }[c] as string));
}
function escapeAttr(s: string): string {
  return s.replace(/[&"]/g, (c) => ({ "&": "&amp;", '"': "&quot;" }[c] as string));
}

function snapshotToHTML(snap: Snapshot): string {
  const csp = `<meta http-equiv="Content-Security-Policy" content="${REPLAY_CSP}">`;
  const doctype = snap.doctype || "<!DOCTYPE html>";
  if (typeof snap.html === "string") {
    // Legacy raw-HTML snapshots (pre-serialization tracker). We cannot
    // reliably sanitize arbitrary HTML without a parser, so a restrictive
    // CSP is injected immediately after <head> (or at the document start)
    // as a best-effort resource block; the structured path above is the
    // hardened one.
    let html = snap.html;
    if (/<head[^>]*>/i.test(html)) {
      html = html.replace(/<head([^>]*)>/i, `<head$1>${csp}`);
    } else {
      html = csp + html;
    }
    return `${doctype}<html>${html}</html>`;
  }
  if (snap.html && typeof snap.html === "object") {
    return `${doctype}${nodeToHTML(snap.html)}`;
  }
  return `${doctype}<html><body><div style="padding:32px;color:#888;font-family:system-ui;">No DOM snapshot available.</div></body></html>`;
}

interface PlayerProps {
  events: ReplayEvent[];
  onClose: () => void;
  // Optional site/URL context so the heatmap toggle can fetch aggregated
  // clicks for *this* page across *all* sessions, not just the current one.
  // Falls back to local-session clicks if either is missing.
  siteId?: string;
  url?: string;
}

export default function ReplayPlayer({ events, onClose, siteId, url }: PlayerProps) {
  const iframeRef = useRef<HTMLIFrameElement>(null);
  const stageRef = useRef<HTMLDivElement>(null);
  const cursorRef = useRef<HTMLDivElement>(null);
  const rippleRef = useRef<HTMLDivElement>(null);
  const rafRef = useRef<number | null>(null);

  const [parsed] = useState<ParsedEvent[]>(() => parseEvents(events));
  const [playing, setPlaying] = useState(true);
  const [speed, setSpeed] = useState(1);
  const [elapsed, setElapsed] = useState(0);
  const [snapshotReady, setSnapshotReady] = useState(false);
  const [heatmapOn, setHeatmapOn] = useState(false);
  const [heatmapClicks, setHeatmapClicks] = useState<Click[]>([]);
  const [stageSize, setStageSize] = useState<{ w: number; h: number }>({ w: 0, h: 0 });

  const startTs = parsed.length ? parsed[0].timestamp : 0;
  const endTs = parsed.length ? parsed[parsed.length - 1].timestamp : 0;
  const duration = Math.max(1, endTs - startTs);

  const snapshotEvent = parsed.find((e) => e.type === "snapshot");

  const loadSnapshot = useCallback(() => {
    const iframe = iframeRef.current;
    if (!iframe || !iframe.contentDocument) return;
    const html = snapshotEvent ? snapshotToHTML(snapshotEvent.data as Snapshot)
      : '<!DOCTYPE html><html><body style="padding:32px;color:#888;font-family:system-ui;">No snapshot recorded for this session.</body></html>';
    iframe.contentDocument.open();
    iframe.contentDocument.write(html);
    iframe.contentDocument.close();
    setSnapshotReady(true);
  }, [snapshotEvent]);

  useEffect(() => {
    loadSnapshot();
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
      if (e.key === " ") { e.preventDefault(); setPlaying((p) => !p); }
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [loadSnapshot, onClose]);

  // Apply events up to the current elapsed offset.
  useEffect(() => {
    if (!snapshotReady) return;
    const deadline = startTs + elapsed;
    let lastMouse: { x: number; y: number } | null = null;
    let lastClick: { x: number; y: number; ts: number } | null = null;
    let lastScroll: { x: number; y: number } | null = null;
    for (const ev of parsed) {
      if (ev.timestamp > deadline) break;
      if (ev.type === "mouse" || ev.type === "mousemove") lastMouse = ev.data;
      if (ev.type === "click") lastClick = { x: ev.data.x, y: ev.data.y, ts: ev.timestamp };
      if (ev.type === "scroll") lastScroll = ev.data;
    }
    if (lastMouse && cursorRef.current) {
      cursorRef.current.style.transform = `translate(${lastMouse.x}px, ${lastMouse.y}px)`;
      cursorRef.current.style.opacity = "1";
    }
    if (lastClick && rippleRef.current) {
      const age = deadline - lastClick.ts;
      if (age >= 0 && age < 600) {
        rippleRef.current.style.transform = `translate(${lastClick.x - 20}px, ${lastClick.y - 20}px)`;
        rippleRef.current.style.opacity = String(1 - age / 600);
      } else {
        rippleRef.current.style.opacity = "0";
      }
    }
    if (lastScroll && iframeRef.current?.contentWindow) {
      try { iframeRef.current.contentWindow.scrollTo(lastScroll.x, lastScroll.y); } catch { /* sandboxed */ }
    }
  }, [parsed, elapsed, snapshotReady, startTs]);

  // Playback tick.
  useEffect(() => {
    if (!playing) {
      if (rafRef.current) cancelAnimationFrame(rafRef.current);
      return;
    }
    let lastFrame = performance.now();
    const tick = (now: number) => {
      const dt = now - lastFrame;
      lastFrame = now;
      setElapsed((prev) => {
        const next = prev + dt * speed;
        if (next >= duration) {
          setPlaying(false);
          return duration;
        }
        return next;
      });
      rafRef.current = requestAnimationFrame(tick);
    };
    rafRef.current = requestAnimationFrame(tick);
    return () => { if (rafRef.current) cancelAnimationFrame(rafRef.current); };
  }, [playing, speed, duration]);

  // Track replay-stage size so the heatmap canvas matches the iframe area
  // exactly. Re-measures on layout changes (resize, modal open, etc.).
  useEffect(() => {
    const stage = stageRef.current;
    if (!stage) return;
    const measure = () => {
      const rect = stage.getBoundingClientRect();
      setStageSize({ w: Math.max(0, rect.width), h: Math.max(0, rect.height) });
    };
    measure();
    if (typeof ResizeObserver !== "undefined") {
      const ro = new ResizeObserver(measure);
      ro.observe(stage);
      return () => ro.disconnect();
    }
    window.addEventListener("resize", measure);
    return () => window.removeEventListener("resize", measure);
  }, []);

  // Lazy-load aggregated clicks the first time the user toggles the
  // overlay on (audit F40): replay ingestion already writes this session's
  // clicks into the aggregate, so ADDING the local clicks to the fetched
  // rollup double-counted them once the rollup was visible. The aggregate
  // is authoritative when it has data; the local-session buckets are a
  // labeled fallback for a fresh install where no rollup exists yet.
  useEffect(() => {
    if (!heatmapOn) return;
    if (heatmapClicks.length > 0) return;
    let cancelled = false;
    const run = async () => {
      const localClicks: Click[] = [];
      const counts = new Map<string, { x: number; y: number; n: number }>();
      for (const ev of parsed) {
        if (ev.type !== "click") continue;
        const x = Number(ev.data?.x);
        const y = Number(ev.data?.y);
        if (!Number.isFinite(x) || !Number.isFinite(y)) continue;
        // Same 10px buckets as the server so the local fallback looks the
        // same as the cross-session view.
        const bx = Math.floor(x / 10);
        const by = Math.floor(y / 10);
        const k = `${bx},${by}`;
        const cur = counts.get(k);
        if (cur) {
          cur.n++;
        } else {
          counts.set(k, { x: bx * 10 + 5, y: by * 10 + 5, n: 1 });
        }
      }
      counts.forEach((v) => localClicks.push({ x: v.x, y: v.y, count: v.n }));

      if (siteId && url) {
        try {
          const now = new Date();
          const from = new Date(now.getTime() - 30 * 86400000).toISOString();
          const to = now.toISOString();
          const remote = await heatmapsApi.query(siteId, url, from, to);
          if (cancelled) return;
          // The rollup includes this session — use it alone, never merged
          // with the local copy of the same clicks.
          if (remote && remote.length > 0) {
            setHeatmapClicks(remote.slice());
            return;
          }
        } catch {
          // Fall through to local-only clicks.
        }
      }
      if (!cancelled) setHeatmapClicks(localClicks);
    };
    run();
    return () => { cancelled = true; };
  }, [heatmapOn, siteId, url, parsed, heatmapClicks.length]);

  const onScrub = (e: Event) => {
    const target = e.target as HTMLInputElement;
    setElapsed(Number(target.value));
    setPlaying(false);
  };

  const fmt = (ms: number) => {
    const total = Math.max(0, Math.floor(ms / 1000));
    const mm = Math.floor(total / 60).toString().padStart(2, "0");
    const ss = (total % 60).toString().padStart(2, "0");
    return `${mm}:${ss}`;
  };

  return (
    <div class="replay-overlay" role="dialog" aria-label="Session replay">
      <div class="replay-modal">
        <div class="replay-header">
          <div class="replay-title">Session replay</div>
          <button class="replay-close" onClick={onClose} aria-label="Close player">×</button>
        </div>

        <div class="replay-stage" ref={stageRef}>
          <iframe
            ref={iframeRef}
            class="replay-iframe"
            sandbox="allow-same-origin"
            title="Session replay"
          />
          <div ref={cursorRef} class="replay-cursor" aria-hidden="true" />
          <div ref={rippleRef} class="replay-ripple" aria-hidden="true" />
          {heatmapOn && (
            <HeatmapOverlay clicks={heatmapClicks} width={stageSize.w} height={stageSize.h} />
          )}
          {!parsed.length && (
            <div class="replay-empty">No replay events recorded.</div>
          )}
        </div>

        <div class="replay-controls">
          <button class="replay-play" onClick={() => setPlaying(!playing)} aria-label={playing ? "Pause" : "Play"}>
            {playing ? "Pause" : "Play"}
          </button>
          <button
            class={`replay-heatmap-toggle${heatmapOn ? " replay-heatmap-toggle--on" : ""}`}
            onClick={() => setHeatmapOn((v) => !v)}
            aria-pressed={heatmapOn}
            aria-label="Toggle click heatmap"
            data-testid="heatmap-toggle"
          >
            {heatmapOn ? "Heatmap on" : "Heatmap"}
          </button>
          <span class="replay-time">{fmt(elapsed)} / {fmt(duration)}</span>
          <input
            class="replay-scrub"
            type="range"
            min={0}
            max={duration}
            step={100}
            value={elapsed}
            onInput={onScrub}
          />
          <select class="replay-speed" value={speed} onChange={(e) => setSpeed(Number((e.target as HTMLSelectElement).value))}>
            <option value={0.5}>0.5×</option>
            <option value={1}>1×</option>
            <option value={2}>2×</option>
            <option value={4}>4×</option>
          </select>
        </div>
      </div>
    </div>
  );
}
