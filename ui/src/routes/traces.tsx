import { useState, useEffect, useCallback } from "preact/hooks";
import { tracesApi } from "../api/traces.js";
import type { Service, Operation, Span, TraceSummary, TraceError, ServiceDependency } from "../api/traces.js";
import SearchInput from "../components/shared/SearchInput.js";
import StatusBadge from "../components/shared/StatusBadge.js";
import CodeBlock from "../components/shared/CodeBlock.js";
import Tabs from "../components/shared/Tabs.js";
import "../styles/traces.css";

export const config = { mode: "app" };

function formatDuration(ms: number): string {
  if (ms < 1) return "<1ms";
  if (ms < 1000) return `${Math.round(ms)}ms`;
  if (ms < 60000) return `${(ms / 1000).toFixed(1)}s`;
  return `${(ms / 60000).toFixed(1)}m`;
}

function formatDate(iso: string): string {
  try {
    return new Date(iso).toLocaleString("en-US", {
      month: "short", day: "numeric",
      hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false,
    });
  } catch { return iso; }
}

function tryParseJson(raw: string): string | null {
  if (!raw || raw === "{}" || raw === "null") return null;
  try {
    const p = JSON.parse(raw);
    if (typeof p === "object" && p !== null && Object.keys(p).length > 0)
      return JSON.stringify(p, null, 2);
    return null;
  } catch { return raw; }
}

function ServicesSkeleton() {
  return (
    <div class="traces-service-grid">
      {Array.from({ length: 4 }).map((_, i) => (
        <div key={i} class="traces-service-card" style={{ opacity: 0.6 }}>
          <div class="traces-skeleton-bar" style={{ width: "120px", height: "16px", marginBottom: "12px" }} />
          <div style={{ display: "flex", gap: "16px" }}>
            <div class="traces-skeleton-bar" style={{ width: "50px", height: "24px" }} />
            <div class="traces-skeleton-bar" style={{ width: "50px", height: "24px" }} />
            <div class="traces-skeleton-bar" style={{ width: "50px", height: "24px" }} />
          </div>
        </div>
      ))}
    </div>
  );
}

function ListSkeleton() {
  return (
    <div class="traces-loading">
      {Array.from({ length: 6 }).map((_, i) => (
        <div key={i} class="traces-skeleton-row">
          <div class="traces-skeleton-bar" style={{ width: "100px" }} />
          <div class="traces-skeleton-bar" style={{ width: "120px" }} />
          <div class="traces-skeleton-bar" style={{ flex: 1 }} />
        </div>
      ))}
    </div>
  );
}

// ─── Span Tree ───

interface SpanNode extends Span {
  children: SpanNode[];
  depth: number;
}

function buildSpanTree(spans: Span[]): SpanNode[] {
  const map = new Map<string, SpanNode>();
  const roots: SpanNode[] = [];

  for (const s of spans) {
    map.set(s.span_id, { ...s, children: [], depth: 0 });
  }

  for (const node of map.values()) {
    if (node.parent_span_id && map.has(node.parent_span_id)) {
      map.get(node.parent_span_id)!.children.push(node);
    } else {
      roots.push(node);
    }
  }

  function setDepth(node: SpanNode, d: number) {
    node.depth = d;
    node.children.sort((a, b) => new Date(a.start_time).getTime() - new Date(b.start_time).getTime());
    for (const c of node.children) setDepth(c, d + 1);
  }
  roots.sort((a, b) => new Date(a.start_time).getTime() - new Date(b.start_time).getTime());
  for (const r of roots) setDepth(r, 0);

  return roots;
}

function flattenTree(nodes: SpanNode[]): SpanNode[] {
  const result: SpanNode[] = [];
  function walk(n: SpanNode) {
    result.push(n);
    for (const c of n.children) walk(c);
  }
  for (const n of nodes) walk(n);
  return result;
}

// ─── Trace Waterfall ───

