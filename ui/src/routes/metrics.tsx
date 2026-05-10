import { useState, useEffect, useMemo } from "preact/hooks";
import { metricsApi } from "../api/metrics.js";
import type { MetricInfo, MetricSeries, Aggregation, StepDuration } from "../api/metrics.js";
import { useFilters } from "../hooks/useFilters.js";
import EmptyState from "../components/shared/EmptyState.js";

export const config = { mode: "app" };

const AGGREGATIONS: Aggregation[] = ["last", "avg", "sum", "min", "max", "rate", "p50", "p95", "p99"];
const STEPS: StepDuration[] = ["15s", "30s", "60s", "5m", "1h", "1d"];

// Tableau-style 8-colour rotation. Stable across re-renders so the same
// label fingerprint always maps to the same colour in a session.
const COLORS = [
  "#6366f1", "#10b981", "#f59e0b", "#ef4444",
  "#8b5cf6", "#06b6d4", "#ec4899", "#84cc16",
];

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

function labelKey(labels: Record<string, string>): string {
  const keys = Object.keys(labels).sort();
  if (keys.length === 0) return "(all)";
  return keys.map(k => `${k}=${labels[k]}`).join(" ");
}

interface ChartProps {
  series: MetricSeries[];
  width: number;
  height: number;
  highlight: number | null;
}

