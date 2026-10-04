package logs

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Log query language (the `lq` parameter of GET /api/v1/logs/search).
//
// A deliberately small grammar, parsed here in Go and never handed to the
// database as text:
//
//	query   = or
//	or      = and { "OR" and }
//	and     = unary { ["AND"] unary }          (adjacent terms are ANDed)
//	unary   = ( "-" | "NOT" ) unary | primary
//	primary = "(" or ")" | term
//	term    = word | "quoted phrase" | field ":" value
//	value   = word | "quoted" | op word        (op: >= <= > <)
//
// Bare words and phrases are a case-insensitive "message contains". AND, OR
// and NOT are keywords only in upper case. Fields: level, service, trace_id,
// span_id, message, timestamp, attr.<key>, resource.<key>.
//
// The parser only builds an AST; the SQL compiler (logql_sql.go) turns the
// AST into a WHERE fragment whose column names come from a fixed list and
// whose values are all bound parameters.

// Query limits. Every one of them is enforced by the parser, before anything
// is compiled, so a hostile query is refused cheaply.
const (
	MaxQueryLen   = 1024 // bytes
	MaxQueryTerms = 20   // leaf terms
	MaxQueryDepth = 5    // nested parentheses, and stacked negations
	MaxKeyLen     = 128  // attr./resource. key length
	maxValueRunes = 256  // a single term value
)

// queryKeyRe is the only shape an attr./resource. key may take. The keys are
// never placed in SQL (they are matched in Go), but the shape is still
// pinned so a key can never be anything surprising.
var queryKeyRe = regexp.MustCompile(`^[A-Za-z0-9_.\-/]{1,128}$`)

// QueryError is a syntax or limit error with the 0-based character position
// it applies to. The HTTP layer maps it to a 400.
type QueryError struct {
	Pos int
	Msg string
}

func (e *QueryError) Error() string {
	return fmt.Sprintf("query error at position %d: %s", e.Pos, e.Msg)
}

// NodeKind is the type of an AST node.
type NodeKind int

const (
	NodeTerm NodeKind = iota
	NodeAnd
	NodeOr
	NodeNot
)

// Field is a queryable field.
type Field string

const (
	FieldMessage   Field = "message"
	FieldLevel     Field = "level"
	FieldService   Field = "service"
	FieldTraceID   Field = "trace_id"
	FieldSpanID    Field = "span_id"
	FieldTimestamp Field = "timestamp"
	FieldAttr      Field = "attr"
	FieldResource  Field = "resource"
)

// Op is a term comparison operator.
type Op int

const (
	OpEq Op = iota // equality, or "contains" for message
	OpGT
	OpGTE
	OpLT
	OpLTE
)

func (o Op) String() string {
	switch o {
	case OpGT:
		return ">"
	case OpGTE:
		return ">="
	case OpLT:
		return "<"
	case OpLTE:
		return "<="
	}
	return ":"
}

// Term is one leaf condition.
type Term struct {
	Field Field
	Key   string // attr./resource. key
	Op    Op
	Value string
	Num   float64 // numeric value for attr./resource. comparisons
	IsNum bool    // Value parsed as a finite number
	TsMs  int64   // timestamp value, unix ms
	Pos   int
}

// Node is an AST node.
type Node struct {
	Kind NodeKind
	Kids []*Node
	Term *Term
}

// String renders the AST in a canonical prefix form (used by golden tests).
func (n *Node) String() string {
	switch n.Kind {
	case NodeTerm:
		t := n.Term
		name := string(t.Field)
		if t.Field == FieldAttr || t.Field == FieldResource {
			name += "." + t.Key
		}
		return fmt.Sprintf("%s%s%q", name, t.Op, t.Value)
	case NodeNot:
		return "(NOT " + n.Kids[0].String() + ")"
	}
	op := "AND"
	if n.Kind == NodeOr {
		op = "OR"
	}
	parts := make([]string, len(n.Kids))
	for i, k := range n.Kids {
		parts[i] = k.String()
	}
	return "(" + op + " " + strings.Join(parts, " ") + ")"
}

// ---------------------------------------------------------------- lexer