function TraceWaterfall({ spans, traceId, siteId }: { spans: Span[]; traceId: string; siteId: string }) {
  const [selectedSpanId, setSelectedSpanId] = useState<string | null>(null);
  const [traceErrors, setTraceErrors] = useState<TraceError[]>([]);

  useEffect(() => {
    tracesApi.traceErrors(traceId, siteId)
      .then(e => setTraceErrors(e || []))
      .catch(() => setTraceErrors([]));
  }, [traceId, siteId]);

  if (!spans.length) return <div class="obs-empty-state">No spans found</div>;

  const tree = buildSpanTree(spans);
  const flat = flattenTree(tree);

  const minStart = Math.min(...flat.map(s => new Date(s.start_time).getTime()));
  const maxEnd = Math.max(...flat.map(s => new Date(s.end_time).getTime()));
  const totalDuration = maxEnd - minStart || 1;

  const selected = selectedSpanId ? flat.find(s => s.span_id === selectedSpanId) : null;
  const selectedAttrs = selected ? tryParseJson(selected.attributes) : null;
  const selectedResource = selected ? tryParseJson(selected.resource) : null;
  const selectedEvents = selected ? tryParseJson(selected.events) : null;

  return (
    <div>
      {/* Summary bar */}
      <div class="traces-summary-bar">
        <span><strong>{flat.length}</strong> spans</span>
        <span><strong>{new Set(flat.map(s => s.service_name)).size}</strong> services</span>
        <span><strong>{formatDuration(totalDuration)}</strong> total</span>
        {flat.filter(s => s.status_code === "ERROR" || s.status_code === "2").length > 0 && (
          <span class="traces-summary-errors">
            <strong>{flat.filter(s => s.status_code === "ERROR" || s.status_code === "2").length}</strong> errors
          </span>
        )}
      </div>

      <div class="traces-waterfall">
        <div class="traces-waterfall-header">
          <div class="traces-waterfall-col-service">Service</div>
          <div class="traces-waterfall-col-operation">Operation</div>
          <div class="traces-waterfall-col-timeline">Timeline ({formatDuration(totalDuration)})</div>
        </div>
        {flat.map(span => {
          const start = new Date(span.start_time).getTime();
          const left = ((start - minStart) / totalDuration) * 100;
          const width = Math.max((span.duration_ms / totalDuration) * 100, 0.5);
          const isError = span.status_code === "ERROR" || span.status_code === "2";

          return (
            <div
              key={span.span_id}
              class={`traces-span-row ${isError ? "traces-span-row--error" : ""} ${selectedSpanId === span.span_id ? "traces-span-row--selected" : ""}`}
              onClick={() => setSelectedSpanId(selectedSpanId === span.span_id ? null : span.span_id)}
            >
              <div class="traces-span-service">
                <span class="traces-span-indent" style={{ width: `${span.depth * 16}px` }} />
                {span.service_name}
              </div>
              <div class="traces-span-operation">{span.operation_name}</div>
              <div class="traces-span-timeline">
                <div
                  class={`traces-span-bar ${isError ? "traces-span-bar--error" : "traces-span-bar--ok"}`}
                  style={{ left: `${left}%`, width: `${width}%` }}
                />
                <span class="traces-span-duration">{formatDuration(span.duration_ms)}</span>
              </div>
            </div>
          );
        })}
      </div>

      {selected && (
        <div class="traces-span-detail">
          <h3 class="traces-span-detail-title">{selected.service_name} - {selected.operation_name}</h3>
          <div class="traces-span-detail-meta">
            <span><span class="traces-span-detail-meta-key">Duration</span>{formatDuration(selected.duration_ms)}</span>
            <span><span class="traces-span-detail-meta-key">Kind</span>{selected.span_kind}</span>
            <span><span class="traces-span-detail-meta-key">Status</span>
              <StatusBadge status={selected.status_code === "ERROR" || selected.status_code === "2" ? "error" : "ok"} size="sm" />
            </span>
            <span><span class="traces-span-detail-meta-key">Start</span>{formatDate(selected.start_time)}</span>
            <span><span class="traces-span-detail-meta-key">Span ID</span>{selected.span_id}</span>
          </div>
          {selected.status_message && (
            <div style={{ marginBottom: "12px", fontSize: "12px", color: "var(--obs-danger)" }}>
              {selected.status_message}
            </div>
          )}
          <Tabs tabs={[
            ...(selectedAttrs ? [{
              key: "attributes",
              label: "Attributes",
              content: <CodeBlock code={selectedAttrs} maxHeight="300px" />,
            }] : []),
            ...(selectedResource ? [{
              key: "resource",
              label: "Resource",
              content: <CodeBlock code={selectedResource} maxHeight="300px" />,
            }] : []),
            ...(selectedEvents ? [{
              key: "events",
              label: "Events",
              content: <CodeBlock code={selectedEvents} maxHeight="300px" />,
            }] : []),
          ]} />
        </div>
      )}

      {/* Correlated errors */}
      {traceErrors.length > 0 && (
        <div style={{ marginTop: "16px" }}>
          <h3 style={{ fontSize: "13px", fontWeight: 600, color: "var(--obs-text)", marginBottom: "8px" }}>
            Correlated Errors ({traceErrors.length})
          </h3>
          <div class="traces-loading" style={{ gap: 0 }}>
            {traceErrors.map((e) => (
              <div key={e.error_id} class="traces-span-row traces-span-row--error"
                onClick={() => { window.location.href = `/errors?site_id=${siteId}&issue_id=${e.issue_id}`; }}
                style={{ cursor: "pointer" }}>
                <div style={{ flex: 1, fontSize: "12px" }}>
                  <strong>{e.error_type}</strong>: {e.error_value}
                </div>
                <span style={{ fontSize: "11px", color: "var(--obs-text-muted)" }}>{formatDate(e.timestamp)}</span>
              </div>
            ))}
          </div>
        </div>
      )}
    </div>
  );
}

