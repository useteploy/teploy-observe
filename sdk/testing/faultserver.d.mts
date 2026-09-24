// Type declarations for the shared fault-injection server (see
// sdk/testing/README.md). Kept next to the .mjs so both Node test suites
// (browser SDK, sentry-shim) typecheck against the one implementation.

export type FaultMode =
  | { kind: "ok" }
  | { kind: "status"; code: number }
  | { kind: "failN"; n: number }
  | { kind: "partial"; ack: Record<string, unknown> }
  | { kind: "slow"; delayMs: number }
  | { kind: "reset" }
  | { kind: "redirect"; location: string };

export interface RecordedRequest {
  path: string;
  headers: Record<string, string | string[] | undefined>;
  bodyText: string;
}

export class FaultServer {
  requests: RecordedRequest[];
  mode: FaultMode;
  constructor();
  url(path?: string): string;
  start(): Promise<void>;
  close(): Promise<void>;
}
