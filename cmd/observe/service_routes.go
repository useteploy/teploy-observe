package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/neutron-build/neutron/go/neutronauth"

	"github.com/useteploy/teploy-observe/internal/audit"
	"github.com/useteploy/teploy-observe/internal/auth"
	"github.com/useteploy/teploy-observe/internal/ingest"
	"github.com/useteploy/teploy-observe/internal/platform"
	"github.com/useteploy/teploy-observe/internal/share"
	"github.com/useteploy/teploy-observe/internal/sites"
	"github.com/useteploy/teploy-observe/internal/svctoken"
)

// The service API is everything an obs_svc_ token can do, in one namespace.
//
// That is the boundary. No existing route gained a credential: every other
// route still authenticates with a JWT, a telemetry key or a share token, and a
// service token is none of those. So a token cannot reach users, SQL, telemetry
// reads or site deletion, and a route added next year cannot accept one by
// forgetting a check. Machine READS stay on the share token, which share:mint
// issues, rather than growing another read credential here.
//
// Every route with a {site_id} is limited to sites the token's managed_by owns.
// That matters beyond tidiness: a share link is a telemetry read and a webhook
// is an outbound feed, so either on someone else's site would be the telemetry
// access this surface must never grant.
//
// Only the dashboard listener serves these; ingestRoutes is default-deny.

const (
	maxExternalRefLen  = 256
	maxManagedByLen    = 128
	maxSiteFieldLen    = 255
	serviceBodyLimit   = 64 << 10
	maxServiceHistory  = 200
	maxAuditFieldLen   = 256
	serviceKeyActorFmt = "service:%s"
)

type serviceWebhooks interface {
	ValidateDestination(url string) error
	Create(ctx context.Context, siteID, name, webhookType, url, severities, kinds string) (*platform.Webhook, error)
	List(ctx context.Context, siteID string) ([]platform.Webhook, error)
	Delete(ctx context.Context, webhookID string) error
}

type serviceAlerts interface {
	ListRules(ctx context.Context, siteID string) ([]platform.AlertRule, error)
	ListHistory(ctx context.Context, siteID string, limit, offset int) ([]platform.AlertHistoryEntry, error)
}

type serviceAPI struct {
	tokens   *svctoken.Store
	sites    *sites.SiteService
	keys     *auth.AuthService
	shares   *share.ShareService
	webhooks serviceWebhooks
	alerts   serviceAlerts
	audit    auditRecorder
	// limit bounds requests per client IP before anything else runs, so nobody can
	// turn refused credentials into unbounded audit rows. Optional for tests.
	limit func(http.Handler) http.Handler
}

// serviceCall is one authorized call. A handler adds what it created to minted:
// credentials and feeds outlive the token that made them, and the audit row is the
// only record tying them back to it.
type serviceCall struct {
	tok    svctoken.Token
	minted map[string]any
}

// serviceHandler runs after authentication, scope and ownership have passed.
type serviceHandler func(w http.ResponseWriter, r *http.Request, call *serviceCall)

type routeHandler interface {
	Handle(pattern string, h http.Handler)
}

func (a *serviceAPI) register(r routeHandler) {
	route := func(pattern, scope string, h serviceHandler) {
		handler := boundedRequestBody(serviceBodyLimit)(a.guard(pattern, scope, h))
		if a.limit != nil {
			handler = a.limit(handler)
		}
		r.Handle(pattern, handler)
	}
	route("POST /api/v1/service/sites", svctoken.ScopeSitesCreate, a.createSite)
	route("POST /api/v1/service/sites/{site_id}/keys", svctoken.ScopeKeysMint, a.mintKey)
	route("POST /api/v1/service/sites/{site_id}/share", svctoken.ScopeShareMint, a.mintShare)
	route("POST /api/v1/service/sites/{site_id}/webhooks", svctoken.ScopeWebhooksWrite, a.createWebhook)
	route("DELETE /api/v1/service/sites/{site_id}/webhooks/{webhook_id}", svctoken.ScopeWebhooksWrite, a.deleteWebhook)
	route("GET /api/v1/service/sites/{site_id}/alerts/rules", svctoken.ScopeAlertsRead, a.alertRules)
	route("GET /api/v1/service/sites/{site_id}/alerts/history", svctoken.ScopeAlertsRead, a.alertHistory)
}