// ─── Operations Table ───

function OperationsTable({ siteId, service, from, to, onSelectTrace }: {
  siteId: string; service: string; from: string; to: string;
  onSelectTrace: (traceId: string) => void;
}) {
  const [operations, setOperations] = useState<Operation[]>([]);
  const [loading, setLoading] = useState(true);
  const [traceList, setTraceList] = useState<TraceSummary[]>([]);
  const [loadingTraces, setLoadingTraces] = useState(false);
  const [selectedOp, setSelectedOp] = useState<string | null>(null);

  useEffect(() => {
    setLoading(true);
    tracesApi.operations(siteId, service, from, to)
      .then(d => setOperations(d || []))
      .catch(() => setOperations([]))
      .finally(() => setLoading(false));
  }, [siteId, service, from, to]);

  const handleOpClick = async (opName: string) => {
    setSelectedOp(opName);
    setLoadingTraces(true);
    try {
      const traces = await tracesApi.search(siteId, from, to, { service, operation: opName });
      setTraceList(traces || []);
    } catch { setTraceList([]); }
    finally { setLoadingTraces(false); }
  };

  if (loading) return <ListSkeleton />;
  if (!operations.length) return <div class="obs-empty-state">No operations found for {service}</div>;

  return (
    <div>
      <div class="traces-ops-table">
        <div class="traces-waterfall-header">
          <div style={{ flex: 2 }}>Operation</div>
          <div style={{ flex: 1, textAlign: "right" }}>Requests</div>
          <div style={{ flex: 1, textAlign: "right" }}>Errors</div>
          <div style={{ flex: 1, textAlign: "right" }}>Avg Duration</div>
        </div>
        {operations.map(op => (
          <div key={op.operation_name} class={`traces-span-row ${selectedOp === op.operation_name ? "traces-span-row--selected" : ""}`}
            onClick={() => handleOpClick(op.operation_name)}>
            <div style={{ flex: 2, fontSize: "13px", fontWeight: 500, color: "var(--obs-text)" }}>{op.operation_name}</div>
            <div style={{ flex: 1, textAlign: "right", fontSize: "12px", color: "var(--obs-text-secondary)", fontVariantNumeric: "tabular-nums" }}>{op.request_count.toLocaleString()}</div>
            <div style={{ flex: 1, textAlign: "right", fontSize: "12px", color: op.error_count > 0 ? "var(--obs-danger)" : "var(--obs-text-secondary)", fontVariantNumeric: "tabular-nums" }}>{op.error_count.toLocaleString()}</div>
            <div style={{ flex: 1, textAlign: "right", fontSize: "12px", color: "var(--obs-text-secondary)", fontVariantNumeric: "tabular-nums" }}>{formatDuration(op.avg_duration_ms)}</div>
          </div>
        ))}
      </div>

      {selectedOp && (
        <div style={{ marginTop: "16px" }}>
          <h3 style={{ fontSize: "13px", fontWeight: 600, color: "var(--obs-text)", marginBottom: "8px" }}>
            Traces for {selectedOp}
          </h3>
          {loadingTraces ? <ListSkeleton /> : traceList.length === 0 ? (
            <div class="obs-empty-state">No traces found</div>
          ) : (
            <TraceListView traces={traceList} onSelectTrace={onSelectTrace} />
          )}
        </div>
      )}
    </div>
  );
}

