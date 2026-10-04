package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/useteploy/teploy-observe/internal/ingest"
	"github.com/useteploy/teploy-observe/internal/surveys"
)

// Browser survey widget (B5). Served like the other trackers; sites opt in
// with <script src=".../t/observe-surveys.js" data-site-id="...">.
//
//go:embed tracker/observe-surveys.js
var surveysWidgetScript []byte

func serveSurveysWidget(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(surveysWidgetScript)
}

// surveyRouter is the slice of the router the public survey routes need.
type surveyRouter interface {
	Handle(pattern string, h http.Handler)
}

// registerSurveyPublicRoutes mounts the browser-facing survey endpoints and
// the widget script. /active evaluates targeting server-side; /expose and
// /respond keep their existing handlers behind per-IP and per-site limits.
//
// Limits (all token buckets, see ingest.RateLimiter):
//
//	/active   per IP 60/min (burst 120), per site 1200/min (burst 600) - hit
//	          once per page view by the widget, so generous.
//	/expose   per IP 30/min (burst 60),  per site 600/min (burst 300).
//	/respond  per IP 10/min (burst 20),  per site 120/min (burst 60) - a
//	          human submits a handful of times; this is the write-flood gate.
func registerSurveyPublicRoutes(r surveyRouter, svc *surveys.SurveyService) {
	known := newSiteKnownCache(svc.SiteKnown)
	activeIP := ingest.NewRateLimiter(60, time.Minute, 120)
	activeSite := ingest.NewRateLimiter(1200, time.Minute, 600)
	exposeIP := ingest.NewRateLimiter(30, time.Minute, 60)
	exposeSite := ingest.NewRateLimiter(600, time.Minute, 300)
	respondIP := ingest.NewRateLimiter(10, time.Minute, 20)
	respondSite := ingest.NewRateLimiter(120, time.Minute, 60)

	r.Handle("GET /api/v1/surveys/active",
		surveyCORS(ipRateLimitMW(activeIP)(surveysActiveHandler(svc, activeSite, known))))
	r.Handle("POST /api/v1/surveys/expose",
		surveyCORS(ipRateLimitMW(exposeIP)(surveySiteLimitMW(exposeSite, known)(surveyExposeHandler(svc)))))
	r.Handle("POST /api/v1/surveys/respond",
		surveyCORS(ipRateLimitMW(respondIP)(surveySiteLimitMW(respondSite, known)(surveyRespondHandler(svc)))))
	r.Handle("GET /t/observe-surveys.js", http.HandlerFunc(serveSurveysWidget))
}

// surveyCORS applies the public-widget CORS posture (same as feedback and
// flags: any origin, no credentials) before the limiters, so a 429 is also
// readable by the widget and can back off. Preflight for POST is answered by
// the shared wildcard OPTIONS /api/v1/{path...} handler.
func surveyCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

// surveySiteLimitMW is the per-site gate for the POST endpoints. The site is
// named in the body, so it peeks (bounded) and restores the body for the real
// handler. An oversized body is refused here with 413 before it is buffered
// past the cap.
func surveySiteLimitMW(rl *ingest.RateLimiter, known func(context.Context, string) bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, publicFormMaxBodyBytes))
			if err != nil {
				writeJSONError(w, http.StatusRequestEntityTooLarge, "request body too large")
				return
			}
			var peek struct {
				SiteID string `json:"site_id"`
			}
			// A body that does not decode falls through: the real handler owns
			// the 400, and the IP limiter already charged this request.
			if json.Unmarshal(raw, &peek) == nil && peek.SiteID != "" {
				// Only real sites get a per-site bucket: attacker-chosen ids
				// must not fill the limiter's bucket budget. Unknown sites
				// were already charged against the per-IP limit.
				if len(peek.SiteID) > maxSurveySiteIDLen || !known(r.Context(), peek.SiteID) {
					writeJSONError(w, http.StatusNotFound, "unknown site")
					return
				}
				if !rl.Allow(peek.SiteID, "") {
					w.Header().Set("Retry-After", "60")
					writeJSONError(w, http.StatusTooManyRequests, "too many requests")
					return
				}
			}
			r.Body = io.NopCloser(bytes.NewReader(raw))
			next.ServeHTTP(w, r)
		})
	}
}