// guard is the whole authorization path, in order: credential, scope, site
// ownership, then a durable record of intent for anything that writes.
//
// Writes are audited, and so is every refusal of a token-shaped credential.
// Successful reads are not: alerts:read is meant to be polled, one row per poll
// would bury the trail, and the token's last_used_at already says it is alive. A
// request with nothing token-shaped is not audited either. Anyone who can reach the
// port can send those, the row would say nothing about any credential, and they
// never reach the store.
func (a *serviceAPI) guard(pattern, scope string, next serviceHandler) http.Handler {
	action := "service." + strings.ReplaceAll(scope, ":", ".")
	write := !strings.HasPrefix(pattern, http.MethodGet+" ")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A value with no Bearer scheme stays "", which Verify refuses.
		var presented string
		if v, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			presented = v
		}
		tok, err := a.tokens.Verify(r.Context(), presented)
		if err != nil {
			if errors.Is(err, svctoken.ErrUnavailable) {
				w.Header().Set("Retry-After", "5")
				writeJSONError(w, http.StatusServiceUnavailable, "authentication temporarily unavailable")
				return
			}
			if svctoken.ValidFormat(presented) {
				_ = a.record(r, svctoken.Token{}, action, audit.ResultDenied, "invalid or revoked service token", nil)
			}
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeJSONError(w, http.StatusUnauthorized, "invalid or revoked service token")
			return
		}
		if !tok.Has(scope) {
			_ = a.record(r, tok, action, audit.ResultDenied, "missing scope "+scope, nil)
			writeJSONError(w, http.StatusForbidden, "token lacks the "+scope+" scope")
			return
		}
		if siteID := r.PathValue("site_id"); siteID != "" {
			owned, err := a.sites.IsManagedBy(r.Context(), siteID, tok.ManagedBy)
			if err != nil {
				w.Header().Set("Retry-After", "5")
				writeJSONError(w, http.StatusServiceUnavailable, "site ownership check unavailable")
				return
			}
			if !owned {
				// The same answer as a site that does not exist, so a token
				// cannot use this route to enumerate other sites.
				_ = a.record(r, tok, action, audit.ResultDenied, "site is not managed by this token", nil)
				writeJSONError(w, http.StatusNotFound, "site not found")
				return
			}
		}
		w.Header().Set("Cache-Control", "no-store")
		call := &serviceCall{tok: tok, minted: map[string]any{}}
		if !write {
			next(w, r, call)
			return
		}
		// Intent is durable before work happens: a credential minted while the
		// trail is down would be an unrecorded grant.
		if err := a.record(r, tok, action, "started", "", nil); err != nil {
			w.Header().Set("Retry-After", "5")
			writeJSONError(w, http.StatusServiceUnavailable, "audit unavailable; nothing was changed")
			return
		}
		rec := &auditStatusRecorder{ResponseWriter: w, status: http.StatusOK}
		next(rec, r, call)
		_ = a.record(r, tok, action, auditResult(rec.status), "", call.minted)
	})
}

// record writes one audit row. It never carries a request body or a credential.
// Callers on the refusal paths ignore the error because the request is already
// refused; record has logged it.
func (a *serviceAPI) record(r *http.Request, tok svctoken.Token, action, result, detail string, minted map[string]any) error {
	meta := map[string]any{}
	for k, v := range minted {
		meta[k] = v
	}
	if tok.ID != "" {
		meta["token_name"] = tok.Name
		meta["managed_by"] = tok.ManagedBy
	}
	if detail != "" {
		meta["detail"] = detail
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Second)
	defer cancel()
	err := a.audit.Record(ctx, audit.AuditEvent{
		SiteID:    truncateAuditField(r.PathValue("site_id")),
		Actor:     tok.ID,
		ActorType: audit.ActorService,
		Action:    action,
		Target:    truncateAuditField(r.URL.Path),
		Result:    result,
		SourceIP:  ingest.ClientIPFromContext(r.Context()),
		UserAgent: truncateAuditField(r.UserAgent()),
		Metadata:  audit.MarshalMetadata(meta),
	})
	if err != nil {
		slog.Error("service token audit write failed", "action", action, "result", result, "err", err)
	}
	return err
}

// truncateAuditField bounds caller-controlled text before it reaches the trail. A
// header or path can be as large as the server's limits allow, and these rows are
// written for callers who failed to authenticate.
func truncateAuditField(v string) string {
	if len(v) <= maxAuditFieldLen {
		return v
	}
	return strings.ToValidUTF8(v[:maxAuditFieldLen], "")
}

