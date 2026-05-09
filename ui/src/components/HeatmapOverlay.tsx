// HeatmapOverlay renders aggregated clicks as a canvas-painted heat layer
// on top of an existing replay frame. Designed to sit absolutely positioned
// inside the same `.replay-stage` container that hosts the replay iframe.
//
// Implementation notes:
//   - One radial gradient per click cluster, with alpha scaled by relative
//     count so a few hot spots dominate while the long tail is still
//     visible.
//   - We render onto a transparent canvas sized to its parent so the math
//     stays in pixel space (the API already returns bucket-centred pixels).
//   - Re-runs on every prop change, including window resize, so the overlay
//     stays aligned with the iframe content area.

import { useEffect, useRef } from "preact/hooks";
import type { Click } from "../api/heatmaps.js";

interface Props {
  clicks: Click[];
  width: number;
  height: number;
  // Radius of each heat circle in CSS pixels. 28 is a sensible default —
  // wide enough to merge adjacent buckets visually, narrow enough that a
  // hot button doesn't drown the rest of the frame.
  radius?: number;
}

export default function HeatmapOverlay({ clicks, width, height, radius = 28 }: Props) {
  const canvasRef = useRef<HTMLCanvasElement>(null);

  useEffect(() => {
    const canvas = canvasRef.current;
    if (!canvas) return;

    // Match canvas pixel resolution to its CSS size so circles stay crisp
    // on high-DPI displays without rendering at 2x cost.
    const dpr = Math.max(1, window.devicePixelRatio || 1);
    canvas.width = Math.floor(width * dpr);
    canvas.height = Math.floor(height * dpr);
    canvas.style.width = `${width}px`;
    canvas.style.height = `${height}px`;

    const ctx = canvas.getContext("2d");
    if (!ctx) return;
    ctx.scale(dpr, dpr);
    ctx.clearRect(0, 0, width, height);

    if (!clicks.length) return;

    // Normalize alpha against the hottest bucket so a single overwhelmingly
    // popular click doesn't wash out the entire surface.
    let maxCount = 0;
    for (const c of clicks) if (c.count > maxCount) maxCount = c.count;
    if (maxCount <= 0) return;

    ctx.globalCompositeOperation = "lighter";
    for (const c of clicks) {
      const intensity = Math.min(1, c.count / maxCount);
      // Alpha bottom-clamped so even the long tail is visible. Top-clamped
      // at 0.55 so multiple overlaps don't saturate to opaque red.
      const alpha = 0.15 + 0.4 * intensity;
      const grad = ctx.createRadialGradient(c.x, c.y, 0, c.x, c.y, radius);
      grad.addColorStop(0, `rgba(255, 64, 64, ${alpha})`);
      grad.addColorStop(0.5, `rgba(255, 160, 32, ${alpha * 0.6})`);
      grad.addColorStop(1, "rgba(255, 200, 64, 0)");
      ctx.fillStyle = grad;
      ctx.beginPath();
      ctx.arc(c.x, c.y, radius, 0, Math.PI * 2);
      ctx.fill();
    }
  }, [clicks, width, height, radius]);

  return (
    <canvas
      ref={canvasRef}
      class="heatmap-overlay-canvas"
      data-testid="heatmap-overlay-canvas"
      style={{
        position: "absolute",
        inset: 0,
        pointerEvents: "none",
        width: "100%",
        height: "100%",
      }}
      aria-hidden="true"
    />
  );
}
