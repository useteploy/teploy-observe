package query

import (
	"context"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
)

type goalCountResult map[string]*string

func (s *StatsService) readGoalCounts(ctx context.Context, sql string, args ...any) ([]goalCountResult, error) {
	if s.goalCountRead != nil {
		return s.goalCountRead(ctx, sql, args...)
	}
	rows, err := s.db.Pool().Query(ctx, sql, append([]any{pgx.QueryExecModeSimpleProtocol}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]goalCountResult, 0, 1)
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		fields := rows.FieldDescriptions()
		raw := make([]*string, len(fields))
		dest := make([]any, len(fields))
		for i := range raw {
			dest[i] = &raw[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		r := make(goalCountResult, len(fields))
		for i, f := range fields {
			if _, duplicate := r[f.Name]; duplicate {
				return nil, fmt.Errorf("duplicate goal count column %q", f.Name)
			}
			r[f.Name] = raw[i]
		}
		out = append(out, r)
		if len(out) > 1 {
			return nil, fmt.Errorf("goal count query returned multiple aggregate rows")
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, ctx.Err()
}

// A scalar COUNT must supply exactly one non-NULL digit-only int64 text.
// Empty windows supply an explicit "0"; absence is never evidence of zero.
func strictGoalCount(rows []goalCountResult, field string) (int64, error) {
	if len(rows) != 1 {
		return 0, fmt.Errorf("%s: expected one count row, got %d", field, len(rows))
	}
	text, ok := rows[0][field]
	if !ok || text == nil || *text == "" {
		return 0, fmt.Errorf("%s: count missing, NULL or blank", field)
	}
	for _, c := range *text {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("%s: count must be a nonnegative integer", field)
		}
	}
	n, err := strconv.ParseInt(*text, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: count exceeds supported int64 domain: %w", field, err)
	}
	return n, nil
}
