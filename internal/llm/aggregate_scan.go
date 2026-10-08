package llm

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The owned boundary reads nullable wire text without the pinned SDK's
// NULL/empty/missing -> native-zero conversions. No aggregate is CAST to TEXT.
type aggregateRows interface {
	Next() bool
	Scan(...any) error
	FieldDescriptions() []pgconn.FieldDescription
	Err() error
	Close()
}

var aggregateInteger = regexp.MustCompile(`^[0-9]+$`)
var aggregateDecimal = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)

func (s *LLMService) aggregateQuery(ctx context.Context, sql string, args ...any) (aggregateRows, error) {
	if s.aggregateRead != nil {
		return s.aggregateRead(ctx, sql, args...)
	}
	return s.db.Pool().Query(ctx, sql, append([]any{pgx.QueryExecModeSimpleProtocol}, args...)...)
}

func queryAggregate[T any](ctx context.Context, s *LLMService, sql string, args ...any) ([]T, error) {
	rows, err := s.aggregateQuery(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("llm aggregate query: %w", err)
	}
	defer rows.Close()
	out := make([]T, 0)
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var r T
		if err := scanAggregateRow(rows, &r); err != nil {
			return nil, fmt.Errorf("llm aggregate row %d: %w", len(out), err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("llm aggregate rows: %w", err)
	}
	return out, ctx.Err()
}

func scanAggregateRow(rows aggregateRows, target any) error {
	fields := rows.FieldDescriptions()
	raw := make([]*string, len(fields))
	dest := make([]any, len(fields))
	for i := range raw {
		dest[i] = &raw[i]
	}
	if err := rows.Scan(dest...); err != nil {
		return err
	}
	byName := make(map[string]*string, len(fields))
	for i, f := range fields {
		if _, exists := byName[f.Name]; exists {
			return fmt.Errorf("duplicate aggregate column %q", f.Name)
		}
		byName[f.Name] = raw[i]
	}
	v := reflect.ValueOf(target).Elem()
	typ := v.Type()
	for i := 0; i < v.NumField(); i++ {
		name := typ.Field(i).Tag.Get("db")
		text, present := byName[name]
		if !present || text == nil {
			return fmt.Errorf("%s: required aggregate missing or NULL", name)
		}
		field := v.Field(i)
		switch field.Kind() {
		case reflect.String:
			field.SetString(*text) // identities may legitimately be empty
		case reflect.Int64:
			if !aggregateInteger.MatchString(*text) {
				return fmt.Errorf("%s: required nonnegative integer is blank or invalid", name)
			}
			n, err := strconv.ParseInt(*text, 10, 64)
			if err != nil {
				return fmt.Errorf("%s: integer outside supported int64 domain: %w", name, err)
			}
			field.SetInt(n)
		case reflect.Float64:
			if !aggregateDecimal.MatchString(*text) {
				return fmt.Errorf("%s: required nonnegative decimal is blank or invalid", name)
			}
			n, err := strconv.ParseFloat(*text, 64)
			if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
				return fmt.Errorf("%s: decimal outside supported finite domain", name)
			}
			field.SetFloat(n)
		default:
			return fmt.Errorf("%s: unsupported aggregate destination", name)
		}
	}
	return nil
}

// A zero-row aggregate is only a legitimate empty window if a separate raw
// existence read confirms emptiness. It is not a scanner/error fallback.
func (s *LLMService) confirmEmptyWindow(ctx context.Context, siteID string, fromMs, toMs any) error {
	rows, err := s.aggregateQuery(ctx, `SELECT trace_id FROM llm_traces WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3 LIMIT 1`, siteID, fromMs, toMs)
	if err != nil {
		return fmt.Errorf("llm empty-window verification: %w", err)
	}
	defer rows.Close()
	if rows.Next() {
		return fmt.Errorf("llm aggregate unavailable: missing result for nonempty window")
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("llm empty-window verification: %w", err)
	}
	return ctx.Err()
}

// This projection refuses invalid legacy numeric text even if SUM/COALESCE
// would hide NULL/blank or cancel negative values with positive values. Sources
// stay independent and are not validated as numbers or inferred from amounts.
const invalidLLMNumericsSQL = `COALESCE(SUM(CASE WHEN
 cost_usd ~ '^[0-9]+(\.[0-9]+)?$'
 AND prompt_tokens ~ '^[0-9]+$'
 AND completion_tokens ~ '^[0-9]+$'
 AND total_tokens ~ '^[0-9]+$'
 AND latency_ms ~ '^[0-9]+$'
 THEN 0 ELSE 1 END),0) AS invalid_numeric_rows,`
