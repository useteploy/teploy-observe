import { useState, useEffect } from "preact/hooks";
import { get, post } from "../api/helpers.js";
import "../styles/explorer.css";

export const config = { mode: "app" };

interface QueryResult {
  columns: string[];
  rows: Record<string, any>[];
  row_count: number;
  error?: string;
}

export default function ExplorerPage() {
  const [sql, setSql] = useState("SELECT * FROM events LIMIT 20");
  const [tables, setTables] = useState<string[]>([]);
  const [result, setResult] = useState<QueryResult | null>(null);
  const [loading, setLoading] = useState(false);
  const [elapsed, setElapsed] = useState(0);
  const [sortCol, setSortCol] = useState<string | null>(null);
  const [sortAsc, setSortAsc] = useState(true);

  useEffect(() => {
    get<string[]>("/api/v1/query/tables")
      .then(t => setTables(t || []))
      .catch(() => setTables([]));
  }, []);

  const runQuery = async () => {
    if (!sql.trim()) return;
    setLoading(true);
    setResult(null);
    const start = performance.now();
    try {
      const data = await post<QueryResult>("/api/v1/query", { sql: sql.trim() });
      setElapsed(Math.round(performance.now() - start));
      setResult(data);
    } catch (err: any) {
      setElapsed(Math.round(performance.now() - start));
      setResult({ columns: [], rows: [], row_count: 0, error: err.message });
    } finally { setLoading(false); }
  };

  const handleKeyDown = (e: KeyboardEvent) => {
    if ((e.metaKey || e.ctrlKey) && e.key === "Enter") {
      e.preventDefault();
      runQuery();
    }
  };

  const insertTable = (name: string) => {
    setSql(prev => {
      if (prev.trim() === "" || prev === "SELECT * FROM events LIMIT 20") {
        return `SELECT * FROM ${name} LIMIT 20`;
      }
      return prev;
    });
  };

  const formatValue = (v: any): string => {
    if (v === null || v === undefined) return "NULL";
    if (typeof v === "object") return JSON.stringify(v);
    return String(v);
  };

  return (
    <div>
      <div class="obs-page-header">
        <h1 class="obs-page-title">SQL Explorer</h1>
      </div>

      <div class="explorer-layout">
        <div class="explorer-sidebar">
          <div class="explorer-sidebar-title">Tables</div>
          {tables.map(t => (
            <button key={t} class="explorer-table-item" onClick={() => insertTable(t)}>
              {t}
            </button>
          ))}
        </div>

        <div class="explorer-main">
          <textarea
            class="explorer-editor"
            value={sql}
            onInput={(e) => setSql((e.target as HTMLTextAreaElement).value)}
            onKeyDown={handleKeyDown}
            placeholder="SELECT * FROM events LIMIT 20"
            rows={5}
          />
          <div class="explorer-toolbar">
            <button class="obs-btn obs-btn--primary" onClick={runQuery} disabled={loading || !sql.trim()}>
              {loading ? "Running..." : "Run Query"}
            </button>
            <span class="explorer-meta" style={{ fontSize: "11px", color: "var(--obs-text-muted)" }}>
              Ctrl+Enter to run
            </span>
            {result && !result.error && (
              <span class="explorer-meta">
                {result.row_count} row{result.row_count !== 1 ? "s" : ""} in {elapsed}ms
              </span>
            )}
          </div>

          {result?.error && (
            <div class="explorer-error">{result.error}</div>
          )}

          {result && !result.error && result.columns.length > 0 && (
            <div class="explorer-results">
              <table class="explorer-table">
                <thead>
                  <tr>
                    {result.columns.map(col => (
                      <th key={col} style={{ cursor: "pointer", userSelect: "none" }}
                        onClick={() => {
                          if (sortCol === col) { setSortAsc(!sortAsc); }
                          else { setSortCol(col); setSortAsc(true); }
                        }}>
                        {col} {sortCol === col ? (sortAsc ? " ^" : " v") : ""}
                      </th>
                    ))}
                  </tr>
                </thead>
                <tbody>
                  {[...result.rows].sort((a, b) => {
                    if (!sortCol) return 0;
                    const av = formatValue(a[sortCol]);
                    const bv = formatValue(b[sortCol]);
                    const cmp = av.localeCompare(bv, undefined, { numeric: true });
                    return sortAsc ? cmp : -cmp;
                  }).map((row, i) => (
                    <tr key={i}>
                      {result.columns.map(col => (
                        <td key={col} title={formatValue(row[col])}>
                          {formatValue(row[col])}
                        </td>
                      ))}
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}

          {result && !result.error && result.row_count === 0 && (
            <div class="obs-empty-state">Query returned no results</div>
          )}
        </div>
      </div>
    </div>
  );
}
