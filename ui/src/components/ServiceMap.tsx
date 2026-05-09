// Service dependency map — directed graph of services + call edges.
// Vanilla SVG with a deterministic Verlet force layout (no external deps).
// Designed for ≤30 nodes; degrades gracefully past that.

import { useEffect, useMemo, useRef, useState } from "preact/hooks";
import type { ServiceDependency, Service } from "../api/traces.js";
import EmptyState from "./shared/EmptyState.js";

interface Props {
  dependencies: ServiceDependency[];
  /** Optional services list — used for RED metrics on hover. */
  services?: Service[];
}

interface Node {
  id: string;
  x: number;
  y: number;
  vx: number;
  vy: number;
  totalCalls: number;
  /** Pre-computed visual radius. */
  r: number;
}

const WIDTH = 760;
const HEIGHT = 480;
const NODE_MIN_R = 18;
const NODE_MAX_R = 44;
const ITERATIONS = 220;

// Hash → deterministic float in [0, 1). Used for stable initial layout.
function hash01(seed: string): number {
  let h = 2166136261 >>> 0;
  for (let i = 0; i < seed.length; i++) {
    h ^= seed.charCodeAt(i);
    h = Math.imul(h, 16777619);
  }
  return ((h >>> 0) % 100000) / 100000;
}

// Log-scale radius from total call count.
function radiusFor(calls: number, maxCalls: number): number {
  if (maxCalls <= 0 || calls <= 0) return NODE_MIN_R;
  const t = Math.log(1 + calls) / Math.log(1 + maxCalls);
  return NODE_MIN_R + t * (NODE_MAX_R - NODE_MIN_R);
}

function buildLayout(
  nodeIds: string[],
  edges: ServiceDependency[],
  totals: Map<string, number>,
): Map<string, Node> {
  const nodes = new Map<string, Node>();
  const maxCalls = Math.max(1, ...Array.from(totals.values()));

  // Seed positions on a circle (deterministic per service name).
  const cx = WIDTH / 2;
  const cy = HEIGHT / 2;
  const ringR = Math.min(WIDTH, HEIGHT) * 0.32;
  for (const id of nodeIds) {
    const angle = hash01(id) * Math.PI * 2;
    nodes.set(id, {
      id,
      x: cx + Math.cos(angle) * ringR,
      y: cy + Math.sin(angle) * ringR,
      vx: 0,
      vy: 0,
      totalCalls: totals.get(id) ?? 0,
      r: radiusFor(totals.get(id) ?? 0, maxCalls),
    });
  }

  // Verlet-ish iteration: repulsion between every pair, spring on edges,
  // mild centering force, viscous damping. ~80 LOC of force resolution.
  const ids = Array.from(nodes.keys());
  const repulsion = 6500;
  const idealLen = 130;
  const stiffness = 0.04;
  const centerPull = 0.012;
  const damping = 0.82;

  // Aggregate edge weights so duplicates don't double-pull.
  const edgeMap = new Map<string, number>();
  for (const e of edges) {
    if (e.src_service === e.dst_service) continue;
    const k = e.src_service + "→" + e.dst_service;
    edgeMap.set(k, (edgeMap.get(k) ?? 0) + e.call_count);
  }

  for (let iter = 0; iter < ITERATIONS; iter++) {
    // Pairwise repulsion.
    for (let i = 0; i < ids.length; i++) {
      const a = nodes.get(ids[i])!;
      for (let j = i + 1; j < ids.length; j++) {
        const b = nodes.get(ids[j])!;
        const dx = a.x - b.x;
        const dy = a.y - b.y;
        const d2 = dx * dx + dy * dy + 0.01;
        const f = repulsion / d2;
        const d = Math.sqrt(d2);
        const ux = dx / d;
        const uy = dy / d;
        a.vx += ux * f;
        a.vy += uy * f;
        b.vx -= ux * f;
        b.vy -= uy * f;
      }
    }

    // Spring along edges.
    for (const key of edgeMap.keys()) {
      const [src, dst] = key.split("→");
      const a = nodes.get(src);
      const b = nodes.get(dst);
      if (!a || !b) continue;
      const dx = b.x - a.x;
      const dy = b.y - a.y;
      const d = Math.sqrt(dx * dx + dy * dy) + 0.01;
      const delta = d - idealLen;
      const f = delta * stiffness;
      const ux = dx / d;
      const uy = dy / d;
      a.vx += ux * f;
      a.vy += uy * f;
      b.vx -= ux * f;
      b.vy -= uy * f;
    }

    // Center pull + integrate.
    for (const n of nodes.values()) {
      n.vx += (cx - n.x) * centerPull;
      n.vy += (cy - n.y) * centerPull;
      n.vx *= damping;
      n.vy *= damping;
      n.x += n.vx;
      n.y += n.vy;
      // Clamp inside the viewport with a margin.
      n.x = Math.max(n.r + 8, Math.min(WIDTH - n.r - 8, n.x));
      n.y = Math.max(n.r + 8, Math.min(HEIGHT - n.r - 8, n.y));
    }
  }

  return nodes;
}

