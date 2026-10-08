package sentrycompat

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	obserrors "github.com/useteploy/teploy-observe/internal/errors"
)

// ProducerID namespaces Sentry event ids in Observe's error inbox so a Sentry
// event_id can never collide with an Observe-native one.
const ProducerID = "sentry"

// Field caps limit individual values. mapEvent also enforces a serialized
// aggregate budget before admission into the 256 KiB error record envelope.
const (
	maxFrames        = 100
	maxBreadcrumbs   = 100
	maxCrumbData     = 2 << 10
	maxContextEntry  = 8 << 10
	maxContextsTotal = 32 << 10
	maxExtraTotal    = 16 << 10
	maxTags          = 100
	maxStr           = 2048
	maxMessage       = 8192
	maxChain         = 10
)

var (
	errNoContent = errors.New("event has no exception or message")
	hexID        = regexp.MustCompile(`^[0-9a-fA-F]{1,32}$`)
	validEventID = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)
	validLevels  = map[string]bool{"fatal": true, "error": true, "warning": true, "log": true, "info": true, "debug": true}
)

// mapEvent converts one Sentry event payload (the JSON of an `event` item or
// a /store/ body) into Observe's error input. It returns the stable event id
// used for idempotency. It never reads the clock or randomness: the same
// bytes always map to the same ErrorInput, so a retried envelope has the same
// payload digest and dedupes instead of conflicting.
func mapEvent(payload []byte, headerEventID string) (obserrors.ErrorInput, string, error) {
	var ev map[string]any
	if err := json.Unmarshal(payload, &ev); err != nil || ev == nil {
		return obserrors.ErrorInput{}, "", fmt.Errorf("event is not a JSON object")
	}

	in := obserrors.ErrorInput{
		Level:      "error",
		Handled:    true,
		ProducerID: ProducerID,
	}
	// Honor the SDK's own event time (float seconds, ms, or RFC 3339 via
	// toMillis). Absent stays zero: server arrival time is stored.
	in.ClientTimestamp = toMillis(ev["timestamp"])

	if l := strings.ToLower(str(ev["level"])); validLevels[l] {
		in.Level = l
	}
	in.ReleaseTag = clip(str(ev["release"]), 200)
	in.Environment = clip(str(ev["environment"]), 100)

	// Identity: the event's own id, else the envelope header's, else a digest
	// of the payload (stable across retries of the same bytes).
	eventID := strings.ReplaceAll(strings.ToLower(str(ev["event_id"])), "-", "")
	if !validEventID.MatchString(eventID) {
		eventID = strings.ReplaceAll(strings.ToLower(headerEventID), "-", "")
	}
	if !validEventID.MatchString(eventID) {
		sum := sha256.Sum256(payload)
		eventID = "d" + hex.EncodeToString(sum[:16])
	}
	in.EventID = eventID

	// Exception: Sentry orders chained exceptions oldest-first, so the last
	// value is the one that surfaced.
	vals := exceptionValues(ev["exception"])
	var primary map[string]any
	if len(vals) > 0 {
		primary = vals[len(vals)-1]
	}
	switch {
	case primary != nil:
		in.ErrorType = clip(str(primary["type"]), 256)
		in.ErrorValue = clip(str(primary["value"]), maxMessage)
		if in.ErrorType == "" {
			in.ErrorType = "Error"
		}
		if mech, ok := primary["mechanism"].(map[string]any); ok {
			in.Mechanism = clip(str(mech["type"]), 100)
			if h, ok := mech["handled"].(bool); ok {
				in.Handled = h
			}
		}
		if st, ok := primary["stacktrace"].(map[string]any); ok {
			in.StackTrace = mapFrames(st["frames"])
		}
	default:
		msg := messageOf(ev)
		if msg == "" {
			return obserrors.ErrorInput{}, "", errNoContent
		}
		in.ErrorType = "Message"
		in.ErrorValue = clip(msg, maxMessage)
		if st, ok := ev["stacktrace"].(map[string]any); ok {
			in.StackTrace = mapFrames(st["frames"])
		}
	}

	// Request URL (the page, for browser SDKs). Sanitized again server-side
	// by CapturedURL.
	if req, ok := ev["request"].(map[string]any); ok {
		in.URL = clip(str(req["url"]), 2048)
	}

	// User: only the identifier crosses, as distinct_id, which the error
	// service HASHES with the per-site salt. The raw user object (email, IP)
	// is deliberately not kept in contexts - the shim does keep it; we do
	// not, so hashing is not defeated by a verbatim copy.
	if u, ok := ev["user"].(map[string]any); ok {
		for _, k := range []string{"id", "username", "email"} {
			if v := scalarStr(u[k]); v != "" {
				in.DistinctID = clip(v, 200)
				break
			}
		}
	}

	// Contexts: sentry contexts + tags + sdk bookkeeping.
	ctxs := map[string]any{}
	if c, ok := ev["contexts"].(map[string]any); ok {
		fillCapped(ctxs, c, maxContextEntry, maxContextsTotal)
		in.Browser = nameVersion(c["browser"])
		in.OS = nameVersion(c["os"])
		in.Device = deviceName(c["device"])
		if tr, ok := c["trace"].(map[string]any); ok {
			if t := str(tr["trace_id"]); hexID.MatchString(t) {
				in.TraceID = t
			}
			if s := str(tr["span_id"]); hexID.MatchString(s) {
				in.SpanID = s
			}
		}
	}
	if tags := mapTags(ev["tags"]); len(tags) > 0 {
		ctxs["tags"] = tags
	}
	meta := map[string]any{}
	for _, k := range []string{"platform", "logger", "transaction", "server_name", "dist"} {
		if v := clip(str(ev[k]), 256); v != "" {
			meta[k] = v
		}
	}
	if sdk, ok := ev["sdk"].(map[string]any); ok {
		meta["sdk"] = clip(str(sdk["name"]), 100) + "/" + clip(str(sdk["version"]), 50)
	}
	if len(vals) > 1 {
		chain := []map[string]string{}
		for _, v := range vals[:len(vals)-1] {
			if len(chain) >= maxChain {
				break
			}
			chain = append(chain, map[string]string{"type": clip(str(v["type"]), 128), "value": clip(str(v["value"]), 512)})
		}
		meta["exception_chain"] = chain
	}
	if len(meta) > 0 {
		ctxs["sentry"] = meta
	}
	if len(ctxs) > 0 {
		in.Contexts = ctxs
	}

	if ex, ok := ev["extra"].(map[string]any); ok {
		extra := map[string]any{}
		fillCapped(extra, ex, maxContextEntry, maxExtraTotal)
		if len(extra) > 0 {
			in.Extra = extra
		}
	}

	// Custom fingerprint. "{{ default }}" asks Sentry to splice its own
	// default hash in; Observe's grouping differs, so any use falls back to
	// Observe's default grouping rather than guessing.
	if fp, ok := ev["fingerprint"].([]any); ok && len(fp) > 0 && len(fp) <= 20 {
		parts := make([]string, 0, len(fp))
		usable := true
		for _, p := range fp {
			s := clip(scalarStr(p), 200)
			if s == "" || strings.Contains(s, "{{") {
				usable = false
				break
			}
			parts = append(parts, s)
		}
		if usable {
			in.Fingerprint = parts
		}
	}

	in.Breadcrumbs = mapBreadcrumbs(ev["breadcrumbs"])
	// Budget the complete serialized record, keeping the newest breadcrumbs.
	// Leave headroom for site identity and scrubber expansion before WAL admission.
	for {
		raw, err := json.Marshal(in)
		if err != nil {
			return obserrors.ErrorInput{}, "", err
		}
		if len(raw) <= 192<<10 {
			break
		}
		if len(in.Breadcrumbs) > 0 {
			in.Breadcrumbs = in.Breadcrumbs[1:]
			continue
		}
		return obserrors.ErrorInput{}, "", fmt.Errorf("mapped event exceeds record budget")
	}
	return in, eventID, nil
}