// Inline SVG multi-line chart. Each series gets a stable colour based on
// its array index — the legend mirrors the colour swatches so hover-
// highlight is unambiguous.
function MultiLineChart({ series, width, height, highlight }: ChartProps) {
  const allPoints = series.flatMap(s => s.points);
  if (allPoints.length === 0) {
    return (
      <div style={{ height: `${height}px`, display: "flex", alignItems: "center", justifyContent: "center", color: "var(--obs-text-secondary)" }}>
        No data points in selected window
      </div>
    );
  }

  const padding = { top: 16, right: 24, bottom: 28, left: 48 };
  const innerW = width - padding.left - padding.right;
  const innerH = height - padding.top - padding.bottom;

  const xs = allPoints.map(p => p.ts_ms);
  const ys = allPoints.map(p => p.value);
  const xMin = Math.min(...xs);
  const xMax = Math.max(...xs);
  const yMin = Math.min(...ys, 0);
  const yMax = Math.max(...ys, 1);
  const xRange = Math.max(1, xMax - xMin);
  const yRange = Math.max(0.0001, yMax - yMin);

  const scaleX = (x: number) => padding.left + ((x - xMin) / xRange) * innerW;
  const scaleY = (y: number) => padding.top + innerH - ((y - yMin) / yRange) * innerH;

  const yTicks = [0, 0.25, 0.5, 0.75, 1].map(t => yMin + t * yRange);
  const xTickCount = Math.min(5, allPoints.length);
  const xTicks = Array.from(
    { length: xTickCount },
    (_, i) => xMin + (i * xRange) / Math.max(1, xTickCount - 1),
  );

  return (
    <svg width={width} height={height} role="img" aria-label="Metric line chart">
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

      {series.map((s, idx) => {
        if (s.points.length === 0) return null;
        const color = COLORS[idx % COLORS.length];
        const dim = highlight !== null && highlight !== idx ? 0.18 : 1;
        const linePath = s.points
          .map((p, i) => `${i === 0 ? "M" : "L"} ${scaleX(p.ts_ms).toFixed(1)} ${scaleY(p.value).toFixed(1)}`)
          .join(" ");
        return (
          <g key={idx} opacity={dim}>
            <path d={linePath} fill="none" stroke={color} stroke-width="2" />
            {s.points.map((p, i) => (
              <circle key={i} cx={scaleX(p.ts_ms)} cy={scaleY(p.value)} r="2.5" fill={color}>
                <title>{`${labelKey(s.labels)} • ${formatTime(p.ts_ms)}: ${formatValue(p.value)}`}</title>
              </circle>
            ))}
          </g>
        );
      })}
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
  const [step, setStep] = useState<StepDuration>("60s");
  const [groupByRaw, setGroupByRaw] = useState<string>("");
  const [series, setSeries] = useState<MetricSeries[]>([]);
  const [pointsLoading, setPointsLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [highlight, setHighlight] = useState<number | null>(null);

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

  const groupBy = useMemo(() => {
    return groupByRaw.split(",").map(s => s.trim()).filter(Boolean);
  }, [groupByRaw]);

  // Load series whenever any of the query inputs change.
  useEffect(() => {
    if (!siteId || !selected) {
      setSeries([]);
      return;
    }
    const fromMs = new Date(from).getTime();
    const toMs = new Date(to).getTime();
    setPointsLoading(true);
    metricsApi.series(siteId, selected, fromMs, toMs, agg, { step, groupBy })
      .then((s) => setSeries(s || []))
      .catch((e) => {
        setError(String(e));
        setSeries([]);
      })
      .finally(() => setPointsLoading(false));
  }, [siteId, selected, agg, step, from, to, groupByRaw]);

  const selectedKind = useMemo(() => {
    return list.find(m => m.name === selected)?.kind ?? "";
  }, [list, selected]);

  const totalPoints = useMemo(() => series.reduce((acc, s) => acc + s.points.length, 0), [series]);

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
          OTLP gauges, sums (counters), and histograms. Use group_by to fan series out by label.
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
                    {selectedKind} • {series.length} series • {totalPoints} point{totalPoints === 1 ? "" : "s"}
                  </div>
                </div>
                <div style={{ display: "flex", gap: "12px", alignItems: "center", flexWrap: "wrap" }}>
                  <label style={{ display: "flex", alignItems: "center", gap: "6px", fontSize: "12px", color: "var(--obs-text-secondary)" }}>
                    group_by
                    <input
                      data-testid="metric-groupby"
                      placeholder="region,instance"
                      value={groupByRaw}
                      onInput={(e) => setGroupByRaw((e.target as HTMLInputElement).value)}
                      style={{
                        padding: "4px 8px",
                        border: "1px solid var(--obs-border)",
                        borderRadius: "4px",
                        background: "var(--obs-bg)",
                        color: "var(--obs-text)",
                        fontSize: "12px",
                        fontFamily: "inherit",
                        width: "140px",
                      }}
                    />
                  </label>
                  <label style={{ display: "flex", alignItems: "center", gap: "6px", fontSize: "12px", color: "var(--obs-text-secondary)" }}>
                    step
                    <select
                      data-testid="metric-step"
                      value={step}
                      onChange={(e) => setStep((e.target as HTMLSelectElement).value as StepDuration)}
                      style={{
                        padding: "4px 8px",
                        border: "1px solid var(--obs-border)",
                        borderRadius: "4px",
                        background: "var(--obs-bg)",
                        color: "var(--obs-text)",
                        fontSize: "12px",
                        fontFamily: "inherit",
                      }}
                    >
                      {STEPS.map(s => <option key={s} value={s}>{s}</option>)}
                    </select>
                  </label>
                  <select
                    data-testid="metric-agg-select"
                    value={agg}
                    onChange={(e) => setAgg((e.target as HTMLSelectElement).value as Aggregation)}
                    style={{
                      padding: "4px 8px",
                      border: "1px solid var(--obs-border)",
                      borderRadius: "4px",
                      background: "var(--obs-bg)",
                      color: "var(--obs-text)",
                      fontSize: "12px",
                      fontFamily: "inherit",
                    }}
                  >
                    {AGGREGATIONS.map(a => (
                      <option key={a} value={a} data-testid={`metric-agg-${a}`}>{a}</option>
                    ))}
                  </select>
                </div>
              </div>

              <div data-testid="metric-chart" style={{ flex: 1, minHeight: "240px" }}>
                {pointsLoading ? (
                  <div style={{ height: "100%", display: "flex", alignItems: "center", justifyContent: "center", color: "var(--obs-text-secondary)" }}>
                    Loading…
                  </div>
                ) : (
                  <MultiLineChart series={series} width={760} height={320} highlight={highlight} />
                )}
              </div>

              {series.length > 0 && (
                <div data-testid="metric-legend" style={{ display: "flex", flexWrap: "wrap", gap: "8px", maxHeight: "120px", overflowY: "auto", paddingTop: "8px", borderTop: "1px solid var(--obs-border)" }}>
                  {series.map((s, idx) => (
                    <div
                      key={idx}
                      data-testid={`metric-legend-item-${idx}`}
                      onMouseEnter={() => setHighlight(idx)}
                      onMouseLeave={() => setHighlight(null)}
                      style={{
                        display: "flex",
                        alignItems: "center",
                        gap: "6px",
                        padding: "4px 8px",
                        border: "1px solid var(--obs-border)",
                        borderRadius: "4px",
                        background: highlight === idx ? "var(--obs-bg-secondary)" : "transparent",
                        cursor: "default",
                        fontSize: "12px",
                        color: "var(--obs-text)",
                      }}
                    >
                      <span style={{ width: "10px", height: "10px", borderRadius: "2px", background: COLORS[idx % COLORS.length], display: "inline-block" }} />
                      <span>{labelKey(s.labels)}</span>
                    </div>
                  ))}
                </div>
              )}
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
