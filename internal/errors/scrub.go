package errors

// Server-side PII / secret scrubbing at error admission.
//
// Where it runs: ErrorBuffer.Push, BEFORE the record is marshalled. That
// marshalled body is the frozen record the WAL frame carries and the
// bytes the inbox digest is computed over, so nothing unscrubbed ever
// reaches the WAL, the quarantine spool, or error_events.
//
// Grouping stability: the v1 grouping hash is derived from error_type,
// error_value, the stack frames, an SDK fingerprint and (rage clicks)
// url+selector - several of which scrubbing may rewrite. Push therefore
// computes the group hash from the RAW (URL-canonicalised) input first
// and carries it on the record (ErrorInput.PreGroupHash); the flush-time
// insert uses it instead of re-deriving from scrubbed text. The golden
// test in scrub_test.go pins that the stored hash equals the raw-input
// hash.
//
// Idempotent-inbox consistency: the digest is sha256 over the SCRUBBED
// canonical payload (scrubbing is deterministic, so a retry of the same
// event hashes equal). Consequence, stated plainly: two payloads that
// differ only inside a value that is replaced by "[Filtered]" digest
// equal and dedupe rather than 409 - the server never retained the
// distinguishing bytes, so it cannot honestly call them conflicting.
//
// Configuration: OBSERVE_SCRUB_KEYS (comma list of extra key fragments),
// OBSERVE_SCRUB_DISABLE=true (opt-out; default ON).

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Filtered is the replacement for every scrubbed value.
const Filtered = "[Filtered]"

// truncated replaces a subtree the scrubber refuses to walk (depth or
// node budget exhausted). It is fail-closed: unexamined data is dropped,
// never passed through.
const truncated = "[Truncated]"

const (
	scrubMaxDepth = 16
	scrubMaxNodes = 20000
	// scrubMaxString bounds the bytes examined per string value.
	scrubMaxString = 64 << 10
	// scrubMaxJSONDepth bounds JSON-inside-a-string-inside-JSON recursion.
	scrubMaxJSONDepth = 3
)

// Substring-matched key fragments (compared against the key lowercased
// with separators removed).
var scrubSubstringKeys = []string{
	"password", "passwd", "passphrase", "secret", "token", "apikey",
	"authorization", "cookie", "session", "csrf", "xsrf", "credit",
	"privatekey", "accesskey", "bearer",
}

// Whole-word key fragments: short words that would false-positive as
// substrings ("author", "cardinality", "classname").
var scrubWordKeys = map[string]bool{
	"auth": true, "ssn": true, "card": true, "pwd": true, "jwt": true, "cvv": true, "cvc": true,
}

// Scrubber redacts secrets and PII from error payloads.
type Scrubber struct {
	disabled bool
	extra    []string // normalised extra key fragments
}

// NewScrubber builds a scrubber with extra key fragments.
func NewScrubber(extraKeys ...string) *Scrubber {
	s := &Scrubber{}
	for _, k := range extraKeys {
		if n := normalizeKey(k); n != "" {
			s.extra = append(s.extra, n)
		}
	}
	return s
}

// NewScrubberFromEnv reads OBSERVE_SCRUB_KEYS and OBSERVE_SCRUB_DISABLE.
func NewScrubberFromEnv() *Scrubber {
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("OBSERVE_SCRUB_DISABLE"))); v == "true" || v == "1" {
		return &Scrubber{disabled: true}
	}
	return NewScrubber(strings.Split(os.Getenv("OBSERVE_SCRUB_KEYS"), ",")...)
}

// Disabled reports whether scrubbing is switched off.
func (s *Scrubber) Disabled() bool { return s == nil || s.disabled }

// confusables maps common cross-script lookalikes (Cyrillic/Greek) to the
// ASCII letter they imitate. NFKC folds fullwidth/compat forms but not
// these, so "pаssword" with a Cyrillic "a" would otherwise dodge the match.
var confusables = map[rune]rune{
	'\u0430': 'a', '\u0435': 'e', '\u043e': 'o', '\u0440': 'p', '\u0441': 'c',
	'\u0445': 'x', '\u0443': 'y', '\u0456': 'i', '\u0455': 's', '\u0458': 'j',
	'\u04bb': 'h', '\u0501': 'd', '\u051b': 'q', '\u051d': 'w', '\u0442': 't',
	'\u03bf': 'o', '\u03c1': 'p', '\u03bd': 'v', '\u03b1': 'a', '\u03b5': 'e',
	'\u03b9': 'i', '\u03ba': 'k', '\u03c4': 't', '\u03c5': 'u', '\u0131': 'i',
	'\u0261': 'g', '\u0251': 'a', '\u0269': 'i',
}

