package observe

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Breadcrumb is a record of something that happened before an error. The
// JSON shape matches the server's error ingest (internal/errors
// Breadcrumb): type, category, message, data, timestamp (Unix ms), level.
type Breadcrumb struct {
	Type     string         `json:"type"`
	Category string         `json:"category"`
	Message  string         `json:"message"`
	Data     map[string]any `json:"data,omitempty"`
	// Timestamp is Unix epoch milliseconds; zero means "now" on AddBreadcrumb.
	Timestamp int64 `json:"timestamp"`
	// Level is one of debug, info, warning, error. Empty means info.
	Level string `json:"level,omitempty"`
}

const (
	// DefaultMaxBreadcrumbs is the ring size when Options.MaxBreadcrumbs is 0.
	DefaultMaxBreadcrumbs = 100

	maxBreadcrumbMessage = 256
	maxBreadcrumbData    = 1024
	// maxBreadcrumbAttach bounds the breadcrumbs attached to one payload;
	// the oldest are dropped first.
	maxBreadcrumbAttach = 32 * 1024
	maxBreadcrumbCap    = 1000
)

// breadcrumbRing is a mutex-guarded bounded buffer (oldest dropped first).
type breadcrumbRing struct {
	mu     sync.Mutex
	max    int
	items  []Breadcrumb
	before func(Breadcrumb) *Breadcrumb
}

func newBreadcrumbRing(max int, before func(Breadcrumb) *Breadcrumb) *breadcrumbRing {
	if max <= 0 {
		max = DefaultMaxBreadcrumbs
	}
	if max > maxBreadcrumbCap {
		max = maxBreadcrumbCap
	}
	return &breadcrumbRing{max: max, before: before}
}

func clampStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Do not cut a UTF-8 sequence in half.
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}

func normLevel(l string) string {
	switch l {
	case "debug", "info", "warning", "error":
		return l
	}
	return "info"
}

// cloneBreadcrumbData takes an owned JSON snapshot, size-capped.
func cloneBreadcrumbData(d map[string]any) map[string]any {
	if len(d) == 0 {
		return nil
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return map[string]any{"_unserializable": true}
	}
	if len(raw) > maxBreadcrumbData {
		return map[string]any{"_truncated": true, "_bytes": len(raw)}
	}
	var out map[string]any
	if json.Unmarshal(raw, &out) != nil {
		return map[string]any{"_unserializable": true}
	}
	return out
}

func normalizeBreadcrumb(b Breadcrumb) Breadcrumb {
	if b.Type == "" {
		b.Type = "default"
	}
	if b.Category == "" {
		b.Category = "default"
	}
	if b.Timestamp == 0 {
		b.Timestamp = time.Now().UnixMilli()
	}
	b.Type = clampStr(b.Type, 64)
	b.Category = clampStr(b.Category, 64)
	b.Message = clampStr(b.Message, maxBreadcrumbMessage)
	b.Level = normLevel(b.Level)
	b.Data = cloneBreadcrumbData(b.Data)
	return b
}

func (r *breadcrumbRing) add(b Breadcrumb) {
	defer func() { _ = recover() }() // recording must never panic into the caller
	b = normalizeBreadcrumb(b)
	if r.before != nil {
		out := r.before(b) // a panicking hook drops the crumb (fail closed)
		if out == nil {
			return
		}
		b = normalizeBreadcrumb(*out)
	}
	r.mu.Lock()
	r.items = append(r.items, b)
	if over := len(r.items) - r.max; over > 0 {
		r.items = append(r.items[:0:0], r.items[over:]...)
	}
	r.mu.Unlock()
}

