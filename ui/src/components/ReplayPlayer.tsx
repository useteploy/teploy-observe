import { useEffect, useRef, useState, useCallback, useMemo } from "preact/hooks";
import type { ReplayEvent } from "../api/replays.js";
import { heatmapsApi, type Click } from "../api/heatmaps.js";
import { streamTicketQuery } from "../api/helpers.js";
import { detectRrwebEvents, isRrwebSession } from "../lib/replayRrweb.js";
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
 * F38: rewrite a snapshot image src to the same-origin asset proxy.
 * Returns "" for anything that cannot be proxied (the attr is then
 * dropped, exactly the pre-F38 behavior). Relative srcs resolve against
 * the session's recorded base URL when one is known. The ticket is the
 * CALLER'S minted credential (TO-024: no module-global coupling between
 * independent player instances).
 */
function rewriteAssetSrc(src: string, baseURL: string, ticket: string): string {
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
  return `/api/v1/replay-assets?u=${encodeURIComponent(abs)}${ticket}`;
}

function renderSnapshotBody(root: unknown, baseURL: string, ticket: string): string {
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
        // TO-023: img/src is handled BEFORE the inert-attribute gate —
        // the old ordering ran the SAFE_ATTRS filter first, and src is
        // not (and must not be) in it, so the F38 rewrite branch below
        // was unreachable and recorded images never loaded through the
        // proxy.
        if (k === "src") {
          if (tag !== "img" || typeof v !== "string" || v.length > 2048) continue;
          const value = rewriteAssetSrc(v, baseURL, ticket);
          if (!value) continue;
          remainingChars -= value.length;
          if (remainingChars < 0) throw new Error("snapshot attribute limit");
          attrs += ` src="${escapeAttr(value)}"`;
          continue;
        }
        if (!SAFE_ATTRS.has(k) || typeof v !== "string" || v.length > 256) continue;
        remainingChars -= v.length;
        if (remainingChars < 0) throw new Error("snapshot attribute limit");
        attrs += ` ${k}="${escapeAttr(v)}"`;
      }
    }
    // img is a void element — never a paired tag with serialized children.
    if (tag === "img") return `<img${attrs}>`;
    return tag === "br" ? "<br>" : `<${tag}${attrs}>${inner}</${tag}>`;
  };
  try {
    return walk(root, 0);
  } catch {
    return '<p style="padding:32px;color:#888;font-family:system-ui;">Snapshot rejected: invalid or too complex.</p>';
  }
}

function nodeToHTML(node: SerializedNode, baseURL: string, ticket: string): string {
  return renderSnapshotBody(node, baseURL, ticket);
}

// ── O06: rrweb delta playback ──
//
// Sessions recorded by observe-replay-delta.js carry wrapped rrweb events
// ({type:'rrweb', data:<rrweb event>}). When they are present the rrweb
// Replayer CLASS (never the rrweb-player svelte UI) drives playback from
// two static bundles under /rrweb/ — same-origin assets shipped in
// ui/public, no new ui package dependencies. The trusted envelope is
// unchanged: rrweb itself creates its iframe with sandbox="allow-same-
// origin" (no allow-scripts — it drives the document from parent
// context), the player injects the REPLAY_CSP meta into every rebuilt
// document, and recorded images load exclusively through the
// /api/v1/replay-assets proxy. Events are re-sanitized at play time with
// the same pure fold the recorder ran — the second, independent layer of
// the F38 posture. Sessions without rrweb shape keep the keyframe player
// below exactly as it was.

interface RrwebReplayer {
  play(timeOffset?: number): void;
  pause(timeOffset?: number): void;
  setSpeed(speed: number): void;
  getCurrentTime(): number;
  getMetaData(): { startTime: number; endTime: number; totalTime: number };
  on(event: string, cb: (payload?: unknown) => void): void;
  destroy(): void;
  iframe?: HTMLIFrameElement | null;
}

interface RrwebRuntime {
  Replayer: new (events: unknown[], config: Record<string, unknown>) => RrwebReplayer;
  createEventSanitizer: (opts?: { baseURL?: string }) => { sanitize: (event: unknown) => unknown | null };
}

