interface Props {
  status: string;
  size?: "sm" | "md";
}

const COLORS: Record<string, { bg: string; text: string }> = {
  open: { bg: "rgba(239, 68, 68, 0.1)", text: "#ef4444" },
  resolved: { bg: "rgba(34, 197, 94, 0.1)", text: "#22c55e" },
  ignored: { bg: "rgba(161, 161, 170, 0.1)", text: "#a1a1aa" },
  up: { bg: "rgba(34, 197, 94, 0.1)", text: "#22c55e" },
  down: { bg: "rgba(239, 68, 68, 0.1)", text: "#ef4444" },
  running: { bg: "rgba(99, 102, 241, 0.1)", text: "#6366f1" },
  stopped: { bg: "rgba(161, 161, 170, 0.1)", text: "#a1a1aa" },
  draft: { bg: "rgba(245, 158, 11, 0.1)", text: "#f59e0b" },
  triggered: { bg: "rgba(239, 68, 68, 0.1)", text: "#ef4444" },
  enabled: { bg: "rgba(34, 197, 94, 0.1)", text: "#22c55e" },
  disabled: { bg: "rgba(161, 161, 170, 0.1)", text: "#a1a1aa" },
  error: { bg: "rgba(239, 68, 68, 0.1)", text: "#ef4444" },
  warning: { bg: "rgba(245, 158, 11, 0.1)", text: "#f59e0b" },
  info: { bg: "rgba(99, 102, 241, 0.1)", text: "#6366f1" },
  debug: { bg: "rgba(161, 161, 170, 0.1)", text: "#a1a1aa" },
  fatal: { bg: "rgba(239, 68, 68, 0.15)", text: "#ef4444" },
};

export default function StatusBadge({ status, size = "sm" }: Props) {
  const s = status.toLowerCase();
  const c = COLORS[s] || { bg: "rgba(161, 161, 170, 0.1)", text: "#a1a1aa" };
  const px = size === "sm" ? "6px 8px" : "6px 12px";
  const fs = size === "sm" ? "11px" : "12px";

  return (
    <span style={{
      display: "inline-flex", alignItems: "center",
      padding: px, borderRadius: "var(--obs-radius-full)",
      background: c.bg, color: c.text,
      fontSize: fs, fontWeight: 600, textTransform: "capitalize",
      lineHeight: 1, whiteSpace: "nowrap",
    }}>
      {status}
    </span>
  );
}