// exceptionValues accepts {"values":[...]} and the bare-list form.
func exceptionValues(v any) []map[string]any {
	var list []any
	switch t := v.(type) {
	case map[string]any:
		list, _ = t["values"].([]any)
	case []any:
		list = t
	}
	var out []map[string]any
	for _, e := range list {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}
	if len(out) > 50 {
		out = out[len(out)-50:]
	}
	return out
}

func messageOf(ev map[string]any) string {
	if le, ok := ev["logentry"].(map[string]any); ok {
		if s := str(le["formatted"]); s != "" {
			return s
		}
		if s := str(le["message"]); s != "" {
			return s
		}
	}
	switch m := ev["message"].(type) {
	case string:
		return m
	case map[string]any:
		if s := str(m["formatted"]); s != "" {
			return s
		}
		return str(m["message"])
	}
	return ""
}

// mapFrames converts Sentry frames (oldest call first) to Observe frames
// (innermost first - IssueCulprit reads the first in-app frame). Works for
// python, node, browser (minified, filename is the script URL) and go frames.
func mapFrames(v any) []obserrors.StackFrame {
	list, _ := v.([]any)
	if len(list) == 0 {
		return nil
	}
	if len(list) > maxFrames {
		list = list[len(list)-maxFrames:] // keep the innermost frames
	}
	anyInApp := false
	for _, f := range list {
		if m, ok := f.(map[string]any); ok {
			if _, has := m["in_app"].(bool); has {
				anyInApp = true
				break
			}
		}
	}
	out := make([]obserrors.StackFrame, 0, len(list))
	for i := len(list) - 1; i >= 0; i-- {
		m, ok := list[i].(map[string]any)
		if !ok {
			continue
		}
		fn := firstNonEmpty(str(m["filename"]), str(m["abs_path"]), str(m["module"]))
		fr := obserrors.StackFrame{
			Filename: clip(fn, 512),
			Function: clip(firstNonEmpty(str(m["function"]), str(m["module"])), 256),
			Lineno:   clampInt(m["lineno"]),
			Colno:    clampInt(m["colno"]),
		}
		if b, has := m["in_app"].(bool); has {
			fr.InApp = b
		} else if !anyInApp {
			// Browser SDKs omit in_app. Treat as app code unless it is
			// plainly a library path.
			fr.InApp = !libraryPath(firstNonEmpty(str(m["abs_path"]), fn))
		}
		out = append(out, fr)
	}
	return out
}

