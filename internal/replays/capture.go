package replays

import (
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/neutron-build/neutron/go/neutron"
)

// Console and network capture events (opt-in on the tracker via data-
// attributes, default OFF). They ride the existing v2 batch transport as two
// extra custom event types. The tracker already truncates and scrubs; the
// server re-validates every bound and re-scrubs, because a producer is not
// trusted (a hand-built batch can carry anything). Violations reject the
// whole batch with a 400, the same rule the rest of the batch validation
// applies.
const (
	// EventConsole and EventNetwork are the stored replay_events.event_type
	// values for the captured console and network records.
	EventConsole = "console"
	EventNetwork = "network"

	// maxConsoleMessageBytes is the per-record cap (1 KiB, UTF-8 bytes).
	maxConsoleMessageBytes = 1024
	// maxNetworkURLBytes bounds the stored, query-stripped URL.
	maxNetworkURLBytes = 2048
	// maxNetworkDurationMS caps a plausible request duration (1 hour).
	maxNetworkDurationMS = 3600 * 1000
	// maxNetworkSizeBytes caps a reported transfer size (1 TiB).
	maxNetworkSizeBytes = 1 << 40

	// Per-batch caps. A batch carries at most 1000 events (tracker
	// MAX_FLUSH_EVENTS), so these only bound a hostile producer; the
	// tracker's own per-session caps (200 console, 500 network) are far
	// below them. The per-session cap is enforced client-side only - a
	// server-wide count would need a storage read on the ingest hot path.
	maxConsoleEventsPerBatch = 200
	maxNetworkEventsPerBatch = 500
)

var (
	consoleLevels = map[string]bool{"log": true, "info": true, "warn": true, "error": true}

	// consoleKeys and networkKeys are the audited key allowlists: anything
	// else (headers, bodies, args) is refused rather than stored.
	consoleKeys = map[string]bool{"level": true, "message": true, "truncated": true}
	networkKeys = map[string]bool{
		"method": true, "url": true, "status": true, "duration_ms": true,
		"size": true, "kind": true,
	}

	methodRe = regexp.MustCompile(`^[A-Z]{1,16}$`)
)

// Secret scrubbing for captured console text. Same denylist idea as the
// tracker (observe-replay-capture.js keeps the patterns in lockstep): key
// = value pairs for secret-looking keys, bearer/basic credentials, JWTs, and
// long opaque tokens.
var scrubPatterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`(?i)\b(Bearer|Basic)\s+[A-Za-z0-9._~+/=-]{8,}`), "$1 [redacted]"},
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]*`), "[redacted-jwt]"},
	{regexp.MustCompile(`(?i)((?:pass(?:word|wd)?|secret|token|api[_-]?key|authorization|auth|credential|session(?:id)?|cookie|private[_-]?key)["']?\s*[:=]\s*)("[^"]*"|'[^']*'|[^\s,;&}]+)`), "${1}[redacted]"},
	{regexp.MustCompile(`\b[A-Fa-f0-9]{32,}\b`), "[redacted-hex]"},
	{regexp.MustCompile(`\b[A-Za-z0-9_-]{40,}\b`), "[redacted-token]"},
}

// ScrubConsoleText removes credential-shaped substrings from captured console
// text. Exported for the tests and so the denylist has one Go home.
func ScrubConsoleText(s string) string {
	for _, p := range scrubPatterns {
		s = p.re.ReplaceAllString(s, p.repl)
	}
	return s
}

// cutUTF8 truncates s to at most max bytes without splitting a rune.
func cutUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	s = s[:max]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// numField reads a JSON number (float64 after decode) as an int64, refusing
// fractions, NaN/Inf and out-of-range values.
func numField(v any, min, max int64) (int64, bool) {
	f, ok := v.(float64)
	if !ok || math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) {
		return 0, false
	}
	if f < float64(min) || f > float64(max) {
		return 0, false
	}
	return int64(f), true
}

