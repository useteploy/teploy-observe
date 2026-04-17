import { useEffect, useState } from "preact/hooks";
import type { OverviewResponse, RealtimeResult } from "../api.js";
import { api } from "../api.js";
import { useFilters } from "../hooks/useFilters.js";
import { useCountUp } from "../hooks/useCountUp.js";
import { formatNumber, formatDuration, formatPercent, formatChange } from "../utils/format.js";

function AnimatedStat({ target, format, live }: { target: number; format: (n: number) => string; live?: boolean }) {
  const value = useCountUp(target);
  return (
    <div class="obs-stat-value" style={live ? "color:var(--obs-success)" : undefined}>
      {format(value)}
    </div>
  );
}

function StatsCards() {
  const { state } = useFilters();
  const { siteId, from, to, compare, filters } = state;

  const [overview, setOverview] = useState<OverviewResponse | null>(null);
  const [realtime, setRealtime] = useState<RealtimeResult | null>(null);

  useEffect(() => {
    api.overview(siteId, from, to, compare, filters).then(setOverview).catch(() => {});
    api.realtime(siteId).then(setRealtime).catch(() => {});

    const interval = setInterval(() => {
      api.realtime(siteId).then(setRealtime).catch(() => {});
    }, 15000);
    return () => clearInterval(interval);
  }, [siteId, from, to, compare, JSON.stringify(filters)]);

  const cur = overview?.current;
  const prev = overview?.previous;

  const cards: Array<{
    label: string;
    target: number | null;
    format: (n: number) => string;
    live?: boolean;
    change?: { value: string; direction: string; color: string };
  }> = [
    {
      label: "Active now",
      target: realtime?.active_visitors ?? null,
      format: (n) => formatNumber(Math.round(n)),
      live: true,
    },
    {
      label: "Pageviews",
      target: cur?.pageviews ?? null,
      format: (n) => formatNumber(Math.round(n)),
      change: prev != null && cur != null ? formatChange(cur.pageviews, prev.pageviews) : undefined,
    },
    {
      label: "Visitors",
      target: cur?.visitors ?? null,
      format: (n) => formatNumber(Math.round(n)),
      change: prev != null && cur != null ? formatChange(cur.visitors, prev.visitors) : undefined,
    },
    {
      label: "Sessions",
      target: cur?.sessions ?? null,
      format: (n) => formatNumber(Math.round(n)),
      change: prev != null && cur != null ? formatChange(cur.sessions, prev.sessions) : undefined,
    },
    {
      label: "Bounce Rate",
      target: cur?.bounce_rate ?? null,
      format: formatPercent,
      change: prev != null && cur != null ? formatChange(cur.bounce_rate, prev.bounce_rate, true) : undefined,
    },
    {
      label: "Avg Duration",
      target: cur?.avg_duration ?? null,
      format: formatDuration,
      change: prev != null && cur != null ? formatChange(cur.avg_duration, prev.avg_duration) : undefined,
    },
  ];

  return (
    <div class="obs-grid-stats">
      {cards.map((c, i) => (
        <div key={c.label} class="obs-card" style={{ animationDelay: `${i * 40}ms` }}>
          {c.target !== null ? (
            <AnimatedStat target={c.target} format={c.format} live={c.live} />
          ) : (
            <div class="obs-stat-value" style={c.live ? "color:var(--obs-success)" : undefined}>--</div>
          )}
          <div class="obs-stat-label">
            {c.live && <span class="obs-live-dot" />}
            {c.label}
          </div>
          {c.change && (
            <div class={`obs-stat-change ${c.change.direction === "up" ? "obs-change-up" : c.change.direction === "down" ? "obs-change-down" : "obs-change-flat"}`}>
              <span class="obs-stat-change-pill">
                {c.change.direction === "up" && "\u2191 "}
                {c.change.direction === "down" && "\u2193 "}
                {c.change.value}
              </span>
            </div>
          )}
        </div>
      ))}
    </div>
  );
}

StatsCards.displayName = "StatsCards";
export default StatsCards;
