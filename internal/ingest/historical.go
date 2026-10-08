package ingest

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/neutron-build/neutron/go/neutron"
	"github.com/useteploy/teploy-observe/internal/session"
	"github.com/useteploy/teploy-observe/internal/sites"
)

// HistoricalInput is the authenticated import contract. Timestamps are original
// epoch milliseconds; event_id and session_id are stable source identities.
// Only the key-bound site is writable. A batch is prepared before any admission.
type HistoricalInput struct {
	Events []Event `json:"events"`
}

// HistoricalHandler must be registered behind APIKeyAuthMiddleware. Imports
// deliberately do not apply the live browser bot filter to the transport UA.
func HistoricalHandler(buf *Buffer, salt string, siteSvc *sites.SiteService) neutron.HandlerFunc[HistoricalInput, IngestResponse] {
	return func(ctx context.Context, input HistoricalInput) (IngestResponse, error) {
		if SiteIDFromContext(ctx) == "" {
			return IngestResponse{}, neutron.ErrUnauthorized("import requires a site API key")
		}
		if len(input.Events) == 0 || len(input.Events) > 100 {
			return IngestResponse{}, neutron.ErrBadRequest("import requires 1-100 events")
		}
		prepared := make([]Event, 0, len(input.Events))
		for _, source := range input.Events {
			e, err := prepareHistorical(ctx, source, salt, siteSvc, time.Now().UTC())
			if err != nil {
				return IngestResponse{}, err
			}
			if err := buf.checkErasure(ctx, e); err != nil {
				return IngestResponse{}, err
			}
			prepared = append(prepared, *e)
		}
		if err := buf.PushBatch(prepared); err != nil {
			return IngestResponse{}, admissionError(err)
		}
		return IngestResponse{OK: true, Accepted: len(prepared)}, nil
	}
}

func prepareHistorical(ctx context.Context, source Event, salt string, siteSvc *sites.SiteService, now time.Time) (*Event, error) {
	if !validProducerID(source.EventID) || len(source.SessionID) == 0 || len(source.SessionID) > 256 || !utf8.ValidString(source.SessionID) || strings.ContainsRune(source.SessionID, 0) {
		return nil, neutron.ErrBadRequest("import requires stable event_id (8-64 URL-safe characters) and session_id (1-256 bytes)")
	}
	if source.Timestamp <= 0 || source.Timestamp > now.Add(24*time.Hour).UnixMilli() {
		return nil, neutron.ErrBadRequest("invalid import timestamp (epoch milliseconds)")
	}
	if len(source.Pathname) > 2048 || (source.Pathname != "" && !strings.HasPrefix(source.Pathname, "/")) {
		return nil, neutron.ErrBadRequest("invalid import pathname")
	}
	if len(source.VisitID) > 256 || !utf8.ValidString(source.VisitID) || strings.ContainsRune(source.VisitID, 0) {
		return nil, neutron.ErrBadRequest("invalid import visit_id")
	}
	// Reuse live validation, privacy policy and URL/property sanitization.
	e, err := prepareEventWithMode(ctx, IngestInput{SiteID: source.SiteID, EventID: source.EventID, EventType: source.EventType, URL: source.URL, Referrer: source.Referrer, Title: source.Title, Language: source.Language, Properties: source.Properties, DistinctID: source.DistinctID, Release: source.ReleaseTag, UTMSource: source.UTMSource, UTMMedium: source.UTMMedium, UTMCampaign: source.UTMCampaign, UTMTerm: source.UTMTerm, UTMContent: source.UTMContent}, salt, optionalPrivacyLookup(siteSvc), true)
	if err != nil {
		return nil, err
	}
	e.Timestamp, e.SessionID = source.Timestamp, source.SessionID
	e.VisitID = source.VisitID
	if e.VisitID == "" {
		e.VisitID = session.VisitID(source.SessionID, time.UnixMilli(source.Timestamp))
	}
	if source.Pathname != "" {
		e.Pathname = source.Pathname
	}
	e.Browser, e.OS, e.Device, e.Country = truncateUTF8(source.Browser, 128), truncateUTF8(source.OS, 128), truncateUTF8(source.Device, 64), truncateUTF8(source.Country, 8)
	if source.ScreenWidth > 0 && source.ScreenWidth <= 65535 && source.ScreenHeight > 0 && source.ScreenHeight <= 65535 {
		e.ScreenWidth, e.ScreenHeight = source.ScreenWidth, source.ScreenHeight
	}
	return e, nil
}
