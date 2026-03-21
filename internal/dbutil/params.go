package dbutil

import "strconv"

// IntParam converts an int64 to a string for use as a pgwire query parameter.
// Nucleus pgwire expects text-encoded parameters; pgx cannot encode int64
// as text (OID 25). Passing the value as a string works because Nucleus
// casts it to BIGINT at evaluation time.
func IntParam(v int64) string {
	return strconv.FormatInt(v, 10)
}
