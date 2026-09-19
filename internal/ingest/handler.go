package ingest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/neutron-dev/neutron-go/neutron"

	"github.com/useteploy/teploy-observe/internal/geo"
	"github.com/useteploy/teploy-observe/internal/identity"
	"github.com/useteploy/teploy-observe/internal/session"
	"github.com/useteploy/teploy-observe/internal/sites"
)

// maxProperties caps the custom properties carried by one event.
const maxProperties = 50

// IngestInput is the JSON body for POST /api/v1/events.
type IngestInput struct {
	SiteID     string         `json:"site_id"`
	EventType  string         `json:"event_type"`
	URL        string         `json:"url"`
	Referrer   string         `json:"referrer"`
	Title      string         `json:"title"`
	Language   string         `json:"language"`
	Screen     string         `json:"screen"`
	Properties map[string]any `json:"properties,omitempty"`
	// F41 URL contract: trackers send location.href minus credentials,
	// fragment, AND query; campaign attribution rides as the explicit
	// utm_* fields below (extracted client-side from the allowlisted
	// query params). Explicit fields win; for legacy producers still
	// sending a query string, the server extracts the same allowlisted
	// params from it before sanitizing the stored URL — attribution keeps
	// working through the upgrade either way.
	UTMSource   string `json:"utm_source,omitempty"`
	UTMMedium   string `json:"utm_medium,omitempty"`
	UTMCampaign string `json:"utm_campaign,omitempty"`
	UTMTerm     string `json:"utm_term,omitempty"`
	UTMContent  string `json:"utm_content,omitempty"`
	// DistinctID, when present, is the user identifier the SDK passed
	// via identify(userId). The server hashes it with the site's
	// session_salt before storage (unless the site has raw_distinct_id
	// opt-in set). Default '' for anonymous events.
	DistinctID string `json:"distinct_id,omitempty"`
	// Release is the application release tag (e.g. git SHA or semver)
	// the SDK was initialized with. Used by the session rollup to
	// stamp release_tag on the resulting sessions row, which feeds
	// crash-free-session computation per release.
	Release string `json:"release,omitempty"`
	// EventID is the producer-assigned identity of this event (protocol
	// v2, audit F12). A producer generates it once when the event is
	// recorded and keeps it through every requeue and retry, so a retried
	// batch carries the same ids and the server can deduplicate at flush.
	// Empty (v1 producers) -> the server generates a random id as before.
	// Invalid shapes are ignored (v1 fallback), not rejected: the field is
	// an optimization boundary, not a security one.
	EventID string `json:"event_id,omitempty"`
}

// ingestKnownKeys are the top-level JSON keys IngestInput actually consumes.
// Anything else in the body is a custom event property (see UnmarshalJSON).
var ingestKnownKeys = map[string]bool{
	"site_id": true, "event_type": true, "url": true, "referrer": true,
	"title": true, "language": true, "screen": true, "properties": true,
	"distinct_id": true, "release": true, "event_id": true,
	// F41 explicit campaign fields (win over legacy URL-query extraction).
	"utm_source": true, "utm_medium": true, "utm_campaign": true,
	"utm_term": true, "utm_content": true,
	// Transport metadata on the v2 single-event path (batch envelope fields
	// accepted inline); consumed for tracing context, never stored.
	"producer_id": true, "v": true,
}

