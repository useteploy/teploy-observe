package explorer

import "testing"

// TestClassifyReadOnlySQL is the regression harness for the only guard on the
// authenticated /api/v1/query path. The banned-keyword set is defense-in-depth,
// not the primary gate, so do not loosen the head switch without adding cases.
func TestClassifyReadOnlySQL(t *testing.T) {
	accept := []string{
		"SELECT 1",
		"select * from events",
		"WITH t AS (SELECT 1) SELECT * FROM t",
		"EXPLAIN SELECT 1",
		"SHOW TABLES",
		"SELECT 'this contains the word DELETE inside a string'", // literal, not a keyword
		"SELECT 1;", // single trailing semicolon allowed
	}
	for _, q := range accept {
		if _, err := ClassifyReadOnlySQL(q); err != nil {
			t.Errorf("expected accept %q, got error %v", q, err)
		}
	}

	reject := []string{
		"INSERT INTO events VALUES (1)",
		"UPDATE events SET x = 1",
		"DELETE FROM events",
		"DROP TABLE events",
		"ALTER TABLE events ADD c INT",
		"CREATE TABLE x (a INT)",
		"TRUNCATE events",
		"GRANT ALL ON events TO x",
		"REVOKE ALL ON events FROM x",
		"COPY events TO '/tmp/x'",
		"MERGE INTO x",
		"CALL proc()",
		"EXECUTE stmt",
		"WITH t AS (SELECT 1) DELETE FROM events",     // buried in CTE
		"/* comment */ UPDATE events SET x = 1",        // after comment
		"EXPLAIN ANALYZE INSERT INTO events VALUES (1)", // after EXPLAIN
		"SELECT 1; DROP TABLE events",                   // stacked
		"",                                              // empty
	}
	for _, q := range reject {
		if _, err := ClassifyReadOnlySQL(q); err == nil {
			t.Errorf("expected reject %q, got no error", q)
		}
	}
}