// foldKey applies compatibility normalisation (NFKC: fullwidth ASCII,
// ligatures, circled/superscript forms) and then the confusables map.
func foldKey(k string) string {
	k = norm.NFKC.String(k)
	return strings.Map(func(r rune) rune {
		r = unicode.ToLower(r)
		if m, ok := confusables[r]; ok {
			return m
		}
		return r
	}, k)
}

// normalizeKey folds compatibility forms, lowercases and strips separators
// (and format characters such as zero-width spaces) so "API-Key", "api_key",
// "apiKey" and fullwidth "ＰＡＳＳＷＯＲＤ" compare equal.
func normalizeKey(k string) string {
	var b strings.Builder
	for _, r := range foldKey(k) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// keyWords splits a key on non-alphanumerics and camelCase boundaries.
func keyWords(k string) []string {
	var words []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			words = append(words, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	var prev rune
	// Fold compat forms first, but keep case for camelCase splitting.
	k = strings.Map(func(r rune) rune {
		if m, ok := confusables[unicode.ToLower(r)]; ok {
			if unicode.IsUpper(r) {
				return unicode.ToUpper(m)
			}
			return m
		}
		return r
	}, norm.NFKC.String(k))
	for _, r := range k {
		switch {
		case !unicode.IsLetter(r) && !unicode.IsDigit(r):
			flush()
		case unicode.IsUpper(r) && unicode.IsLower(prev):
			flush()
			cur = append(cur, r)
		default:
			cur = append(cur, r)
		}
		prev = r
	}
	flush()
	return words
}

// sensitiveKey reports whether a map key names a secret.
func (s *Scrubber) sensitiveKey(k string) bool {
	n := normalizeKey(k)
	if n == "" {
		return false
	}
	for _, f := range scrubSubstringKeys {
		if strings.Contains(n, f) {
			return true
		}
	}
	for _, f := range s.extra {
		if strings.Contains(n, f) {
			return true
		}
	}
	for _, w := range keyWords(k) {
		if scrubWordKeys[w] {
			return true
		}
	}
	return false
}

var (
	jwtRe    = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}(?:\.[A-Za-z0-9_-]*)?`)
	bearerRe = regexp.MustCompile(`(?i)\b(Bearer)\s+[A-Za-z0-9._~+/=-]{8,}`)
	cardRe   = regexp.MustCompile(`\b(?:\d[ -]?){13,18}\d\b`)
	// Query-string parameters: ?name=value / &name=value / ;name=value.
	queryParamRe = regexp.MustCompile(`([?&;])([^=&#;\s"']+)=([^&#;\s"']*)`)
	// name=value pairs for obviously secret names in free text.
	kvSecretRe = regexp.MustCompile(`(?i)\b(password|passwd|pwd|secret|token|api[_-]?key|apikey|access[_-]?token|client[_-]?secret)=([^\s&"',;]+)`)
	// "password: x", "password = x", "api_key : 'x y'" in free text.
	colonSecretRe = regexp.MustCompile(`(?i)\b(password|passwd|passphrase|pwd|secret|token|api[_-]?key|apikey|access[_-]?token|client[_-]?secret|private[_-]?key)(["']?\s*[:=]\s*)("[^"]*"|'[^']*'|[^\s&"',;]+)`)
	// "Authorization: Basic xxx" (any scheme) up to the end of the value.
	authHeaderRe = regexp.MustCompile(`(?i)\b(authorization|proxy-authorization)(["']?\s*[:=]\s*)((?:basic|digest|negotiate|bearer|token)\s+)?[^\s"',;]+`)
	// A bare "Basic <base64>" that decodes to user:pass.
	basicRe = regexp.MustCompile(`(?i)\bBasic\s+([A-Za-z0-9+/_-]{8,}={0,2})`)
)

// luhnValid reports whether the digit string passes the Luhn check.
func luhnValid(digits string) bool {
	sum, alt := 0, false
	for i := len(digits) - 1; i >= 0; i-- {
		d := int(digits[i] - '0')
		if alt {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		alt = !alt
	}
	return len(digits) > 0 && sum%10 == 0
}

func countDigits(v string) int {
	n := 0
	for i := 0; i < len(v); i++ {
		if v[i] >= '0' && v[i] <= '9' {
			n++
		}
	}
	return n
}

// scrubString applies the value-pattern rules to one string.
func (s *Scrubber) scrubString(v string) string {
	return s.scrubStringIn(v, nil)
}

// scrubStringIn is scrubString with the caller's node budget / JSON depth
// (st nil = a fresh budget).
func (s *Scrubber) scrubStringIn(v string, st *scrubState) string {
	if v == "" {
		return v
	}
	if len(v) > scrubMaxString {
		// Bounded work per string; the unexamined tail is dropped, not
		// passed through.
		v = v[:scrubMaxString] + truncated
	}
	if st == nil {
		st = &scrubState{s: s}
	}
	v = s.scrubJSONString(v, st)
	v = s.scrubTokens(v)
	if countDigits(v) < 14 {
		return v
	}
	v = cardRe.ReplaceAllStringFunc(v, func(m string) string {
		digits := make([]byte, 0, 19)
		for i := 0; i < len(m); i++ {
			if m[i] >= '0' && m[i] <= '9' {
				digits = append(digits, m[i])
			}
		}
		// 14-19 digits: 13-digit runs are indistinguishable from epoch-ms
		// timestamps (10% pass Luhn by chance) and 13-digit cards are
		// effectively extinct.
		if len(digits) >= 14 && len(digits) <= 19 && luhnValid(string(digits)) {
			return Filtered
		}
		return m
	})
	return v
}

// scrubJSONString scrubs a string that is itself a JSON document (a
// stringified request body in an exception message, say): it is parsed,
// walked with the same key rules and re-serialised. Parse failure, or a
// string that does not look like an object/array, falls through untouched
// to the pattern rules. Depth and node budgets bound the work.
func (s *Scrubber) scrubJSONString(v string, st *scrubState) string {
	t := strings.TrimSpace(v)
	if len(t) < 2 || (t[0] != '{' && t[0] != '[') || st.jd >= scrubMaxJSONDepth || st.nodes > scrubMaxNodes {
		return v
	}
	dec := json.NewDecoder(strings.NewReader(t))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil || dec.More() {
		return v
	}
	st.jd++
	out := st.value(doc, 0)
	st.jd--
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		return v
	}
	return strings.TrimRight(buf.String(), "\n")
}

// scrubTokens applies the token / credential patterns only (no card
// numbers): safe for identifiers such as fingerprints, selectors, release
// tags and function names, where digit runs are legitimate.
func (s *Scrubber) scrubTokens(v string) string {
	if v == "" {
		return v
	}
	if len(v) > scrubMaxString {
		v = v[:scrubMaxString] + truncated
	}
	// Cheap substring pre-filters keep the regexes off strings that cannot
	// match.
	lower := strings.ToLower(v)
	if strings.Contains(v, "eyJ") {
		v = jwtRe.ReplaceAllString(v, Filtered)
	}
	if strings.Contains(lower, "authorization") {
		v = authHeaderRe.ReplaceAllString(v, "$1$2$3"+Filtered)
	}
	if strings.Contains(lower, "bearer") {
		v = bearerRe.ReplaceAllString(v, "$1 "+Filtered)
	}
	if strings.Contains(lower, "basic") {
		v = basicRe.ReplaceAllStringFunc(v, func(m string) string {
			sub := basicRe.FindStringSubmatch(m)
			raw := strings.TrimRight(sub[1], "=")
			for _, enc := range []*base64.Encoding{base64.RawStdEncoding, base64.RawURLEncoding} {
				if dec, err := enc.DecodeString(raw); err == nil && bytes.IndexByte(dec, ':') > 0 {
					return "Basic " + Filtered
				}
			}
			return m
		})
	}
	if strings.Contains(v, "=") {
		if strings.ContainsAny(v, "?&;") {
			v = queryParamRe.ReplaceAllStringFunc(v, func(m string) string {
				sub := queryParamRe.FindStringSubmatch(m)
				name := sub[2]
				ln := strings.ToLower(name)
				if s.sensitiveKey(name) || ln == "key" || ln == "sig" || ln == "signature" {
					return sub[1] + name + "=" + Filtered
				}
				return m
			})
		}
		v = kvSecretRe.ReplaceAllString(v, "$1="+Filtered)
	}
	return foldedSecretPass(v)
}

// foldedSecretPass redacts "name: value" / "name = value" forms. For
// non-ASCII text the match runs on the compat-folded copy so fullwidth
// "ＰＡＳＳＷＯＲＤ：x" is caught; the folded text replaces the original only
// when something was actually redacted (other text is left byte-identical).
func foldedSecretPass(v string) string {
	ascii := true
	for i := 0; i < len(v); i++ {
		if v[i] >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		if !strings.ContainsAny(v, ":=") {
			return v
		}
		return colonSecretRe.ReplaceAllString(v, "$1$2"+Filtered)
	}
	f := foldKeyKeepCase(v)
	if r := colonSecretRe.ReplaceAllString(f, "$1$2"+Filtered); r != f {
		return r
	}
	return v
}

func foldKeyKeepCase(v string) string {
	return strings.Map(func(r rune) rune {
		if m, ok := confusables[unicode.ToLower(r)]; ok {
			return m
		}
		return r
	}, norm.NFKC.String(v))
}

// scrubState carries the per-input node budget.
type scrubState struct {
	s     *Scrubber
	nodes int
	jd    int // JSON-in-string nesting depth
}

// value returns a scrubbed COPY of v (the caller's decoded structure is
// never mutated). depth is the current nesting level.
func (st *scrubState) value(v any, depth int) any {
	st.nodes++
	if st.nodes > scrubMaxNodes || depth > scrubMaxDepth {
		switch v.(type) {
		case nil, bool, float64, int, int64:
			return v
		}
		return truncated
	}
	switch x := v.(type) {
	case string:
		return st.s.scrubStringIn(x, st)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			if st.s.sensitiveKey(k) {
				out[k] = Filtered
				continue
			}
			out[k] = st.value(val, depth+1)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = st.value(val, depth+1)
		}
		return out
	case map[string]string:
		out := make(map[string]string, len(x))
		for k, val := range x {
			if st.s.sensitiveKey(k) {
				out[k] = Filtered
				continue
			}
			out[k] = st.s.scrubString(val)
		}
		return out
	case []string:
		out := make([]string, len(x))
		for i, val := range x {
			out[i] = st.s.scrubString(val)
		}
		return out
	default:
		// Numbers, bools, nil: no pattern applies (a secret-named key
		// was already filtered by the map case).
		return v
	}
}

// ScrubInput returns a scrubbed copy of the input. The original (and
// every map/slice reachable from it) is left untouched. Disabled or nil
// scrubbers return the input unchanged.
func (s *Scrubber) ScrubInput(in ErrorInput) ErrorInput {
	if s.Disabled() {
		return in
	}
	st := &scrubState{s: s}
	out := in
	out.ErrorType = s.scrubString(in.ErrorType)
	out.ErrorValue = s.scrubString(in.ErrorValue)
	out.URL = s.scrubString(in.URL)
	// Identifier-like fields: token/credential patterns only. The grouping
	// hash was pinned from the RAW input in prepareRecord (PreGroupHash)
	// before this runs, so rewriting these cannot change grouping.
	out.ReleaseTag = s.scrubTokens(in.ReleaseTag)
	out.ReleaseTagAlt = s.scrubTokens(in.ReleaseTagAlt)
	out.Selector = s.scrubTokens(in.Selector)
	if in.Fingerprint != nil {
		out.Fingerprint = make([]string, len(in.Fingerprint))
		for i, f := range in.Fingerprint {
			out.Fingerprint[i] = s.scrubTokens(f)
		}
	}
	out.Contexts = st.value(in.Contexts, 0)
	out.Extra = st.value(in.Extra, 0)
	if in.StackTrace != nil {
		out.StackTrace = make([]StackFrame, len(in.StackTrace))
		for i, f := range in.StackTrace {
			f.Filename = s.scrubString(f.Filename)
			f.Function = s.scrubTokens(f.Function)
			out.StackTrace[i] = f
		}
	}
	if in.Breadcrumbs != nil {
		out.Breadcrumbs = make([]Breadcrumb, len(in.Breadcrumbs))
		for i, b := range in.Breadcrumbs {
			b.Message = s.scrubString(b.Message)
			b.Category = s.scrubTokens(b.Category)
			b.Data = st.value(b.Data, 0)
			out.Breadcrumbs[i] = b
		}
	}
	return out
}
