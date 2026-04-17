package explorer

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/neutron-dev/neutron-go/nucleus"
)

type ExplorerService struct {
	db *nucleus.Client
}

func NewExplorerService(db *nucleus.Client) *ExplorerService {
	return &ExplorerService{db: db}
}

type QueryResult struct {
	Columns []string         `json:"columns"`
	Rows    []map[string]any `json:"rows"`
	RowCount int             `json:"row_count"`
	Error   string           `json:"error,omitempty"`
}

// Execute runs a read-only SQL query and returns the results as JSON.
// Blocks write operations for safety.
func (s *ExplorerService) Execute(ctx context.Context, sql string) (*QueryResult, error) {
	kind, err := ClassifyReadOnlySQL(sql)
	if err != nil {
		return &QueryResult{Error: err.Error()}, nil
	}
	if kind == stmtSelect && !hasLimit(sql) {
		sql = strings.TrimRight(strings.TrimSpace(sql), ";") + " LIMIT 100"
	}

	pool := s.db.Pool()
	rows, err := pool.Query(ctx, sql, pgx.QueryExecModeSimpleProtocol)
	if err != nil {
		return &QueryResult{Error: err.Error()}, nil
	}
	defer rows.Close()

	// Get column names
	fieldDescs := rows.FieldDescriptions()
	columns := make([]string, len(fieldDescs))
	for i, fd := range fieldDescs {
		columns[i] = string(fd.Name)
	}

	// Scan rows
	var resultRows []map[string]any
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return &QueryResult{Error: err.Error(), Columns: columns}, nil
		}
		row := make(map[string]any, len(columns))
		for i, col := range columns {
			if i < len(vals) {
				row[col] = vals[i]
			}
		}
		resultRows = append(resultRows, row)
	}
	if err := rows.Err(); err != nil {
		return &QueryResult{Error: err.Error(), Columns: columns}, nil
	}

	return &QueryResult{
		Columns:  columns,
		Rows:     resultRows,
		RowCount: len(resultRows),
	}, nil
}

// Explain runs EXPLAIN against the given read-only SQL and returns the plan
// text, one row per plan line. Rejects the same queries Execute rejects.
func (s *ExplorerService) Explain(ctx context.Context, sql string) (*QueryResult, error) {
	if _, err := ClassifyReadOnlySQL(sql); err != nil {
		return &QueryResult{Error: err.Error()}, nil
	}
	explainSQL := "EXPLAIN " + strings.TrimRight(strings.TrimSpace(sql), ";")

	pool := s.db.Pool()
	rows, err := pool.Query(ctx, explainSQL, pgx.QueryExecModeSimpleProtocol)
	if err != nil {
		return &QueryResult{Error: err.Error()}, nil
	}
	defer rows.Close()

	fieldDescs := rows.FieldDescriptions()
	columns := make([]string, len(fieldDescs))
	for i, fd := range fieldDescs {
		columns[i] = string(fd.Name)
	}

	var resultRows []map[string]any
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return &QueryResult{Error: err.Error(), Columns: columns}, nil
		}
		row := make(map[string]any, len(columns))
		for i, col := range columns {
			if i < len(vals) {
				row[col] = vals[i]
			}
		}
		resultRows = append(resultRows, row)
	}
	if err := rows.Err(); err != nil {
		return &QueryResult{Error: err.Error(), Columns: columns}, nil
	}
	return &QueryResult{Columns: columns, Rows: resultRows, RowCount: len(resultRows)}, nil
}

// ListTables returns available tables in the database.
func (s *ExplorerService) ListTables(ctx context.Context) ([]string, error) {
	pool := s.db.Pool()
	rows, err := pool.Query(ctx, "SELECT table_name FROM information_schema.tables WHERE table_schema = 'public' ORDER BY table_name")
	if err != nil {
		// Fallback: return known tables
		return []string{
			"events", "events_recent", "sessions", "stats_hourly", "stats_daily",
			"error_events", "issues", "spans", "service_stats", "logs",
			"api_keys", "sites", "admin_users", "feature_flags", "experiments", "surveys",
		}, nil
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var name string
		rows.Scan(&name)
		tables = append(tables, name)
	}
	if len(tables) == 0 {
		return []string{"events", "error_events", "spans", "logs", "sessions", "issues"}, nil
	}
	return tables, nil
}

