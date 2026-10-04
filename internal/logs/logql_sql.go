package logs

import (
	"bytes"
	"encoding/json"
	"math"
	"strconv"
	"strings"

	"github.com/useteploy/teploy-observe/internal/dbutil"
)

// compiledQuery is the SQL side of a parsed query.
//
// Column names come from the fixed switch in leafSQL and nowhere else; every
// user value is a bound parameter. attr./resource. terms cannot be expressed
// as bound SQL (the logs.attributes column is JSONB whose keys Nucleus will
// not take as bind parameters, and this repo's only precedent for
// interpolating a key is the validated-regex one in internal/query/goals.go)
// so they are never sent to SQL at all: the SQL fragment is a SUPERSET of the
// rows that can match, and verify (Go) decides the rest.
type compiledQuery struct {
	where  string // "" when the query adds no SQL constraint
	params []any
	// exact reports that the SQL fragment is the whole query: no row it
	// returns needs Go-side re-checking. False whenever a term was dropped
	// from, or loosened in, the SQL (attribute terms, LIKE metacharacters).
	exact bool
	root  *Node
}

type sqlGen struct {
	start  int // first free $N
	params []any
	exact  bool
}

func (g *sqlGen) bind(v any) string {
	g.params = append(g.params, v)
	return "$" + strconv.Itoa(g.start+len(g.params)-1)
}

// compileQuery builds the WHERE fragment for ast. firstParam is the number of
// the first free placeholder.
//
// Negation is pushed down to the leaves first (De Morgan, done inline by the
// neg flag), because "superset of matches" only composes that way: a dropped
// term under an odd number of NOTs would otherwise turn a superset into a
// subset and silently lose rows. A leaf that cannot be expressed contributes
// no constraint; an OR with such a branch contributes none at all.
func compileQuery(ast *Node, firstParam int) compiledQuery {
	g := &sqlGen{start: firstParam, exact: true}
	frag, ok := g.gen(ast, false)
	cq := compiledQuery{params: g.params, exact: g.exact, root: ast}
	if ok {
		cq.where = frag
	}
	return cq
}

func (g *sqlGen) gen(n *Node, neg bool) (string, bool) {
	switch n.Kind {
	case NodeTerm:
		return g.leafSQL(n.Term, neg)
	case NodeNot:
		return g.gen(n.Kids[0], !neg)
	}
	conj := (n.Kind == NodeAnd) != neg // AND, or OR under negation
	mark := len(g.params)
	var parts []string
	dropped := false
	for _, k := range n.Kids {
		f, ok := g.gen(k, neg)
		if !ok {
			dropped = true
			continue
		}
		parts = append(parts, f)
	}
	if conj {
		if dropped {
			g.exact = false
		}
		if len(parts) == 0 {
			return "", false
		}
		return "(" + strings.Join(parts, " AND ") + ")", true
	}
	if dropped {
		// The whole disjunction is unconstrained, so the parameters its
		// satisfied branches bound have no placeholder left: unbind them.
		g.params = g.params[:mark]
		g.exact = false
		return "", false
	}
	return "(" + strings.Join(parts, " OR ") + ")", true
}

