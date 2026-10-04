package surveys

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"

	"github.com/neutron-build/neutron/go/neutron"
)

// Targeting is the evaluated form of a survey's stored `targeting` JSON.
//
// Stored schema (a JSON object; every key optional, all present keys ANDed):
//
//	url_equals     string | [string]  exact path match (query/fragment stripped,
//	                                  one trailing slash ignored); each must start "/"
//	url_prefix     string | [string]  path starts with; each must start "/"
//	url_contains   string | [string]  path contains the substring
//	device         [string]           any of desktop, mobile, tablet
//	referrer_host  [string]           any of these exact (lowercase) hosts
//	sample_percent integer 0..100     deterministic per-visitor bucket
//	once           bool               client shows it at most once per browser
//
// Within one list the values are ORed; the url_* keys are ORed with each other
// (a page matches if ANY url rule matches) and ANDed with the other keys.
// There is deliberately no regex: the patterns are author-supplied but
// evaluated on every public page view, so only linear-time operators exist
// (ReDoS-proof by construction).
//
// Backward compatibility: surveys stored before this schema (empty string,
// "{}", "null", non-JSON, other keys such as a legacy "url") match every page.
// Strict parsing (ParseTargeting) runs on write and rejects anything outside
// the schema with a clear error; lenient parsing (lenientTargeting) runs on
// read and silently drops any key it cannot interpret, so a stored value can
// never make /surveys/active fail or throw.
type Targeting struct {
	URLEquals     []string
	URLPrefix     []string
	URLContains   []string
	Device        []string
	ReferrerHost  []string
	SamplePercent *int
	Once          bool
}

const (
	maxTargetingBytes   = 4096
	maxTargetingEntries = 20
	maxTargetingString  = 200
	maxRequestPath      = 2048
)

var validDevices = map[string]bool{"desktop": true, "mobile": true, "tablet": true}

var hostPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)

// ParseTargeting validates the stored-form targeting JSON for a write. Empty,
// whitespace and "null" are accepted (no targeting). Errors are HTTP 400s that
// name the offending key.
func ParseTargeting(raw string) (Targeting, error) {
	return parseTargeting(raw, true)
}

func lenientTargeting(raw string) Targeting {
	t, _ := parseTargeting(raw, false)
	return t
}

func bad(format string, a ...any) error {
	return neutron.ErrBadRequest("targeting: " + fmt.Sprintf(format, a...))
}

func parseTargeting(raw string, strict bool) (Targeting, error) {
	var t Targeting
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return t, nil
	}
	if len(raw) > maxTargetingBytes {
		if strict {
			return t, bad("too large (max %d bytes)", maxTargetingBytes)
		}
		return t, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		if strict {
			return t, bad("must be a JSON object")
		}
		return t, nil
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic error reporting
	for _, k := range keys {
		v := obj[k]
		var err error
		switch k {
		case "url_equals":
			t.URLEquals, err = pathList(k, v, true)
		case "url_prefix":
			t.URLPrefix, err = pathList(k, v, true)
		case "url_contains":
			t.URLContains, err = pathList(k, v, false)
		case "device":
			t.Device, err = deviceList(v)
		case "referrer_host":
			t.ReferrerHost, err = hostList(v)
		case "sample_percent":
			t.SamplePercent, err = percent(v)
		case "once":
			if json.Unmarshal(v, &t.Once) != nil {
				err = bad("once must be true or false")
			}
		default:
			if strict {
				return Targeting{}, bad("unknown key %q (allowed: url_equals, url_prefix, url_contains, device, referrer_host, sample_percent, once)", k)
			}
			continue
		}
		if err != nil {
			if strict {
				return Targeting{}, err
			}
			// Lenient: drop just this key. Every field's zero value is
			// "no constraint", so reset whichever one failed.
			switch k {
			case "url_equals":
				t.URLEquals = nil
			case "url_prefix":
				t.URLPrefix = nil
			case "url_contains":
				t.URLContains = nil
			case "device":
				t.Device = nil
			case "referrer_host":
				t.ReferrerHost = nil
			case "sample_percent":
				t.SamplePercent = nil
			case "once":
				t.Once = false
			}
		}
	}
	return t, nil
}

// stringList accepts "x" or ["x", ...]; empty strings and over-long or
// control-character values are errors.
func stringList(key string, v json.RawMessage) ([]string, error) {
	var list []string
	trimmed := bytes.TrimSpace(v)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var one string
		if err := json.Unmarshal(trimmed, &one); err != nil {
			return nil, bad("%s must be a string or a list of strings", key)
		}
		list = []string{one}
	} else if err := json.Unmarshal(trimmed, &list); err != nil {
		return nil, bad("%s must be a string or a list of strings", key)
	}
	if len(list) == 0 {
		return nil, bad("%s must not be empty", key)
	}
	if len(list) > maxTargetingEntries {
		return nil, bad("%s has too many entries (max %d)", key, maxTargetingEntries)
	}
	for _, s := range list {
		if s == "" || len(s) > maxTargetingString {
			return nil, bad("%s entries must be 1-%d bytes", key, maxTargetingString)
		}
		for i := 0; i < len(s); i++ {
			if s[i] < 0x20 || s[i] == 0x7f {
				return nil, bad("%s entries must not contain control characters", key)
			}
		}
	}
	return list, nil
}

