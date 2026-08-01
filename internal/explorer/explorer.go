package explorer

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/neutron-dev/neutron-go/nucleus"
)

// A var, not a const, so tests can shrink it rather than waiting out the real
// duration to observe cancellation.
//
// OBS-017: Nucleus does not enforce READ ONLY transactions or GRANT-restricted
// roles (verified empirically — both silently accept a write inside a
// "read only" transaction and under a role granted only SELECT). pgx's
// context-based cancellation, by contrast, IS honored: a query run with a
// short context.WithTimeout is genuinely cancelled server-side. That makes a
// context deadline the only currently-real database-layer containment
// available here (alongside the existing lexer and row caps) — there is no
// dedicated least-privilege role or transaction-level read-only boundary to
// fall back on until Nucleus implements one.
var queryTimeout = 10 * time.Second

// maxConcurrentQueries bounds how many explorer queries can run at once, so a
// handful of expensive-but-lexer-legal queries (allowed functions, big scans)
// can't collectively starve ingestion or the dashboard.
const maxConcurrentQueries = 4

type ExplorerService struct {
	db  *nucleus.Client
	sem chan struct{}
}

func NewExplorerService(db *nucleus.Client) *ExplorerService {
	return &ExplorerService{db: db, sem: make(chan struct{}, maxConcurrentQueries)}
}

// acquire blocks for a free execution slot or until ctx is done, whichever
// comes first — a queued query still respects its own timeout instead of
// waiting indefinitely behind other queries.
func (s *ExplorerService) acquire(ctx context.Context) (func(), error) {
	select {
	case s.sem <- struct{}{}:
		return func() { <-s.sem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type QueryResult struct {
	Columns  []string         `json:"columns"`
	Rows     []map[string]any `json:"rows"`
	RowCount int              `json:"row_count"`
	Error    string           `json:"error,omitempty"`
}

// Execute runs a read-only SQL query and returns the results as JSON.
// Blocks write operations for safety.
func (s *ExplorerService) Execute(ctx context.Context, sql string) (*QueryResult, error) {
	kind, err := ClassifyReadOnlySQL(sql)
	if err != nil {
		return &QueryResult{Error: err.Error()}, nil
	}
	if kind == stmtSelect {
		// Always wrap in an outer LIMIT rather than substring-matching for an
		// existing LIMIT — a LIMIT inside a subquery does not bound the outer
		// result, so hasLimit() could leave a query effectively unbounded.
		sql = "SELECT * FROM (" + strings.TrimRight(strings.TrimSpace(sql), ";") + ") _q LIMIT 100"
	}

	release, err := s.acquire(ctx)
	if err != nil {
		return &QueryResult{Error: err.Error()}, nil
	}
	defer release()

	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

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

	// Scan rows. Hard cap as defense in depth so memory stays bounded even if
	// the outer LIMIT wrapper is ever bypassed.
	const maxScanRows = 1000
	var resultRows []map[string]any
	for rows.Next() {
		if len(resultRows) >= maxScanRows {
			break
		}
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

	release, err := s.acquire(ctx)
	if err != nil {
		return &QueryResult{Error: err.Error()}, nil
	}
	defer release()

	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

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

	// Same hard cap as Execute: an unbounded plan (e.g. a query joining many
	// tables) can otherwise accumulate an arbitrarily large response in memory.
	const maxScanRows = 1000
	var resultRows []map[string]any
	for rows.Next() {
		if len(resultRows) >= maxScanRows {
			break
		}
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
// ListTables returns the tables visible in the public schema. OBS-024: this
// used to swallow the query error (and every row-scan error) behind a
// hard-coded fallback list, and even overrode a genuinely empty-but-valid
// result with a second hard-coded list — a database outage or permission
// failure was indistinguishable from a normal response, and a real schema
// change could silently diverge from what the fallback claimed existed.
func (s *ExplorerService) ListTables(ctx context.Context) ([]string, error) {
	pool := s.db.Pool()
	rows, err := pool.Query(ctx, "SELECT table_name FROM information_schema.tables WHERE table_schema = 'public' ORDER BY table_name")
	if err != nil {
		return nil, fmt.Errorf("explorer: list tables: %w", err)
	}
	defer rows.Close()

	tables := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("explorer: scan table name: %w", err)
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("explorer: list tables: %w", err)
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
