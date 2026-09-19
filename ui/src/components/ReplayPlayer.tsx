import { useEffect, useRef, useState, useCallback, useMemo } from "preact/hooks";
import type { ReplayEvent } from "../api/replays.js";
import { heatmapsApi, type Click } from "../api/heatmaps.js";
import { streamTicketQuery } from "../api/helpers.js";
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
    // AUD-031 (round 2): non-finite timestamps would poison the sort and
    // the playback math — drop them instead of trusting stored data.
    if (!Number.isFinite(ts)) continue;
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
  // F38: img renders through the same-origin asset proxy (see
  // rewriteAssetSrc) — never by letting the operator's browser dial the
  // recorded page's origins directly.
  "img",
]);
const SAFE_ATTRS = new Set(["class", "id", "colspan", "rowspan", "dir", "lang", "alt", "width", "height"]);
const REPLAY_CSP =
  "default-src 'none'; script-src 'none'; connect-src 'none'; img-src 'self'; " +
  "style-src 'none'; media-src 'none'; frame-src 'none'; object-src 'none'; " +
  "base-uri 'none'; form-action 'none'";

// AUD-031 (round 2): the renderer is bounded and shape-validating — a
// stored snapshot with a null child, a non-array children field, or a
// 500k-node tree used to throw through (or freeze) the operator's player.
// Over-budget or malformed snapshots render an explicit placeholder.
const MAX_RENDER_NODES = 5000;
const MAX_RENDER_DEPTH = 32;
const MAX_RENDER_CHARS = 128 * 1024;
const MAX_RENDER_ATTRS = 32;

/**
 * F38: the credential fragment the player appends to proxied asset URLs.
 * Re-minted per snapshot write (tickets live 2 minutes and bind to the
 * asset route); empty when no session token exists (share view) or minting
 * failed — then srcs are simply not rewritten and images do not load.
 */
let assetTicket = "";

/**
 * Rewrite a snapshot image src to the same-origin asset proxy. Returns ""
 * for anything that cannot be proxied (the attr is then dropped, exactly
 * the pre-F38 behavior). Relative srcs resolve against the session's
 * recorded base URL when one is known.
 */