func pathList(key string, v json.RawMessage, mustStartSlash bool) ([]string, error) {
	list, err := stringList(key, v)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(list))
	for i, s := range list {
		if mustStartSlash && !strings.HasPrefix(s, "/") {
			return nil, bad("%s entries must start with \"/\" (got %q)", key, s)
		}
		if strings.ContainsAny(s, "?#") {
			return nil, bad("%s entries match the path only; remove the query/fragment (got %q)", key, s)
		}
		if key == "url_equals" {
			s = trimSlash(s)
		}
		out[i] = s
	}
	return out, nil
}

func deviceList(v json.RawMessage) ([]string, error) {
	list, err := stringList("device", v)
	if err != nil {
		return nil, err
	}
	for i, s := range list {
		s = strings.ToLower(s)
		if !validDevices[s] {
			return nil, bad("device must be one of desktop, mobile, tablet (got %q)", s)
		}
		list[i] = s
	}
	return list, nil
}

func hostList(v json.RawMessage) ([]string, error) {
	list, err := stringList("referrer_host", v)
	if err != nil {
		return nil, err
	}
	for i, s := range list {
		s = strings.ToLower(s)
		if !hostPattern.MatchString(s) {
			return nil, bad("referrer_host entries must be bare hostnames such as news.example.com (got %q)", s)
		}
		list[i] = s
	}
	return list, nil
}

func percent(v json.RawMessage) (*int, error) {
	var f float64
	if err := json.Unmarshal(v, &f); err != nil || math.IsNaN(f) || f != math.Trunc(f) || f < 0 || f > 100 {
		return nil, bad("sample_percent must be an integer from 0 to 100")
	}
	n := int(f)
	return &n, nil
}

// ClientContext is what the public /surveys/active request tells us about the
// current page view. Every field is client-asserted and untrusted: it only
// decides which survey to OFFER, never what is recorded about the reader.
type ClientContext struct {
	Path         string // location.pathname
	Device       string // desktop|mobile|tablet, else derived from UserAgent
	ReferrerHost string // lowercase host of document.referrer, may be ""
	UserAgent    string
	// Visitor is the stable anonymous entity used for sample_percent
	// bucketing (the visitor-estimate the exposure path records).
	Visitor string
}

// NormalizePath reduces a client-supplied path to the comparable form: query
// and fragment cut, control characters dropped, bounded length, leading "/",
// one trailing slash ignored (the root stays "/").
func NormalizePath(p string) string {
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	if len(p) > maxRequestPath {
		p = p[:maxRequestPath]
	}
	b := make([]byte, 0, len(p)+1)
	for i := 0; i < len(p); i++ {
		if c := p[i]; c >= 0x20 && c != 0x7f {
			b = append(b, c)
		}
	}
	p = string(b)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return trimSlash(p)
}

func trimSlash(p string) string {
	if len(p) > 1 {
		p = strings.TrimSuffix(p, "/")
		if p == "" {
			p = "/"
		}
	}
	return p
}

// ClassifyDevice returns a valid hint verbatim (lowercased) or derives a
// coarse class from the User-Agent. Coarse by design: this gates which survey
// is offered, not analytics.
func ClassifyDevice(hint, userAgent string) string {
	if h := strings.ToLower(strings.TrimSpace(hint)); validDevices[h] {
		return h
	}
	ua := strings.ToLower(userAgent)
	switch {
	case strings.Contains(ua, "ipad"), strings.Contains(ua, "tablet"),
		strings.Contains(ua, "android") && !strings.Contains(ua, "mobile"):
		return "tablet"
	case strings.Contains(ua, "mobi"), strings.Contains(ua, "iphone"), strings.Contains(ua, "android"):
		return "mobile"
	}
	return "desktop"
}

// Bucket maps (survey, visitor) to 0..99 deterministically, so a visitor sees
// a stable in/out decision for a survey and changing one survey's sample does
// not correlate with another's.
func Bucket(surveyID, visitor string) int {
	h := sha256.Sum256([]byte(surveyID + ":" + visitor))
	return int((uint32(h[0])<<8 | uint32(h[1])) % 100)
}

// Matches reports whether a page view qualifies for the survey.
func (t Targeting) Matches(surveyID string, c ClientContext) bool {
	if len(t.URLEquals)+len(t.URLPrefix)+len(t.URLContains) > 0 {
		p := NormalizePath(c.Path)
		ok := false
		for _, s := range t.URLEquals {
			if p == s {
				ok = true
			}
		}
		for _, s := range t.URLPrefix {
			if strings.HasPrefix(p, s) {
				ok = true
			}
		}
		for _, s := range t.URLContains {
			if strings.Contains(p, s) {
				ok = true
			}
		}
		if !ok {
			return false
		}
	}
	if len(t.Device) > 0 {
		d := ClassifyDevice(c.Device, c.UserAgent)
		ok := false
		for _, s := range t.Device {
			if s == d {
				ok = true
			}
		}
		if !ok {
			return false
		}
	}
	if len(t.ReferrerHost) > 0 {
		ok := false
		for _, s := range t.ReferrerHost {
			if s == strings.ToLower(c.ReferrerHost) {
				ok = true
			}
		}
		if !ok {
			return false
		}
	}
	if t.SamplePercent != nil && Bucket(surveyID, c.Visitor) >= *t.SamplePercent {
		return false
	}
	return true
}
