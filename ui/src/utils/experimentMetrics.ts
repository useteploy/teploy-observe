// Pure display helpers for the experiment results panel.

export type MetricKindLike = "binary" | "count" | "mean" | undefined;

/** Formats an arm's headline value: a rate for binary goals, a mean otherwise. */
export function formatArmValue(
  kind: MetricKindLike,
  v: { conversion_rate: number; mean?: number },
): string {
  if (kind === "count" || kind === "mean") {
    if (v.mean === undefined || !Number.isFinite(v.mean)) return "--";
    const abs = Math.abs(v.mean);
    return abs >= 100 ? v.mean.toFixed(0) : abs >= 1 ? v.mean.toFixed(2) : v.mean.toFixed(3);
  }
  return `${(v.conversion_rate * 100).toFixed(2)}%`;
}

/** Short human label for a metric kind. */
export function metricKindLabel(kind: MetricKindLike): string {
  switch (kind) {
    case "count": return "count per user";
    case "mean": return "mean per user";
    default: return "conversion rate";
  }
}

/** Heading for a secondary metric row: always carries the exploratory label. */
export function secondaryLabel(m: { key: string; name?: string }): string {
  return `${m.name || m.key} (secondary, exploratory)`;
}