function rewriteAssetSrc(src: string, baseURL: string): string {
  if (!src || src.startsWith("data:") || src.startsWith("blob:")) return "";
  let abs = src;
  if (baseURL && !/^https?:\/\//i.test(src)) {
    try {
      abs = new URL(src, baseURL).toString();
    } catch {
      return "";
    }
  }
  if (!/^https?:\/\//i.test(abs)) return "";
  return `/api/v1/replay-assets?u=${encodeURIComponent(abs)}${assetTicket}`;
}

function renderSnapshotBody(root: unknown, baseURL: string): string {
  let remainingNodes = MAX_RENDER_NODES;
  let remainingChars = MAX_RENDER_CHARS;
  const walk = (value: unknown, depth: number): string => {
    if (--remainingNodes < 0 || depth > MAX_RENDER_DEPTH) throw new Error("snapshot complexity limit");
    if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error("invalid node");
    const n = value as Record<string, unknown>;
    if (n.type === "text") {
      if (typeof n.value !== "string") throw new Error("invalid text node");
      remainingChars -= n.value.length;
      if (remainingChars < 0) throw new Error("snapshot text limit");
      return escapeText(n.value);
    }
    if (n.type !== "element" || typeof n.tag !== "string" || !Array.isArray(n.children)) {
      throw new Error("invalid element node");
    }
    const tag = n.tag.toLowerCase();
    // AUD-028 (round 2): a serialized head subtree is dropped — the
    // trusted envelope carries its own head with the CSP.
    if (tag === "head") return "";
    const inner = n.children.map((c) => walk(c, depth + 1)).join("");
    if (tag === "html" || tag === "body" || !SAFE_TAGS.has(tag)) return inner;
    let attrs = "";
    if (n.attrs && typeof n.attrs === "object" && !Array.isArray(n.attrs)) {
      const entries = Object.entries(n.attrs as Record<string, unknown>);
      if (entries.length > MAX_RENDER_ATTRS) throw new Error("snapshot attribute count limit");
      for (const [k, v] of entries) {
        if (!SAFE_ATTRS.has(k) || typeof v !== "string" || v.length > 256) continue;
        // F38: img src never passes through — either the asset proxy URL
        // or nothing. Every other URL-bearing attr was already outside
        // SAFE_ATTRS.
        let value = v;
        if (k === "src") {
          if (tag !== "img") continue;
          value = rewriteAssetSrc(v, baseURL);
          if (!value) continue;
        }
        remainingChars -= v.length;
        if (remainingChars < 0) throw new Error("snapshot attribute limit");
        attrs += ` ${k}="${escapeAttr(value)}"`;
      }
    }
    return tag === "br" ? "<br>" : `<${tag}${attrs}>${inner}</${tag}>`;
  };
  try {
    return walk(root, 0);
  } catch {
    return '<p style="padding:32px;color:#888;font-family:system-ui;">Snapshot rejected: invalid or too complex.</p>';
  }
}

function nodeToHTML(node: SerializedNode, baseURL: string): string {
  return renderSnapshotBody(node, baseURL);
}

function escapeText(s: string): string {
  return s.replace(/[&<>]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;" }[c] as string));
}
function escapeAttr(s: string): string {
  return s.replace(/[&"]/g, (c) => ({ "&": "&amp;", '"': "&quot;" }[c] as string));
}

// AUD-028 (round 2): the document envelope is TRUSTED and constant — the
// doctype and the CSP-bearing head never come from stored replay data (the
// untrusted snap.doctype could carry markup outside the tag allowlist, and
// the structured branch returned without inserting the CSP it built).
// The structured body renders through the bounded allowlist renderer; the
// legacy raw-HTML path prepends the CSP at byte zero so no resource-bearing
// markup can precede it (best-effort — the hardened path is the structured
// one, tracked with F38's legacy work).
function snapshotToHTML(snap: Snapshot, baseURL: string): string {
  const csp = `<meta http-equiv="Content-Security-Policy" content="${REPLAY_CSP}">`;
  const head = `<!DOCTYPE html><html><head>${csp}</head><body>`;
  if (typeof snap.html === "string") {
    return `${head}${snap.html}</body></html>`;
  }
  if (snap.html && typeof snap.html === "object") {
    return `${head}${renderSnapshotBody(snap.html, baseURL)}</body></html>`;
  }
  return `${head}<div style="padding:32px;color:#888;font-family:system-ui;">No DOM snapshot available.</div></body></html>`;
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

  // AUD-032 (round 2): parsed input is DERIVED from the events prop
  // (useMemo) — the one-time useState kept the previous dataset when the
  // prop changed without a remount.
  const parsed = useMemo(() => parseEvents(events), [events]);
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

  // AUD-032: the active snapshot is the most recent keyframe at or before
  // the playhead — a session with multiple snapshots used to render the
  // first one forever, so post-navigation playback showed stale content.
  const keyframes = useMemo(() => parsed.filter((e) => e.type === "snapshot"), [parsed]);
  const snapshotEvent = useMemo(() => {
    const deadline = startTs + elapsed;
    let selected: ParsedEvent | undefined;
    for (const k of keyframes) {
      if (k.timestamp > deadline) break;
      selected = k;
    }
    return selected;
  }, [keyframes, startTs, elapsed]);

  const loadSnapshot = useCallback(async () => {
    const iframe = iframeRef.current;
    if (!iframe || !iframe.contentDocument) return;
    // F38: (re)mint the asset-route ticket for this snapshot's <img> load —
    // tickets live 2 minutes and every seek rewrites the DOM anyway.
    assetTicket = await streamTicketQuery("/api/v1/replay-assets");
    const html = snapshotEvent ? snapshotToHTML(snapshotEvent.data as Snapshot, url || "")
      : '<!DOCTYPE html><html><body style="padding:32px;color:#888;font-family:system-ui;">No snapshot recorded for this session.</body></html>';
    iframe.contentDocument.open();
    iframe.contentDocument.write(html);
    iframe.contentDocument.close();
    setSnapshotReady(true);
  }, [snapshotEvent, url]);

  useEffect(() => {
    void loadSnapshot();
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
      // AUD-031 (round 2): event data is untrusted stored JSON — a null
      // click payload or non-object data used to throw during playback.
      if (!ev.data || typeof ev.data !== "object") continue;
      if (ev.type === "mouse" || ev.type === "mousemove") {
        if (Number.isFinite(ev.data.x) && Number.isFinite(ev.data.y)) lastMouse = ev.data;
      }
      if (ev.type === "click" && Number.isFinite(ev.data.x) && Number.isFinite(ev.data.y)) {
        lastClick = { x: ev.data.x, y: ev.data.y, ts: ev.timestamp };
      }
      if (ev.type === "scroll" && Number.isFinite(ev.data.x) && Number.isFinite(ev.data.y)) {
        lastScroll = ev.data;
      }
    }
    // AUD-032 (round 2): state resets when no event of a kind precedes the
    // playhead — seeking backward used to leave the future cursor, click
    // ripple, and scroll position on screen.
    if (cursorRef.current) {
      if (lastMouse) {
        cursorRef.current.style.transform = `translate(${lastMouse.x}px, ${lastMouse.y}px)`;
        cursorRef.current.style.opacity = "1";
      } else {
        cursorRef.current.style.opacity = "0";
      }
    }
    if (rippleRef.current) {
      if (lastClick) {
        const age = deadline - lastClick.ts;
        if (age >= 0 && age < 600) {
          rippleRef.current.style.transform = `translate(${lastClick.x - 20}px, ${lastClick.y - 20}px)`;
          rippleRef.current.style.opacity = String(1 - age / 600);
        } else {
          rippleRef.current.style.opacity = "0";
        }
      } else {
        rippleRef.current.style.opacity = "0";
      }
    }
    if (iframeRef.current?.contentWindow) {
      try { iframeRef.current.contentWindow.scrollTo(lastScroll?.x ?? 0, lastScroll?.y ?? 0); } catch { /* sandboxed */ }
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