type tokKind int

const (
	tkEOF tokKind = iota
	tkWord
	tkString
	tkLParen
	tkRParen
	tkMinus
)

type token struct {
	kind     tokKind
	text     string // word text, or unescaped string contents
	pos, end int    // character positions [pos, end)
}

func lex(runes []rune) ([]token, error) {
	var toks []token
	n := len(runes)
	i := 0
	for i < n {
		r := runes[i]
		switch {
		case unicode.IsSpace(r):
			i++
		case r == '(':
			toks = append(toks, token{kind: tkLParen, pos: i, end: i + 1})
			i++
		case r == ')':
			toks = append(toks, token{kind: tkRParen, pos: i, end: i + 1})
			i++
		case r == '"':
			start := i
			i++
			var b strings.Builder
			closed := false
			for i < n {
				c := runes[i]
				if c == '\\' && i+1 < n && (runes[i+1] == '"' || runes[i+1] == '\\') {
					b.WriteRune(runes[i+1])
					i += 2
					continue
				}
				if c == '"' {
					closed = true
					i++
					break
				}
				b.WriteRune(c)
				i++
			}
			if !closed {
				return nil, &QueryError{Pos: start, Msg: "unterminated quoted string"}
			}
			toks = append(toks, token{kind: tkString, text: b.String(), pos: start, end: i})
		case r == '-' && i+1 < n && !unicode.IsSpace(runes[i+1]):
			toks = append(toks, token{kind: tkMinus, pos: i, end: i + 1})
			i++
		default:
			start := i
			for i < n {
				c := runes[i]
				if unicode.IsSpace(c) || c == '(' || c == ')' || c == '"' {
					break
				}
				i++
			}
			if i-start == 1 && runes[start] == '-' {
				return nil, &QueryError{Pos: start, Msg: "dangling '-' (quote it to search for a hyphen)"}
			}
			toks = append(toks, token{kind: tkWord, text: string(runes[start:i]), pos: start, end: i})
		}
	}
	toks = append(toks, token{kind: tkEOF, pos: n, end: n})
	return toks, nil
}

// ---------------------------------------------------------------- parser

type parser struct {
	toks  []token
	i     int
	terms int
}

// ParseQuery parses a log query. It never panics and never returns a nil
// node without an error. Surrounding whitespace is ignored; an empty query
// is an error (callers treat a blank parameter as "no query" before this).
func ParseQuery(q string) (*Node, error) {
	if len(q) > MaxQueryLen {
		return nil, &QueryError{Pos: 0, Msg: fmt.Sprintf("query too long (%d bytes, max %d)", len(q), MaxQueryLen)}
	}
	if !utf8.ValidString(q) {
		return nil, &QueryError{Pos: 0, Msg: "query is not valid UTF-8"}
	}
	runes := []rune(q)
	for i, r := range runes {
		// NUL and other control characters (tab, CR, LF excepted) have no
		// place in a search expression.
		if (unicode.IsControl(r) && r != '\t' && r != '\n' && r != '\r') || r == utf8.RuneError {
			return nil, &QueryError{Pos: i, Msg: "control or invalid character in query"}
		}
	}
	toks, err := lex(runes)
	if err != nil {
		return nil, err
	}
	if len(toks) == 1 {
		return nil, &QueryError{Pos: 0, Msg: "empty query"}
	}
	p := &parser{toks: toks}
	n, err := p.parseOr(0)
	if err != nil {
		return nil, err
	}
	if t := p.peek(); t.kind != tkEOF {
		if t.kind == tkRParen {
			return nil, &QueryError{Pos: t.pos, Msg: "unmatched ')'"}
		}
		return nil, &QueryError{Pos: t.pos, Msg: "unexpected token"}
	}
	return n, nil
}

func (p *parser) peek() token { return p.toks[p.i] }
func (p *parser) next() token {
	t := p.toks[p.i]
	if t.kind != tkEOF {
		p.i++
	}
	return t
}

func isKeyword(t token, kw string) bool {
	return t.kind == tkWord && t.text == kw
}

