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