// scrubNetworkURL reduces a captured request URL to scheme+host+path and
// masks path segments that look like opaque credentials.
func scrubNetworkURL(raw string) (string, bool) {
	if raw == "" || len(raw) > maxNetworkURLBytes {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", false
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	u.RawFragment = ""
	segs := strings.Split(u.Path, "/")
	for i, seg := range segs {
		if looksOpaque(seg) {
			segs[i] = ":redacted"
		}
	}
	u.Path = strings.Join(segs, "/")
	u.RawPath = ""
	out := u.String()
	if len(out) > maxNetworkURLBytes {
		return "", false
	}
	return out, true
}

var opaqueSegmentRe = regexp.MustCompile(`^[A-Za-z0-9_-]{24,}$`)

// looksOpaque reports a path segment that reads as a credential or random
// id (24+ URL-safe characters including a digit) rather than a slug.
func looksOpaque(seg string) bool {
	return opaqueSegmentRe.MatchString(seg) && strings.ContainsAny(seg, "0123456789")
}

// validateCaptureEvents enforces the bounds on console/network events and
// scrubs them in place. Other event types are not touched. The scrub runs
// before the batch digest is taken, and is deterministic, so retries of one
// batch still digest identically.
func validateCaptureEvents(in *IngestInput) error {
	var consoleN, networkN int
	for i := range in.Events {
		ev := &in.Events[i]
		switch ev.Type {
		case EventConsole:
			consoleN++
			if consoleN > maxConsoleEventsPerBatch {
				return neutron.ErrBadRequest(fmt.Sprintf("too many console events in one batch (max %d)", maxConsoleEventsPerBatch))
			}
			if err := validateConsoleData(ev.Data); err != nil {
				return err
			}
			m := ev.Data.(map[string]any)
			m["message"] = cutUTF8(ScrubConsoleText(m["message"].(string)), maxConsoleMessageBytes)
		case EventNetwork:
			networkN++
			if networkN > maxNetworkEventsPerBatch {
				return neutron.ErrBadRequest(fmt.Sprintf("too many network events in one batch (max %d)", maxNetworkEventsPerBatch))
			}
			if err := validateNetworkData(ev.Data); err != nil {
				return err
			}
			m := ev.Data.(map[string]any)
			clean, _ := scrubNetworkURL(m["url"].(string))
			m["url"] = clean
		}
	}
	return nil
}

func validateConsoleData(data any) error {
	m, ok := data.(map[string]any)
	if !ok {
		return neutron.ErrBadRequest("console event data must be an object")
	}
	for k := range m {
		if !consoleKeys[k] {
			return neutron.ErrBadRequest(fmt.Sprintf("console event has unknown field %q", k))
		}
	}
	level, _ := m["level"].(string)
	if !consoleLevels[level] {
		return neutron.ErrBadRequest("console event level must be log, info, warn or error")
	}
	msg, ok := m["message"].(string)
	if !ok {
		return neutron.ErrBadRequest("console event message must be a string")
	}
	if len(msg) > maxConsoleMessageBytes {
		return neutron.ErrBadRequest(fmt.Sprintf("console event message exceeds %d bytes", maxConsoleMessageBytes))
	}
	if !utf8.ValidString(msg) {
		return neutron.ErrBadRequest("console event message must be valid UTF-8")
	}
	if t, present := m["truncated"]; present {
		if _, isBool := t.(bool); !isBool {
			return neutron.ErrBadRequest("console event truncated must be a boolean")
		}
	}
	return nil
}

func validateNetworkData(data any) error {
	m, ok := data.(map[string]any)
	if !ok {
		return neutron.ErrBadRequest("network event data must be an object")
	}
	for k := range m {
		if !networkKeys[k] {
			return neutron.ErrBadRequest(fmt.Sprintf("network event has unknown field %q", k))
		}
	}
	method, _ := m["method"].(string)
	if !methodRe.MatchString(method) {
		return neutron.ErrBadRequest("network event method must be 1-16 uppercase letters")
	}
	raw, _ := m["url"].(string)
	if _, ok := scrubNetworkURL(raw); !ok {
		return neutron.ErrBadRequest("network event url must be an absolute http(s) URL of at most 2048 bytes")
	}
	// status 0 = network failure / aborted.
	if _, ok := numField(m["status"], 0, 999); !ok {
		return neutron.ErrBadRequest("network event status must be an integer 0-999")
	}
	if _, ok := numField(m["duration_ms"], 0, maxNetworkDurationMS); !ok {
		return neutron.ErrBadRequest("network event duration_ms must be an integer 0-3600000")
	}
	if sz, present := m["size"]; present {
		if _, ok := numField(sz, 0, maxNetworkSizeBytes); !ok {
			return neutron.ErrBadRequest("network event size must be a non-negative integer")
		}
	}
	if k, present := m["kind"]; present {
		if ks, _ := k.(string); ks != "fetch" && ks != "xhr" {
			return neutron.ErrBadRequest("network event kind must be fetch or xhr")
		}
	}
	return nil
}
