package dbutil

import "strconv"

// IntParam converts an int64 to a string for use as a pgwire query parameter.
// Under neutron-go SimpleProtocol, parameters become text literals (quoted)
// in the query string. Callers compare these against BIGINT columns in SQL
// using `CAST($n AS BIGINT)` to force numeric comparison.
func IntParam(v int64) string {
	return strconv.FormatInt(v, 10)
}
