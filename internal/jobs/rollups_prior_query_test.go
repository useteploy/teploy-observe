package jobs

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestSessionPriorQueryMatchesSessionsSchema guards the prod failure where the
// prior-summary read was derived from the events SELECT and asked sessions for
// a pathname column it has never had (SQLSTATE 42703 on every 5-minute tick).
// Every source column the query reads must exist in the sessions table as
// migration 033 (the current definition) creates it.
func TestSessionPriorQueryMatchesSessionsSchema(t *testing.T) {
	raw, err := os.ReadFile("../schema/migrations/033_collapse_rollup_duplicates.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	ddl := string(raw)
	start := strings.Index(ddl, "CREATE TABLE IF NOT EXISTS sessions (")
	if start < 0 {
		t.Fatal("sessions definition not found in migration 033")
	}
	body := ddl[start:]
	body = body[strings.Index(body, "(")+1 : strings.Index(body, ") WITH")]
	cols := map[string]bool{}
	for _, line := range strings.Split(body, "\n") {
		if f := strings.Fields(strings.TrimSpace(line)); len(f) > 0 {
			cols[f[0]] = true
		}
	}

	q := sessionPriorQuery[strings.Index(sessionPriorQuery, "SELECT")+len("SELECT") : strings.Index(sessionPriorQuery, "FROM sessions")]
	src := regexp.MustCompile(`^(?:COALESCE\()?([a-z_]+)`)
	for _, item := range strings.Split(q, ",\n") {
		for _, part := range strings.Split(strings.TrimSpace(item), ", ") {
			part = strings.TrimSpace(part)
			if part == "" || strings.HasPrefix(part, "'") || strings.HasPrefix(part, "0)") {
				continue
			}
			m := src.FindStringSubmatch(part)
			if m == nil {
				t.Fatalf("unparsed select item %q", part)
			}
			if !cols[m[1]] {
				t.Errorf("sessionPriorQuery reads %q, which sessions does not have", m[1])
			}
		}
	}
	if !cols["version"] {
		t.Error("sessions has no version column for ORDER BY")
	}
}
