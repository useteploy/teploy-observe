package main

import (
	"context"
	"net/http"
	"regexp"
	"strings"

	"github.com/neutron-dev/neutron-go/neutron"
	"github.com/useteploy/teploy-observe/internal/audit"
	"github.com/useteploy/teploy-observe/internal/ingest"
)

type auditRecorder interface {
	Record(context.Context, audit.AuditEvent) error
}

// auditSkipPrefixes are paths NOT written to the audit trail: the telemetry
// firehose (high-volume, not admin actions), auth (login is recorded directly,
// with the attempted username even on failure), and the audit endpoints
// themselves (the POST producer path would double-record; reads aren't
// mutations).
var auditSkipPrefixes = []string{
	"/api/v1/ingest",
	"/api/v1/track",
	"/api/v1/event",
	"/api/v1/checkin",
	"/api/v1/feedback",
	"/api/v1/surveys",
	"/api/v1/flags/evaluate",
	"/api/v1/sourcemaps",
	"/api/v1/infra/report",
	"/api/v1/auth",
	"/api/v1/audit",
}

// parseActorFunc resolves the acting username from a bearer token. It returns
// ("", false) for a missing/invalid token. Wired from authSvc.ValidateToken.
type parseActorFunc func(bearerToken string) (username string, ok bool)

// auditMiddleware records every mutating (non-GET) admin API call — actor,
// action, target, result, source — so the "who did what" trail is comprehensive
// without wiring each handler. Denied attempts (401/403) are recorded too.
// Recording is best-effort and happens after the response, so a slow or failing
// audit write can never block or break the request it describes.
//
// The actor is resolved by parsing the request's own bearer token, NOT from the
// context: this middleware runs OUTSIDE the per-route JWT middleware, so the
// authenticated claims aren't in its context — parsing the token directly is
// what lets the trail attribute the real user instead of "system".
func auditMiddleware(store auditRecorder, parseActor parseActorFunc) neutron.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !auditableRequest(r.Method, r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			rec := &auditStatusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)

			actor, actorType := auditActor(r, parseActor)
			_ = store.Record(r.Context(), audit.AuditEvent{
				Actor:     actor,
				ActorType: actorType,
				Action:    deriveAction(r.Method, r.URL.Path),
				Target:    r.URL.Path,
				Result:    auditResult(rec.status),
				SourceIP:  ingest.ClientIPFromContext(r.Context()),
				UserAgent: r.UserAgent(),
			})
		})
	}
}

func auditableRequest(method, path string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	if !strings.HasPrefix(path, "/api/v1/") {
		return false
	}
	for _, p := range auditSkipPrefixes {
		if strings.HasPrefix(path, p) {
			return false
		}
	}
	return true
}

// auditActor resolves who is acting: the JWT user (parsed from the request's
// own bearer token), else an API-key principal (bound to a site), else system.
func auditActor(r *http.Request, parseActor parseActorFunc) (string, string) {
	if authz := r.Header.Get("Authorization"); strings.HasPrefix(authz, "Bearer ") && parseActor != nil {
		if u, ok := parseActor(strings.TrimPrefix(authz, "Bearer ")); ok && u != "" {
			return u, audit.ActorUser
		}
	}
	if sid := ingest.SiteIDFromContext(r.Context()); sid != "" {
		return sid, audit.ActorAPIKey
	}
	return "", audit.ActorSystem
}

func auditResult(status int) string {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return audit.ResultDenied
	case status >= 200 && status < 300:
		return audit.ResultSuccess
	default:
		return audit.ResultFailure
	}
}

var auditIDSeg = regexp.MustCompile(`^([0-9]+|[0-9a-fA-F]{16,})$`)

// deriveAction turns METHOD + /api/v1/a/b/{id} into "a.b.<verb>" — the resource
// path (minus id-looking segments) plus a verb from the method.
func deriveAction(method, path string) string {
	p := strings.TrimPrefix(path, "/api/v1/")
	var parts []string
	for _, s := range strings.Split(p, "/") {
		if s == "" || auditIDSeg.MatchString(s) {
			continue
		}
		parts = append(parts, s)
	}
	resource := strings.Join(parts, ".")
	verb := map[string]string{
		http.MethodPost:   "create",
		http.MethodPut:    "update",
		http.MethodPatch:  "update",
		http.MethodDelete: "delete",
	}[method]
	if verb == "" {
		verb = strings.ToLower(method)
	}
	if resource == "" {
		return verb
	}
	return resource + "." + verb
}

// auditStatusRecorder captures the response status for the audit result.
type auditStatusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *auditStatusRecorder) WriteHeader(code int) {
	if !r.wrote {
		r.status = code
		r.wrote = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *auditStatusRecorder) Write(b []byte) (int, error) {
	r.wrote = true
	return r.ResponseWriter.Write(b)
}