// likeMeta are the characters that mean something inside a LIKE pattern
// (and the escape character itself).
const likeMeta = `%_\`

// longestClean returns the longest run of s containing no LIKE metacharacter.
func longestClean(s string) string {
	best, cur := "", ""
	flush := func() {
		if len(cur) > len(best) {
			best = cur
		}
		cur = ""
	}
	for _, r := range s {
		if strings.ContainsRune(likeMeta, r) {
			flush()
			continue
		}
		cur += string(r)
	}
	flush()
	return best
}

// titleCase upper-cases the first byte and lower-cases the rest.
func titleCase(v string) string {
	if v == "" {
		return v
	}
	l := strings.ToLower(v)
	return strings.ToUpper(l[:1]) + l[1:]
}

// levelVariants are the spellings a level term matches. Ingest stores the
// level as sent (OTLP lower-cases it, the JSON API does not) and the SQL here
// avoids LOWER() since no other query in this repo relies on it, so the
// as-typed, lower, upper and Title spellings are matched as bound equalities.
func levelVariants(v string) []string {
	out := []string{v}
	for _, c := range []string{strings.ToLower(v), strings.ToUpper(v), titleCase(v)} {
		dup := false
		for _, o := range out {
			dup = dup || o == c
		}
		if !dup {
			out = append(out, c)
		}
	}
	return out
}

func (g *sqlGen) leafSQL(t *Term, neg bool) (string, bool) {
	not := func(s string) string {
		if neg {
			return "NOT (" + s + ")"
		}
		return s
	}
	switch t.Field {
	case FieldLevel:
		var ors []string
		for _, v := range levelVariants(t.Value) {
			ors = append(ors, "level = "+g.bind(v))
		}
		return not("(" + strings.Join(ors, " OR ") + ")"), true
	case FieldService:
		return not("service_name = " + g.bind(t.Value)), true
	case FieldTraceID:
		return not("trace_id = " + g.bind(t.Value)), true
	case FieldSpanID:
		return not("span_id = " + g.bind(t.Value)), true
	case FieldTimestamp:
		p := "CAST(" + g.bind(dbutil.IntParam(t.TsMs)) + " AS BIGINT)"
		op := "="
		switch t.Op {
		case OpGT:
			op = ">"
		case OpGTE:
			op = ">="
		case OpLT:
			op = "<"
		case OpLTE:
			op = "<="
		}
		return not("timestamp " + op + " " + p), true
	case FieldMessage:
		if !strings.ContainsAny(t.Value, likeMeta) {
			return not("message ILIKE '%' || " + g.bind(t.Value) + " || '%'"), true
		}
		// The value has LIKE metacharacters, and whether this engine honours
		// a backslash escape is unverified, so none is relied on: the SQL
		// only asks for the longest metacharacter-free piece (a superset of
		// the true matches) and Go does the exact comparison.
		g.exact = false
		seg := longestClean(t.Value)
		if neg || seg == "" {
			return "", false
		}
		return "message ILIKE '%' || " + g.bind(seg) + " || '%'", true
	}
	// attr. / resource.: Go-side only.
	g.exact = false
	return "", false
}

// ---------------------------------------------------------------- Go-side evaluation

const maxAttrBytes = 1 << 20

// rowView is the part of a stored log the evaluator needs.
type rowView struct {
	l     *Log
	tsMs  int64
	attrs map[string]any
	done  bool
}

func (r *rowView) attributes() map[string]any {
	if r.done {
		return r.attrs
	}
	r.done = true
	raw := strings.TrimSpace(r.l.Attributes)
	if raw == "" || raw == "null" || len(raw) > maxAttrBytes {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil
	}
	// Tolerate an object that was stored as a JSON string of an object.
	if s, ok := v.(string); ok {
		dec = json.NewDecoder(bytes.NewReader([]byte(s)))
		dec.UseNumber()
		v = nil
		if err := dec.Decode(&v); err != nil {
			return nil
		}
	}
	if m, ok := v.(map[string]any); ok {
		r.attrs = m
	}
	return r.attrs
}

// attrString renders a stored attribute value the way terms compare it.
func attrString(v any) (string, bool) {
	switch x := v.(type) {
	case nil:
		return "", false
	case string:
		return x, true
	case json.Number:
		return x.String(), true
	case bool:
		return strconv.FormatBool(x), true
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return "", false
		}
		return string(b), true
	}
}

// evalQuery reports whether the row matches the query, with the same
// semantics the SQL fragment has for the terms it expresses.
func evalQuery(n *Node, r *rowView) bool {
	switch n.Kind {
	case NodeNot:
		return !evalQuery(n.Kids[0], r)
	case NodeAnd:
		for _, k := range n.Kids {
			if !evalQuery(k, r) {
				return false
			}
		}
		return true
	case NodeOr:
		for _, k := range n.Kids {
			if evalQuery(k, r) {
				return true
			}
		}
		return false
	}
	t := n.Term
	switch t.Field {
	case FieldMessage:
		return strings.Contains(strings.ToLower(r.l.Message), strings.ToLower(t.Value))
	case FieldLevel:
		for _, v := range levelVariants(t.Value) {
			if r.l.Level == v {
				return true
			}
		}
		return false
	case FieldService:
		return r.l.ServiceName == t.Value
	case FieldTraceID:
		return r.l.TraceID == t.Value
	case FieldSpanID:
		return r.l.SpanID == t.Value
	case FieldTimestamp:
		return cmpInt(r.tsMs, t.TsMs, t.Op)
	}
	// attr. / resource.: OTLP ingest merges resource attributes into the same
	// attributes object, so both prefixes read one flat key space.
	v, ok := r.attributes()[t.Key]
	if !ok {
		return false
	}
	s, ok := attrString(v)
	if !ok {
		return false
	}
	if t.Op == OpEq {
		if s == t.Value {
			return true
		}
		if t.IsNum {
			if f, err := strconv.ParseFloat(s, 64); err == nil && f == t.Num {
				return true
			}
		}
		return false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return false
	}
	switch t.Op {
	case OpGT:
		return f > t.Num
	case OpGTE:
		return f >= t.Num
	case OpLT:
		return f < t.Num
	case OpLTE:
		return f <= t.Num
	}
	return false
}

func cmpInt(a, b int64, op Op) bool {
	switch op {
	case OpGT:
		return a > b
	case OpGTE:
		return a >= b
	case OpLT:
		return a < b
	case OpLTE:
		return a <= b
	}
	return a == b
}