// ─── Trace List from Search ───

function TraceListView({ traces, onSelectTrace }: { traces: TraceSummary[]; onSelectTrace: (id: string) => void }) {
  const sorted = [...traces].sort((a, b) =>
    new Date(b.start_time).getTime() - new Date(a.start_time).getTime()
  );

  return (
    <div class="traces-loading" style={{ gap: 0 }}>
      {sorted.slice(0, 50).map((t) => {
        const hasError = t.status_code === "ERROR" || t.status_code === "2";
        return (
          <div key={t.trace_id}
            class={`traces-span-row ${hasError ? "traces-span-row--error" : ""}`}
            onClick={() => onSelectTrace(t.trace_id)}
            style={{ cursor: "pointer" }}>
            <div style={{ flex: 2, display: "flex", flexDirection: "column", gap: "2px" }}>
              <span style={{ fontSize: "13px", fontWeight: 500, color: "var(--obs-text)" }}>
                {t.root_service} - {t.root_operation}
              </span>
              <span style={{ fontSize: "11px", color: "var(--obs-text-muted)", fontFamily: "'SF Mono', monospace" }}>
                {t.trace_id.slice(0, 16)}...
              </span>
            </div>
            <div style={{ display: "flex", gap: "12px", alignItems: "center", fontSize: "12px", color: "var(--obs-text-secondary)" }}>
              <span>{t.span_count} spans</span>
              <span style={{ fontVariantNumeric: "tabular-nums" }}>{formatDuration(t.duration_ms)}</span>
              {hasError && <StatusBadge status="error" size="sm" />}
              <span style={{ fontSize: "11px", color: "var(--obs-text-muted)" }}>{formatDate(t.start_time)}</span>
            </div>
          </div>
        );
      })}
    </div>
  );
}

// ─── Search Filter Panel ───

function SearchFilters({ siteId, from, to, services, onSelectTrace }: {
  siteId: string; from: string; to: string; services: Service[];
  onSelectTrace: (id: string) => void;
}) {
  const [filterService, setFilterService] = useState("");
  const [filterOperation, setFilterOperation] = useState("");
  const [filterStatus, setFilterStatus] = useState("");
  const [filterMinDuration, setFilterMinDuration] = useState("");
  const [filterMaxDuration, setFilterMaxDuration] = useState("");
  const [results, setResults] = useState<TraceSummary[]>([]);
  const [loading, setLoading] = useState(false);
  const [searched, setSearched] = useState(false);

  const handleSearch = async () => {
    setLoading(true);
    setSearched(true);
    try {
      const opts: any = {};
      if (filterService) opts.service = filterService;
      if (filterOperation) opts.operation = filterOperation;
      if (filterStatus) opts.status = filterStatus;
      if (filterMinDuration) opts.min_duration = parseInt(filterMinDuration);
      if (filterMaxDuration) opts.max_duration = parseInt(filterMaxDuration);
      const data = await tracesApi.search(siteId, from, to, opts);
      setResults(data || []);
    } catch { setResults([]); }
    finally { setLoading(false); }
  };

  return (
    <div>
      <div class="traces-filter-panel">
        <div class="obs-form-group" style={{ marginBottom: "8px" }}>
          <select class="obs-select" value={filterService}
            onChange={(e) => setFilterService((e.target as HTMLSelectElement).value)}>
            <option value="">All Services</option>
            {services.map(s => <option key={s.service_name} value={s.service_name}>{s.service_name}</option>)}
          </select>
        </div>
        <div class="obs-form-group" style={{ marginBottom: "8px" }}>
          <input class="obs-input" placeholder="Operation..." value={filterOperation}
            onInput={(e) => setFilterOperation((e.target as HTMLInputElement).value)} />
        </div>
        <div class="obs-form-group" style={{ marginBottom: "8px" }}>
          <select class="obs-select" value={filterStatus}
            onChange={(e) => setFilterStatus((e.target as HTMLSelectElement).value)}>
            <option value="">Any Status</option>
            <option value="ok">OK</option>
            <option value="error">Error</option>
          </select>
        </div>
        <div style={{ display: "flex", gap: "8px" }}>
          <input class="obs-input" type="number" placeholder="Min ms" value={filterMinDuration}
            onInput={(e) => setFilterMinDuration((e.target as HTMLInputElement).value)}
            style={{ flex: 1 }} />
          <input class="obs-input" type="number" placeholder="Max ms" value={filterMaxDuration}
            onInput={(e) => setFilterMaxDuration((e.target as HTMLInputElement).value)}
            style={{ flex: 1 }} />
        </div>
        <button class="obs-btn obs-btn--primary" onClick={handleSearch} style={{ marginTop: "8px", width: "100%" }}>
          Search Traces
        </button>
      </div>

      {loading ? <ListSkeleton /> : searched && results.length === 0 ? (
        <div class="obs-empty-state">No traces match the filters</div>
      ) : results.length > 0 ? (
        <TraceListView traces={results} onSelectTrace={onSelectTrace} />
      ) : null}
    </div>
  );
}

