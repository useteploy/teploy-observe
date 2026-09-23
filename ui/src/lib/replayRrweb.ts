// O06 replay mode detection: decides whether a stored event stream is an
// rrweb delta session (recorded by observe-replay-delta.js) or a
// structural-keyframe session (observe-replay.js). Pure on purpose — the
// seam is pinned by unit tests; a wrong detection here silently renders
// delta sessions through the keyframe fallback (losing all DOM change
// playback) or feeds structural sessions to the rrweb Replayer (which
// would throw on the missing full snapshot).

export interface ParsedReplayEvent {
  type: string;
  timestamp: number;
  data: any;
}

export interface WrappedRrwebEvent {
  type: number;
  data: any;
  timestamp: number;
}

export function detectRrwebEvents(parsed: ParsedReplayEvent[]): WrappedRrwebEvent[] {
  const out: WrappedRrwebEvent[] = [];
  for (const e of parsed) {
    if (e.type !== "rrweb" || !e.data || typeof e.data !== "object") continue;
    const d = e.data as Record<string, unknown>;
    if (typeof d.type !== "number") continue;
    out.push({ type: d.type, data: d.data, timestamp: e.timestamp });
  }
  return out;
}

// The Replayer requires at least two events and needs a full snapshot
// (rrweb EventType 2) to rebuild a document; without one the keyframe
// player is the only honest renderer.
export function isRrwebSession(wrapped: WrappedRrwebEvent[]): boolean {
  return wrapped.length >= 2 && wrapped.some((e) => e.type === 2);
}
