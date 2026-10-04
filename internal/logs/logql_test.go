package logs

import (
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestParseQueryGolden(t *testing.T) {
	cases := []struct{ in, want string }{
		{`error`, `message:"error"`},
		{`"connection refused"`, `message:"connection refused"`},
		{`foo bar`, `(AND message:"foo" message:"bar")`},
		{`foo AND bar`, `(AND message:"foo" message:"bar")`},
		{`foo OR bar baz`, `(OR message:"foo" (AND message:"bar" message:"baz"))`},
		{`(foo OR bar) baz`, `(AND (OR message:"foo" message:"bar") message:"baz")`},
		{`-foo`, `(NOT message:"foo")`},
		{`NOT foo`, `(NOT message:"foo")`},
		{`NOT -foo`, `(NOT (NOT message:"foo"))`},
		{`level:error service:api`, `(AND level:"error" service:"api")`},
		{`Level:ERROR`, `level:"ERROR"`},
		{`message:timeout`, `message:"timeout"`},
		{`trace_id:abc123 span_id:def`, `(AND trace_id:"abc123" span_id:"def")`},
		{`attr.http.status:>=500`, `attr.http.status>="500"`},
		{`attr.user:"jane doe"`, `attr.user:"jane doe"`},
		{`resource.service.name:api -attr.env:dev`, `(AND resource.service.name:"api" (NOT attr.env:"dev"))`},
		{`timestamp:>=2026-01-02`, `timestamp>="2026-01-02"`},
		{`timestamp:<2026-01-02T03:04:05Z`, `timestamp<"2026-01-02T03:04:05Z"`},
		{`foo-bar`, `message:"foo-bar"`},
		{`"a \"quoted\" b"`, `message:"a \"quoted\" b"`},
		{`and or not`, `(AND message:"and" message:"or" message:"not")`},
		{`level:"error"`, `level:"error"`},
		{"  a\tb\nc ", `(AND message:"a" message:"b" message:"c")`},
		{`ünïcode 日本語`, `(AND message:"ünïcode" message:"日本語")`},
	}
	for _, c := range cases {
		n, err := ParseQuery(c.in)
		if err != nil {
			t.Errorf("ParseQuery(%q): %v", c.in, err)
			continue
		}
		if got := n.String(); got != c.want {
			t.Errorf("ParseQuery(%q)\n got  %s\n want %s", c.in, got, c.want)
		}
	}
}

func TestParseQueryErrors(t *testing.T) {
	cases := []struct {
		in      string
		wantPos int
		wantMsg string
	}{
		{``, 0, "empty"},
		{`   `, 0, "empty"},
		{`foo AND`, 7, "unexpected end"},
		{`OR foo`, 0, "unexpected OR"},
		{`foo OR`, 6, "unexpected end"},
		{`(foo`, 0, "unclosed"},
		{`foo)`, 3, "unmatched"},
		{`()`, 0, "empty parentheses"},
		{`"abc`, 0, "unterminated"},
		{`foo "abc`, 4, "unterminated"},
		{`bogus:thing`, 0, "unknown field"},
		{`foo 10:30`, 4, "unknown field"},
		{`:x`, 0, "missing field"},
		{`level:`, 0, "missing value"},
		{`level:>error`, 0, "comparison operators only"},
		{`attr.:x`, 0, "invalid key"},
		{`attr.a'b:x`, 0, "invalid key"},
		{`attr.x:>abc`, 0, "numeric"},
		{`timestamp:yesterday`, 0, "invalid timestamp"},
		{`foo -`, 4, "dangling"},
		{"foo\x00bar", 3, "control"},
		{"foo \x01", 4, "control"},
		{"\xff\xfe", 0, "UTF-8"},
		{`level:""`, 0, "empty value"},
		{`""`, 0, "empty value"},
	}
	for _, c := range cases {
		_, err := ParseQuery(c.in)
		var qe *QueryError
		if !errors.As(err, &qe) {
			t.Errorf("ParseQuery(%q): want QueryError, got %v", c.in, err)
			continue
		}
		if qe.Pos != c.wantPos || !strings.Contains(qe.Msg, c.wantMsg) {
			t.Errorf("ParseQuery(%q): got pos=%d msg=%q, want pos=%d msg~%q", c.in, qe.Pos, qe.Msg, c.wantPos, c.wantMsg)
		}
	}
}

func TestParseQueryLimits(t *testing.T) {
	// length
	if _, err := ParseQuery(strings.Repeat(strings.Repeat("a", 100)+" ", 10) + strings.Repeat("a", 14)); err != nil {
		t.Errorf("query of exactly max length rejected: %v", err)
	}
	if _, err := ParseQuery(strings.Repeat(strings.Repeat("a", 100)+" ", 10) + strings.Repeat("a", 15)); err == nil {
		t.Error("over-long query accepted")
	}
	// terms
	ok := strings.TrimSpace(strings.Repeat("a ", MaxQueryTerms))
	if _, err := ParseQuery(ok); err != nil {
		t.Errorf("%d terms rejected: %v", MaxQueryTerms, err)
	}
	if _, err := ParseQuery(ok + " a"); err == nil {
		t.Error("21 terms accepted")
	}
	// parenthesis depth
	nest := func(d int) string { return strings.Repeat("(", d) + "a" + strings.Repeat(")", d) }
	if _, err := ParseQuery(nest(MaxQueryDepth)); err != nil {
		t.Errorf("depth %d rejected: %v", MaxQueryDepth, err)
	}
	if _, err := ParseQuery(nest(MaxQueryDepth + 1)); err == nil {
		t.Error("depth 6 accepted")
	}
	if _, err := ParseQuery(nest(500)); err == nil {
		t.Error("depth 500 accepted")
	}
	// stacked negation
	if _, err := ParseQuery(strings.Repeat("NOT ", MaxQueryDepth) + "a"); err != nil {
		t.Errorf("%d negations rejected: %v", MaxQueryDepth, err)
	}
	if _, err := ParseQuery(strings.Repeat("-", MaxQueryDepth+1) + "a"); err == nil {
		t.Error("6 stacked '-' accepted")
	}
	// key length
	longKey := strings.Repeat("k", MaxKeyLen)
	if _, err := ParseQuery("attr." + longKey + ":x"); err != nil {
		t.Errorf("128-char key rejected: %v", err)
	}
	if _, err := ParseQuery("attr." + longKey + "k:x"); err == nil {
		t.Error("129-char key accepted")
	}
	// value length
	if _, err := ParseQuery(strings.Repeat("v", maxValueRunes+1)); err == nil {
		t.Error("over-long value accepted")
	}
}

func TestQueryKeyRegex(t *testing.T) {
	for _, k := range []string{"a", "http.status_code", "a-b/c", "A9"} {
		if !queryKeyRe.MatchString(k) {
			t.Errorf("key %q should be valid", k)
		}
	}
	for _, k := range []string{"", "a b", "a'b", "a\"b", "a;b", "a\nb", "a\x00b", "a$b", "ключ", "a)b", "a\\b", "a%b"} {
		if queryKeyRe.MatchString(k) {
			t.Errorf("key %q should be invalid", k)
		}
	}
}

// injectionCorpus is hostile text a user could type as a term value.
var injectionCorpus = []string{
	`' OR 1=1 --`,
	`'; DROP TABLE logs; --`,
	`" OR ""="`,
	`\`,
	`\\`,
	`\'`,
	`%`,
	`_`,
	`%_%`,
	`100%`,
	`a_b`,
	`/* comment */`,
	`--`,
	`;`,
	`$1`,
	`$$`,
	`) OR (1=1`,
	`日本語' --`,
	`ＵＮＩＯＮ ＳＥＬＥＣＴ`,
	"tab\there",
	`\x00`,
	`' UNION SELECT attributes FROM logs --`,
	`\u0027`,
	`%27`,
}

// allowedSQL is every token the compiler may emit besides placeholders: the
// fixed column names, operators and the one '%' literal pair. User text must
// never appear in generated SQL.
var sqlShape = regexp.MustCompile(`^[A-Za-z_ ()$0-9=<>|%']*$`)

func compileFor(t *testing.T, q string) compiledQuery {
	t.Helper()
	ast, err := ParseQuery(q)
	if err != nil {
		t.Fatalf("ParseQuery(%q): %v", q, err)
	}
	return compileQuery(ast, 4)
}

func TestInjectionCorpusIsBoundNeverInterpolated(t *testing.T) {
	for _, v := range injectionCorpus {
		quoted := `"` + strings.ReplaceAll(strings.ReplaceAll(v, `\`, `\\`), `"`, `\"`) + `"`
		for _, q := range []string{
			quoted,
			`message:` + quoted,
			`service:` + quoted,
			`level:` + quoted,
			`trace_id:` + quoted,
			`span_id:` + quoted,
			`-` + quoted,
			`attr.k:` + quoted,
			`resource.k:` + quoted,
			quoted + ` OR ` + quoted,
		} {
			ast, err := ParseQuery(q)
			if err != nil {
				// A refusal is safe; none of these should be refused except
				// by the control-character rule, which is checked elsewhere.
				if !strings.Contains(v, "\x00") {
					t.Errorf("ParseQuery(%q): unexpected error %v", q, err)
				}
				continue
			}
			cq := compileQuery(ast, 4)
			if !sqlShape.MatchString(cq.where) {
				t.Errorf("query %q produced SQL with unexpected characters: %q", q, cq.where)
			}
			// No fragment of the user's text may be in the SQL text.
			for _, frag := range []string{"DROP", "UNION", "1=1", "comment", "--", ";", "\\", "日本語"} {
				if strings.Contains(v, frag) && strings.Contains(cq.where, frag) {
					t.Errorf("query %q leaked %q into SQL %q", q, frag, cq.where)
				}
			}
			// Placeholder count must match the bound parameter count.
			n := strings.Count(cq.where, "$")
			if n != len(cq.params) {
				t.Errorf("query %q: %d placeholders but %d params (%q)", q, n, len(cq.params), cq.where)
			}
		}
	}
}

func TestSQLColumnsAreFixed(t *testing.T) {
	cq := compileFor(t, `level:error service:api trace_id:t span_id:s message:m timestamp:>=2026-01-01 attr.x:1 resource.y:2`)
	for _, col := range []string{"level", "service_name", "trace_id", "span_id", "message", "timestamp"} {
		if !strings.Contains(cq.where, col) {
			t.Errorf("expected column %s in %q", col, cq.where)
		}
	}
	if strings.Contains(cq.where, "attributes") || strings.Contains(cq.where, "x") && strings.Contains(cq.where, "'x'") {
		t.Errorf("attribute terms must not reach SQL: %q", cq.where)
	}
	if cq.exact {
		t.Error("attribute terms must make the query non-exact (Go verification)")
	}
}

func TestSQLGolden(t *testing.T) {
	cases := []struct {
		q      string
		where  string
		params int
		exact  bool
	}{
		{`foo`, `message ILIKE '%' || $4 || '%'`, 1, true},
		{`-foo`, `NOT (message ILIKE '%' || $4 || '%')`, 1, true},
		{`foo OR service:api`, `(message ILIKE '%' || $4 || '%' OR service_name = $5)`, 2, true},
		{`foo attr.a:b`, `(message ILIKE '%' || $4 || '%')`, 1, false},
		{`foo OR attr.a:b`, ``, 0, false},
		{`-attr.a:b`, ``, 0, false},
		{`-(foo attr.a:b)`, ``, 0, false},
		{`-(foo OR attr.a:b)`, `(NOT (message ILIKE '%' || $4 || '%'))`, 1, false},
		{`timestamp:>=2026-01-01`, `timestamp >= CAST($4 AS BIGINT)`, 1, true},
		{`100%`, `message ILIKE '%' || $4 || '%'`, 1, false},
		{`%`, ``, 0, false},
		{`-100%`, ``, 0, false},
		{`level:error`, `(level = $4 OR level = $5)`, 2, true},
	}
	for _, c := range cases {
		cq := compileFor(t, c.q)
		if cq.where != c.where || len(cq.params) != c.params || cq.exact != c.exact {
			t.Errorf("%q:\n got where=%q params=%d exact=%v\nwant where=%q params=%d exact=%v",
				c.q, cq.where, len(cq.params), cq.exact, c.where, c.params, c.exact)
		}
	}
}

func TestLikeMetacharsAreNeverInPattern(t *testing.T) {
	cq := compileFor(t, `"50%_off\\" tail`)
	for _, p := range cq.params {
		s, _ := p.(string)
		if strings.ContainsAny(s, likeMeta) {
			t.Errorf("LIKE pattern parameter %q still has metacharacters", s)
		}
	}
	if cq.exact {
		t.Error("a value with LIKE metacharacters must be verified in Go")
	}
}

func TestEvalAttributes(t *testing.T) {
	row := func(attrs string) *rowView {
		l := &Log{Message: "Payment FAILED for user", Level: "error", ServiceName: "api", Attributes: attrs,
			Timestamp: time.UnixMilli(1_700_000_000_000)}
		return &rowView{l: l, tsMs: l.Timestamp.UnixMilli()}
	}
	attrs := `{"http.status":500,"user":"jane","ok":false,"nested":{"a":1},"ratio":"0.5"}`
	cases := []struct {
		q    string
		want bool
	}{
		{`attr.user:jane`, true},
		{`attr.user:JANE`, false},
		{`attr.user:bob`, false},
		{`-attr.user:bob`, true},
		{`attr.http.status:500`, true},
		{`attr.http.status:>=500`, true},
		{`attr.http.status:>500`, false},
		{`attr.http.status:<600`, true},
		{`attr.ratio:<1`, true},
		{`attr.user:>1`, false},
		{`attr.ok:false`, true},
		{`attr.missing:x`, false},
		{`-attr.missing:x`, true},
		{`resource.user:jane`, true},
		{`payment attr.user:jane`, true},
		{`payment OR attr.user:bob`, true},
		{`"FAILED for" level:error`, true},
		{`message:failed`, true},
		{`timestamp:>=1700000000000`, true},
		{`timestamp:<1700000000000`, false},
		{`50%`, false},
	}
	for _, c := range cases {
		ast, err := ParseQuery(c.q)
		if err != nil {
			t.Fatalf("%q: %v", c.q, err)
		}
		if got := evalQuery(ast, row(attrs)); got != c.want {
			t.Errorf("eval %q = %v, want %v", c.q, got, c.want)
		}
	}
	// Corrupt / hostile attribute blobs never panic and never match.
	for _, bad := range []string{``, `null`, `not json`, `[1,2]`, `"str"`, `{"a":`, `"{\"user\":\"jane\"}"`} {
		ast, _ := ParseQuery(`attr.user:jane`)
		got := evalQuery(ast, row(bad))
		if got != (bad == `"{\"user\":\"jane\"}"`) {
			t.Errorf("attributes %q: match=%v", bad, got)
		}
	}
}

func TestPageResult(t *testing.T) {
	mk := func(n int) []Log {
		rows := make([]Log, n)
		for i := range rows {
			rows[i] = Log{LogID: "id" + string(rune('a'+i)), Timestamp: time.UnixMilli(int64(1000 - i)), Attributes: `{"k":"v"}`}
			if i%2 == 1 {
				rows[i].Attributes = `{"k":"x"}`
			}
		}
		return rows
	}
	exact := compileFor(t, `foo`)
	r := pageResult(mk(4), exact, 3, 4)
	if len(r.Logs) != 3 || r.NextCursor == "" || r.Truncated || r.Verification != "sql" {
		t.Errorf("exact page: %+v", r)
	}
	r = pageResult(mk(3), exact, 3, 4)
	if len(r.Logs) != 3 || r.NextCursor != "" {
		t.Errorf("exact last page: %+v", r)
	}

	attr := compileFor(t, `attr.k:v`)
	// 6 candidates, matches at index 0,2,4; page of 2 stops at index 2.
	r = pageResult(mk(6), attr, 2, 100)
	if len(r.Logs) != 2 || r.NextCursor != "998.idc" || r.Truncated || r.Verification != "go_window" {
		t.Errorf("verified page: %+v", r)
	}
	// Window exhausted (fetch == len) without filling the page.
	r = pageResult(mk(6), attr, 10, 6)
	if len(r.Logs) != 3 || !r.Truncated || r.NextCursor == "" {
		t.Errorf("truncated page: %+v", r)
	}
	// Fewer candidates than the window: complete, not truncated.
	r = pageResult(mk(6), attr, 10, 100)
	if len(r.Logs) != 3 || r.Truncated || r.NextCursor != "" {
		t.Errorf("complete page: %+v", r)
	}
}

func TestCursorShape(t *testing.T) {
	for _, c := range []string{"1700000000000.abcdef0123", "0.a", "1.A-_z"} {
		if !cursorRe.MatchString(c) {
			t.Errorf("cursor %q should be valid", c)
		}
	}
	for _, c := range []string{"", "x", "1700.", ".abc", "1;DROP.abc", "1.a b", "1.a'b", "-1.a", "1.a\n", "12345678901234567.a"} {
		if cursorRe.MatchString(c) {
			t.Errorf("cursor %q should be invalid", c)
		}
	}
}

// TestLegacySearchQueryUnchanged pins the plain (no lq) search SQL byte for
// byte: adding the query language must not alter it.
func TestLegacySearchQueryUnchanged(t *testing.T) {
	q, params := legacySearchQuery("site1", "10", "20", "error", "api", "boom", 50, 100)
	want := `SELECT log_id, tenant_id, site_id,
			CAST(timestamp AS TEXT) AS timestamp,
			level, message, service_name,
			COALESCE(trace_id, '') AS trace_id,
			COALESCE(span_id, '') AS span_id,
			COALESCE(attributes, '') AS attributes
		 FROM logs
		 WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3 AND level = $4 AND service_name = $5 AND message ILIKE '%' || $6 || '%'
		 ORDER BY timestamp DESC
		 LIMIT 50 OFFSET 100`
	if q != want {
		t.Errorf("legacy SQL changed:\n got  %q\n want %q", q, want)
	}
	if len(params) != 6 || params[0] != "site1" || params[5] != "boom" {
		t.Errorf("legacy params changed: %v", params)
	}
	q, params = legacySearchQuery("s", "1", "2", "", "", "", 50, 0)
	if !strings.Contains(q, "WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3\n") || len(params) != 3 {
		t.Errorf("legacy no-filter SQL changed: %q %v", q, params)
	}
}

func TestParseQueryParamBlank(t *testing.T) {
	for _, b := range []string{"", "   ", "\t\n"} {
		n, ok, err := ParseQueryParam(b)
		if n != nil || ok || err != nil {
			t.Errorf("blank %q: %v %v %v", b, n, ok, err)
		}
	}
}

// TestHostileInputsStayBounded runs inputs designed to blow up a naive
// parser: they must return (error or AST) promptly without panicking.
func TestHostileInputsStayBounded(t *testing.T) {
	inputs := []string{
		strings.Repeat("(", 100000),
		strings.Repeat(")", 100000),
		strings.Repeat("-", 100000),
		strings.Repeat("NOT ", 100000),
		strings.Repeat("a OR ", 100000),
		strings.Repeat(`"`, 100000),
		strings.Repeat(`\`, 100000),
		strings.Repeat("a:", 100000),
		strings.Repeat("\x00", 100000),
		strings.Repeat("日", 100000),
		"attr." + strings.Repeat("k", 100000) + ":x",
		strings.Repeat("(a ", 1000),
	}
	start := time.Now()
	for _, in := range inputs {
		if n, err := ParseQuery(in); err == nil {
			_ = compileQuery(n, 1)
		}
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("hostile inputs took %v", d)
	}
}

func FuzzParseQuery(f *testing.F) {
	seeds := append([]string{
		`foo bar`, `(a OR b) -c`, `level:error`, `attr.x:>=5`, `"a b"`, `NOT (a AND b)`,
		`timestamp:>2026-01-01`, `a:`, `((((((a))))))`, "a\x00", `-"x"`, `attr.:`,
	}, injectionCorpus...)
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		n, err := ParseQuery(in)
		if err != nil {
			var qe *QueryError
			if !errors.As(err, &qe) || qe.Pos < 0 {
				t.Fatalf("non-QueryError or bad position for %q: %v", in, err)
			}
			return
		}
		if len(in) > MaxQueryLen {
			t.Fatalf("accepted %d-byte query", len(in))
		}
		cq := compileQuery(n, 4)
		if !sqlShape.MatchString(cq.where) {
			t.Fatalf("SQL for %q has unexpected characters: %q", in, cq.where)
		}
		if strings.Count(cq.where, "$") != len(cq.params) {
			t.Fatalf("placeholder/param mismatch for %q: %q %v", in, cq.where, cq.params)
		}
		// Evaluation must never panic.
		l := &Log{Message: in, Attributes: in}
		_ = evalQuery(n, &rowView{l: l})
	})
}
