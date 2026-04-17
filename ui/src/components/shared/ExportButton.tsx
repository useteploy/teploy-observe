import { useState } from "preact/hooks";

interface Column<T> {
  key: keyof T | string;
  label: string;
  /** Optional custom extractor when a raw field lookup isn't enough. */
  get?: (row: T) => string | number | null | undefined;
}

interface Props<T> {
  filename: string;
  rows: T[];
  columns: Column<T>[];
  disabled?: boolean;
}

function escapeCSV(value: unknown): string {
  if (value === null || value === undefined) return "";
  const s = String(value);
  if (/[",\n\r]/.test(s)) return `"${s.replace(/"/g, '""')}"`;
  return s;
}

function triggerDownload(filename: string, content: string) {
  const blob = new Blob([content], { type: "text/csv;charset=utf-8" });
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  document.body.removeChild(a);
  URL.revokeObjectURL(url);
}

export default function ExportButton<T>({ filename, rows, columns, disabled }: Props<T>) {
  const [status, setStatus] = useState<"idle" | "done">("idle");

  const onClick = () => {
    if (!rows.length) return;
    const header = columns.map((c) => escapeCSV(c.label)).join(",");
    const body = rows
      .map((row) =>
        columns
          .map((c) => {
            const raw = c.get ? c.get(row) : (row as any)[c.key];
            return escapeCSV(raw);
          })
          .join(","),
      )
      .join("\n");
    triggerDownload(filename, header + "\n" + body);
    setStatus("done");
    setTimeout(() => setStatus("idle"), 1500);
  };

  return (
    <button
      class="obs-btn obs-btn--sm obs-export-btn"
      disabled={disabled || rows.length === 0}
      onClick={onClick}
      title={rows.length ? `Download ${rows.length} rows as CSV` : "Nothing to export"}
    >
      {status === "done" ? "✓ Exported" : "Export CSV"}
    </button>
  );
}
