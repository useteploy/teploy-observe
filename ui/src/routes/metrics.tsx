import { useState, useEffect, useMemo } from "preact/hooks";
import { metricsApi } from "../api/metrics.js";
import type { MetricInfo, MetricPoint, Aggregation } from "../api/metrics.js";
import { useFilters } from "../hooks/useFilters.js";
import EmptyState from "../components/shared/EmptyState.js";

export const config = { mode: "app" };

const AGGREGATIONS: Aggregation[] = ["last", "avg", "sum", "min", "max"];

function formatValue(v: number): string {
  if (!isFinite(v)) return "-";
  if (Math.abs(v) >= 1000) return v.toFixed(0);
  if (Math.abs(v) >= 1) return v.toFixed(2);
  return v.toFixed(4);
}

function formatTime(ts: number): string {
  try {
    return new Date(ts).toLocaleTimeString("en-US", {
      hour: "2-digit",
      minute: "2-digit",
      hour12: false,
    });
  } catch {
    return String(ts);
  }
}

interface ChartProps {
  points: MetricPoint[];
  width: number;
  height: number;
}

// Inline SVG line chart. Kept self-contained so this route doesn't
// depend on the canvas-based TimeSeriesChart component (which is wired
// to the analytics-only timeseries API).
function LineChart({ points, width, height }: ChartProps) {
  if (points.length === 0) {
    return (
      <div style={{ height: `${height}px`, display: "flex", alignItems: "center", justifyContent: "center", color: "var(--obs-text-secondary)" }}>
        No data points in selected window
      </div>
    );
  }

  const padding = { top: 16, right: 24, bottom: 28, left: 48 };
  const innerW = width - padding.left - padding.right;
  const innerH = height - padding.top - padding.bottom;

  const xs = points.map(p => p.ts_ms);
  const ys = points.map(p => p.value);
  const xMin = Math.min(...xs);
  const xMax = Math.max(...xs);
  const yMin = Math.min(...ys, 0);
  const yMax = Math.max(...ys, 1);
  const xRange = Math.max(1, xMax - xMin);
  const yRange = Math.max(0.0001, yMax - yMin);

  const scaleX = (x: number) => padding.left + ((x - xMin) / xRange) * innerW;
  const scaleY = (y: number) => padding.top + innerH - ((y - yMin) / yRange) * innerH;

  const linePath = points
    .map((p, i) => `${i === 0 ? "M" : "L"} ${scaleX(p.ts_ms).toFixed(1)} ${scaleY(p.value).toFixed(1)}`)
    .join(" ");

  const areaPath = `${linePath} L ${scaleX(xs[xs.length - 1]).toFixed(1)} ${(padding.top + innerH).toFixed(1)} L ${scaleX(xs[0]).toFixed(1)} ${(padding.top + innerH).toFixed(1)} Z`;

  // Y-axis ticks (4 horizontal lines)
  const yTicks = [0, 0.25, 0.5, 0.75, 1].map(t => yMin + t * yRange);
  // X-axis ticks (5 evenly spaced labels)
  const xTickCount = Math.min(5, points.length);
  const xTicks = Array.from({ length: xTickCount }, (_, i) => xMin + (i * xRange) / Math.max(1, xTickCount - 1));

  return (
    <svg width={width} height={height} role="img" aria-label="Metric line chart">
      <defs>
        <linearGradient id="metric-area" x1="0" y1="0" x2="0" y2="1">
          <stop offset="0%" stop-color="#6366f1" stop-opacity="0.3" />
          <stop offset="100%" stop-color="#6366f1" stop-opacity="0.0" />
        </linearGradient>
      </defs>

      {yTicks.map((t, i) => (
        <g key={`y-${i}`}>
          <line
            x1={padding.left} x2={width - padding.right}
            y1={scaleY(t)} y2={scaleY(t)}
            stroke="var(--obs-border)" stroke-dasharray="2,2"
          />
          <text x={padding.left - 6} y={scaleY(t) + 4}
            text-anchor="end" font-size="10"
            fill="var(--obs-text-secondary)">
            {formatValue(t)}
          </text>
        </g>
      ))}

      {xTicks.map((t, i) => (
        <text key={`x-${i}`} x={scaleX(t)} y={height - padding.bottom + 18}
          text-anchor="middle" font-size="10"
          fill="var(--obs-text-secondary)">
          {formatTime(t)}
        </text>
      ))}

      <path d={areaPath} fill="url(#metric-area)" />
      <path d={linePath} fill="none" stroke="#6366f1" stroke-width="2" />

      {points.map((p, i) => (
        <circle key={i} cx={scaleX(p.ts_ms)} cy={scaleY(p.value)} r="2.5" fill="#6366f1">
          <title>{`${formatTime(p.ts_ms)}: ${formatValue(p.value)}`}</title>
        </circle>
      ))}
    </svg>
  );
}