interface HoverState {
  kind: "node" | "edge";
  x: number;
  y: number;
  title: string;
  rows: Array<[string, string]>;
}

export default function ServiceMap({ dependencies, services = [] }: Props) {
  const svgRef = useRef<SVGSVGElement>(null);
  const [selected, setSelected] = useState<string | null>(null);
  const [hover, setHover] = useState<HoverState | null>(null);

  // ─── Build node + edge model ───
  const layout = useMemo(() => {
    const totals = new Map<string, number>();
    const ids = new Set<string>();
    for (const d of dependencies) {
      ids.add(d.src_service);
      ids.add(d.dst_service);
      totals.set(d.dst_service, (totals.get(d.dst_service) ?? 0) + d.call_count);
      // Treat callers as having activity too (smaller weight).
      totals.set(d.src_service, (totals.get(d.src_service) ?? 0) + Math.floor(d.call_count / 2));
    }
    const nodeIds = Array.from(ids).sort();
    return {
      nodes: buildLayout(nodeIds, dependencies, totals),
      nodeIds,
    };
  }, [dependencies]);

  const servicesByName = useMemo(() => {
    const m = new Map<string, Service>();
    for (const s of services) m.set(s.service_name, s);
    return m;
  }, [services]);

  const maxCalls = useMemo(
    () => Math.max(1, ...dependencies.map(d => d.call_count)),
    [dependencies],
  );

  // Adjacency for selection-dimming.
  const adjacency = useMemo(() => {
    const m = new Map<string, Set<string>>();
    for (const d of dependencies) {
      if (!m.has(d.src_service)) m.set(d.src_service, new Set());
      if (!m.has(d.dst_service)) m.set(d.dst_service, new Set());
      m.get(d.src_service)!.add(d.dst_service);
      m.get(d.dst_service)!.add(d.src_service);
    }
    return m;
  }, [dependencies]);

  const isDimmedNode = (id: string) => {
    if (!selected) return false;
    if (id === selected) return false;
    return !adjacency.get(selected)?.has(id);
  };
  const isDimmedEdge = (e: ServiceDependency) => {
    if (!selected) return false;
    return e.src_service !== selected && e.dst_service !== selected;
  };

  // Translate page coords → SVG coords for tooltip placement.
  const toSvgPoint = (clientX: number, clientY: number) => {
    const svg = svgRef.current;
    if (!svg) return { x: clientX, y: clientY };
    const rect = svg.getBoundingClientRect();
    const sx = (clientX - rect.left) * (WIDTH / rect.width);
    const sy = (clientY - rect.top) * (HEIGHT / rect.height);
    return { x: sx, y: sy };
  };

  // Close selection when clicking empty space.
  useEffect(() => {
    if (!selected) return;
    const handler = (e: KeyboardEvent) => {
      if (e.key === "Escape") setSelected(null);
    };
    window.addEventListener("keydown", handler);
    return () => window.removeEventListener("keydown", handler);
  }, [selected]);

  if (dependencies.length === 0) {
    return (
      <EmptyState
        title="No service dependencies yet"
        description="Emit OTLP traces with parent-child spans across services to populate this view. Each cross-service span becomes an edge in the map."
        icon="layers"
        actions={[
          { label: "Get started", href: "/onboard", primary: true },
          { label: "Read the docs", href: "/docs#traces" },
        ]}
      />
    );
  }

  return (
    <div class="service-map">
      <div class="service-map-toolbar">
        <span class="service-map-meta">
          {layout.nodeIds.length} service{layout.nodeIds.length === 1 ? "" : "s"}
          {" · "}
          {dependencies.length} edge{dependencies.length === 1 ? "" : "s"}
        </span>
        {selected && (
          <button class="obs-btn" onClick={() => setSelected(null)}>
            Clear selection
          </button>
        )}
      </div>
      <div class="service-map-stage">
        <svg
          ref={svgRef}
          class="service-map-svg"
          viewBox={`0 0 ${WIDTH} ${HEIGHT}`}
          preserveAspectRatio="xMidYMid meet"
          onClick={(e) => {
            // Background click clears selection.
            if (e.target === svgRef.current) setSelected(null);
          }}
        >
          <defs>
            <marker id="svc-arrow" viewBox="0 0 10 10" refX="9" refY="5"
              markerWidth="7" markerHeight="7" orient="auto-start-reverse">
              <path d="M0,0 L10,5 L0,10 z" fill="var(--obs-accent)" />
            </marker>
            <marker id="svc-arrow-error" viewBox="0 0 10 10" refX="9" refY="5"
              markerWidth="7" markerHeight="7" orient="auto-start-reverse">
              <path d="M0,0 L10,5 L0,10 z" fill="var(--obs-danger)" />
            </marker>
          </defs>

          {/* Edges */}
          <g class="service-map-edges">
            {dependencies.map((d, i) => {
              const a = layout.nodes.get(d.src_service);
              const b = layout.nodes.get(d.dst_service);
              if (!a || !b) return null;
              // Trim line to circle edges.
              const dx = b.x - a.x;
              const dy = b.y - a.y;
              const dist = Math.sqrt(dx * dx + dy * dy) || 1;
              const ux = dx / dist;
              const uy = dy / dist;
              const x1 = a.x + ux * a.r;
              const y1 = a.y + uy * a.r;
              const x2 = b.x - ux * (b.r + 4);
              const y2 = b.y - uy * (b.r + 4);
              const errRate = d.call_count > 0 ? d.error_count / d.call_count : 0;
              const isError = errRate > 0;
              const dimmed = isDimmedEdge(d);
              const opacity = dimmed
                ? 0.07
                : 0.35 + (d.call_count / maxCalls) * 0.55;
              const width = 1 + (d.call_count / maxCalls) * 3;
              return (
                <line
                  key={`${d.src_service}→${d.dst_service}-${i}`}
                  x1={x1} y1={y1} x2={x2} y2={y2}
                  stroke={isError ? "var(--obs-danger)" : "var(--obs-accent)"}
                  strokeWidth={width}
                  opacity={opacity}
                  markerEnd={`url(#${isError ? "svc-arrow-error" : "svc-arrow"})`}
                  style={{ cursor: "pointer", transition: "opacity 0.15s" }}
                  onMouseEnter={(e) => {
                    const p = toSvgPoint(e.clientX, e.clientY);
                    setHover({
                      kind: "edge",
                      x: p.x, y: p.y,
                      title: `${d.src_service} → ${d.dst_service}`,
                      rows: [
                        ["Calls", d.call_count.toLocaleString()],
                        ["Errors", d.error_count.toLocaleString()],
                        ["Error rate", (errRate * 100).toFixed(2) + "%"],
                        ["Avg duration", `${Math.round(d.avg_duration_ms)}ms`],
                      ],
                    });
                  }}
                  onMouseLeave={() => setHover(null)}
                />
              );
            })}
          </g>

          {/* Nodes */}
          <g class="service-map-nodes">
            {layout.nodeIds.map((id) => {
              const n = layout.nodes.get(id);
              if (!n) return null;
              const dimmed = isDimmedNode(id);
              const sel = selected === id;
              const svc = servicesByName.get(id);
              const errRate = svc && svc.request_count > 0
                ? svc.error_count / svc.request_count
                : 0;
              const stroke = sel
                ? "var(--obs-accent)"
                : errRate > 0.05
                  ? "var(--obs-danger)"
                  : errRate > 0.01
                    ? "var(--obs-warning)"
                    : "var(--obs-border)";
              const strokeWidth = sel ? 3 : 1.5;
              return (
                <g
                  key={id}
                  style={{
                    cursor: "pointer",
                    opacity: dimmed ? 0.25 : 1,
                    transition: "opacity 0.15s",
                  }}
                  data-service={id}
                  data-testid={`service-node-${id}`}
                  onClick={(e) => {
                    e.stopPropagation();
                    setSelected(prev => (prev === id ? null : id));
                  }}
                  onMouseEnter={(e) => {
                    const p = toSvgPoint(e.clientX, e.clientY);
                    const rows: Array<[string, string]> = [];
                    if (svc) {
                      rows.push(["Requests", svc.request_count.toLocaleString()]);
                      rows.push(["Errors", svc.error_count.toLocaleString()]);
                      rows.push(["Error rate", (errRate * 100).toFixed(2) + "%"]);
                      rows.push(["p50", Math.round(svc.p50_ms) + "ms"]);
                      rows.push(["p95", Math.round(svc.p95_ms) + "ms"]);
                      rows.push(["p99", Math.round(svc.p99_ms) + "ms"]);
                    } else {
                      rows.push(["Total calls (in)", n.totalCalls.toLocaleString()]);
                      rows.push(["No RED metrics", "service has no spans in window"]);
                    }
                    setHover({ kind: "node", x: p.x, y: p.y, title: id, rows });
                  }}
                  onMouseLeave={() => setHover(null)}
                >
                  <circle
                    cx={n.x} cy={n.y} r={n.r}
                    fill="var(--obs-card)"
                    stroke={stroke}
                    strokeWidth={strokeWidth}
                  />
                  <text
                    x={n.x} y={n.y + 4}
                    textAnchor="middle"
                    fontSize="11"
                    fontWeight="600"
                    fill="var(--obs-text)"
                    style={{ pointerEvents: "none", userSelect: "none" }}
                  >
                    {id.length > 14 ? id.slice(0, 13) + "…" : id}
                  </text>
                </g>
              );
            })}
          </g>

          {/* Tooltip — rendered as foreignObject so we keep one DOM tree. */}
          {hover && (
            <g
              transform={`translate(${
                Math.min(WIDTH - 220, Math.max(8, hover.x + 14))
              }, ${
                Math.min(HEIGHT - 8 - (40 + hover.rows.length * 16), Math.max(8, hover.y + 14))
              })`}
              style={{ pointerEvents: "none" }}
            >
              <rect
                width="220"
                height={36 + hover.rows.length * 16}
                rx="6"
                fill="var(--obs-elevated)"
                stroke="var(--obs-border)"
                strokeWidth="1"
              />
              <text x="12" y="20" fontSize="12" fontWeight="600" fill="var(--obs-text)">
                {hover.title.length > 28 ? hover.title.slice(0, 27) + "…" : hover.title}
              </text>
              {hover.rows.map((row, i) => (
                <g key={i}>
                  <text x="12" y={38 + i * 16} fontSize="11" fill="var(--obs-text-muted)">
                    {row[0]}
                  </text>
                  <text x="208" y={38 + i * 16} textAnchor="end" fontSize="11" fill="var(--obs-text)" style={{ fontVariantNumeric: "tabular-nums" }}>
                    {row[1]}
                  </text>
                </g>
              ))}
            </g>
          )}
        </svg>
      </div>
      <div class="service-map-legend">
        <span class="service-map-legend-item">
          <span class="service-map-swatch" style={{ background: "var(--obs-accent)" }} /> healthy edge
        </span>
        <span class="service-map-legend-item">
          <span class="service-map-swatch" style={{ background: "var(--obs-danger)" }} /> edge with errors
        </span>
        <span class="service-map-legend-item service-map-legend-hint">
          node size: log(call volume) · click a node to focus its neighborhood
        </span>
      </div>
    </div>
  );
}