// validProducerID accepts the id shapes producers may use for event ids,
// producer ids, and batch ids (protocol v2): 8-64 chars of url-safe
// letters/digits/hyphens/underscores. Anything else falls back to v1
// (server-generated) semantics.
func validProducerID(id string) bool {
	if len(id) < 8 || len(id) > 64 {
		return false
	}
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// UnmarshalJSON decodes the documented shape and, in addition, collects any
// unrecognised top-level key into Properties.
//
// Custom event properties belong under `properties`, but several producers
// spread them across the top level instead — sdk/browser did until it was
// fixed, and docs/migrations/from-posthog.md still teaches a jq recipe that
// does. Those payloads are already deployed and cannot be recalled, and the
// alternative is that every one of their properties is stored as {}. An
// explicitly nested `properties` entry always wins over a same-named flat key.
func (in *IngestInput) UnmarshalJSON(data []byte) error {
	type alias IngestInput
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*in = IngestInput(a)

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	extra := make([]string, 0, len(raw))
	for k := range raw {
		if !ingestKnownKeys[k] {
			extra = append(extra, k)
		}
	}
	if len(extra) == 0 {
		return nil
	}
	// Sorted so the cap below drops the same keys on every request rather
	// than whichever ones Go's map iteration happened to reach first.
	sort.Strings(extra)
	for _, k := range extra {
		if len(in.Properties) >= maxProperties {
			break
		}
		if _, exists := in.Properties[k]; exists {
			continue
		}
		var v any
		if err := json.Unmarshal(raw[k], &v); err != nil {
			continue
		}
		if in.Properties == nil {
			in.Properties = make(map[string]any, len(extra))
		}
		in.Properties[k] = v
	}
	return nil
}

// IngestResponse is returned to the tracker.
type IngestResponse struct {
	OK bool `json:"ok"`
	// Accepted/Rejected are populated by the batch endpoint; omitempty keeps the
	// single-event response shape unchanged.
	Accepted int `json:"accepted,omitempty"`
	Rejected int `json:"rejected,omitempty"`
	// Deduped is true when a v2 batch was recognized as a retry of one this
	// process already admitted (F12): nothing was buffered again.
	Deduped bool `json:"deduped,omitempty"`
}

// maxStoredEventBytes caps the serialized size of one stored event
// (AUD-015, round 2). The HTTP body cap bounds a single request, not the
// aggregate of many individually legal large events.
const maxStoredEventBytes = 64 << 10

// truncateUTF8 shortens s to at most limit bytes without splitting a
// multi-byte character (AUD-015).
func truncateUTF8(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(s) <= limit {
		return s
	}
	for limit > 0 && !utf8.RuneStart(s[limit]) {
		limit--
	}
	return s[:limit]
}

// Handler returns the typed Neutron handler for event ingestion.
//
// `siteSvc` is used to look up per-site privacy config for hashing
// distinct_id. Pass nil to fall back to the global salt for hashing —
// useful in tests.
func Handler(buf *Buffer, salt string, siteSvc *sites.SiteService) neutron.HandlerFunc[IngestInput, IngestResponse] {
	return func(ctx context.Context, input IngestInput) (IngestResponse, error) {
		e, err := prepareEvent(ctx, input, salt, siteSvc)
		if err != nil {
			return IngestResponse{}, err
		}
		if e == nil {
			// Bot traffic: dropped silently with OK so bots don't retry.
			return IngestResponse{OK: true}, nil
		}
		if !buf.Push(*e) {
			return IngestResponse{}, neutron.ErrRateLimited("buffer full, try again later")
		}
		return IngestResponse{OK: true}, nil
	}
}

// prepareEvent validates and normalizes one input into a storage-ready
// Event WITHOUT any admission side effect (AUD-010, round 2): BatchHandler
// prepares every event first, then admits the survivors in ONE atomic
// Buffer.PushBatch, so a mid-batch admission failure can never leave an
// accepted prefix behind. A nil Event means "silently skip" (bot traffic).
func prepareEvent(ctx context.Context, input IngestInput, salt string, siteSvc *sites.SiteService) (*Event, error) {
	now := time.Now().UTC()
	ip := ClientIPFromContext(ctx)
	ua := UserAgentFromContext(ctx)

	if IsBot(ua) {
		return nil, nil
	}

	// Input validation
	if len(input.URL) > 2048 {
		return nil, neutron.ErrBadRequest("url too long (max 2048)")
	}
	input.Title = truncateUTF8(input.Title, 512)
	input.Referrer = truncateUTF8(input.Referrer, 2048)
	if input.Properties != nil && len(input.Properties) > maxProperties {
		return nil, neutron.ErrBadRequest("too many properties (max 50)")
	}
	// AUD-015: reject unserializable properties at admission instead of
	// letting propertiesJSON silently store "{}" — a silent data swap.
	if raw, err := json.Marshal(input.Properties); err != nil || (input.Properties != nil && len(raw) > maxStoredEventBytes) {
		return nil, neutron.ErrBadRequest("properties must be JSON-serializable and under 64 KiB")
	}

	// Site scoping: when the request is authenticated by an API key, that
	// key's site (bound into the context by APIKeyAuthMiddleware) is
	// AUTHORITATIVE. A body site_id that disagrees is a cross-tenant write
	// attempt and is rejected — otherwise a holder of any one valid key
	// could forge/poison events under any other site. The body site_id is
	// only honored when no key-bound context site exists (direct/test
	// callers; the middleware itself always requires a key — AUD-002).
	ctxSite := SiteIDFromContext(ctx)
	siteID := ctxSite
	if ctxSite != "" {
		if input.SiteID != "" && input.SiteID != ctxSite {
			return nil, neutron.ErrForbidden("site_id does not match API key")
		}
	} else {
		siteID = input.SiteID
	}
	if siteID == "" {
		return nil, neutron.ErrBadRequest("missing site_id")
	}

	sessionID := session.ID(siteID, ip, ua, salt)
	visitID := session.VisitID(sessionID, now)
	eventID := generateID()
	if validProducerID(input.EventID) {
		// F12: stable producer-side event id - the identity that survives
		// requeue and retry, letting the flush path deduplicate.
		eventID = input.EventID
	}
	parsed := ParseUA(ua)
	country := geo.Lookup(ip)

	// Hash the user-supplied distinct_id (if any) with the per-site
	// session_salt — falls back to the global salt if the site is
	// unknown or the SiteService isn't wired (tests).
	distinctID := ""
	if input.DistinctID != "" {
		privSalt := salt
		rawOptIn := false
		if siteSvc != nil {
			if s, raw, ok := siteSvc.PrivacyConfig(ctx, siteID); ok {
				privSalt = s
				rawOptIn = raw
			}
		}
		distinctID = identity.MaybeHashDistinctID(input.DistinctID, privSalt, rawOptIn)
	}

	eventType := input.EventType
	if eventType == "" {
		eventType = "pageview"
	}

	var hostname, pathname string
	sanitizedURL := ""
	if input.URL != "" {
		if u, err := url.Parse(input.URL); err == nil {
			hostname = u.Hostname()
			pathname = u.Path
		}
		sanitizedURL = sanitizeEventURL(input.URL)
	}

	// F41 UTM resolution: explicit fields first (the new tracker contract);
	// for a legacy producer still sending the query string, the same
	// allowlisted params are extracted from it — attribution survives the
	// upgrade. Nothing outside the allowlist is ever read or stored.
	var utmSource, utmMedium, utmCampaign, utmTerm, utmContent string
	if input.URL != "" && hasEmptyUTMField(&input) {
		if u, err := url.Parse(input.URL); err == nil {
			q := u.Query()
			utmSource = q.Get("utm_source")
			utmMedium = q.Get("utm_medium")
			utmCampaign = q.Get("utm_campaign")
			utmTerm = q.Get("utm_term")
			utmContent = q.Get("utm_content")
		}
	}
	if input.UTMSource != "" {
		utmSource = input.UTMSource
	}
	if input.UTMMedium != "" {
		utmMedium = input.UTMMedium
	}
	if input.UTMCampaign != "" {
		utmCampaign = input.UTMCampaign
	}
	if input.UTMTerm != "" {
		utmTerm = input.UTMTerm
	}
	if input.UTMContent != "" {
		utmContent = input.UTMContent
	}

	// Clean referrer: strip userinfo, query params, and fragments
	referrer := cleanReferrer(input.Referrer, hostname)

	var sw, sh int
	if input.Screen != "" {
		// Require both fields to parse and clamp to a sane range so
		// negative/oversized/partial ("1920x0") garbage never reaches the
		// INTEGER columns or the rollups.
		if n, _ := fmt.Sscanf(input.Screen, "%dx%d", &sw, &sh); n != 2 ||
			sw <= 0 || sw > 65535 || sh <= 0 || sh > 65535 {
			sw, sh = 0, 0
		}
	}

	e := &Event{
		EventID:        eventID,
		TenantID:       "default",
		SiteID:         siteID,
		SessionID:      sessionID,
		VisitID:        visitID,
		EventType:      eventType,
		Timestamp:      now.UnixMilli(),
		URL:            sanitizedURL,
		Referrer:       referrer,
		Title:          input.Title,
		Hostname:       hostname,
		Pathname:       pathname,
		Language:       input.Language,
		Country:        country,
		Browser:        parsed.Browser,
		BrowserVersion: parsed.BrowserVersion,
		OS:             parsed.OS,
		OSVersion:      parsed.OSVersion,
		Device:         parsed.Device,
		ScreenWidth:    sw,
		ScreenHeight:   sh,
		UTMSource:      utmSource,
		UTMMedium:      utmMedium,
		UTMCampaign:    utmCampaign,
		UTMTerm:        utmTerm,
		UTMContent:     utmContent,
		Properties:     input.Properties,
		DistinctID:     distinctID,
		ReleaseTag:     input.Release,
	}

	// AUD-015: one final whole-event size gate so every admitted record is
	// bounded, whatever combination of fields got it here.
	if raw, err := json.Marshal(e); err != nil || len(raw) > maxStoredEventBytes {
		return nil, neutron.ErrBadRequest("event too large after normalization (max 64 KiB)")
	}
	return e, nil
}

// sanitizeEventURL reduces a tracker-captured page URL to scheme + host +
// path (F41): credentials, query, and fragment never reach storage — the
// wire contract has trackers strip them client-side, and this is the
// server-side backstop for every producer, legacy included. An unparseable
// or non-http(s) URL is dropped entirely (fail closed, same posture as
// cleanReferrer).
func sanitizeEventURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	u.RawFragment = ""
	u.ForceQuery = false
	return u.String()
}