func (p *parser) parseOr(depth int) (*Node, error) {
	first, err := p.parseAnd(depth)
	if err != nil {
		return nil, err
	}
	kids := []*Node{first}
	for isKeyword(p.peek(), "OR") {
		p.next()
		k, err := p.parseAnd(depth)
		if err != nil {
			return nil, err
		}
		kids = append(kids, k)
	}
	if len(kids) == 1 {
		return first, nil
	}
	return &Node{Kind: NodeOr, Kids: kids}, nil
}

func (p *parser) parseAnd(depth int) (*Node, error) {
	first, err := p.parseUnary(depth, 0)
	if err != nil {
		return nil, err
	}
	kids := []*Node{first}
	for {
		t := p.peek()
		if isKeyword(t, "AND") {
			p.next()
		} else if t.kind == tkEOF || t.kind == tkRParen || isKeyword(t, "OR") {
			break
		}
		k, err := p.parseUnary(depth, 0)
		if err != nil {
			return nil, err
		}
		kids = append(kids, k)
	}
	if len(kids) == 1 {
		return first, nil
	}
	return &Node{Kind: NodeAnd, Kids: kids}, nil
}

func (p *parser) parseUnary(depth, negs int) (*Node, error) {
	t := p.peek()
	if t.kind == tkMinus || isKeyword(t, "NOT") {
		if negs+1 > MaxQueryDepth {
			return nil, &QueryError{Pos: t.pos, Msg: fmt.Sprintf("too many stacked negations (max %d)", MaxQueryDepth)}
		}
		p.next()
		k, err := p.parseUnary(depth, negs+1)
		if err != nil {
			return nil, err
		}
		return &Node{Kind: NodeNot, Kids: []*Node{k}}, nil
	}
	return p.parsePrimary(depth)
}

func (p *parser) parsePrimary(depth int) (*Node, error) {
	t := p.peek()
	switch t.kind {
	case tkLParen:
		if depth+1 > MaxQueryDepth {
			return nil, &QueryError{Pos: t.pos, Msg: fmt.Sprintf("parentheses nested too deeply (max %d)", MaxQueryDepth)}
		}
		p.next()
		if p.peek().kind == tkRParen {
			return nil, &QueryError{Pos: t.pos, Msg: "empty parentheses"}
		}
		n, err := p.parseOr(depth + 1)
		if err != nil {
			return nil, err
		}
		if c := p.peek(); c.kind != tkRParen {
			return nil, &QueryError{Pos: t.pos, Msg: "unclosed '('"}
		}
		p.next()
		return n, nil
	case tkRParen:
		return nil, &QueryError{Pos: t.pos, Msg: "unmatched ')'"}
	case tkEOF:
		return nil, &QueryError{Pos: t.pos, Msg: "unexpected end of query"}
	case tkMinus:
		return nil, &QueryError{Pos: t.pos, Msg: "unexpected '-'"}
	case tkString:
		p.next()
		return p.leaf(&Term{Field: FieldMessage, Op: OpEq, Value: t.text, Pos: t.pos})
	}
	// word
	if t.text == "AND" || t.text == "OR" {
		return nil, &QueryError{Pos: t.pos, Msg: "unexpected " + t.text + " (quote it to search for the word)"}
	}
	p.next()
	return p.wordTerm(t)
}

func (p *parser) leaf(t *Term) (*Node, error) {
	if p.terms++; p.terms > MaxQueryTerms {
		return nil, &QueryError{Pos: t.Pos, Msg: fmt.Sprintf("too many terms (max %d)", MaxQueryTerms)}
	}
	if t.Value == "" {
		return nil, &QueryError{Pos: t.Pos, Msg: "empty value"}
	}
	if utf8.RuneCountInString(t.Value) > maxValueRunes {
		return nil, &QueryError{Pos: t.Pos, Msg: fmt.Sprintf("value too long (max %d characters)", maxValueRunes)}
	}
	return &Node{Kind: NodeTerm, Term: t}, nil
}

