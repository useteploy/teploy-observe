package main

// Persons API. C2 (Wave 4): read-only aggregate over events.distinct_id.
// C3: person properties (identify traits), explicit alias/merge and GDPR
// erasure, within the privacy model of docs/IDENTITY_MODEL_ADR.md.
// Routes live here so the integration into main.go is a single
// RegisterPersonsRoutes call (matches the boards / metrics / cohorts
// convention).
//
// Authorization:
//   - reads (list, detail): JWT (viewer+)
//   - POST /persons/merge: editor+
//   - POST /persons/erase: admin only
//   - POST /persons/properties: telemetry (API) key, site bound by the key.
//     The key ships in browsers, so this route is write-only and narrow:
//     merge mode only, person must already have events, per-person rate
//     limit, and it never returns stored values.
//   - POST /persons/properties/replace: editor+ (JWT); the only way to
//     replace a person's whole trait set.
//
// Merge and erase take their person key in the BODY, not the path, so a
// raw (raw_distinct_id opt-in) identifier never lands in the audit trail's
// target path. All mutating routes are recorded by auditMiddleware.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/neutron-build/neutron/go/neutron"

	"github.com/useteploy/teploy-observe/internal/identity"
	"github.com/useteploy/teploy-observe/internal/ingest"
	"github.com/useteploy/teploy-observe/internal/persons"
)

// personsPrivacy resolves a site's identity hashing config (satisfied by
// *sites.SiteService).
type personsPrivacy interface {
	PrivacyConfig(ctx context.Context, siteID string) (salt string, rawOptIn bool, ok bool)
}

// PersonsRouteDeps carries everything RegisterPersonsRoutes wires.
type PersonsRouteDeps struct {
	JWT, Editor, Admin neutron.Middleware
	// Ingest is the API-key authenticated group rooted at /api/v1.
	Ingest *neutron.Router
	Svc    *persons.Service
	// PropLimit caps telemetry property writes per person (nil: default
	// 20 writes per minute per person).
	PropLimit  *persons.WriteLimiter
	Privacy    personsPrivacy // nil: global salt only (tests)
	GlobalSalt string
	// Actor resolves the acting username from the request (JWT).
	Actor func(*http.Request) string
}

// RegisterPersonsRoutes wires the persons endpoints onto the given router.
func RegisterPersonsRoutes(r *neutron.Router, d PersonsRouteDeps) {
	api := r.Group("/api/v1/persons", d.JWT)

	neutron.Get(api, "", listPersonsHandler(d.Svc),
		neutron.WithTags("persons"),
		neutron.WithSummary("List identified users (aliases resolved, erased persons excluded)"),
	)
	neutron.Get(api, "/{distinct_id}", personDetailHandler(d.Svc),
		neutron.WithTags("persons"),
		neutron.WithSummary("Person detail with properties, aliases and last 100 events"),
	)
	api.Handle("POST /merge", d.Editor(personsMergeHandler(d.Svc, d.Actor)))
	api.Handle("POST /erase", d.Admin(personsEraseHandler(d.Svc, d.Actor)))

	api.Handle("POST /properties/replace", d.Editor(personsReplacePropertiesHandler(d.Svc)))

	if d.Ingest != nil {
		lim := d.PropLimit
		if lim == nil {
			lim = persons.NewWriteLimiter(20, time.Minute)
		}
		d.Ingest.Handle("POST /persons/properties", personsPropertiesHandler(d.Svc, d.Privacy, d.GlobalSalt, lim))
	}
}

// personsHTTPError maps service sentinels to HTTP errors.
func personsHTTPError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, persons.ErrInvalid):
		return neutron.ErrBadRequest(err.Error())
	case errors.Is(err, persons.ErrNotFound):
		return neutron.ErrNotFound(err.Error())
	case errors.Is(err, persons.ErrConflict), errors.Is(err, persons.ErrLimit):
		return neutron.ErrConflict(err.Error())
	case errors.Is(err, persons.ErrErased):
		return neutron.ErrConflict("person has been erased")
	}
	return err
}

func writePersonsJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writePersonsError(w http.ResponseWriter, r *http.Request, err error) {
	var ae *neutron.AppError
	if errors.As(err, &ae) {
		neutron.WriteError(w, r, ae)
		return
	}
	neutron.WriteError(w, r, neutron.ErrInternal("persons request failed"))
}

func decodePersonsBody(w http.ResponseWriter, r *http.Request, v any, max int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, max)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		neutron.WriteError(w, r, neutron.ErrBadRequest("invalid JSON body"))
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

type listPersonsInput struct {
	SiteID           string `query:"site_id"`
	From             string `query:"from"`
	To               string `query:"to"`
	Limit            int    `query:"limit"`
	Offset           int    `query:"offset"`
	IncludeAnonymous bool   `query:"include_anonymous"`
}

// listPersonsResult bundles the page + total so the UI can render
// pagination without a second round-trip. Truncated is set when alias
// resolution scanned only the most recently active persons.
type listPersonsResult struct {
	Persons   []persons.Person `json:"persons"`
	Total     int64            `json:"total"`
	Limit     int              `json:"limit"`
	Offset    int              `json:"offset"`
	Truncated bool             `json:"truncated,omitempty"`
}

func listPersonsHandler(svc *persons.Service) neutron.HandlerFunc[listPersonsInput, listPersonsResult] {
	return func(ctx context.Context, in listPersonsInput) (listPersonsResult, error) {
		if in.SiteID == "" {
			return listPersonsResult{}, neutron.ErrBadRequest("site_id required")
		}
		from, to, err := parseTimeRange(in.From, in.To)
		if err != nil {
			return listPersonsResult{}, neutron.ErrBadRequest(err.Error())
		}
		res, err := svc.ListResolved(ctx, in.SiteID, from.UnixMilli(), to.UnixMilli(), in.Limit, in.Offset, in.IncludeAnonymous)
		if err != nil {
			return listPersonsResult{}, personsHTTPError(err)
		}
		rows := res.Persons
		if rows == nil {
			rows = []persons.Person{}
		}
		limit := in.Limit
		if limit <= 0 {
			limit = 50
		}
		return listPersonsResult{
			Persons: rows, Total: res.Total,
			Limit: limit, Offset: in.Offset, Truncated: res.Truncated,
		}, nil
	}
}

type personDetailInput struct {
	DistinctID string `path:"distinct_id"`
	SiteID     string `query:"site_id"`
}

func personDetailHandler(svc *persons.Service) neutron.HandlerFunc[personDetailInput, persons.PersonDetail] {
	return func(ctx context.Context, in personDetailInput) (persons.PersonDetail, error) {
		if in.SiteID == "" {
			return persons.PersonDetail{}, neutron.ErrBadRequest("site_id required")
		}
		if strings.TrimSpace(in.DistinctID) == "" {
			return persons.PersonDetail{}, neutron.ErrBadRequest("distinct_id required")
		}
		d, err := svc.PersonDetail(ctx, in.SiteID, in.DistinctID)
		if errors.Is(err, persons.ErrErased) {
			return persons.PersonDetail{}, neutron.ErrNotFound("person not found")
		}
		return d, personsHTTPError(err)
	}
}

type personsMergeBody struct {
	SiteID  string `json:"site_id"`
	FromKey string `json:"from_key"`
	IntoKey string `json:"into_key"`
}

func personsMergeHandler(svc *persons.Service, actor func(*http.Request) string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in personsMergeBody
		if !decodePersonsBody(w, r, &in, 4<<10) {
			return
		}
		res, err := svc.Merge(r.Context(), in.SiteID, in.FromKey, in.IntoKey, personsActor(actor, r))
		if err != nil {
			writePersonsError(w, r, personsHTTPError(err))
			return
		}
		writePersonsJSON(w, http.StatusOK, res)
	}
}

type personsEraseBody struct {
	SiteID    string `json:"site_id"`
	PersonKey string `json:"person_key"`
}