const maxSurveySiteIDLen = 128

var surveyRefHostPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)

// surveysActiveHandler returns the active surveys that target this page view.
// Inputs (all optional except site_id, all untrusted, all bounded):
//
//	path           location.pathname
//	device_type    desktop|mobile|tablet (else derived from the User-Agent)
//	referrer_host  host of document.referrer
//
// A store failure is a 503 (never an empty 200 that looks like "no surveys",
// OBS-004). The response is never cached: sampling is per visitor.
func surveysActiveHandler(svc *surveys.SurveyService, siteLimiter *ingest.RateLimiter, known func(context.Context, string) bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		siteID := q.Get("site_id")
		if siteID == "" || len(siteID) > maxSurveySiteIDLen {
			writeJSONError(w, http.StatusBadRequest, "site_id required")
			return
		}
		if !known(r.Context(), siteID) {
			// Unknown site: per-IP limit only, empty answer (the widget
			// treats it as "no surveys"), no per-site bucket created.
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			w.Write([]byte("[]\n"))
			return
		}
		if !siteLimiter.Allow(siteID, "") {
			w.Header().Set("Retry-After", "60")
			writeJSONError(w, http.StatusTooManyRequests, "too many requests")
			return
		}
		host := strings.ToLower(q.Get("referrer_host"))
		if !surveyRefHostPattern.MatchString(host) {
			host = ""
		}
		ua := ingest.UserAgentFromContext(r.Context())
		active, err := svc.ActiveFor(r.Context(), siteID, surveys.ClientContext{
			Path:         q.Get("path"),
			Device:       q.Get("device_type"),
			ReferrerHost: host,
			UserAgent:    ua,
		}, ingest.ClientIPFromContext(r.Context()))
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{"error": "active surveys temporarily unavailable"})
			return
		}
		json.NewEncoder(w).Encode(active)
	}
}

// siteKnownCache wraps a site-existence lookup with a short TTL so a flood
// of unknown site ids costs one lookup per id per TTL, not one per request.
// Entries are capped; at the cap the map is reset, which only costs
// re-lookups.
type siteKnownCache struct {
	lookup func(context.Context, string) bool
	ttl    time.Duration
	max    int
	now    func() time.Time
	mu     sync.Mutex
	m      map[string]siteKnownEntry
}

type siteKnownEntry struct {
	ok  bool
	exp time.Time
}

func newSiteKnownCache(lookup func(context.Context, string) bool) func(context.Context, string) bool {
	return newSiteKnownCacheSized(lookup, 30*time.Second, 4096, time.Now).known
}

func newSiteKnownCacheSized(lookup func(context.Context, string) bool, ttl time.Duration, max int, now func() time.Time) *siteKnownCache {
	return &siteKnownCache{lookup: lookup, ttl: ttl, max: max, now: now, m: map[string]siteKnownEntry{}}
}

func (c *siteKnownCache) known(ctx context.Context, siteID string) bool {
	now := c.now()
	c.mu.Lock()
	if e, hit := c.m[siteID]; hit && now.Before(e.exp) {
		c.mu.Unlock()
		return e.ok
	}
	c.mu.Unlock()
	ok := c.lookup(ctx, siteID)
	c.mu.Lock()
	if len(c.m) >= c.max {
		c.m = map[string]siteKnownEntry{}
	}
	c.m[siteID] = siteKnownEntry{ok: ok, exp: now.Add(c.ttl)}
	c.mu.Unlock()
	return ok
}

// writeSurveyRespondError maps a SubmitResponse failure to an HTTP answer
// for anonymous callers. Only surveys.PublicError carries fixed, safe text
// (400); everything else may wrap store text, so it is logged server-side
// and answered with a generic 500.
func writeSurveyRespondError(w http.ResponseWriter, err error) {
	var pe *surveys.PublicError
	if errors.As(err, &pe) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": pe.Msg})
		return
	}
	slog.Error("survey respond failed", "error", err)
	w.WriteHeader(http.StatusInternalServerError)
	json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "could not record response"})
}
