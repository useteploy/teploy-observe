// Pure helpers for the replay console/network panels. The recorder's opt-in
// capture stores `console` and `network` events in the replay stream; the
// player's clock is milliseconds since the earliest event, so offsets here use
// the same origin and a panel can show "everything up to now".

export interface ReplayEventLike {
  timestamp: string | number;
  event_type: string;
  data: string;
}

export interface ConsoleEntry {
  offsetMs: number;
  level: "log" | "info" | "warn" | "error";
  message: string;
  truncated: boolean;
}

export interface NetworkEntry {
  offsetMs: number;
  method: string;
  url: string;
  status: number;
  durationMs: number;
  size: number | null;
  kind: string;
}

export interface CapturedStreams {
  console: ConsoleEntry[];
  network: NetworkEntry[];
}

const LEVELS = new Set(["log", "info", "warn", "error"]);
// Render bounds: stored data is untrusted, so the panel never shows more than
// the capture caps (200 console / 500 network) and cuts long strings.
const MAX_CONSOLE = 200;
const MAX_NETWORK = 500;
const MAX_TEXT = 1024;

function toMs(ts: string | number): number {
  return typeof ts === "number" ? ts : new Date(ts).getTime();
}

function parseObject(raw: string): Record<string, unknown> | null {
  try {
    const v = JSON.parse(raw);
    return v && typeof v === "object" && !Array.isArray(v) ? (v as Record<string, unknown>) : null;
  } catch {
    return null;
  }
}

export function extractCaptured(events: ReplayEventLike[]): CapturedStreams {
  let start = Infinity;
  for (const e of events) {
    const t = toMs(e.timestamp);
    if (Number.isFinite(t) && t < start) start = t;
  }
  const out: CapturedStreams = { console: [], network: [] };
  if (!Number.isFinite(start)) return out;

  for (const e of events) {
    if (e.event_type !== "console" && e.event_type !== "network") continue;
    const t = toMs(e.timestamp);
    if (!Number.isFinite(t)) continue;
    const d = parseObject(e.data);
    if (!d) continue;
    const offsetMs = Math.max(0, t - start);
    if (e.event_type === "console") {
      if (out.console.length >= MAX_CONSOLE) continue;
      const level = typeof d.level === "string" && LEVELS.has(d.level) ? (d.level as ConsoleEntry["level"]) : null;
      if (!level || typeof d.message !== "string") continue;
      out.console.push({ offsetMs, level, message: d.message.slice(0, MAX_TEXT), truncated: d.truncated === true });
    } else {
      if (out.network.length >= MAX_NETWORK) continue;
      if (typeof d.method !== "string" || typeof d.url !== "string") continue;
      out.network.push({
        offsetMs,
        method: d.method.slice(0, 16),
        url: d.url.slice(0, 2048),
        status: typeof d.status === "number" ? d.status : 0,
        durationMs: typeof d.duration_ms === "number" ? d.duration_ms : 0,
        size: typeof d.size === "number" ? d.size : null,
        kind: typeof d.kind === "string" ? d.kind.slice(0, 8) : "",
      });
    }
  }
  out.console.sort((a, b) => a.offsetMs - b.offsetMs);
  out.network.sort((a, b) => a.offsetMs - b.offsetMs);
  return out;
}

/** Entries at or before the player clock (items must be sorted by offsetMs). */
export function upTo<T extends { offsetMs: number }>(items: T[], elapsedMs: number): T[] {
  let lo = 0;
  let hi = items.length;
  while (lo < hi) {
    const mid = (lo + hi) >> 1;
    if (items[mid].offsetMs <= elapsedMs) lo = mid + 1;
    else hi = mid;
  }
  return items.slice(0, lo);
}

export function formatOffset(ms: number): string {
  const s = Math.floor(ms / 1000);
  return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}`;
}