func (p *parser) wordTerm(w token) (*Node, error) {
	ci := strings.IndexByte(w.text, ':')
	if ci < 0 {
		return p.leaf(&Term{Field: FieldMessage, Op: OpEq, Value: w.text, Pos: w.pos})
	}
	if ci == 0 {
		return nil, &QueryError{Pos: w.pos, Msg: "missing field name before ':'"}
	}
	fieldName, rest := w.text[:ci], w.text[ci+1:]
	t := &Term{Op: OpEq, Pos: w.pos}

	lower := asciiLower(fieldName)
	switch {
	case lower == "level":
		t.Field = FieldLevel
	case lower == "service":
		t.Field = FieldService
	case lower == "trace_id":
		t.Field = FieldTraceID
	case lower == "span_id":
		t.Field = FieldSpanID
	case lower == "message":
		t.Field = FieldMessage
	case lower == "timestamp":
		t.Field = FieldTimestamp
	case strings.HasPrefix(lower, "attr.") || strings.HasPrefix(lower, "resource."):
		dot := strings.IndexByte(fieldName, '.')
		t.Field = FieldAttr
		if lower[:dot] == "resource" {
			t.Field = FieldResource
		}
		t.Key = fieldName[dot+1:]
		if !queryKeyRe.MatchString(t.Key) {
			return nil, &QueryError{Pos: w.pos, Msg: fmt.Sprintf("invalid key %q (allowed: letters, digits, _ . - /, max %d characters)", truncateForMsg(t.Key), MaxKeyLen)}
		}
	default:
		return nil, &QueryError{Pos: w.pos, Msg: fmt.Sprintf("unknown field %q (quote the text to search for it literally)", truncateForMsg(fieldName))}
	}

	// Optional comparison operator, then the value: the rest of the word, or
	// an immediately adjacent quoted string (level:"x", attr.k:>"5").
	switch {
	case strings.HasPrefix(rest, ">="):
		t.Op, rest = OpGTE, rest[2:]
	case strings.HasPrefix(rest, "<="):
		t.Op, rest = OpLTE, rest[2:]
	case strings.HasPrefix(rest, ">"):
		t.Op, rest = OpGT, rest[1:]
	case strings.HasPrefix(rest, "<"):
		t.Op, rest = OpLT, rest[1:]
	}
	if rest == "" {
		nx := p.peek()
		if nx.kind == tkString && nx.pos == w.end {
			p.next()
			rest = nx.text
		} else {
			return nil, &QueryError{Pos: w.pos, Msg: "missing value for field " + fieldName}
		}
	}
	t.Value = rest

	if t.Op != OpEq && t.Field != FieldTimestamp && t.Field != FieldAttr && t.Field != FieldResource {
		return nil, &QueryError{Pos: w.pos, Msg: "comparison operators only apply to timestamp, attr.* and resource.* fields"}
	}
	switch t.Field {
	case FieldTimestamp:
		ms, err := parseQueryTime(rest)
		if err != nil {
			return nil, &QueryError{Pos: w.pos, Msg: err.Error()}
		}
		t.TsMs = ms
	case FieldAttr, FieldResource:
		f, err := strconv.ParseFloat(rest, 64)
		if err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
			t.Num, t.IsNum = f, true
		}
		if t.Op != OpEq && !t.IsNum {
			return nil, &QueryError{Pos: w.pos, Msg: "comparison needs a numeric value"}
		}
	}
	return p.leaf(t)
}

// asciiLower lower-cases A-Z only, so byte offsets into the result line up
// with the input (strings.ToLower can change a string's length).
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

func truncateForMsg(s string) string {
	r := []rune(s)
	if len(r) > 40 {
		return string(r[:40]) + "..."
	}
	return s
}

// parseQueryTime accepts RFC3339, YYYY-MM-DD (UTC midnight) or unix
// milliseconds.
func parseQueryTime(s string) (int64, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UnixMilli(), nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UnixMilli(), nil
	}
	if len(s) <= 16 {
		if ms, err := strconv.ParseInt(s, 10, 64); err == nil && ms >= 0 {
			return ms, nil
		}
	}
	return 0, fmt.Errorf("invalid timestamp %q (use RFC3339, YYYY-MM-DD or unix milliseconds)", truncateForMsg(s))
}
