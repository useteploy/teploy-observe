import { useEffect, useRef, useState, useCallback } from "preact/hooks";
import type { ReplayEvent } from "../api/replays.js";

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

function nodeToHTML(node: SerializedNode): string {
  if (node.type === "text") return escapeText(node.value);
  const attrs = Object.entries(node.attrs || {})
    .map(([k, v]) => ` ${k}="${escapeAttr(String(v))}"`)
    .join("");
  const voidTags = new Set(["area", "base", "br", "col", "embed", "hr", "img", "input", "link", "meta", "source", "track", "wbr"]);
  if (voidTags.has(node.tag)) return `<${node.tag}${attrs}>`;
  const inner = (node.children || []).map(nodeToHTML).join("");
  return `<${node.tag}${attrs}>${inner}</${node.tag}>`;
}

function escapeText(s: string): string {
  return s.replace(/[&<>]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;" }[c] as string));
}
function escapeAttr(s: string): string {
  return s.replace(/[&"]/g, (c) => ({ "&": "&amp;", '"': "&quot;" }[c] as string));
}

function snapshotToHTML(snap: Snapshot): string {
  const doctype = snap.doctype || "<!DOCTYPE html>";
  if (typeof snap.html === "string") {
    return `${doctype}<html>${snap.html}</html>`;
  }
  if (snap.html && typeof snap.html === "object") {
    return `${doctype}${nodeToHTML(snap.html)}`;
  }
  return `${doctype}<html><body><div style="padding:32px;color:#888;font-family:system-ui;">No DOM snapshot available.</div></body></html>`;
}

interface PlayerProps {
  events: ReplayEvent[];
  onClose: () => void;
}

export default function ReplayPlayer({ events, onClose }: PlayerProps) {
  const iframeRef = useRef<HTMLIFrameElement>(null);
  const cursorRef = useRef<HTMLDivElement>(null);
  const rippleRef = useRef<HTMLDivElement>(null);
  const rafRef = useRef<number | null>(null);

  const [parsed] = useState<ParsedEvent[]>(() => parseEvents(events));
  const [playing, setPlaying] = useState(true);
  const [speed, setSpeed] = useState(1);
  const [elapsed, setElapsed] = useState(0);
  const [snapshotReady, setSnapshotReady] = useState(false);

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

        <div class="replay-stage">
          <iframe
            ref={iframeRef}
            class="replay-iframe"
            sandbox="allow-same-origin"
            title="Session replay"
          />
          <div ref={cursorRef} class="replay-cursor" aria-hidden="true" />
          <div ref={rippleRef} class="replay-ripple" aria-hidden="true" />
          {!parsed.length && (
            <div class="replay-empty">No replay events recorded.</div>
          )}
        </div>

        <div class="replay-controls">
          <button class="replay-play" onClick={() => setPlaying(!playing)} aria-label={playing ? "Pause" : "Play"}>
            {playing ? "Pause" : "Play"}
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
