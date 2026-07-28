import { useEffect, useRef, useState, useCallback } from "preact/hooks";
import type { TimeSeriesPoint } from "../api.js";
import { api } from "../api.js";
import { useFilters } from "../hooks/useFilters.js";
import { formatNumber } from "../utils/format.js";

const PAGEVIEW_COLOR = "#6366f1";
const VISITOR_COLOR = "#22c55e";
const INTERVALS = ["hour", "day", "week", "month"] as const;

function TimeSeriesChart() {
  const { state, dispatch } = useFilters();
  const { siteId, from, to, interval, filters, compare } = state;

  const wrapperRef = useRef<HTMLDivElement>(null);
  const canvasRef = useRef<HTMLCanvasElement>(null);
  const [data, setData] = useState<TimeSeriesPoint[]>([]);
  const [prevData, setPrevData] = useState<TimeSeriesPoint[]>([]);
  const [markers, setMarkers] = useState<Array<{ id: string; title: string; severity: string; started_at: number; ended_at: number }>>([]);
  const [loading, setLoading] = useState(true);
  const [tooltip, setTooltip] = useState<{
    x: number; y: number; date: string; pageviews: number; visitors: number;
  } | null>(null);

  const dataRef = useRef<TimeSeriesPoint[]>([]);
  const padRef = useRef({ top: 20, right: 20, bottom: 40, left: 55 });

  useEffect(() => {
    setLoading(true);
    const fetchCurrent = api.timeseries(siteId, from, to, interval, filters);
    if (!compare) {
      fetchCurrent.then((d) => {
        const result = d || [];
        setData(result);
        setPrevData([]);
        dataRef.current = result;
        setLoading(false);
      }).catch(() => setLoading(false));
      return;
    }
    // Compare mode: fetch the previous-period window too.
    const fromMs = new Date(from).getTime();
    const toMs = new Date(to).getTime();
    const duration = Math.max(1, toMs - fromMs);
    const prevFrom = new Date(fromMs - duration).toISOString();
    const prevTo = new Date(fromMs).toISOString();
    Promise.all([
      fetchCurrent,
      api.timeseries(siteId, prevFrom, prevTo, interval, filters).catch(() => [] as TimeSeriesPoint[]),
    ]).then(([cur, prev]) => {
      const curList = cur || [];
      const prevList = prev || [];
      setData(curList);
      // Shift previous buckets forward by the window so they align visually with the current range.
      setPrevData(prevList.map((p) => ({ ...p, bucket: p.bucket + duration })));
      dataRef.current = curList;
      setLoading(false);
    }).catch(() => setLoading(false));
  }, [siteId, from, to, interval, compare, JSON.stringify(filters)]);

  // Fetch incidents overlapping the current window. Best-effort — 404 / error
  // means "no markers," not a chart failure.
  useEffect(() => {
    const fromMs = new Date(from).getTime();
    const toMs = new Date(to).getTime();
    fetch(`/api/v1/incidents?site_id=${encodeURIComponent(siteId)}&from=${fromMs}&to=${toMs}`, {
      headers: { Authorization: "Bearer " + (typeof localStorage !== "undefined" ? localStorage.getItem("obs_token") || "" : "") },
    })
      .then((r) => r.ok ? r.json() : [])
      .then((list) => setMarkers(Array.isArray(list) ? list.map((inc: any) => ({
        id: inc.incident_id, title: inc.title, severity: inc.severity,
        started_at: inc.started_at, ended_at: inc.ended_at,
      })) : []))
      .catch(() => setMarkers([]));
  }, [siteId, from, to]);

  const draw = useCallback(() => {
    if (!canvasRef.current || data.length === 0) return;
    const canvas = canvasRef.current;
    const ctx = canvas.getContext("2d");
    if (!ctx) return;

    const dpr = window.devicePixelRatio || 1;
    const rect = canvas.getBoundingClientRect();
    canvas.width = rect.width * dpr;
    canvas.height = rect.height * dpr;
    ctx.scale(dpr, dpr);
    const w = rect.width;
    const h = rect.height;

    const pad = padRef.current;
    const plotW = w - pad.left - pad.right;
    const plotH = h - pad.top - pad.bottom;

    const pvMax = Math.max(
      ...data.map((d) => d.pageviews),
      ...prevData.map((d) => d.pageviews),
      1,
    );
    const vMax = Math.max(
      ...data.map((d) => d.visitors),
      ...prevData.map((d) => d.visitors),
      1,
    );
    const maxVal = Math.max(pvMax, vMax);
    const minT = Math.min(...data.map((d) => d.bucket));
    const maxT = Math.max(...data.map((d) => d.bucket));
    const rangeT = maxT - minT || 1;

    ctx.clearRect(0, 0, w, h);

    // Grid lines
    ctx.strokeStyle = "#1a1a1e";
    ctx.lineWidth = 1;
    for (let i = 0; i <= 4; i++) {
      const y = pad.top + (plotH / 4) * i;
      ctx.beginPath();
      ctx.moveTo(pad.left, y);
      ctx.lineTo(w - pad.right, y);
      ctx.stroke();
    }

    // Incident markers: shaded vertical bands underneath the series lines.
    // ended_at === 0 means "ongoing" — render to the right edge.
    for (const m of markers) {
      const sx = pad.left + ((m.started_at - minT) / rangeT) * plotW;
      const ex = m.ended_at === 0
        ? pad.left + plotW
        : pad.left + ((m.ended_at - minT) / rangeT) * plotW;
      const clipL = Math.max(pad.left, Math.min(sx, pad.left + plotW));
      const clipR = Math.max(pad.left, Math.min(ex, pad.left + plotW));
      if (clipR - clipL < 1) {
        // Single-point marker — draw a 2px line.
        ctx.fillStyle = sevFill(m.severity);
        ctx.fillRect(clipL, pad.top, 2, plotH);
        continue;
      }
      ctx.fillStyle = sevFill(m.severity);
      ctx.fillRect(clipL, pad.top, clipR - clipL, plotH);
      ctx.strokeStyle = sevStroke(m.severity);
      ctx.beginPath();
      ctx.moveTo(clipL, pad.top);
      ctx.lineTo(clipL, pad.top + plotH);
      ctx.stroke();
    }

    // Y-axis labels
    ctx.fillStyle = "#52525b";
    ctx.font = "11px Inter, system-ui, sans-serif";
    ctx.textAlign = "right";
    for (let i = 0; i <= 4; i++) {
      const y = pad.top + (plotH / 4) * i;
      const val = Math.round(maxVal * (1 - i / 4));
      ctx.fillText(String(val), pad.left - 8, y + 4);
    }

    // X-axis labels
    ctx.fillStyle = "#52525b";
    ctx.textAlign = "center";
    const labelCount = Math.min(data.length, 8);
    const step = Math.max(1, Math.floor(data.length / labelCount));
    for (let i = 0; i < data.length; i += step) {
      const x = pad.left + ((data[i].bucket - minT) / rangeT) * plotW;
      const date = new Date(data[i].bucket);
      let label: string;
      if (interval === "hour") {
        label = `${date.getMonth() + 1}/${date.getDate()} ${String(date.getHours()).padStart(2, "0")}:00`;
      } else if (interval === "month") {
        label = `${date.getFullYear()}-${String(date.getMonth() + 1).padStart(2, "0")}`;
      } else {
        label = `${date.getMonth() + 1}/${date.getDate()}`;
      }
      ctx.fillText(label, x, h - pad.bottom + 20);
    }

    // Compute bezier control points for smooth curves
    function getPoints(key: "pageviews" | "visitors"): Array<{ x: number; y: number }> {
      return data.map((pt) => ({
        x: pad.left + ((pt.bucket - minT) / rangeT) * plotW,
        y: pad.top + plotH - ((pt[key]) / maxVal) * plotH,
      }));
    }

    function drawBezierPath(ctx: CanvasRenderingContext2D, points: Array<{ x: number; y: number }>) {
      if (points.length < 2) return;
      ctx.moveTo(points[0].x, points[0].y);
      if (points.length === 2) {
        ctx.lineTo(points[1].x, points[1].y);
        return;
      }
      for (let i = 0; i < points.length - 1; i++) {
        const p0 = points[Math.max(0, i - 1)];
        const p1 = points[i];
        const p2 = points[i + 1];
        const p3 = points[Math.min(points.length - 1, i + 2)];
        const cp1x = p1.x + (p2.x - p0.x) / 6;
        const cp1y = p1.y + (p2.y - p0.y) / 6;
        const cp2x = p2.x - (p3.x - p1.x) / 6;
        const cp2y = p2.y - (p3.y - p1.y) / 6;
        ctx.bezierCurveTo(cp1x, cp1y, cp2x, cp2y, p2.x, p2.y);
      }
    }

    // Previous-period lines (drawn first so current sits on top).
    if (prevData.length > 0) {
      const prevPoints = (key: "pageviews" | "visitors") =>
        prevData.map((pt) => ({
          x: pad.left + ((pt.bucket - minT) / rangeT) * plotW,
          y: pad.top + plotH - (pt[key] / maxVal) * plotH,
        }));

      ctx.save();
      ctx.setLineDash([4, 4]);
      ctx.globalAlpha = 0.4;
      ctx.lineWidth = 1.5;

      const prevPV = prevPoints("pageviews");
      if (prevPV.length > 1) {
        ctx.beginPath();
        drawBezierPath(ctx, prevPV);
        ctx.strokeStyle = PAGEVIEW_COLOR;
        ctx.stroke();
      }

      const prevV = prevPoints("visitors");
      if (prevV.length > 1) {
        ctx.beginPath();
        drawBezierPath(ctx, prevV);
        ctx.strokeStyle = VISITOR_COLOR;
        ctx.stroke();
      }

      ctx.restore();
    }

    // Pageviews filled area
    const pvPoints = getPoints("pageviews");
    ctx.beginPath();
    ctx.moveTo(pvPoints[0].x, pad.top + plotH);
    ctx.lineTo(pvPoints[0].x, pvPoints[0].y);
    drawBezierPath(ctx, pvPoints);
    ctx.lineTo(pvPoints[pvPoints.length - 1].x, pad.top + plotH);
    ctx.closePath();
    ctx.fillStyle = "rgba(99, 102, 241, 0.1)";
    ctx.fill();

    // Pageviews line
    ctx.beginPath();
    drawBezierPath(ctx, pvPoints);
    ctx.strokeStyle = PAGEVIEW_COLOR;
    ctx.lineWidth = 2;
    ctx.stroke();

    // Visitors filled area
    const vPoints = getPoints("visitors");
    ctx.beginPath();
    ctx.moveTo(vPoints[0].x, pad.top + plotH);
    ctx.lineTo(vPoints[0].x, vPoints[0].y);
    drawBezierPath(ctx, vPoints);
    ctx.lineTo(vPoints[vPoints.length - 1].x, pad.top + plotH);
    ctx.closePath();
    ctx.fillStyle = "rgba(34, 197, 94, 0.08)";
    ctx.fill();

    // Visitors line
    ctx.beginPath();
    drawBezierPath(ctx, vPoints);
    ctx.strokeStyle = VISITOR_COLOR;
    ctx.lineWidth = 2;
    ctx.stroke();
  }, [data, prevData, interval, markers]);

  useEffect(() => {
    draw();
  }, [draw]);

  // ResizeObserver
  useEffect(() => {
    if (!wrapperRef.current) return;
    const obs = new ResizeObserver(() => draw());
    obs.observe(wrapperRef.current);
    return () => obs.disconnect();
  }, [draw]);

  // Mouse tracking for tooltip
  const handleMouseMove = useCallback((e: MouseEvent) => {
    const canvas = canvasRef.current;
    const d = dataRef.current;
    if (!canvas || d.length === 0) return;

    const rect = canvas.getBoundingClientRect();
    const mouseX = e.clientX - rect.left;
    const pad = padRef.current;
    const plotW = rect.width - pad.left - pad.right;

    const minT = Math.min(...d.map((p) => p.bucket));
    const maxT = Math.max(...d.map((p) => p.bucket));
    const rangeT = maxT - minT || 1;

    // Find nearest point
    let closest = 0;
    let closestDist = Infinity;
    for (let i = 0; i < d.length; i++) {
      const px = pad.left + ((d[i].bucket - minT) / rangeT) * plotW;
      const dist = Math.abs(px - mouseX);
      if (dist < closestDist) {
        closestDist = dist;
        closest = i;
      }
    }

    if (closestDist > 50) {
      setTooltip(null);
      return;
    }

    const pt = d[closest];
    const date = new Date(pt.bucket);
    let dateStr: string;
    if (state.interval === "hour") {
      dateStr = `${date.getMonth() + 1}/${date.getDate()} ${String(date.getHours()).padStart(2, "0")}:00`;
    } else {
      dateStr = `${date.getFullYear()}-${String(date.getMonth() + 1).padStart(2, "0")}-${String(date.getDate()).padStart(2, "0")}`;
    }

    const px = pad.left + ((pt.bucket - minT) / rangeT) * plotW;
    setTooltip({
      x: px,
      y: e.clientY - rect.top - 70,
      date: dateStr,
      pageviews: pt.pageviews,
      visitors: pt.visitors,
    });
  }, [state.interval]);

  const handleMouseLeave = useCallback(() => {
    setTooltip(null);
  }, []);

  if (loading) {
    return (
      <div style="height:300px;display:flex;align-items:center;justify-content:center;">
        <div class="obs-skeleton" style="width:100%;padding:20px;">
          <div class="obs-skeleton-bar" style="width:100%;height:200px;border-radius:8px;" />
        </div>
      </div>
    );
  }
  if (data.length === 0) {
    return (
      <div class="obs-empty" style="height:300px;">
        <div class="obs-empty-icon">--</div>
        <span>No data for this period</span>
      </div>
    );
  }

  return (
    <div>
      <div style="display:flex;justify-content:space-between;align-items:center;margin-bottom:12px;">
        <div class="obs-chart-legend">
          <div class="obs-chart-legend-item">
            <span class="obs-chart-legend-dot" style={`background:${PAGEVIEW_COLOR}`} />
            <span>Pageviews</span>
          </div>
          <div class="obs-chart-legend-item">
            <span class="obs-chart-legend-dot" style={`background:${VISITOR_COLOR}`} />
            <span>Visitors</span>
          </div>
        </div>
        <div class="obs-interval-btns">
          {INTERVALS.map((iv) => (
            <button
              key={iv}
              class={`obs-btn ${interval === iv ? "obs-btn-active" : ""}`}
              onClick={() => dispatch({ type: "SET_INTERVAL", interval: iv })}
            >
              {iv.charAt(0).toUpperCase() + iv.slice(1)}
            </button>
          ))}
        </div>
      </div>
      <div ref={wrapperRef} class="obs-chart-wrapper">
        <canvas
          ref={canvasRef}
          style="width:100%;height:300px;display:block;cursor:crosshair;"
          onMouseMove={handleMouseMove as any}
          onMouseLeave={handleMouseLeave}
        />
        {tooltip && (
          <div
            class="obs-chart-tooltip"
            style={`left:${Math.min(tooltip.x, (wrapperRef.current?.offsetWidth || 400) - 170)}px;top:${Math.max(0, tooltip.y)}px;`}
          >
            <div class="obs-chart-tooltip-date">{tooltip.date}</div>
            <div class="obs-chart-tooltip-row">
              <span class="obs-chart-tooltip-dot" style={`background:${PAGEVIEW_COLOR}`} />
              <span>Pageviews</span>
              <span style="margin-left:auto;font-weight:600;">{formatNumber(tooltip.pageviews)}</span>
            </div>
            <div class="obs-chart-tooltip-row">
              <span class="obs-chart-tooltip-dot" style={`background:${VISITOR_COLOR}`} />
              <span>Visitors</span>
              <span style="margin-left:auto;font-weight:600;">{formatNumber(tooltip.visitors)}</span>
            </div>
          </div>
        )}
        {tooltip && (
          <div class="obs-chart-crosshair" style={`left:${tooltip.x}px;`} />
        )}
      </div>
    </div>
  );
}

TimeSeriesChart.displayName = "TimeSeriesChart";
export default TimeSeriesChart;

// Translucent fill for incident markers, keyed on severity.
function sevFill(s: string): string {
  switch (s) {
    case "critical": return "rgba(229, 72, 77, 0.14)";
    case "warning":  return "rgba(245, 165, 36, 0.12)";
    default:          return "rgba(110, 168, 254, 0.10)";
  }
}

// Opaque stroke for the incident marker's leading edge.
function sevStroke(s: string): string {
  switch (s) {
    case "critical": return "rgba(229, 72, 77, 0.9)";
    case "warning":  return "rgba(245, 165, 36, 0.8)";
    default:          return "rgba(110, 168, 254, 0.7)";
  }
}
