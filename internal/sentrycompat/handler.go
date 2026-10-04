// Package sentrycompat accepts the Sentry wire protocol (envelope + legacy
// store endpoints) so stock Sentry SDKs can point their DSN at Observe.
//
// DSN format: https://<observe_api_key>@<host>/<site_id>. The DSN public key
// is an Observe telemetry API key; the DSN "project" is the Observe site id
// (or any numeric alias, see checkProject). Only error events are applied;
// every other item type is acknowledged and dropped.
package sentrycompat

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/useteploy/teploy-observe/internal/auth"
	"github.com/useteploy/teploy-observe/internal/authguard"
	obserrors "github.com/useteploy/teploy-observe/internal/errors"
	"github.com/useteploy/teploy-observe/internal/ingest"
)

// KeyValidator is *auth.AuthService's key check.
type KeyValidator interface {
	ValidateAPIKey(ctx context.Context, key string) (auth.ValidatedKey, error)
}

// ipKeyValidator is implemented by *authguard.Guard: a validator that also
// takes the client IP so failed attempts can be limited before the store is
// consulted. A plain KeyValidator keeps working without that protection.
type ipKeyValidator interface {
	Validate(ctx context.Context, key, clientIP string) (auth.ValidatedKey, error)
}

// maxPreAuthBytes bounds what is read (and decompressed) before the key is
// known. It only needs to cover the envelope-header line holding the DSN; the
// rest of the body is read after authentication succeeds.
const maxPreAuthBytes = 64 << 10

// ErrorSink is *errors.ErrorBuffer's admission method.
type ErrorSink interface {
	Push(siteID string, input obserrors.ErrorInput) error
}

// Limiter is *ingest.RateLimiter's admission check (per site and client IP).
type Limiter interface {
	Allow(siteID, ip string) bool
}

// knownIgnored is the closed set of item types counted by name. Anything
// else collapses into "unknown" so a client cannot grow the counter map
// with invented type names.
var knownIgnored = map[string]bool{
	"transaction": true, "span": true, "session": true, "sessions": true,
	"attachment": true, "client_report": true, "profile": true,
	"profile_chunk": true, "replay_event": true, "replay_recording": true,
	"check_in": true, "log": true, "user_report": true, "feedback": true,
	"statsd": true, "metric_buckets": true, "trace_metric": true,
	"event_ignored": true,
}

// Handler serves the Sentry ingest routes.
type Handler struct {
	Keys    KeyValidator
	Sink    ErrorSink
	Limiter Limiter // nil disables per-site limiting
	Logger  *slog.Logger
	// RetryAfter is advertised on 429/503 (default 5s).
	RetryAfter time.Duration

	accepted  atomic.Int64
	malformed atomic.Int64
	oversize  atomic.Int64
	dedup     atomic.Int64
	conflicts atomic.Int64
	mu        sync.Mutex
	ignored   map[string]int64
}

// Stats is a point-in-time counter snapshot.
type Stats struct {
	Accepted      int64            `json:"accepted"`
	Deduped       int64            `json:"deduped"`
	Conflicts     int64            `json:"conflicts"`
	Malformed     int64            `json:"malformed"`
	Oversize      int64            `json:"oversize"`
	IgnoredByType map[string]int64 `json:"ignored_by_type"`
}

// Stats returns the counters.
func (h *Handler) Stats() Stats {
	h.mu.Lock()
	ign := make(map[string]int64, len(h.ignored))
	for k, v := range h.ignored {
		ign[k] = v
	}
	h.mu.Unlock()
	return Stats{
		Accepted: h.accepted.Load(), Deduped: h.dedup.Load(), Conflicts: h.conflicts.Load(),
		Malformed: h.malformed.Load(), Oversize: h.oversize.Load(), IgnoredByType: ign,
	}
}

func (h *Handler) countIgnored(typ string) {
	if !knownIgnored[typ] {
		typ = "unknown"
	}
	h.mu.Lock()
	if h.ignored == nil {
		h.ignored = map[string]int64{}
	}
	h.ignored[typ]++
	h.mu.Unlock()
}

func (h *Handler) retryAfter() time.Duration {
	if h.RetryAfter > 0 {
		return h.RetryAfter
	}
	return 5 * time.Second
}

func (h *Handler) logger() *slog.Logger {
	if h.Logger != nil {
		return h.Logger
	}
	return slog.Default()
}

// Envelope serves POST /api/{project_id}/envelope/.
func (h *Handler) Envelope(w http.ResponseWriter, r *http.Request) { h.serve(w, r, false) }

// Store serves POST /api/{project_id}/store/ (legacy single-event JSON).
func (h *Handler) Store(w http.ResponseWriter, r *http.Request) { h.serve(w, r, true) }

func setCORS(w http.ResponseWriter) {
	hd := w.Header()
	hd.Set("Access-Control-Allow-Origin", "*")
	hd.Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	hd.Set("Access-Control-Allow-Headers", "Content-Type, Content-Encoding, X-Sentry-Auth, X-API-Key")
	hd.Set("Access-Control-Expose-Headers", "X-Sentry-Rate-Limits, Retry-After")
	hd.Set("Access-Control-Max-Age", "86400")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, detail string) {
	writeJSON(w, status, map[string]string{"detail": detail})
}