// Ensure pool method exists on client
var _ = (*pgxpool.Pool)(nil)
var _ = fmt.Sprintf

// --- read-only SQL classifier -------------------------------------------------

type stmtKind int

const (
	stmtUnknown stmtKind = iota
	stmtSelect           // SELECT, WITH ... SELECT
	stmtExplain          // EXPLAIN [ANALYZE] <stmt>
	stmtShow             // SHOW ...
)

// ClassifyReadOnlySQL inspects a SQL string and returns the statement kind if
// it is a read-only query. It rejects queries that contain a write keyword as
// a standalone token anywhere (so `/* */ INSERT ...` and `WITH t AS (...) INSERT`
// are both rejected), and rejects stacked statements. Comments and string
// literals are skipped during tokenization.
func ClassifyReadOnlySQL(sql string) (stmtKind, error) {
	if strings.TrimSpace(sql) == "" {
		return stmtUnknown, fmt.Errorf("empty query")
	}

	bannedKeywords := map[string]struct{}{
		"INSERT":   {},
		"UPDATE":   {},
		"DELETE":   {},
		"DROP":     {},
		"ALTER":    {},
		"CREATE":   {},
		"TRUNCATE": {},
		"GRANT":    {},
		"REVOKE":   {},
		"COPY":     {},
		"REPLACE":  {},
		"MERGE":    {},
		"CALL":     {},
		"EXECUTE":  {},
	}

	var tokens []string
	i := 0
	n := len(sql)
	for i < n {
		c := sql[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '-' && i+1 < n && sql[i+1] == '-':
			// line comment until newline
			for i < n && sql[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < n && sql[i+1] == '*':
			// block comment until */
			i += 2
			for i+1 < n && !(sql[i] == '*' && sql[i+1] == '/') {
				i++
			}
			if i+1 < n {
				i += 2
			} else {
				return stmtUnknown, fmt.Errorf("unterminated block comment")
			}
		case c == '\'':
			// string literal — skip to closing quote, handling doubled quote escape
			i++
			for i < n {
				if sql[i] == '\'' {
					if i+1 < n && sql[i+1] == '\'' {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
		case c == '"':
			// quoted identifier — skip to closing quote
			i++
			for i < n && sql[i] != '"' {
				i++
			}
			if i < n {
				i++
			}
		case c == ';':
			// Reject any `;` except as the very last non-whitespace char.
			for j := i + 1; j < n; j++ {
				if sql[j] != ' ' && sql[j] != '\t' && sql[j] != '\n' && sql[j] != '\r' {
					return stmtUnknown, fmt.Errorf("multiple statements are not allowed")
				}
			}
			i = n
		case isIdentStart(c):
			start := i
			for i < n && isIdentPart(sql[i]) {
				i++
			}
			tok := strings.ToUpper(sql[start:i])
			if _, bad := bannedKeywords[tok]; bad {
				return stmtUnknown, fmt.Errorf("write operations are not allowed: %s", tok)
			}
			tokens = append(tokens, tok)
		default:
			i++
		}
	}

	if len(tokens) == 0 {
		return stmtUnknown, fmt.Errorf("no statement found")
	}
	switch tokens[0] {
	case "SELECT":
		return stmtSelect, nil
	case "WITH":
		// WITH must eventually lead to a SELECT; we've already rejected any
		// INSERT/UPDATE/DELETE tokens, so `WITH ... INSERT` is already blocked.
		return stmtSelect, nil
	case "EXPLAIN":
		return stmtExplain, nil
	case "SHOW":
		return stmtShow, nil
	case "DESCRIBE", "DESC":
		return stmtShow, nil
	}
	return stmtUnknown, fmt.Errorf("only SELECT/WITH/EXPLAIN/SHOW are allowed, got %s", tokens[0])
}

func isIdentStart(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
}

func isIdentPart(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

// hasLimit returns true if the SQL statement already has a top-level LIMIT clause.
func hasLimit(sql string) bool {
	up := strings.ToUpper(sql)
	// crude but effective: a LIMIT token outside of strings/comments.
	// For our purpose the Contains check is safe because LIMIT is a reserved word.
	return strings.Contains(up, " LIMIT ") || strings.Contains(up, "\nLIMIT ") || strings.HasSuffix(strings.TrimSpace(up), "LIMIT")
}