// writeServiceUnavailable is the answer to any store failure on this surface: 503
// with Retry-After, never a detail from the store. A client must be able to tell
// "try again" from "you asked for something wrong".
func writeServiceUnavailable(w http.ResponseWriter, message string, err error) {
	slog.Error("service API store failure", "message", message, "err", err)
	w.Header().Set("Retry-After", "5")
	writeJSONError(w, http.StatusServiceUnavailable, message)
}

// decodeServiceBody refuses unknown fields. A manager or a key capability
// passed in a body must be an error, not silently ignored: both come from the
// token and from this code, never from the caller. An empty body is allowed
// and leaves dst at its zero value.
func decodeServiceBody(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func writeServiceJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func validSiteField(v string, maxLen int) bool {
	return len(v) <= maxLen && strings.IndexFunc(v, unicode.IsControl) < 0
}

func (a *serviceAPI) createSite(w http.ResponseWriter, r *http.Request, call *serviceCall) {
	var in struct {
		ExternalRef string `json:"external_ref"`
		Name        string `json:"name"`
		Domain      string `json:"domain"`
	}
	if err := decodeServiceBody(r, &in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	in.ExternalRef = strings.TrimSpace(in.ExternalRef)
	in.Name = strings.TrimSpace(in.Name)
	in.Domain = strings.TrimSpace(in.Domain)
	if in.ExternalRef == "" || !validSiteField(in.ExternalRef, maxExternalRefLen) {
		writeJSONError(w, http.StatusBadRequest, "external_ref is required and must be at most 256 printable bytes")
		return
	}
	if !validSiteField(in.Name, maxSiteFieldLen) || !validSiteField(in.Domain, maxSiteFieldLen) {
		writeJSONError(w, http.StatusBadRequest, "name and domain must be at most 255 printable bytes")
		return
	}
	if in.Name == "" {
		in.Name = in.ExternalRef
	}
	site, created, err := a.sites.EnsureManaged(r.Context(), call.tok.ManagedBy, in.ExternalRef, in.Domain, in.Name)
	if err != nil {
		writeServiceUnavailable(w, "creating the site failed; try again", err)
		return
	}
	call.minted["site_id"] = site.SiteID
	call.minted["external_ref"] = truncateAuditField(in.ExternalRef)
	call.minted["created"] = created
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeServiceJSON(w, status, map[string]any{"site": site, "created": created})
}

// mintKey issues a telemetry key. It has no scopes field to pass: the body is
// decoded strictly, so asking for publish is a 400, and publish stays with an
// admin.
func (a *serviceAPI) mintKey(w http.ResponseWriter, r *http.Request, call *serviceCall) {
	var in struct {
		Label string `json:"label"`
	}
	if err := decodeServiceBody(r, &in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	in.Label = strings.TrimSpace(in.Label)
	if in.Label == "" || !validSiteField(in.Label, maxSiteFieldLen) {
		writeJSONError(w, http.StatusBadRequest, "label is required and must be at most 255 printable bytes")
		return
	}
	key, info, err := a.keys.CreateAPIKeyBy(r.Context(), r.PathValue("site_id"), in.Label,
		fmt.Sprintf(serviceKeyActorFmt, call.tok.ID), []string{auth.ScopeTelemetry})
	if err != nil {
		writeServiceUnavailable(w, "minting the key failed; try again", err)
		return
	}
	call.minted["key_id"] = info.KeyID
	call.minted["key_label"] = in.Label
	writeServiceJSON(w, http.StatusCreated, createAPIKeyResponse{Key: key, Info: info})
}

func (a *serviceAPI) mintShare(w http.ResponseWriter, r *http.Request, call *serviceCall) {
	var in struct {
		TTLDays int `json:"ttl_days"`
	}
	if err := decodeServiceBody(r, &in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if in.TTLDays < 0 || in.TTLDays > maxShareTTLDays {
		writeJSONError(w, http.StatusBadRequest, "ttl_days must be between 1 and 3650")
		return
	}
	var (
		link share.ShareLink
		err  error
	)
	if in.TTLDays == 0 {
		link, err = a.shares.Create(r.Context(), r.PathValue("site_id"))
	} else {
		link, err = a.shares.CreateWithTTL(r.Context(), r.PathValue("site_id"), time.Duration(in.TTLDays)*24*time.Hour)
	}
	if err != nil {
		writeServiceUnavailable(w, "minting the share link failed; try again", err)
		return
	}
	// The link's non-secret id, never the token. It is what revokes the link.
	call.minted["share_link_id"] = link.ID
	call.minted["share_expires_at"] = link.ExpiresAt
	writeServiceJSON(w, http.StatusCreated, link)
}

func (a *serviceAPI) createWebhook(w http.ResponseWriter, r *http.Request, call *serviceCall) {
	var in struct {
		Name        string `json:"name"`
		WebhookType string `json:"webhook_type"`
		URL         string `json:"url"`
		Severities  string `json:"severities"`
		Kinds       string `json:"kinds"`
	}
	if err := decodeServiceBody(r, &in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if in.URL == "" {
		writeJSONError(w, http.StatusBadRequest, "url is required")
		return
	}
	if err := a.webhooks.ValidateDestination(in.URL); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	hook, err := a.webhooks.Create(r.Context(), r.PathValue("site_id"), in.Name, in.WebhookType, in.URL, in.Severities, in.Kinds)
	if err != nil {
		if errors.Is(err, platform.ErrInvalidWebhook) {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeServiceUnavailable(w, "creating the webhook failed; try again", err)
		return
	}
	// The host only: a webhook URL's path can itself be the posting credential.
	call.minted["webhook_id"] = hook.WebhookID
	call.minted["webhook_host"] = endpointHost(in.URL)
	writeServiceJSON(w, http.StatusCreated, hook)
}

// deleteWebhook confirms the webhook belongs to the site in the path, because
// WebhookService.Delete takes a bare id and the ownership check covered the site.
func (a *serviceAPI) deleteWebhook(w http.ResponseWriter, r *http.Request, call *serviceCall) {
	hooks, err := a.webhooks.List(r.Context(), r.PathValue("site_id"))
	if err != nil {
		writeServiceUnavailable(w, "looking up the webhook failed; try again", err)
		return
	}
	id := r.PathValue("webhook_id")
	for _, h := range hooks {
		if h.WebhookID != id {
			continue
		}
		if err := a.webhooks.Delete(r.Context(), id); err != nil {
			writeServiceUnavailable(w, "deleting the webhook failed; try again", err)
			return
		}
		call.minted["webhook_id"] = id
		call.minted["webhook_host"] = endpointHost(h.URL)
		writeServiceJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	writeJSONError(w, http.StatusNotFound, "webhook not found")
}

func (a *serviceAPI) alertRules(w http.ResponseWriter, r *http.Request, _ *serviceCall) {
	rules, err := a.alerts.ListRules(r.Context(), r.PathValue("site_id"))
	if err != nil {
		writeServiceUnavailable(w, "reading alert rules failed; try again", err)
		return
	}
	if rules == nil {
		rules = []platform.AlertRule{}
	}
	writeServiceJSON(w, http.StatusOK, rules)
}

func (a *serviceAPI) alertHistory(w http.ResponseWriter, r *http.Request, _ *serviceCall) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if limit > maxServiceHistory {
		limit = maxServiceHistory
	}
	history, err := a.alerts.ListHistory(r.Context(), r.PathValue("site_id"), limit, offset)
	if err != nil {
		writeServiceUnavailable(w, "reading alert history failed; try again", err)
		return
	}
	if history == nil {
		history = []platform.AlertHistoryEntry{}
	}
	writeServiceJSON(w, http.StatusOK, history)
}

// --- Token management API (admin only; the audit middleware records writes) ---

// signedInAdmin is the gate every credential-management route here shares, on top
// of the JWT and role middleware. Those wave every request through during the
// first-run grace period, which is fine for onboarding but must not let an anonymous
// caller see, plant or revoke a durable credential, or hand a site to a manager.
func signedInAdmin(w http.ResponseWriter, r *http.Request) (string, bool) {
	username := ""
	if claims, err := neutronauth.ClaimsFromContext(r.Context()); err == nil {
		username, _ = claims["username"].(string)
	}
	if username == "" || auth.RoleFromContext(r.Context()) != auth.RoleAdmin {
		writeJSONError(w, http.StatusForbidden, "this requires a signed-in admin")
		return "", false
	}
	return username, true
}

func serviceTokensListHandler(store *svctoken.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := signedInAdmin(w, r); !ok {
			return
		}
		list, err := store.List(r.Context())
		if err != nil {
			writeServiceUnavailable(w, "listing service tokens failed; try again", err)
			return
		}
		if list == nil {
			list = []svctoken.Token{}
		}
		w.Header().Set("Cache-Control", "no-store")
		writeServiceJSON(w, http.StatusOK, list)
	}
}

func serviceTokensCreateHandler(store *svctoken.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		createdBy, ok := signedInAdmin(w, r)
		if !ok {
			return
		}
		var in struct {
			Name      string   `json:"name"`
			ManagedBy string   `json:"managed_by"`
			Scopes    []string `json:"scopes"`
		}
		if err := decodeServiceBody(r, &in); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
			return
		}
		plaintext, tok, err := store.Create(r.Context(), in.Name, in.ManagedBy, createdBy, in.Scopes)
		if err != nil {
			if errors.Is(err, svctoken.ErrInvalidRequest) {
				writeJSONError(w, http.StatusBadRequest, err.Error())
				return
			}
			writeServiceUnavailable(w, "creating the service token failed; try again", err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		// The plaintext crosses the wire exactly once, here.
		writeServiceJSON(w, http.StatusCreated, map[string]any{"token": plaintext, "record": tok})
	}
}

func serviceTokensRevokeHandler(store *svctoken.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := signedInAdmin(w, r); !ok {
			return
		}
		err := store.Revoke(r.Context(), r.PathValue("token_id"))
		switch {
		case errors.Is(err, svctoken.ErrNotFound):
			writeJSONError(w, http.StatusNotFound, "service token not found")
		case err != nil:
			writeServiceUnavailable(w, "revoking the service token failed; try again", err)
		default:
			writeServiceJSON(w, http.StatusOK, map[string]bool{"ok": true})
		}
	}
}

// adoptSiteHandler hands an existing operator site to a manager, so Dash can manage
// a site that predates it. This is the one route that moves a site into service
// token reach, and it exposes what is already stored there: a token holding
// share:mint can then read that site. So it is JWT and admin only, a service token
// is not a JWT and cannot call it, and intent is durable before anything changes.
//
// The audit middleware records that the route was called; this adds what was asked.
func adoptSiteHandler(siteSvc *sites.SiteService, rec auditRecorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin, ok := signedInAdmin(w, r)
		if !ok {
			return
		}
		var in struct {
			ManagedBy   string `json:"managed_by"`
			ExternalRef string `json:"external_ref"`
		}
		if err := decodeServiceBody(r, &in); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
			return
		}
		in.ManagedBy = strings.TrimSpace(in.ManagedBy)
		in.ExternalRef = strings.TrimSpace(in.ExternalRef)
		if in.ManagedBy == "" || !validSiteField(in.ManagedBy, maxManagedByLen) {
			writeJSONError(w, http.StatusBadRequest, "managed_by is required and must be at most 128 printable bytes")
			return
		}
		if in.ExternalRef == "" || !validSiteField(in.ExternalRef, maxExternalRefLen) {
			writeJSONError(w, http.StatusBadRequest, "external_ref is required and must be at most 256 printable bytes")
			return
		}
		siteID := r.PathValue("site_id")
		record := func(result string) error {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Second)
			defer cancel()
			err := rec.Record(ctx, audit.AuditEvent{
				SiteID:    truncateAuditField(siteID),
				Actor:     admin,
				ActorType: audit.ActorUser,
				Action:    "sites.adopt",
				Target:    truncateAuditField(r.URL.Path),
				Result:    result,
				SourceIP:  ingest.ClientIPFromContext(r.Context()),
				UserAgent: truncateAuditField(r.UserAgent()),
				Metadata:  audit.MarshalMetadata(map[string]any{"managed_by": in.ManagedBy, "external_ref": in.ExternalRef}),
			})
			if err != nil {
				slog.Error("site adoption audit write failed", "result", result, "err", err)
			}
			return err
		}
		if err := record("started"); err != nil {
			w.Header().Set("Retry-After", "5")
			writeJSONError(w, http.StatusServiceUnavailable, "audit unavailable; nothing was changed")
			return
		}
		site, adopted, err := siteSvc.Adopt(r.Context(), siteID, in.ManagedBy, in.ExternalRef)
		switch {
		case errors.Is(err, sites.ErrSiteNotFound):
			_ = record(audit.ResultFailure)
			writeJSONError(w, http.StatusNotFound, "site not found")
		case errors.Is(err, sites.ErrManagementConflict):
			_ = record(audit.ResultFailure)
			writeJSONError(w, http.StatusConflict, err.Error())
		case err != nil:
			_ = record(audit.ResultFailure)
			writeServiceUnavailable(w, "adopting the site failed; try again", err)
		default:
			_ = record(audit.ResultSuccess)
			status := http.StatusOK
			if adopted {
				status = http.StatusCreated
			}
			w.Header().Set("Cache-Control", "no-store")
			writeServiceJSON(w, status, map[string]any{"site": site, "adopted": adopted})
		}
	}
}
