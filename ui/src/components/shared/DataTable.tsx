import type { ComponentChildren } from "preact";

export interface Column<T> {
  key: string;
  label: string;
  render?: (row: T) => ComponentChildren;
  width?: string;
  align?: "left" | "right" | "center";
}

interface Props<T> {
  columns: Column<T>[];
  data: T[];
  onRowClick?: (row: T) => void;
  emptyMessage?: string;
  loading?: boolean;
}

export default function DataTable<T extends Record<string, any>>({ columns, data, onRowClick, emptyMessage, loading }: Props<T>) {
  if (loading) {
    return (
      <div class="obs-table-skeleton">
        {Array.from({ length: 5 }).map((_, i) => (
          <div key={i} class="obs-skeleton-row" />
        ))}
      </div>
    );
  }

  if (!data.length) {
    return <div class="obs-empty-state">{emptyMessage || "No data"}</div>;
  }

  return (
    <div class="obs-table-wrap">
      <table class="obs-table">
        <thead>
          <tr>
            {columns.map(col => (
              <th key={col.key} style={{ width: col.width, textAlign: col.align || "left" }}>{col.label}</th>
            ))}
          </tr>
        </thead>
        <tbody>
          {data.map((row, i) => (
            <tr key={i} onClick={() => onRowClick?.(row)}
              class={onRowClick ? "obs-table-row--clickable" : ""}>
              {columns.map(col => (
                <td key={col.key} style={{ textAlign: col.align || "left" }}>
                  {col.render ? col.render(row) : String(row[col.key] ?? "")}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