// ─── Main Page ───

// ─── Dependency Graph ───

function DependencyGraph({ siteId, from, to }: { siteId: string; from: string; to: string }) {
  const [deps, setDeps] = useState<ServiceDependency[]>([]);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    setLoading(true);
    tracesApi.dependencies(siteId, from, to)
      .then(d => setDeps(d || []))
      .catch(() => setDeps([]))
      .finally(() => setLoading(false));
  }, [siteId, from, to]);

  if (loading) return <ListSkeleton />;
  if (!deps.length) return <div class="obs-empty-state">No service dependencies found</div>;

  // Build node positions
  const nodes = new Set<string>();
  deps.forEach(d => { nodes.add(d.src_service); nodes.add(d.dst_service); });
  const nodeList = Array.from(nodes);
  const nodeMap = new Map<string, { x: number; y: number }>();
  const cols = Math.ceil(Math.sqrt(nodeList.length));
  nodeList.forEach((n, i) => {
    const col = i % cols;
    const row = Math.floor(i / cols);
    nodeMap.set(n, { x: 120 + col * 200, y: 80 + row * 120 });
  });
  const svgWidth = 120 + cols * 200;
  const svgHeight = 80 + (Math.ceil(nodeList.length / cols)) * 120;
  const maxCalls = Math.max(...deps.map(d => d.call_count), 1);

  return (
    <div style={{ overflow: "auto" }}>
      <svg width={svgWidth} height={svgHeight} style={{ display: "block" }}>
        {/* Edges */}
        {deps.map((d, i) => {
          const src = nodeMap.get(d.src_service);
          const dst = nodeMap.get(d.dst_service);
          if (!src || !dst) return null;
          const opacity = 0.3 + (d.call_count / maxCalls) * 0.7;
          const hasError = d.error_count > 0;
          return (
            <g key={i}>
              <line x1={src.x} y1={src.y} x2={dst.x} y2={dst.y}
                stroke={hasError ? "var(--obs-danger)" : "var(--obs-accent)"}
                strokeWidth={1 + (d.call_count / maxCalls) * 3}
                opacity={opacity}
                markerEnd="url(#arrowhead)" />
              <text x={(src.x + dst.x) / 2} y={(src.y + dst.y) / 2 - 6}
                fill="var(--obs-text-muted)" fontSize="10" textAnchor="middle">
                {d.call_count.toLocaleString()} calls
              </text>
            </g>
          );
        })}
        {/* Arrowhead marker */}
        <defs>
          <marker id="arrowhead" markerWidth="8" markerHeight="6" refX="8" refY="3" orient="auto">
            <polygon points="0 0, 8 3, 0 6" fill="var(--obs-accent)" />
          </marker>
        </defs>
        {/* Nodes */}
        {nodeList.map(n => {
          const pos = nodeMap.get(n)!;
          return (
            <g key={n}>
              <circle cx={pos.x} cy={pos.y} r="28" fill="var(--obs-surface)" stroke="var(--obs-border)" strokeWidth="2" />
              <text x={pos.x} y={pos.y + 4} fill="var(--obs-text)" fontSize="11" textAnchor="middle" fontWeight="600">
                {n.length > 12 ? n.slice(0, 11) + "..." : n}
              </text>
            </g>
          );
        })}
      </svg>
    </div>
  );
}