func personsEraseHandler(svc *persons.Service, actor func(*http.Request) string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in personsEraseBody
		if !decodePersonsBody(w, r, &in, 4<<10) {
			return
		}
		res, err := svc.Erase(r.Context(), in.SiteID, in.PersonKey, personsActor(actor, r))
		if err != nil {
			writePersonsError(w, r, personsHTTPError(err))
			return
		}
		writePersonsJSON(w, http.StatusOK, res)
	}
}

func personsActor(actor func(*http.Request) string, r *http.Request) string {
	if actor == nil {
		return ""
	}
	return actor(r)
}

type personsPropertiesBody struct {
	SiteID     string         `json:"site_id"`
	DistinctID string         `json:"distinct_id"`
	Properties map[string]any `json:"properties"`
	// Replace is refused on the telemetry route (see
	// personsReplacePropertiesHandler); it is decoded only to say so.
	Replace bool `json:"replace"`
}

// personsPropertiesHandler stores identify() traits. The raw distinct_id is
// hashed exactly as the event ingest path does (per-site salt, or raw when
// the site opted in) and never stored; the key-bound site is authoritative.
//
// The telemetry key is public, so the route is deliberately write-only and
// narrow: stored values are never returned (only the key names the caller
// sent), replace mode is refused, the person must already have events in the
// site (404 otherwise, so the key cannot mint person rows), and writes are
// rate limited per person. Residual: the 404/200 split tells a caller who
// can guess a raw id whether that id has events - the same signal posting
// events with a chosen distinct_id already gives.
func personsPropertiesHandler(svc *persons.Service, priv personsPrivacy, globalSalt string, lim *persons.WriteLimiter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in personsPropertiesBody
		if !decodePersonsBody(w, r, &in, 256<<10) {
			return
		}
		site, err := ingest.BoundSite(r.Context(), in.SiteID)
		if err != nil {
			neutron.WriteError(w, r, neutron.ErrForbidden("site_id does not match API key"))
			return
		}
		if site == "" {
			neutron.WriteError(w, r, neutron.ErrBadRequest("site_id required"))
			return
		}
		if in.Replace {
			neutron.WriteError(w, r, neutron.ErrForbidden("replace is not available to telemetry keys"))
			return
		}
		if in.DistinctID == "" || len(in.DistinctID) > 512 {
			neutron.WriteError(w, r, neutron.ErrBadRequest("distinct_id required (max 512 bytes)"))
			return
		}
		salt, raw := globalSalt, false
		if priv != nil {
			if s, ro, ok := priv.PrivacyConfig(r.Context(), site); ok {
				salt, raw = s, ro
			}
		}
		key := identity.MaybeHashDistinctID(in.DistinctID, salt, raw)
		if lim != nil && !lim.Allow(site, key) {
			w.Header().Set("Retry-After", "60")
			neutron.WriteError(w, r, neutron.ErrRateLimited("too many property writes for this person"))
			return
		}
		written, err := svc.SetKnownProperties(r.Context(), site, key, in.Properties)
		if err != nil {
			writePersonsError(w, r, personsHTTPError(err))
			return
		}
		writePersonsJSON(w, http.StatusOK, map[string]any{"ok": true, "keys_written": written})
	}
}

type personsReplacePropertiesBody struct {
	SiteID     string         `json:"site_id"`
	PersonKey  string         `json:"person_key"`
	Properties map[string]any `json:"properties"`
}

// personsReplacePropertiesHandler is the editor+ (JWT) route that replaces a
// person's whole trait set. The key is the stored person key (as shown by
// the persons API) and travels in the body so a raw id never lands in the
// audit path.
func personsReplacePropertiesHandler(svc *persons.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in personsReplacePropertiesBody
		if !decodePersonsBody(w, r, &in, 256<<10) {
			return
		}
		written, err := svc.ReplaceProperties(r.Context(), in.SiteID, in.PersonKey, in.Properties)
		if err != nil {
			writePersonsError(w, r, personsHTTPError(err))
			return
		}
		writePersonsJSON(w, http.StatusOK, map[string]any{"ok": true, "keys_written": written})
	}
}
