package errors

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

// StackFrame represents a single frame in a stack trace.
type StackFrame struct {
	Filename string `json:"filename"`
	Function string `json:"function"`
	Lineno   int    `json:"lineno,omitempty"`
	Colno    int    `json:"colno,omitempty"`
	InApp    bool   `json:"in_app"`
}

// GroupHash computes a fingerprint for an error event.
// Priority:
//  1. If there are in-app frames: MD5(error_type + sorted in-app frame filenames+functions)
//  2. Fallback: MD5(error_type + parameterized message)
func GroupHash(errorType, message string, frames []StackFrame) string {
	inApp := filterInApp(frames)
	if len(inApp) > 0 {
		return hashTypeAndFrames(errorType, inApp)
	}
	return hashTypeAndMessage(errorType, message)
}

// filterInApp returns only in-app frames (user code, not library/browser internals).
func filterInApp(frames []StackFrame) []StackFrame {
	var result []StackFrame
	for _, f := range frames {
		if f.InApp {
			result = append(result, f)
		}
	}
	return result
}

// hashTypeAndFrames builds: MD5(errorType + "|" + frame1_basename:func + "|" + ...)
func hashTypeAndFrames(errorType string, frames []StackFrame) string {
	h := md5.New()
	fmt.Fprintf(h, "%s", errorType)
	for _, f := range frames {
		base := filepath.Base(f.Filename)
		// Strip query strings and hashes from filenames (bundler fingerprints)
		if idx := strings.IndexAny(base, "?#"); idx != -1 {
			base = base[:idx]
		}
		fn := f.Function
		if fn == "" {
			fn = "<anonymous>"
		}
		fmt.Fprintf(h, "|%s:%s", base, fn)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// hashTypeAndMessage builds: MD5(errorType + "|" + parameterized_message)
func hashTypeAndMessage(errorType, message string) string {
	h := md5.New()
	fmt.Fprintf(h, "%s|%s", errorType, ParameterizeMessage(message))
	return hex.EncodeToString(h.Sum(nil))
}

// GroupHashRageClick groups rage-click auto-issues on (type + URL path + selector)
// so repeated rage clicks on the same target collapse into a single issue.
// The query string and fragment are stripped from the URL to avoid splitting
// the same logical page across many issues.
func GroupHashRageClick(rawURL, selector string) string {
	page := stripURL(rawURL)
	h := md5.New()
	fmt.Fprintf(h, "RageClick|%s|%s", page, selector)
	return hex.EncodeToString(h.Sum(nil))
}

// stripURL returns the URL with query string and fragment removed.
func stripURL(u string) string {
	if i := strings.IndexAny(u, "?#"); i != -1 {
		return u[:i]
	}
	return u
}

// Parameterization patterns — replace volatile values with placeholders
// so that identical errors with different runtime values group together.
var paramPatterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	// UUIDs (v4 and v1-5 variants)
	{regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`), "{uuid}"},
	// IPv4
	{regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`), "{ip}"},
	// IPv6 (simplified)
	{regexp.MustCompile(`\b[0-9a-fA-F]{1,4}(:[0-9a-fA-F]{1,4}){7}\b`), "{ipv6}"},
	// Email addresses
	{regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`), "{email}"},
	// URLs
	{regexp.MustCompile(`https?://[^\s"'>]+`), "{url}"},
	// Hex hashes (32+ chars)
	{regexp.MustCompile(`\b[0-9a-fA-F]{32,}\b`), "{hash}"},
	// Large numbers (6+ digits — likely IDs, timestamps, etc.)
	{regexp.MustCompile(`\b\d{6,}\b`), "{num}"},
	// Quoted strings (single or double)
	{regexp.MustCompile(`"[^"]{20,}"`), `"{str}"`},
	{regexp.MustCompile(`'[^']{20,}'`), `'{str}'`},
}

// ParameterizeMessage replaces volatile runtime values in an error message
// with stable placeholders so that semantically identical errors group together.
func ParameterizeMessage(msg string) string {
	for _, p := range paramPatterns {
		msg = p.re.ReplaceAllString(msg, p.repl)
	}
	return msg
}

// IssueTitle generates a human-readable title for an issue from the error type and message.
func IssueTitle(errorType, message string) string {
	if errorType == "" && message == "" {
		return "Unknown Error"
	}
	if errorType == "" {
		return truncate(message, 120)
	}
	if message == "" {
		return errorType
	}
	return truncate(fmt.Sprintf("%s: %s", errorType, message), 120)
}

// IssueCulprit extracts the most likely culprit from stack frames.
// Returns the topmost in-app frame's function@filename, or empty string.
func IssueCulprit(frames []StackFrame) string {
	for _, f := range frames {
		if f.InApp {
			base := filepath.Base(f.Filename)
			if f.Function != "" {
				return fmt.Sprintf("%s in %s", f.Function, base)
			}
			return base
		}
	}
	if len(frames) > 0 {
		f := frames[0]
		base := filepath.Base(f.Filename)
		if f.Function != "" {
			return fmt.Sprintf("%s in %s", f.Function, base)
		}
		return base
	}
	return ""
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= 3 {
		return ""
	}
	cut := max - 3
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}