var libraryMarkers = []string{"node_modules", "site-packages", "dist-packages", "/usr/lib/", "/usr/local/lib/", "/vendor/", "/pkg/mod/", "/go/src/", "node:internal", "webpack/bootstrap"}

func libraryPath(p string) bool {
	for _, m := range libraryMarkers {
		if strings.Contains(p, m) {
			return true
		}
	}
	return false
}

func mapBreadcrumbs(v any) []obserrors.Breadcrumb {
	var list []any
	switch t := v.(type) {
	case map[string]any:
		list, _ = t["values"].([]any)
	case []any:
		list = t
	}
	if len(list) > maxBreadcrumbs {
		list = list[len(list)-maxBreadcrumbs:] // keep the most recent
	}
	var out []obserrors.Breadcrumb
	for _, c := range list {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		b := obserrors.Breadcrumb{
			Type:      clip(firstNonEmpty(str(m["type"]), "default"), 64),
			Category:  clip(str(m["category"]), 128),
			Message:   clip(str(m["message"]), 1024),
			Timestamp: toMillis(m["timestamp"]),
		}
		if l := strings.ToLower(str(m["level"])); validLevels[l] {
			b.Level = l
		}
		if d, ok := m["data"]; ok && d != nil {
			if raw, err := json.Marshal(d); err == nil {
				if len(raw) <= maxCrumbData {
					b.Data = d
				} else {
					b.Data = map[string]any{"truncated": true}
				}
			}
		}
		out = append(out, b)
	}
	return out
}

// mapTags accepts {"k":"v"} and [["k","v"],...] / [{"key","value"}].
func mapTags(v any) map[string]string {
	out := map[string]string{}
	add := func(k, val string) {
		if k != "" && len(out) < maxTags {
			out[clip(k, 128)] = clip(val, 256)
		}
	}
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys) // deterministic truncation
		for _, k := range keys {
			add(k, scalarStr(t[k]))
		}
	case []any:
		for _, e := range t {
			switch p := e.(type) {
			case []any:
				if len(p) == 2 {
					add(scalarStr(p[0]), scalarStr(p[1]))
				}
			case map[string]any:
				add(str(p["key"]), scalarStr(p["value"]))
			}
		}
	}
	return out
}

// fillCapped copies src entries into dst in key order, replacing any entry
// over entryMax with a marker and stopping once totalMax is reached.
func fillCapped(dst, src map[string]any, entryMax, totalMax int) {
	keys := make([]string, 0, len(src))
	for k := range src {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	total := 0
	for _, k := range keys {
		raw, err := json.Marshal(src[k])
		if err != nil {
			continue
		}
		val := src[k]
		if len(raw) > entryMax {
			val = map[string]any{"truncated": true}
			raw = []byte(`{"truncated":true}`)
		}
		if total+len(raw)+len(k) > totalMax {
			break
		}
		total += len(raw) + len(k)
		dst[clip(k, 128)] = val
	}
}

func nameVersion(v any) string {
	m, _ := v.(map[string]any)
	n := clip(str(m["name"]), 100)
	if n == "" {
		return ""
	}
	if ver := clip(scalarStr(m["version"]), 50); ver != "" {
		return n + " " + ver
	}
	return n
}

func deviceName(v any) string {
	m, _ := v.(map[string]any)
	return clip(firstNonEmpty(str(m["model"]), str(m["name"]), str(m["family"])), 100)
}

// toMillis converts a Sentry timestamp (float seconds, ms, or RFC 3339) to
// epoch milliseconds, 0 when absent or unusable.
func toMillis(v any) int64 {
	switch t := v.(type) {
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) || t <= 0 {
			return 0
		}
		if t > 1e11 { // already milliseconds
			if t >= float64(math.MaxInt64) {
				return math.MaxInt64
			}
			return int64(t)
		}
		return int64(t * 1000)
	case string:
		if ts, err := time.Parse(time.RFC3339Nano, t); err == nil {
			return ts.UnixMilli()
		}
	}
	return 0
}

func clampInt(v any) int {
	f, ok := v.(float64)
	if !ok || math.IsNaN(f) || f < 0 || f > 1e9 {
		return 0
	}
	return int(f)
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// scalarStr renders strings and numbers/bools; nested values are ignored.
func scalarStr(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	}
	return ""
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// clip truncates to max bytes on a rune boundary and strips NULs.
func clip(s string, max int) string {
	if strings.IndexByte(s, 0) >= 0 {
		s = strings.ReplaceAll(s, "\x00", "")
	}
	if len(s) <= max {
		return s
	}
	s = s[:max]
	for !utf8.ValidString(s) && len(s) > 0 {
		s = s[:len(s)-1]
	}
	return s
}
