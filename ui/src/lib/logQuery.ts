// Pure helpers for the log query language (the `lq` parameter of
// GET /api/v1/logs/search). The grammar lives in internal/logs/logql.go; this
// file only builds requests, reads responses and renders syntax errors.

export const LOG_QUERY_MAX_LENGTH = 1024;

export interface LogSearchPage<T> {
  logs: T[];
  nextCursor: string;
  // true when attribute filters were checked over a bounded window and older
  // matches may exist beyond it.
  truncated: boolean;
  // "sql" (database evaluated everything), "go_window" (checked in Go over a
  // bounded candidate window) or "legacy" (no query language in use).
  verification: string;
}

export interface QuerySyntaxError {
  position: number;
  message: string;
}

// The server answers a bad query with a 400 whose detail reads
// "query error at position N: message".
export function parseQuerySyntaxError(detail: string): QuerySyntaxError | null {
  const m = /^query error at position (\d+): (.*)$/s.exec(detail.trim());
  if (!m) return null;
  return { position: Number(m[1]), message: m[2] };
}

// Accepts both response shapes: the plain search returns a bare array, the
// query-language search returns { logs, next_cursor, truncated, verification }.
export function normalizeSearchResponse<T>(resp: unknown): LogSearchPage<T> {
  if (Array.isArray(resp)) {
    return { logs: resp as T[], nextCursor: "", truncated: false, verification: "legacy" };
  }
  const r = (resp ?? {}) as Record<string, unknown>;
  return {
    logs: Array.isArray(r.logs) ? (r.logs as T[]) : [],
    nextCursor: typeof r.next_cursor === "string" ? r.next_cursor : "",
    truncated: r.truncated === true,
    verification: typeof r.verification === "string" ? r.verification : "sql",
  };
}

export interface SearchParamsInput {
  siteId: string;
  from: string;
  to: string;
  query?: string;
  level?: string;
  service?: string;
  limit?: number;
  offset?: number;
  cursor?: string;
}

// Builds the query string. A non-blank query goes in `lq` and pages by
// cursor (offset is not accepted alongside it); without one the request is the
// original level/service/offset search.
export function buildSearchQueryString(o: SearchParamsInput): string {
  const enc = encodeURIComponent;
  let q = `site_id=${enc(o.siteId)}&from=${enc(o.from)}&to=${enc(o.to)}`;
  const text = (o.query ?? "").trim();
  if (text) q += `&lq=${enc(text)}`;
  if (o.level) q += `&level=${enc(o.level)}`;
  if (o.service) q += `&service=${enc(o.service)}`;
  if (o.limit) q += `&limit=${o.limit}`;
  if (text) {
    if (o.cursor) q += `&cursor=${enc(o.cursor)}`;
  } else if (o.offset) {
    q += `&offset=${o.offset}`;
  }
  return q;
}

// Client-side pre-check so an obviously over-long query is not sent.
export function queryTooLong(query: string): boolean {
  return new TextEncoder().encode(query).length > LOG_QUERY_MAX_LENGTH;
}

// A caret line to print under the query, pointing at the error position.
export function caretLine(query: string, position: number): string {
  const chars = Array.from(query);
  const p = Math.max(0, Math.min(position, chars.length));
  return " ".repeat(p) + "^";
}

export interface SyntaxHelpRow {
  syntax: string;
  meaning: string;
}

export const LOG_QUERY_HELP: SyntaxHelpRow[] = [
  { syntax: "timeout", meaning: "message contains the word (case-insensitive)" },
  { syntax: '"connection refused"', meaning: "message contains the phrase" },
  { syntax: "level:error", meaning: "field match: level, service, trace_id, span_id, message" },
  { syntax: "-debug   NOT debug", meaning: "exclude" },
  { syntax: "a b   a AND b", meaning: "both (AND is implicit)" },
  { syntax: "a OR b", meaning: "either; AND, OR, NOT must be upper case" },
  { syntax: "(a OR b) -c", meaning: "grouping, up to 5 levels deep" },
  { syntax: "attr.http.status:>=500", meaning: "attribute compare: > >= < <= on numbers" },
  { syntax: "resource.service.name:api", meaning: "resource attribute" },
  { syntax: "timestamp:>=2026-01-01", meaning: "time compare (RFC3339, date or unix ms)" },
  { syntax: '"a:b"', meaning: "quote text that contains a colon" },
];

export const LOG_QUERY_LIMITS_NOTE =
  "Max 1024 characters and 20 terms. attr. and resource. filters are checked over the newest 5000 matching rows; " +
  "a note appears when older rows were not scanned.";