// hasEmptyUTMField reports whether any explicit campaign field is unset, so
// the legacy URL-query extraction only runs when it has something to fill.
func hasEmptyUTMField(in *IngestInput) bool {
	return in.UTMSource == "" || in.UTMMedium == "" || in.UTMCampaign == "" ||
		in.UTMTerm == "" || in.UTMContent == ""
}

// cleanReferrer normalizes a referrer URL:
//   - Strips user info, query parameters, and fragments
//   - Returns empty string for self-referrals (same hostname)
//   - Returns just scheme+host+path
//
// AUD-030 containment (round 2): a referrer that cannot be parsed is
// dropped (fail closed) rather than stored verbatim — malformed URLs are
// exactly where userinfo-style material hides — and URL user info is
// stripped even on a successful parse. The full URL-capture policy
// (top-level url field, replay payloads) stays deferred with F41.
func cleanReferrer(raw, selfHost string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	// Drop self-referrals
	if selfHost != "" && u.Hostname() == selfHost {
		return ""
	}
	// Strip userinfo, query, and fragment
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	u.RawFragment = ""
	u.ForceQuery = false
	// Remove trailing slash for consistency
	result := u.String()
	if len(result) > 1 && result[len(result)-1] == '/' {
		result = result[:len(result)-1]
	}
	return result
}

