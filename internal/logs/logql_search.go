package logs

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
)

// Query-language search limits.
const (
	// MaxQLLimit caps the page size of a query-language search.
	MaxQLLimit = 200
	// DefaultQLLimit is the page size when none is given.
	DefaultQLLimit = 50
	// qlCandidateCap bounds how many rows the Go-side verification path
	// reads from SQL per request (further capped by the O12 row budget).
	qlCandidateCap = 5000
)

var cursorRe = regexp.MustCompile(`^([0-9]{1,16})\.([A-Za-z0-9_-]{1,64})$`)

// ErrBadCursor reports a malformed pagination cursor.
var ErrBadCursor = fmt.Errorf("invalid cursor")

// QLSearchResult is the response of a query-language search.
//
// Verification is "sql" when the database evaluated the whole query, and
// "go_window" when attribute (or LIKE-metacharacter) terms were checked in Go
// over a bounded window of SQL-narrowed candidates. In that mode Truncated
// means the whole candidate window was scanned without filling the page, so
// older matches may exist beyond it: follow NextCursor to keep scanning.
type QLSearchResult struct {
	Logs         []Log  `json:"logs"`
	NextCursor   string `json:"next_cursor,omitempty"`
	Truncated    bool   `json:"truncated"`
	Verification string `json:"verification"`
}

// SearchLogsQL runs a parsed log query, ANDed with the optional level and
// service filters, newest first, with keyset pagination on (timestamp,
// log_id). There is no OFFSET, so a deep page costs the same as the first.
//
// UNVERIFIED against live Nucleus: the generated SQL (ILIKE, NOT (...), OR,
// the (timestamp, log_id) keyset comparison) is only compile- and unit-tested.
func (s *LogService) SearchLogsQL(ctx context.Context, siteID string, from, to time.Time, level, service string, ast *Node, limit int, cursor string) (*QLSearchResult, error) {
	if limit <= 0 {
		limit = DefaultQLLimit
	}
	if limit > MaxQLLimit {
		limit = MaxQLLimit
	}
	if int64(limit) > s.guard.MaxRows() {
		return nil, s.guard.RowRefusal()
	}

	where := "site_id = $1 AND timestamp >= $2 AND timestamp < $3"
	params := []any{siteID, dbutil.IntParam(from.UnixMilli()), dbutil.IntParam(to.UnixMilli())}
	idx := 4
	if level != "" {
		where += fmt.Sprintf(" AND level = $%d", idx)
		params = append(params, level)
		idx++
	}
	if service != "" {
		where += fmt.Sprintf(" AND service_name = $%d", idx)
		params = append(params, service)
		idx++
	}
	if cursor != "" {
		m := cursorRe.FindStringSubmatch(cursor)
		if m == nil {
			return nil, ErrBadCursor
		}
		where += fmt.Sprintf(" AND (timestamp < CAST($%d AS BIGINT) OR (timestamp = CAST($%d AS BIGINT) AND log_id < $%d))", idx, idx, idx+1)
		params = append(params, m[1], m[2])
		idx += 2
	}
	cq := compileQuery(ast, idx)
	if cq.where != "" {
		where += " AND " + cq.where
		params = append(params, cq.params...)
	}

	// Rows to read: one page plus a lookahead row when SQL decides the whole
	// query, a bounded candidate window when Go has to verify.
	fetch := int64(limit) + 1
	if !cq.exact {
		fetch = qlCandidateCap
		if m := s.guard.MaxRows(); m < fetch {
			fetch = m
		}
		if fetch < int64(limit) {
			fetch = int64(limit)
		}
	}

	qctx, release, err := s.guard.Begin(ctx, siteID)
	if err != nil {
		return nil, err
	}
	defer release()

	query := `SELECT log_id, tenant_id, site_id,
			CAST(timestamp AS TEXT) AS timestamp,
			level, message, service_name,
			COALESCE(trace_id, '') AS trace_id,
			COALESCE(span_id, '') AS span_id,
			COALESCE(attributes, '') AS attributes
		 FROM logs
		 WHERE ` + where + `
		 ORDER BY timestamp DESC, log_id DESC
		 LIMIT ` + strconv.FormatInt(fetch, 10)

	rows, err := nucleus.Query[Log](qctx, s.db.SQL(), query, params...)
	if err != nil {
		return nil, s.guard.DeadlineError(ctx, err)
	}
	return pageResult(rows, cq, limit, fetch), nil
}

func rowCursor(l *Log) string {
	return strconv.FormatInt(l.Timestamp.UnixMilli(), 10) + "." + l.LogID
}

// pageResult turns the fetched rows into one page. It is pure so the paging
// and verification rules can be tested without a database.
func pageResult(rows []Log, cq compiledQuery, limit int, fetch int64) *QLSearchResult {
	res := &QLSearchResult{Logs: []Log{}, Verification: "sql"}
	if cq.exact {
		if len(rows) > limit {
			rows = rows[:limit]
			res.NextCursor = rowCursor(&rows[limit-1])
		}
		res.Logs = append(res.Logs, rows...)
		return res
	}

	res.Verification = "go_window"
	for i := range rows {
		rv := &rowView{l: &rows[i], tsMs: rows[i].Timestamp.UnixMilli()}
		if !evalQuery(cq.root, rv) {
			continue
		}
		res.Logs = append(res.Logs, rows[i])
		if len(res.Logs) == limit {
			if i+1 < len(rows) || int64(len(rows)) >= fetch {
				res.NextCursor = rowCursor(&rows[i])
			}
			return res
		}
	}
	if int64(len(rows)) >= fetch && len(rows) > 0 {
		// The window was exhausted without filling the page.
		res.Truncated = true
		res.NextCursor = rowCursor(&rows[len(rows)-1])
	}
	return res
}

// ParseQueryParam parses the lq parameter; ok is false for a blank value
// (no query).
func ParseQueryParam(raw string) (ast *Node, ok bool, err error) {
	if strings.TrimSpace(raw) == "" {
		return nil, false, nil
	}
	ast, err = ParseQuery(raw)
	return ast, err == nil, err
}