// Module-level SCRIPT cache (content, not credentials — TO-024's rule is
// about per-player tickets, which stay local below).
let rrwebRuntimePromise: Promise<RrwebRuntime> | null = null;

function loadRrwebScript(src: string): Promise<void> {
  return new Promise((resolve, reject) => {
    if (document.querySelector(`script[data-observe-replay-runtime="${src}"]`)) {
      resolve();
      return;
    }
    const el = document.createElement("script");
    el.src = src;
    el.async = true;
    el.setAttribute("data-observe-replay-runtime", src);
    el.onload = () => resolve();
    el.onerror = () => reject(new Error(`failed to load ${src}`));
    document.head.appendChild(el);
  });
}

function loadRrwebRuntime(): Promise<RrwebRuntime> {
  if (!rrwebRuntimePromise) {
    rrwebRuntimePromise = Promise.all([
      loadRrwebScript("/rrweb/replayer.js"),
      loadRrwebScript("/rrweb/sanitize.js"),
    ]).then(() => {
      const rt = (window as unknown as { __observeReplayRuntime?: RrwebRuntime }).__observeReplayRuntime;
      if (!rt || typeof rt.Replayer !== "function" || typeof rt.createEventSanitizer !== "function") {
        throw new Error("rrweb runtime did not expose Replayer/sanitizer");
      }
      return rt;
    });
    rrwebRuntimePromise.catch(() => { rrwebRuntimePromise = null; });
  }
  return rrwebRuntimePromise;
}

// Inject the trusted CSP into a rebuilt replay document. rrweb rebuilds
// the document from (sanitized) recorded events on every full snapshot;
// the meta is re-inserted at byte-zero of the head each time. The
// sanitizer replaces the recorded head subtree with a placeholder (head
// is a private tag — meta CSRF tokens, style URLs), so the rebuilt
// document has NO head element and one is created to host the CSP.
function injectReplayCSP(replayer: RrwebReplayer | null): void {
  try {
    const doc = replayer?.iframe?.contentDocument;
    if (!doc || !doc.documentElement) return;
    if (doc.querySelector('meta[http-equiv="Content-Security-Policy"]')) return;
    let head = doc.head;
    if (!head) {
      head = doc.createElement("head");
      doc.documentElement.insertBefore(head, doc.documentElement.firstChild);
    }
    const meta = doc.createElement("meta");
    meta.setAttribute("http-equiv", "Content-Security-Policy");
    meta.setAttribute("content", REPLAY_CSP);
    head.insertBefore(meta, head.firstChild);
  } catch { /* destroyed or cross-origin */ }
}