// BatchInput accepts an array of events in one POST.
//
// Protocol v2 (F12): V is the wire version, ProducerID identifies the
// tracker instance, BatchID identifies one detached batch and MUST be
// reused when that batch is retried (the SDKs derive it from the first
// event's stable event_id, which survives requeue by construction).
type BatchInput struct {
	V          int           `json:"v,omitempty"`
	ProducerID string        `json:"producer_id,omitempty"`
	BatchID    string        `json:"batch_id,omitempty"`
	Events     []IngestInput `json:"events"`
}

// BatchDeduper is the in-process admission cache for v2 event batches
// (F12). It exists to stop a client retrying an ambiguous (response-lost)
// batch from re-buffering events the server already admitted - the fast
// path. The durable boundary is the flush-time event-id existence filter
// (see Buffer.insertBatch), which covers restarts, WAL replay, and any
// cache miss; this cache is an optimization, and its loss (restart, TTL,
// bound eviction) never produces duplicates, only a redundant admission
// that the flush filter then drops.
//
// Deliberately in-memory and per-process: observe runs one instance per
// database (the same documented single-process boundary as the replay
// striped locks, AUD-018); a multi-replica deployment needs the
// stable-key/CAS design this defers to.
type BatchDeduper struct {
	mu    sync.Mutex
	seen  map[string]time.Time
	order []string
	head  int
	ttl   time.Duration
	max   int
	now   func() time.Time
}

func NewBatchDeduper(ttl time.Duration, max int) *BatchDeduper {
	return &BatchDeduper{
		seen: make(map[string]time.Time),
		ttl:  ttl,
		max:  max,
		now:  time.Now,
	}
}

// duplicate reports whether key was recorded within the TTL, refreshing its
// timestamp so a busy retrying producer cannot slide its own batch back
// under the TTL by hammering it.
func (d *BatchDeduper) duplicate(key string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if t, ok := d.seen[key]; ok && d.now().Sub(t) < d.ttl {
		d.seen[key] = d.now()
		return true
	}
	return false
}

// record remembers key as admitted. Called only AFTER the batch was
// successfully buffered, so a refused (429) first attempt stays retryable.
// order is a fixed-capacity ring: at capacity the oldest recorded key is
// overwritten, bounding both the map and the ring to max entries.
func (d *BatchDeduper) record(key string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.seen[key] = d.now()
	if len(d.order) < d.max {
		d.order = append(d.order, key)
		return
	}
	delete(d.seen, d.order[d.head])
	d.order[d.head] = key
	d.head = (d.head + 1) % d.max
}

