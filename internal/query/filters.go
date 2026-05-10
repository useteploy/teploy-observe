package query

import "fmt"

// FilterBuilder generates dynamic WHERE clauses from filter parameters.
// Parameter numbering starts at startIdx to avoid collisions with the
// base query parameters ($1=site_id, $2=from, $3=to).
type FilterBuilder struct {
	clauses []string
	params  []any
	nextIdx int
}

// NewFilterBuilder creates a FilterBuilder starting parameter numbering at startIdx.
func NewFilterBuilder(startIdx int) *FilterBuilder {
	return &FilterBuilder{
		nextIdx: startIdx,
	}
}

// Add appends a filter clause for the given column if value is non-empty.
// Values are passed as strings (Nucleus quirk).
func (fb *FilterBuilder) Add(column, value string) {
	if value == "" {
		return
	}
	fb.clauses = append(fb.clauses, fmt.Sprintf("%s = $%d", column, fb.nextIdx))
	fb.params = append(fb.params, value)
	fb.nextIdx++
}

// AddIn appends a `column IN (...)` clause. Used by cohort filtering
// where a single filter expands into N distinct_id parameters. Empty
// values slice short-circuits to a clause that matches no rows so
// downstream queries don't have to special-case "cohort with zero
// members" — they just return zero rows, which is the right answer.
//
// Each value gets its own $N placeholder so SimpleProtocol can quote
// each one separately. Caller is responsible for column-name safety
// (only call with hard-coded column names — never user-supplied).
func (fb *FilterBuilder) AddIn(column string, values []string) {
	if len(values) == 0 {
		// "1 = 0" is the standard "match nothing" sentinel — preserves
		// the AND-chain shape so SQL() output stays well-formed.
		fb.clauses = append(fb.clauses, "1 = 0")
		return
	}
	placeholders := make([]string, len(values))
	for i, v := range values {
		placeholders[i] = fmt.Sprintf("$%d", fb.nextIdx)
		fb.params = append(fb.params, v)
		fb.nextIdx++
	}
	fb.clauses = append(fb.clauses, fmt.Sprintf("%s IN (%s)", column, joinComma(placeholders)))
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

// SQL returns the combined filter clauses as " AND col = $N AND col2 = $M"
// or an empty string if no filters were added.
func (fb *FilterBuilder) SQL() string {
	if len(fb.clauses) == 0 {
		return ""
	}
	result := ""
	for _, c := range fb.clauses {
		result += " AND " + c
	}
	return result
}

// Params returns the parameter values for the filter clauses.
func (fb *FilterBuilder) Params() []any {
	return fb.params
}

// NextIdx returns the next available parameter index.
func (fb *FilterBuilder) NextIdx() int {
	return fb.nextIdx
}
