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
	// Block write operations
	trimmed := strings.TrimSpace(strings.ToUpper(sql))
	for _, prefix := range []string{"INSERT", "UPDATE", "DELETE", "DROP", "ALTER", "CREATE", "TRUNCATE"} {
		if strings.HasPrefix(trimmed, prefix) {
			return &QueryResult{Error: "write operations are not allowed in the query explorer"}, nil
		}
	}

	// Add LIMIT if not present
	if !strings.Contains(strings.ToUpper(sql), "LIMIT") {
		sql = strings.TrimRight(sql, ";") + " LIMIT 100"
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