func (h *Handler) limited(w http.ResponseWriter, why string) {
	secs := strconv.Itoa(int(h.retryAfter() / time.Second))
	w.Header().Set("Retry-After", secs)
	// retry_after:categories:scope - empty categories means every category.
	w.Header().Set("X-Sentry-Rate-Limits", secs+"::key")
	fail(w, http.StatusTooManyRequests, why)
}

func (h *Handler) serve(w http.ResponseWriter, r *http.Request, legacyStore bool) {
	setCORS(w)
	projectID := r.PathValue("project_id")

	// 1. Credentials, from the cheap places first (query, header). The DSN
	// in the envelope header is the last resort and needs a bounded peek.
	key := keyFromRequest(r)

	// 2. The body is capped. With a header/query key we authenticate before
	// touching it; with only the envelope-header DSN as a credential just a
	// 64 KiB prefix is read and decompressed pre-auth, and the remainder is
	// read only after the key validated.
	if r.ContentLength > MaxEnvelopeBytes {
		fail(w, http.StatusRequestEntityTooLarge, "payload too large")
		return
	}
	body := http.MaxBytesReader(w, r.Body, MaxEnvelopeBytes)
	var pre, raw []byte
	readFail := func(err error) {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			fail(w, http.StatusRequestEntityTooLarge, "payload too large")
		} else {
			fail(w, http.StatusBadRequest, "could not read body")
		}
	}
	readBody := func() bool {
		rest, err := io.ReadAll(body)
		if err != nil {
			readFail(err)
			return false
		}
		raw = append(pre, rest...)
		return true
	}
	if key == "" && !legacyStore {
		var err error
		pre, err = io.ReadAll(io.LimitReader(body, maxPreAuthBytes))
		if err != nil {
			readFail(err)
			return
		}
		line := peekHeaderLine(pre, r.Header.Get("Content-Encoding"), maxHeaderLine)
		var hdr struct {
			DSN string `json:"dsn"`
		}
		if json.Unmarshal(line, &hdr) == nil {
			key = keyFromDSN(hdr.DSN)
		}
	}
	if key == "" {
		fail(w, http.StatusUnauthorized, "missing sentry key")
		return
	}

	var validated auth.ValidatedKey
	var err error
	if gk, ok := h.Keys.(ipKeyValidator); ok {
		validated, err = gk.Validate(r.Context(), key, preAuthIP(r))
	} else {
		validated, err = h.Keys.ValidateAPIKey(r.Context(), key)
	}
	if err != nil {
		if errors.Is(err, authguard.ErrTooManyAttempts) {
			h.limited(w, "too many failed authentication attempts")
			return
		}
		if errors.Is(err, auth.ErrAuthUnavailable) {
			w.Header().Set("Retry-After", "5")
			fail(w, http.StatusServiceUnavailable, "authentication temporarily unavailable")
			return
		}
		fail(w, http.StatusUnauthorized, "invalid key")
		return
	}
	if !auth.HasScope(validated.Scopes, auth.ScopeTelemetry) {
		fail(w, http.StatusForbidden, "key lacks the telemetry capability")
		return
	}
	// BoundSite invariant: the key's site is authoritative; a project that
	// names another site is a cross-tenant write attempt.
	ctx := ingest.WithSiteID(r.Context(), validated.SiteID)
	siteID, err := boundProject(ctx, projectID)
	if err != nil {
		fail(w, http.StatusForbidden, "project does not match the key")
		return
	}

	if h.Limiter != nil {
		ip := ingest.ClientIPFromContext(ctx)
		if ip == "" {
			ip = r.RemoteAddr
		}
		if !h.Limiter.Allow(siteID, ip) {
			h.limited(w, "rate limited")
			return
		}
	}

	if !readBody() {
		return
	}
	data, err := decodeBody(raw, r.Header.Get("Content-Encoding"))
	if err != nil {
		switch {
		case errors.Is(err, errTooLarge):
			fail(w, http.StatusRequestEntityTooLarge, "payload too large")
		case errors.Is(err, errEncoding):
			fail(w, http.StatusUnsupportedMediaType, "unsupported content encoding (use gzip, deflate or none)")
		default:
			h.malformed.Add(1)
			fail(w, http.StatusBadRequest, "malformed body")
		}
		return
	}

	var pe *parsedEnvelope
	if legacyStore {
		if len(data) > MaxItemBytes {
			h.oversize.Add(1)
			fail(w, http.StatusRequestEntityTooLarge, "event too large")
			return
		}
		pe = &parsedEnvelope{Items: []item{{Type: "event", Payload: data}}}
	} else {
		pe, err = parseEnvelope(data)
		if err != nil {
			h.malformed.Add(1)
			fail(w, http.StatusBadRequest, "malformed envelope")
			return
		}
	}
	h.oversize.Add(int64(pe.Oversize))
	h.malformed.Add(int64(pe.Malformed))

	respID := pe.Header.EventID
	goodItems, events, bad := 0, 0, 0
	evIdx := -1
	for _, it := range pe.Items {
		if it.Type != "event" {
			goodItems++
			h.countIgnored(it.Type)
			continue
		}
		evIdx++
		if events >= maxEventsPerEnvelope {
			h.malformed.Add(1)
			bad++
			continue
		}
		in, eventID, merr := mapEvent(it.Payload, itemHeaderID(pe.Header.EventID, evIdx))
		if merr != nil {
			h.malformed.Add(1)
			bad++
			continue
		}
		goodItems++
		events++
		in.SiteID = siteID
		switch perr := h.Sink.Push(siteID, in); {
		case perr == nil:
			h.accepted.Add(1)
		case errors.Is(perr, obserrors.ErrAdmittedDuplicate):
			h.dedup.Add(1)
		case errors.Is(perr, obserrors.ErrEventIDConflict):
			// A producer bug, not something a retry fixes: ack so the SDK
			// does not loop, count it for the operator.
			h.conflicts.Add(1)
		case errors.Is(perr, obserrors.ErrInvalidEventID):
			h.malformed.Add(1)
		case errors.Is(perr, obserrors.ErrErrorBufferFull):
			h.limited(w, "error buffer full")
			return
		default:
			w.Header().Set("Retry-After", strconv.Itoa(int(h.retryAfter()/time.Second)))
			fail(w, http.StatusServiceUnavailable, "error durability unavailable, retry later")
			return
		}
		if respID == "" {
			respID = eventID
		}
	}

	// Nothing usable: an envelope whose every item was oversize is a 413;
	// one whose items were all undecodable is a 400. A header-only envelope
	// (no items at all) is valid and acknowledged.
	if goodItems == 0 && (pe.Oversize > 0 || pe.Malformed > 0 || bad > 0 || pe.Dropped > 0 || legacyStore) {
		if pe.Oversize > 0 && pe.Malformed == 0 && bad == 0 {
			fail(w, http.StatusRequestEntityTooLarge, "item too large")
			return
		}
		fail(w, http.StatusBadRequest, "no valid items")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": respID})
}