function EmptyMetricsState() {
  return (
    <EmptyState
      title="No metrics ingested yet"
      description="Send OTLP metrics to /v1/metrics to start graphing."
      icon="signal"
    >
      <div style={{ marginTop: "12px", fontFamily: "monospace", fontSize: "12px", textAlign: "left", maxWidth: "520px" }}>
        <div>{`import "github.com/useteploy/teploy-observe/sdk/go"`}</div>
        <div style={{ marginTop: "4px" }}>{`client.Counter("requests_total").Add(1)`}</div>
      </div>
      <div style={{ marginTop: "12px", fontSize: "13px" }}>
        See <a href="/docs">docs</a> for the full SDK reference.
      </div>
    </EmptyState>
  );
}

export default function MetricsRoute() {
  const { state } = useFilters();
  const { siteId, from, to } = state;

  const [list, setList] = useState<MetricInfo[]>([]);
  const [listLoading, setListLoading] = useState(true);
  const [selected, setSelected] = useState<string | null>(null);
  const [agg, setAgg] = useState<Aggregation>("last");
  const [points, setPoints] = useState<MetricPoint[]>([]);
  const [pointsLoading, setPointsLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Load metric names whenever the active site changes.
  useEffect(() => {
    if (!siteId) return;
    setListLoading(true);
    metricsApi.list(siteId)
      .then((items) => {
        const arr = items || [];
        setList(arr);
        if (arr.length > 0 && !selected) {
          setSelected(arr[0].name);
        }
      })
      .catch((e) => setError(String(e)))
      .finally(() => setListLoading(false));
  }, [siteId]);

  // Load points whenever the metric or aggregation or range changes.
  useEffect(() => {
    if (!siteId || !selected) {
      setPoints([]);
      return;
    }
    const fromMs = new Date(from).getTime();
    const toMs = new Date(to).getTime();
    setPointsLoading(true);
    metricsApi.query(siteId, selected, fromMs, toMs, agg)
      .then((p) => setPoints(p || []))
      .catch((e) => {
        setError(String(e));
        setPoints([]);
      })
      .finally(() => setPointsLoading(false));
  }, [siteId, selected, agg, from, to]);

  const selectedKind = useMemo(() => {
    return list.find(m => m.name === selected)?.kind ?? "";
  }, [list, selected]);

  if (!listLoading && list.length === 0) {
    return (
      <div style={{ padding: "32px" }}>
        <h1 style={{ fontSize: "24px", marginBottom: "16px" }}>Metrics</h1>
        <EmptyMetricsState />
      </div>
    );
  }

  return (
    <div style={{ padding: "24px", display: "flex", flexDirection: "column", gap: "16px", height: "100%" }}>
      <div>
        <h1 style={{ fontSize: "24px", margin: 0 }}>Metrics</h1>
        <p style={{ color: "var(--obs-text-secondary)", margin: "4px 0 0", fontSize: "13px" }}>
          OTLP gauges, sums (counters), and histograms — Phase 1 viewer.
        </p>
      </div>

      {error && (
        <div style={{ background: "rgba(239,68,68,0.1)", border: "1px solid rgba(239,68,68,0.4)", borderRadius: "6px", padding: "8px 12px", color: "#ef4444", fontSize: "13px" }}>
          {error}
        </div>
      )}

      <div style={{ display: "grid", gridTemplateColumns: "260px 1fr", gap: "16px", flex: 1, minHeight: 0 }}>
        <aside style={{ border: "1px solid var(--obs-border)", borderRadius: "8px", overflow: "hidden", display: "flex", flexDirection: "column" }}>
          <div style={{ padding: "10px 12px", borderBottom: "1px solid var(--obs-border)", fontSize: "12px", textTransform: "uppercase", letterSpacing: "0.05em", color: "var(--obs-text-secondary)" }}>
            Metrics ({list.length})
          </div>
          <div style={{ overflowY: "auto", flex: 1 }}>
            {listLoading ? (
              <div style={{ padding: "12px", color: "var(--obs-text-secondary)", fontSize: "13px" }}>Loading…</div>
            ) : (
              list.map(m => (
                <button
                  key={m.name}
                  onClick={() => setSelected(m.name)}
                  data-testid={`metric-item-${m.name}`}
                  style={{
                    display: "block",
                    width: "100%",
                    textAlign: "left",
                    padding: "8px 12px",
                    background: m.name === selected ? "var(--obs-bg-secondary)" : "transparent",
                    border: "none",
                    borderBottom: "1px solid var(--obs-border)",
                    cursor: "pointer",
                    fontSize: "13px",
                    color: "var(--obs-text)",
                    fontFamily: "inherit",
                  }}
                >
                  <div style={{ fontWeight: 500 }}>{m.name}</div>
                  <div style={{ fontSize: "11px", color: "var(--obs-text-secondary)", marginTop: "2px" }}>{m.kind}</div>
                </button>
              ))
            )}
          </div>
        </aside>

        <main style={{ border: "1px solid var(--obs-border)", borderRadius: "8px", padding: "16px", display: "flex", flexDirection: "column", gap: "16px", overflow: "hidden" }}>
          {selected ? (
            <>
              <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", flexWrap: "wrap", gap: "12px" }}>
                <div>
                  <h2 style={{ margin: 0, fontSize: "16px" }} data-testid="metric-selected-name">{selected}</h2>
                  <div style={{ fontSize: "12px", color: "var(--obs-text-secondary)", marginTop: "2px" }}>
                    {selectedKind} • {points.length} bucket{points.length === 1 ? "" : "s"}
                  </div>
                </div>
                <div style={{ display: "flex", gap: "6px" }}>
                  {AGGREGATIONS.map(a => (
                    <button
                      key={a}
                      onClick={() => setAgg(a)}
                      data-testid={`metric-agg-${a}`}
                      style={{
                        padding: "4px 10px",
                        border: "1px solid var(--obs-border)",
                        borderRadius: "4px",
                        background: a === agg ? "var(--obs-accent)" : "transparent",
                        color: a === agg ? "#fff" : "var(--obs-text)",
                        cursor: "pointer",
                        fontSize: "12px",
                        fontFamily: "inherit",
                      }}
                    >
                      {a}
                    </button>
                  ))}
                </div>
              </div>

              <div data-testid="metric-chart" style={{ flex: 1, minHeight: "240px" }}>
                {pointsLoading ? (
                  <div style={{ height: "100%", display: "flex", alignItems: "center", justifyContent: "center", color: "var(--obs-text-secondary)" }}>
                    Loading…
                  </div>
                ) : (
                  <LineChart points={points} width={760} height={320} />
                )}
              </div>
            </>
          ) : (
            <div style={{ padding: "32px", color: "var(--obs-text-secondary)", textAlign: "center" }}>
              Select a metric on the left to view its chart.
            </div>
          )}
        </main>
      </div>
    </div>
  );
}