type View = "services" | "operations" | "trace" | "search" | "deps";

export default function TracesPage() {
  const siteId = typeof window !== "undefined"
    ? new URLSearchParams(window.location.search).get("site_id") || "default"
    : "default";

  const [services, setServices] = useState<Service[]>([]);
  const [loading, setLoading] = useState(true);
  const [view, setView] = useState<View>("services");
  const [selectedService, setSelectedService] = useState<string>("");
  const [traceId, setTraceId] = useState<string | null>(null);
  const [traceSpans, setTraceSpans] = useState<Span[]>([]);
  const [loadingTrace, setLoadingTrace] = useState(false);
  const [searchId, setSearchId] = useState("");

  const now = new Date();
  const from = new Date(now.getTime() - 86400000).toISOString();
  const to = now.toISOString();

  const fetchServices = useCallback(async () => {
    setLoading(true);
    try {
      const data = await tracesApi.services(siteId, from, to);
      setServices(data || []);
    } catch { setServices([]); }
    finally { setLoading(false); }
  }, [siteId]);

  useEffect(() => {
    const params = typeof window !== "undefined" ? new URLSearchParams(window.location.search) : null;
    const urlTraceId = params?.get("trace_id");
    if (urlTraceId) {
      loadTrace(urlTraceId);
    } else {
      fetchServices();
    }
  }, []);

  const loadTrace = async (id: string) => {
    setLoadingTrace(true);
    setTraceId(id);
    setView("trace");
    try {
      const spans = await tracesApi.trace(id, siteId);
      setTraceSpans(spans || []);
    } catch { setTraceSpans([]); }
    finally { setLoadingTrace(false); }
  };

  const goBack = () => {
    if (view === "trace" && selectedService) {
      setView("operations");
      setTraceId(null);
    } else if (view === "operations") {
      setView("services");
      setSelectedService("");
    } else if (view === "trace") {
      setView("services");
      setTraceId(null);
    } else if (view === "search") {
      setView("services");
    }
  };

  const handleServiceClick = (serviceName: string) => {
    setSelectedService(serviceName);
    setView("operations");
  };

  const handleSearchTrace = () => {
    if (searchId.trim()) loadTrace(searchId.trim());
  };

  // ─── Trace Detail View ───
  if (view === "trace" && traceId) {
    return (
      <div>
        <button class="traces-back-btn" onClick={goBack}>
          <svg width="14" height="14" viewBox="0 0 24 24" fill="currentColor">
            <path d="M20 11H7.83l5.59-5.59L12 4l-8 8 8 8 1.41-1.41L7.83 13H20v-2z" />
          </svg>
          {selectedService ? `Back to ${selectedService} operations` : "Back to services"}
        </button>
        <div class="obs-page-header">
          <h1 class="obs-page-title">Trace {traceId.slice(0, 16)}...</h1>
        </div>
        {loadingTrace ? <ListSkeleton /> : <TraceWaterfall spans={traceSpans} traceId={traceId} siteId={siteId} />}
      </div>
    );
  }

  // ─── Operations View ───
  if (view === "operations" && selectedService) {
    return (
      <div>
        <button class="traces-back-btn" onClick={goBack}>
          <svg width="14" height="14" viewBox="0 0 24 24" fill="currentColor">
            <path d="M20 11H7.83l5.59-5.59L12 4l-8 8 8 8 1.41-1.41L7.83 13H20v-2z" />
          </svg>
          Back to services
        </button>
        <div class="obs-page-header">
          <h1 class="obs-page-title">{selectedService}</h1>
        </div>
        <OperationsTable siteId={siteId} service={selectedService} from={from} to={to} onSelectTrace={loadTrace} />
      </div>
    );
  }

  // ─── Services View (Default) ───
  return (
    <div>
      <div class="obs-page-header">
        <h1 class="obs-page-title">Traces</h1>
        <div class="obs-page-actions" style={{ display: "flex", gap: "8px" }}>
          <button class={`obs-btn ${view === "deps" ? "obs-btn--primary" : ""}`}
            onClick={() => setView(view === "deps" ? "services" : "deps")}>
            {view === "deps" ? "Show Services" : "Dependencies"}
          </button>
          <button class={`obs-btn ${view === "search" ? "obs-btn--primary" : ""}`}
            onClick={() => setView(view === "search" ? "services" : "search")}>
            {view === "search" ? "Show Services" : "Search Traces"}
          </button>
        </div>
      </div>

      <div class="traces-toolbar">
        <SearchInput
          value={searchId}
          onInput={setSearchId}
          placeholder="Jump to trace ID..."
          onSubmit={handleSearchTrace}
        />
      </div>

      {/* RED summary cards */}
      {services.length > 0 && view === "services" && (
        <div style={{ display: "grid", gridTemplateColumns: "repeat(3, 1fr)", gap: "12px", marginBottom: "16px" }}>
          {(() => {
            const totalReqs = services.reduce((s, svc) => s + svc.request_count, 0);
            const totalErrs = services.reduce((s, svc) => s + svc.error_count, 0);
            const avgDur = services.length > 0
              ? services.reduce((s, svc) => s + svc.avg_duration_ms * svc.request_count, 0) / Math.max(totalReqs, 1) : 0;
            const errRate = totalReqs > 0 ? (totalErrs / totalReqs * 100) : 0;
            return [
              { label: "Total Requests", value: totalReqs.toLocaleString(), color: "var(--obs-accent)" },
              { label: "Error Rate", value: `${errRate.toFixed(2)}%`, color: errRate > 5 ? "var(--obs-danger)" : errRate > 1 ? "var(--obs-warning)" : "var(--obs-success)" },
              { label: "Avg Duration", value: `${Math.round(avgDur)}ms`, color: "var(--obs-text)" },
            ].map((card, i) => (
              <div key={i} style={{ background: "var(--obs-surface)", borderRadius: "var(--obs-radius-md)", padding: "16px", borderLeft: `3px solid ${card.color}` }}>
                <div style={{ fontSize: "11px", color: "var(--obs-text-muted)", textTransform: "uppercase", letterSpacing: "0.5px", marginBottom: "4px" }}>{card.label}</div>
                <div style={{ fontSize: "22px", fontWeight: 700, color: card.color, fontVariantNumeric: "tabular-nums" }}>{card.value}</div>
              </div>
            ));
          })()}
        </div>
      )}

      {view === "deps" ? (
        <DependencyGraph siteId={siteId} from={from} to={to} />
      ) : view === "search" ? (
        <SearchFilters siteId={siteId} from={from} to={to} services={services} onSelectTrace={loadTrace} />
      ) : loading ? (
        <ServicesSkeleton />
      ) : services.length === 0 ? (
        <div class="obs-empty-state">No services reporting traces</div>
      ) : (
        <div class="traces-service-grid">
          {services.map(svc => {
            const errorRate = svc.request_count > 0
              ? ((svc.error_count / svc.request_count) * 100).toFixed(1)
              : "0.0";
            return (
              <div key={svc.service_name} class="traces-service-card"
                onClick={() => handleServiceClick(svc.service_name)}>
                <div class="traces-service-name">{svc.service_name}</div>
                <div class="traces-service-metrics">
                  <div class="traces-metric">
                    <span class="traces-metric-value">{svc.request_count.toLocaleString()}</span>
                    <span class="traces-metric-label">Requests</span>
                  </div>
                  <div class={`traces-metric ${svc.error_count > 0 ? "traces-metric--error" : ""}`}>
                    <span class="traces-metric-value">{errorRate}%</span>
                    <span class="traces-metric-label">Error Rate</span>
                  </div>
                  <div class="traces-metric">
                    <span class="traces-metric-value">{formatDuration(svc.avg_duration_ms)}</span>
                    <span class="traces-metric-label">Avg Duration</span>
                  </div>
                </div>
                <div class="traces-latency-badges">
                  <span class="traces-latency-badge">
                    <span class="traces-latency-badge-label">p50</span> {formatDuration(svc.p50_ms)}
                  </span>
                  <span class="traces-latency-badge">
                    <span class="traces-latency-badge-label">p95</span> {formatDuration(svc.p95_ms)}
                  </span>
                  <span class="traces-latency-badge">
                    <span class="traces-latency-badge-label">p99</span> {formatDuration(svc.p99_ms)}
                  </span>
                </div>
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}