// itemHeaderID derives the fallback identity for the idx-th event item of an
// envelope. The envelope's event_id names ONE event; an event lacking its own
// id would otherwise inherit it, so events 2..N would collide with event 1
// (same id, different payload) and be dropped as conflicts. Item 0 keeps the
// header id (the common single-event envelope is unchanged); later items get
// a deterministic "_<idx>" suffix, so a retried envelope maps identically.
func itemHeaderID(headerID string, idx int) string {
	if idx <= 0 || headerID == "" {
		return headerID
	}
	return headerID + "_" + strconv.Itoa(idx)
}

// preAuthIP is the client address used to limit failed key attempts.
func preAuthIP(r *http.Request) string {
	if ip := ingest.ClientIPFromContext(r.Context()); ip != "" {
		return ip
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// boundProject enforces the BoundSite invariant on the DSN project segment.
// The segment must be the key's own site id, or a short all-digit alias:
// the JavaScript Sentry SDKs refuse a non-numeric project id in a DSN, so
// `https://<key>@host/1` must work. An alias names no site - the key alone
// decides the site - so it cannot address another tenant. (Real site ids are
// 32 hex characters or "default", never 1-12 digits.)
func boundProject(ctx context.Context, project string) (string, error) {
	if isNumericAlias(project) {
		return ingest.BoundSite(ctx, "")
	}
	return ingest.BoundSite(ctx, project)
}

func isNumericAlias(s string) bool {
	if s == "" || len(s) > 12 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// keyFromRequest reads the DSN public key from the sentry_key query
// parameter or the X-Sentry-Auth header (query wins, as in Relay).
func keyFromRequest(r *http.Request) string {
	if k := strings.TrimSpace(r.URL.Query().Get("sentry_key")); k != "" {
		return k
	}
	if k := authHeaderKey(r.Header.Get("X-Sentry-Auth")); k != "" {
		return k
	}
	// Convenience for hand-rolled clients: the repo's standard header.
	return strings.TrimSpace(r.Header.Get("X-API-Key"))
}

// authHeaderKey parses `Sentry sentry_version=7, sentry_key=abc, ...`.
func authHeaderKey(v string) string {
	v = strings.TrimSpace(v)
	if len(v) < 7 || !strings.EqualFold(v[:6], "sentry") {
		return ""
	}
	for _, part := range strings.Split(v[6:], ",") {
		k, val, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && strings.TrimSpace(k) == "sentry_key" {
			return strings.Trim(strings.TrimSpace(val), `"`)
		}
	}
	return ""
}

// keyFromDSN extracts the public key (userinfo) from a DSN string.
func keyFromDSN(dsn string) string {
	if dsn == "" || len(dsn) > 2048 {
		return ""
	}
	u, err := url.Parse(dsn)
	if err != nil || u.User == nil {
		return ""
	}
	return u.User.Username()
}