// snapshot returns an oldest-first deep copy within the attach byte budget.
func (r *breadcrumbRing) snapshot() []Breadcrumb {
	r.mu.Lock()
	items := make([]Breadcrumb, len(r.items))
	copy(items, r.items)
	r.mu.Unlock()
	used := 2
	start := len(items)
	for i := len(items) - 1; i >= 0; i-- {
		raw, err := json.Marshal(items[i])
		if err != nil || used+len(raw)+1 > maxBreadcrumbAttach {
			break
		}
		used += len(raw) + 1
		start = i
	}
	out := items[start:]
	for i := range out {
		if out[i].Data != nil {
			d := make(map[string]any, len(out[i].Data))
			for k, v := range out[i].Data {
				d[k] = v
			}
			out[i].Data = d
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (r *breadcrumbRing) clear() {
	r.mu.Lock()
	r.items = nil
	r.mu.Unlock()
}

// AddBreadcrumb records a breadcrumb that is attached to later captured
// errors and messages. Safe for concurrent use; never panics or blocks on
// the network. The buffer is a process-wide ring per Client
// (Options.MaxBreadcrumbs, default 100), not request-scoped.
func (c *Client) AddBreadcrumb(b Breadcrumb) { c.crumbs.add(b) }

// ClearBreadcrumbs drops every buffered breadcrumb.
func (c *Client) ClearBreadcrumbs() { c.crumbs.clear() }

// Breadcrumbs returns a copy of the buffered breadcrumbs, oldest first.
func (c *Client) Breadcrumbs() []Breadcrumb { return c.crumbs.snapshot() }

// CaptureMessage submits a message (no error value) as an issue event.
// Level defaults to "info"; override with WithLevel. Buffered breadcrumbs
// are attached.
func (c *Client) CaptureMessage(msg string, opts ...ExceptionOption) error {
	payload := ErrorPayload{
		SiteID:      c.opts.SiteID,
		ErrorType:   "Message",
		ErrorValue:  msg,
		ReleaseTag:  c.opts.Release,
		Environment: c.opts.Environment,
		Level:       "info",
		Breadcrumbs: c.crumbs.snapshot(),
	}
	for _, opt := range opts {
		opt(&payload)
	}
	return c.post(context.Background(), "/api/v1/errors", payload)
}

// ---- feature flags ----

// FlagDefault is what EvaluateFlag returns when the server cannot decide.
type FlagDefault struct {
	Enabled bool
	Variant string
}

// FlagResult is the outcome of EvaluateFlag.
type FlagResult struct {
	Key     string
	Enabled bool
	Variant string
	// Reason is the server reason ("evaluated"), or "default" when Default was used.
	Reason string
	Detail string
	// Source is "server" or "default".
	Source string
	// Err is why the default was used (nil when Source is "server").
	Err error
}

// FlagOption customizes EvaluateFlag.
type FlagOption func(*flagReq)

type flagReq struct {
	userID  string
	attrs   map[string]string
	def     FlagDefault
	timeout time.Duration
}

// WithFlagUser sets the user id used for rollout bucketing.
func WithFlagUser(id string) FlagOption { return func(r *flagReq) { r.userID = id } }

// WithFlagAttributes sets the targeting context.
func WithFlagAttributes(a map[string]string) FlagOption { return func(r *flagReq) { r.attrs = a } }

// WithFlagDefault sets the value returned on any failure (default: disabled).
func WithFlagDefault(d FlagDefault) FlagOption { return func(r *flagReq) { r.def = d } }

// WithFlagTimeout overrides the request timeout (default 3 s).
func WithFlagTimeout(d time.Duration) FlagOption { return func(r *flagReq) { r.timeout = d } }

// EvaluateFlag asks the server to evaluate a feature flag
// (POST /api/v1/flags/evaluate). It never fails: a network error, timeout,
// non-2xx status, malformed body, or a server fail-safe answer (reason
// "unavailable" or "invalid") yields the caller's default with Source
// "default" and the cause in Err. There is no client-side cache and no
// exposure is recorded; the server logs each enabled evaluation, so cache
// results yourself on hot paths.
func (c *Client) EvaluateFlag(ctx context.Context, key string, opts ...FlagOption) (res FlagResult) {
	req := flagReq{timeout: 3 * time.Second}
	for _, o := range opts {
		o(&req)
	}
	fallback := func(err error) FlagResult {
		return FlagResult{Key: key, Enabled: req.def.Enabled, Variant: req.def.Variant, Reason: "default", Source: "default", Err: err}
	}
	defer func() {
		if r := recover(); r != nil {
			res = fallback(fmt.Errorf("observe: flag evaluation panicked: %v", r))
		}
	}()
	if strings.TrimSpace(key) == "" {
		return fallback(fmt.Errorf("observe: flag key required"))
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if req.timeout <= 0 {
		req.timeout = 3 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, req.timeout)
	defer cancel()
	ctxMap := req.attrs
	if ctxMap == nil {
		ctxMap = map[string]string{}
	}
	body := map[string]any{
		"site_id":  c.opts.SiteID,
		"flag_key": key,
		"user_id":  req.userID,
		"context":  ctxMap,
	}
	var out struct {
		Enabled *bool  `json:"enabled"`
		Variant string `json:"variant"`
		Reason  string `json:"reason"`
		Detail  string `json:"detail"`
	}
	if err := c.postJSON(ctx, "/api/v1/flags/evaluate", body, &out); err != nil {
		return fallback(err)
	}
	if out.Enabled == nil {
		return fallback(fmt.Errorf("observe: malformed flag response"))
	}
	if out.Reason != "evaluated" {
		return fallback(fmt.Errorf("observe: server reason %q", out.Reason))
	}
	return FlagResult{Key: key, Enabled: *out.Enabled, Variant: out.Variant, Reason: out.Reason, Detail: out.Detail, Source: "server"}
}