// DefaultBatchDedupeTTL bounds how long an admitted batch id is remembered.
// A producer that retries the same batch after this window (with the
// process alive) re-admits it; the flush-time event-id filter still drops
// the duplicates within the flush dedupe horizon.
const DefaultBatchDedupeTTL = 10 * time.Minute

// DefaultBatchDeduperCapacity bounds the admission cache.
const DefaultBatchDeduperCapacity = 16384

// BatchHandler processes multiple events in a single request.
// This is the preferred ingestion path — the tracker sends all queued
// events as one POST instead of one request per event.
func BatchHandler(buf *Buffer, salt string, siteSvc *sites.SiteService, deduper *BatchDeduper) neutron.HandlerFunc[BatchInput, IngestResponse] {
	return func(ctx context.Context, input BatchInput) (IngestResponse, error) {
		if len(input.Events) > 100 {
			return IngestResponse{}, neutron.ErrBadRequest("batch too large (max 100 events)")
		}
		if len(input.Events) == 0 {
			return IngestResponse{OK: true}, nil
		}
		// F12: a v2 batch whose identity this process already admitted is a
		// retry of an ambiguous submit - acknowledge as a duplicate WITHOUT
		// re-buffering. The key is recorded only after successful admission
		// below, so a 429'd first attempt stays retryable; a concurrent
		// double-submit both miss here and the flush-time event-id filter
		// drops the loser.
		var batchKey string
		if deduper != nil && validProducerID(input.ProducerID) && validProducerID(input.BatchID) {
			batchKey = input.ProducerID + "\x00" + input.BatchID
			if deduper.duplicate(batchKey) {
				return IngestResponse{OK: true, Deduped: true}, nil
			}
		}
		// AUD-010 (round 2): prepare EVERY event side-effect-free first,
		// then admit the survivors in one atomic Buffer.PushBatch under the
		// buffer lock. The old Avail-snapshot + per-event Push loop let two
		// requests interleave, accept a prefix, and refuse the tail — a 429
		// that concealed already-admitted events, duplicating them on retry.
		prepared := make([]Event, 0, len(input.Events))
		accepted, rejected := 0, 0
		for _, ev := range input.Events {
			e, err := prepareEvent(ctx, ev, salt, siteSvc)
			if err != nil {
				// A permanent per-event client error (4xx, e.g. a malformed
				// event) must not fail the whole batch — that previously left
				// earlier events ingested and made the client retry the lot,
				// duplicating them. Skip the bad event and count it. The
				// only remaining transient failure after preparation is the
				// single atomic admission below.
				var appErr *neutron.AppError
				if errors.As(err, &appErr) && appErr.Status >= 400 && appErr.Status < 500 && appErr.Status != http.StatusTooManyRequests {
					rejected++
					continue
				}
				return IngestResponse{}, err
			}
			if e == nil {
				// Bot traffic: silently skipped, reported accepted so bots
				// don't retry.
				accepted++
				continue
			}
			prepared = append(prepared, *e)
		}
		if !buf.PushBatch(prepared) {
			return IngestResponse{}, neutron.ErrRateLimited(
				fmt.Sprintf("buffer capacity below batch size %d, retry the whole batch later", len(prepared)))
		}
		if batchKey != "" {
			deduper.record(batchKey)
		}
		accepted += len(prepared)
		return IngestResponse{OK: true, Accepted: accepted, Rejected: rejected}, nil
	}
}

func generateID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

type siteIDKey struct{}

// WithSiteID stores the site ID in the context (set by auth middleware).
func WithSiteID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, siteIDKey{}, id)
}

// SiteIDFromContext returns the site ID stored by auth middleware.
func SiteIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(siteIDKey{}).(string); ok {
		return v
	}
	return ""
}

// ErrSiteMismatch is returned by BoundSite when a body-supplied site_id
// disagrees with the site the authenticated API key was resolved to.
var ErrSiteMismatch = errors.New("site_id does not match the authenticated key")

// BoundSite resolves the effective site for an ingest write (audit F07).
// The key-bound context site is AUTHORITATIVE: a body site_id that disagrees
// with it is a cross-tenant write attempt (ErrSiteMismatch -> 403). An empty
// body site binds to the key's site. With no key-bound site (the no-keys
// grace period on a fresh install) the supplied site is trusted as before.
func BoundSite(ctx context.Context, requested string) (string, error) {
	authenticated := SiteIDFromContext(ctx)
	if authenticated == "" {
		return requested, nil
	}
	if requested != "" && requested != authenticated {
		return "", ErrSiteMismatch
	}
	return authenticated, nil
}
