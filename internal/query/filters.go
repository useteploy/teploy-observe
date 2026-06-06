package query

import "fmt"

// FilterBuilder generates dynamic WHERE clauses from filter parameters.
// Parameter numbering starts at startIdx to avoid collisions with the
// base query parameters ($1=site_id, $2=from, $3=to).
//
// Filters are stored as structured entries (not pre-rendered strings) so the
// builder can re-render with correct param numbering and produce a column-scoped
// subset for tables that lack some columns (e.g. the sessions table has no
// distinct_id/pathname).
type FilterBuilder struct {
	startIdx int
	entries  []filterEntry
}

type filterEntry struct {
	column string
	op     string   // "eq" or "in"
	values []string // 1 for eq; 0..N for in (0 = match-nothing sentinel)
}

// NewFilterBuilder creates a FilterBuilder starting parameter numbering at startIdx.
func NewFilterBuilder(startIdx int) *FilterBuilder {
	return &FilterBuilder{startIdx: startIdx}
}

// Add appends a `column = value` filter if value is non-empty.
// Values are passed as strings (Nucleus quirk).
func (fb *FilterBuilder) Add(column, value string) {
	if value == "" {
		return
	}
	fb.entries = append(fb.entries, filterEntry{column: column, op: "eq", values: []string{value}})
}

// AddIn appends a `column IN (...)` clause. Used by cohort filtering where a
// single filter expands into N distinct_id parameters. An empty values slice
// becomes a match-nothing sentinel so a zero-member cohort returns zero rows.
// Caller is responsible for column-name safety (hard-coded names only).
func (fb *FilterBuilder) AddIn(column string, values []string) {
	fb.entries = append(fb.entries, filterEntry{column: column, op: "in", values: values})
}

// SQL returns the combined filter clauses as " AND ...", numbering params from
// startIdx, or an empty string if there are no filters.
func (fb *FilterBuilder) SQL() string {
	if fb == nil || len(fb.entries) == 0 {
		return ""
	}
	idx := fb.startIdx
	result := ""
	for _, e := range fb.entries {
		switch e.op {
		case "in":
			if len(e.values) == 0 {
				result += " AND 1 = 0"
				continue
			}
			ph := make([]string, len(e.values))
			for i := range e.values {
				ph[i] = fmt.Sprintf("$%d", idx)
				idx++
			}
			result += fmt.Sprintf(" AND %s IN (%s)", e.column, joinComma(ph))
		default: // eq
			result += fmt.Sprintf(" AND %s = $%d", e.column, idx)
			idx++
		}
	}
	return result
}

// Params returns the parameter values for the filter clauses, in SQL() order.
func (fb *FilterBuilder) Params() []any {
	if fb == nil {
		return nil
	}
	var params []any
	for _, e := range fb.entries {
		for _, v := range e.values {
			params = append(params, v)
		}
	}
	return params
}

// NextIdx returns the next available parameter index after all filter params.
func (fb *FilterBuilder) NextIdx() int {
	if fb == nil {
		return 0
	}
	return fb.startIdx + len(fb.Params())
}

// Columns returns the distinct column names referenced by the filters.
func (fb *FilterBuilder) Columns() []string {
	if fb == nil {
		return nil
	}
	cols := make([]string, 0, len(fb.entries))
	for _, e := range fb.entries {
		cols = append(cols, e.column)
	}
	return cols
}

// ReferencesColumnsOutside reports whether any referenced filter column is NOT
// in the given available-column set. Used to force a query onto the raw events
// table when a rollup table lacks a column the filter needs (e.g. distinct_id
// for cohorts, or language) — otherwise the filter silently breaks or zeros out
// the result for ranges that would normally use a rollup table.
func (fb *FilterBuilder) ReferencesColumnsOutside(available map[string]bool) bool {
	if fb == nil {
		return false
	}
	for _, e := range fb.entries {
		if !available[e.column] {
			return true
		}
	}
	return false
}

// Subset returns a new FilterBuilder (same startIdx) keeping only the filters
// whose column is in allowed. Used to apply filters to a table that lacks some
// columns (e.g. sessions has no distinct_id/pathname) without breaking param
// numbering or erroring on a missing column.
func (fb *FilterBuilder) Subset(allowed map[string]bool) *FilterBuilder {
	if fb == nil {
		return nil
	}
	sub := &FilterBuilder{startIdx: fb.startIdx}
	for _, e := range fb.entries {
		if allowed[e.column] {
			sub.entries = append(sub.entries, e)
		}
	}
	return sub
}

// joinComma is strings.Join hand-rolled to keep this file dependency-free.
func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}
