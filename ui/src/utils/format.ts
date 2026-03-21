export function formatNumber(n: number): string {
  if (n == null || isNaN(n)) return "0";
  return n.toLocaleString("en-US");
}

export function formatDuration(ms: number): string {
  if (!ms || ms <= 0) return "0s";
  const totalSeconds = Math.floor(ms / 1000);
  const minutes = Math.floor(totalSeconds / 60);
  const seconds = totalSeconds % 60;
  if (minutes > 0) {
    return seconds > 0 ? `${minutes}m ${seconds}s` : `${minutes}m`;
  }
  return `${seconds}s`;
}

export function formatPercent(n: number): string {
  if (n == null || isNaN(n)) return "0%";
  return `${n.toFixed(1)}%`;
}

export function formatChange(
  current: number,
  previous: number,
  invert?: boolean
): { value: string; direction: "up" | "down" | "flat"; color: string } {
  if (previous === 0 && current === 0) {
    return { value: "0%", direction: "flat", color: "#888" };
  }
  if (previous === 0) {
    return {
      value: "+100%",
      direction: "up",
      color: invert ? "#ef4444" : "#22c55e",
    };
  }

  const change = ((current - previous) / previous) * 100;
  const absChange = Math.abs(change);
  const formatted = absChange < 10 ? absChange.toFixed(1) : Math.round(absChange).toString();

  if (Math.abs(change) < 0.1) {
    return { value: "0%", direction: "flat", color: "#888" };
  }

  if (change > 0) {
    return {
      value: `+${formatted}%`,
      direction: "up",
      color: invert ? "#ef4444" : "#22c55e",
    };
  }

  return {
    value: `-${formatted}%`,
    direction: "down",
    color: invert ? "#22c55e" : "#ef4444",
  };
}
