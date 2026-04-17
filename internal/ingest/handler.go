package ingest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"time"

	"github.com/neutron-dev/neutron-go/neutron"

	"github.com/useteploy/observe/internal/geo"
	"github.com/useteploy/observe/internal/session"
)

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
}

// IngestResponse is returned to the tracker.
type IngestResponse struct {
	OK bool `json:"ok"`
}

// Handler returns the typed Neutron handler for event ingestion.
func Handler(buf *Buffer, salt string) neutron.HandlerFunc[IngestInput, IngestResponse] {
	return func(ctx context.Context, input IngestInput) (IngestResponse, error) {
		now := time.Now().UTC()
		ip := ClientIPFromContext(ctx)
		ua := UserAgentFromContext(ctx)

		// Drop bot traffic silently — return OK so bots don't retry
		if IsBot(ua) {
			return IngestResponse{OK: true}, nil
		}

		// Input validation
		if len(input.URL) > 2048 {
			return IngestResponse{}, neutron.ErrBadRequest("url too long (max 2048)")
		}
		if len(input.Title) > 512 {
			input.Title = input.Title[:512]
		}
		if len(input.Referrer) > 2048 {
			input.Referrer = input.Referrer[:2048]
		}
		if input.Properties != nil && len(input.Properties) > 50 {
			return IngestResponse{}, neutron.ErrBadRequest("too many properties (max 50)")
		}

		// Site ID priority: JSON body > middleware context (header) > empty
		siteID := input.SiteID
		if siteID == "" {
			siteID = SiteIDFromContext(ctx)
		}
		if siteID == "" {
			return IngestResponse{}, neutron.ErrBadRequest("missing site_id")
		}

		sessionID := session.ID(siteID, ip, ua, salt)
		visitID := session.VisitID(sessionID, now)
		eventID := generateID()
		parsed := ParseUA(ua)
		country := geo.Lookup(ip)

		eventType := input.EventType
		if eventType == "" {
			eventType = "pageview"
		}

		var hostname, pathname string
		if input.URL != "" {
			if u, err := url.Parse(input.URL); err == nil {
				hostname = u.Hostname()
				pathname = u.Path
			}
		}

		// Clean referrer: strip query params, keep scheme+host+path
		referrer := cleanReferrer(input.Referrer, hostname)

		var sw, sh int
		if input.Screen != "" {
			fmt.Sscanf(input.Screen, "%dx%d", &sw, &sh)
		}

		// Extract UTM params from URL
		var utmSource, utmMedium, utmCampaign, utmTerm, utmContent string
		if input.URL != "" {
			if u, err := url.Parse(input.URL); err == nil {
				q := u.Query()
				utmSource = q.Get("utm_source")
				utmMedium = q.Get("utm_medium")
				utmCampaign = q.Get("utm_campaign")
				utmTerm = q.Get("utm_term")
				utmContent = q.Get("utm_content")
			}
		}

		e := Event{
			EventID:        eventID,
			TenantID:       "default",
			SiteID:         siteID,
			SessionID:      sessionID,
			VisitID:        visitID,
			EventType:      eventType,
			Timestamp:      now.UnixMilli(),
			URL:            input.URL,
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
		}

		if !buf.Push(e) {
			return IngestResponse{}, neutron.ErrRateLimited("buffer full, try again later")
		}

		return IngestResponse{OK: true}, nil
	}
}

// cleanReferrer normalizes a referrer URL:
//   - Strips query parameters and fragments
//   - Returns empty string for self-referrals (same hostname)
//   - Returns just scheme+host+path
func cleanReferrer(raw, selfHost string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	// Drop self-referrals
	if selfHost != "" && u.Hostname() == selfHost {
		return ""
	}
	// Strip query and fragment
	u.RawQuery = ""
	u.Fragment = ""
	// Remove trailing slash for consistency
	result := u.String()
	if len(result) > 1 && result[len(result)-1] == '/' {
		result = result[:len(result)-1]
	}
	return result
}

// BatchInput accepts an array of events in one POST.
type BatchInput struct {
	Events []IngestInput `json:"events"`
}

// BatchHandler processes multiple events in a single request.
// This is the preferred ingestion path — the tracker sends all queued
// events as one POST instead of one request per event.
func BatchHandler(buf *Buffer, salt string) neutron.HandlerFunc[BatchInput, IngestResponse] {
	singleHandler := Handler(buf, salt)
	return func(ctx context.Context, input BatchInput) (IngestResponse, error) {
		if len(input.Events) > 100 {
			return IngestResponse{}, neutron.ErrBadRequest("batch too large (max 100 events)")
		}
		if len(input.Events) == 0 {
			return IngestResponse{OK: true}, nil
		}
		for _, ev := range input.Events {
			if _, err := singleHandler(ctx, ev); err != nil {
				return IngestResponse{}, err
			}
		}
		return IngestResponse{OK: true}, nil
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