// Rewrite recorded img srcs to the same-origin asset proxy at play time
// (full-snapshot trees and mutation addedNodes; attribute-mutation srcs
// never survive sanitization, which drops src without tag context).
function rewriteRrwebAssetSrcs(events: any[], baseURL: string, ticket: string): void {
  const rewriteNode = (node: unknown) => {
    if (!node || typeof node !== "object") return;
    const n = node as Record<string, any>;
    if (n.type === 2) {
      const tag = String(n.tagName || "").toLowerCase();
      const attrs = n.attributes;
      if (tag === "img" && attrs && typeof attrs.src === "string") {
        const value = rewriteAssetSrc(attrs.src, baseURL, ticket);
        if (value) attrs.src = value;
        else delete attrs.src;
      }
    }
    if (Array.isArray(n.childNodes)) n.childNodes.forEach(rewriteNode);
  };
  for (const ev of events) {
    if (!ev || typeof ev !== "object") continue;
    if (ev.type === 2 && ev.data?.node) rewriteNode(ev.data.node);
    if (ev.type === 3 && ev.data?.source === 0 && Array.isArray(ev.data.adds)) {
      for (const add of ev.data.adds) rewriteNode(add?.node);
    }
  }
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
function snapshotToHTML(snap: Snapshot, baseURL: string, ticket: string): string {
  const csp = `<meta http-equiv="Content-Security-Policy" content="${REPLAY_CSP}">`;
  const head = `<!DOCTYPE html><html><head>${csp}</head><body>`;
  if (typeof snap.html === "string") {
    return `${head}${snap.html}</body></html>`;
  }
  if (snap.html && typeof snap.html === "object") {
    return `${head}${renderSnapshotBody(snap.html, baseURL, ticket)}</body></html>`;
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
  const rrwebRootRef = useRef<HTMLDivElement>(null);
  const replayerRef = useRef<RrwebReplayer | null>(null);

  // AUD-032: parsed input is DERIVED from the events prop
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
  const [rrwebReady, setRrwebReady] = useState(false);
  const [rrwebDuration, setRrwebDuration] = useState(0);
  const [rrwebError, setRrwebError] = useState<string | null>(null);

  // rrweb delta sessions: wrapped rrweb events with a full snapshot and
  // at least two events (the Replayer's floor). Everything else keeps the
  // keyframe player. Detection lives in lib/replayRrweb.ts (pinned there
  // by unit tests).
  const rrwebRaw = useMemo(() => detectRrwebEvents(parsed), [parsed]);
  const rrwebMode = isRrwebSession(rrwebRaw);

  const startTs = parsed.length ? parsed[0].timestamp : 0;
  const endTs = parsed.length ? parsed[parsed.length - 1].timestamp : 0;
  const duration = rrwebMode
    ? Math.max(1, rrwebDuration || endTs - startTs)
    : Math.max(1, endTs - startTs);

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

  // TO-024: generation counter invalidates obsolete snapshot loads. The
  // ticket mint is async; two quick seeks can otherwise complete out of
  // order and paint the WRONG (older) keyframe over the newer one, and an
  // unmounted player could still write into a dead iframe.
  const loadGeneration = useRef(0);

  const loadSnapshot = useCallback(async () => {
    const iframe = iframeRef.current;
    if (!iframe || !iframe.contentDocument) return;
    const generation = ++loadGeneration.current;
    setSnapshotReady(false);
    // F38: (re)mint the asset-route ticket for this snapshot's <img> load —
    // tickets live 2 minutes and every seek rewrites the DOM anyway. The
    // ticket is a LOCAL value (no module-global credential).
    const ticket = await streamTicketQuery("/api/v1/replay-assets");
    if (generation !== loadGeneration.current) return; // a newer seek won
    const doc = iframe.contentDocument;
    if (!doc) return;
    const html = snapshotEvent ? snapshotToHTML(snapshotEvent.data as Snapshot, url || "", ticket)
      : '<!DOCTYPE html><html><body style="padding:32px;color:#888;font-family:system-ui;">No snapshot recorded for this session.</body></html>';
    doc.open();
    doc.write(html);
    doc.close();
    setSnapshotReady(true);
  }, [snapshotEvent, url]);

  // TO-024: the snapshot load is its own effect (the keyboard listener no
  // longer re-registers on every keyframe change); cleanup invalidates any
  // in-flight load for this player. Keyframe mode only — the rrweb
  // replayer owns its document in delta mode.
  useEffect(() => {
    if (rrwebMode) return;
    void loadSnapshot().catch(() => {
      // A failed ticket mint leaves srcs unproxied (the documented F38
      // behavior); an unexpected failure must not become an unhandled
      // rejection.
      setSnapshotReady(false);
    });
    return () => { ++loadGeneration.current; };
  }, [loadSnapshot, rrwebMode]);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
      if (e.key === " ") { e.preventDefault(); setPlaying((p) => !p); }
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [onClose]);

  // ── rrweb replayer lifecycle ──
  useEffect(() => {
    if (!rrwebMode) return;
    let destroyed = false;
    let replayer: RrwebReplayer | null = null;
    setRrwebReady(false);
    setRrwebError(null);
    setElapsed(0);
    (async () => {
      try {
        const rt = await loadRrwebRuntime();
        if (destroyed) return;
        // Second sanitizer layer: the same pure fold the recorder ran,
        // re-applied to stored (untrusted) events before replay.
        const sanitizer = rt.createEventSanitizer(url ? { baseURL: url } : undefined);
        const ticket = await streamTicketQuery("/api/v1/replay-assets");
        if (destroyed) return;
        const sanitized: unknown[] = [];
        for (const e of rrwebRaw) {
          const s = sanitizer.sanitize({ type: e.type, data: e.data, timestamp: e.timestamp });
          if (s) sanitized.push(s);
        }
        rewriteRrwebAssetSrcs(sanitized as any[], url || "", ticket);
        const root = rrwebRootRef.current;
        if (!root) return;
        replayer = new rt.Replayer(sanitized, {
          root,
          triggerFocus: false,
          mouseTail: false,
          showWarning: false,
          insertStyleRules: [],
        });
        replayerRef.current = replayer;
        replayer.on("fullsnapshot-rebuilded", () => injectReplayCSP(replayer));
        replayer.on("finish", () => setPlaying(false));
        const meta = replayer.getMetaData();
        setRrwebDuration(Math.max(1, meta.totalTime));
        setRrwebReady(true);
      } catch (err) {
        if (!destroyed) setRrwebError(err instanceof Error ? err.message : String(err));
      }
    })();
    return () => {
      destroyed = true;
      try { replayer?.destroy(); } catch { /* already gone */ }
      if (replayerRef.current === replayer) replayerRef.current = null;
      setRrwebReady(false);
    };
  }, [rrwebMode, rrwebRaw, url]);

  // Play/pause and speed map straight onto the replayer.
  useEffect(() => {
    if (!rrwebMode || !rrwebReady) return;
    const r = replayerRef.current;
    if (!r) return;
    if (playing) r.play();
    else r.pause();
  }, [playing, rrwebReady, rrwebMode]);

  useEffect(() => {
    if (!rrwebMode || !rrwebReady) return;
    replayerRef.current?.setSpeed(speed);
  }, [speed, rrwebReady, rrwebMode]);

  // Elapsed tracking reads the replayer's clock while playing (the
  // replayer owns seek/ff state; re-render cost matches the keyframe
  // player's per-frame updates).
  useEffect(() => {
    if (!rrwebMode || !rrwebReady || !playing) return;
    let raf = 0;
    const tick = () => {
      const r = replayerRef.current;
      if (r) setElapsed(r.getCurrentTime());
      raf = requestAnimationFrame(tick);
    };
    raf = requestAnimationFrame(tick);
    return () => cancelAnimationFrame(raf);
  }, [rrwebMode, rrwebReady, playing]);

  // Apply events up to the current elapsed offset. Keyframe mode only.
  useEffect(() => {
    if (rrwebMode || !snapshotReady) return;
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
  }, [parsed, elapsed, snapshotReady, startTs, rrwebMode]);

  // Playback tick. Keyframe mode only — the rrweb replayer runs its own
  // timer in delta mode.
  useEffect(() => {
    if (rrwebMode) return;
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
  }, [playing, speed, duration, rrwebMode]);

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
    const value = Number(target.value);
    setElapsed(value);
    setPlaying(false);
    // rrweb: pause(offset) seeks and holds the frame at the offset.
    if (rrwebMode && replayerRef.current) replayerRef.current.pause(value);
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
          <div class="replay-title">
            Session replay
            <span class={`replay-mode-badge${rrwebMode ? " replay-mode-badge--delta" : ""}`}>
              {rrwebMode ? "delta recording (rrweb)" : "keyframes (structural snapshots)"}
            </span>
          </div>
          <button class="replay-close" onClick={onClose} aria-label="Close player">×</button>
        </div>

        <div class="replay-stage" ref={stageRef}>
          {rrwebMode ? (
            <div ref={rrwebRootRef} class="replay-rrweb-root" title="Session replay" />
          ) : (
            <>
              <iframe
                ref={iframeRef}
                class="replay-iframe"
                sandbox="allow-same-origin"
                title="Session replay"
              />
              <div ref={cursorRef} class="replay-cursor" aria-hidden="true" />
              <div ref={rippleRef} class="replay-ripple" aria-hidden="true" />
            </>
          )}
          {heatmapOn && (
            <HeatmapOverlay clicks={heatmapClicks} width={stageSize.w} height={stageSize.h} />
          )}
          {!parsed.length && (
            <div class="replay-empty">No replay events recorded.</div>
          )}
          {rrwebMode && rrwebError && (
            <div class="replay-empty">Replayer failed to load: {rrwebError}</div>
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
