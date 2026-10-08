package query

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/neutron-build/neutron/go/neutron"

	"github.com/useteploy/teploy-observe/internal/cohorts"
	"github.com/useteploy/teploy-observe/internal/guardmap"
)

// StatsInput is the common query input for dashboard API endpoints.
type StatsInput struct {
	SiteID      string `query:"site_id"`
	From        string `query:"from"`
	To          string `query:"to"`
	Limit       int    `query:"limit"`
	Interval    string `query:"interval"`
	Compare     string `query:"compare"`
	EventType   string `query:"event_type"`
	Channel     string `query:"channel"`
	Screen      string `query:"screen"`
	Pathname    string `query:"pathname"`
	Referrer    string `query:"referrer"`
	Browser     string `query:"browser"`
	OS          string `query:"os"`
	Device      string `query:"device"`
	Country     string `query:"country"`
	Language    string `query:"language"`
	UTMSource   string `query:"utm_source"`
	UTMMedium   string `query:"utm_medium"`
	UTMCampaign string `query:"utm_campaign"`
	// CohortID is the C2 cohort filter. When non-empty, the API layer
	// resolves it to a distinct_id IN (...) clause before issuing the
	// query. Backwards-compat: every existing chart keeps working with
	// no changes when cohort_id is absent.
	CohortID string `query:"cohort_id"`
}

func (i StatsInput) TimeRange() (time.Time, time.Time) {
	from, _ := time.Parse(time.RFC3339, i.From)
	to, _ := time.Parse(time.RFC3339, i.To)
	if from.IsZero() {
		from = time.Now().UTC().Add(-24 * time.Hour)
	}
	if to.IsZero() {
		to = time.Now().UTC()
	}
	return from, to
}

// Filters builds a FilterBuilder from the input's filter fields.
// Parameter numbering starts at $4 since $1=site_id, $2=from, $3=to.
func (i StatsInput) Filters() *FilterBuilder {
	fb := NewFilterBuilder(4)
	fb.Add("event_type", i.EventType)
	fb.Add("CAST(screen_width AS TEXT) || 'x' || CAST(screen_height AS TEXT)", i.Screen)
	fb.Add("pathname", i.Pathname)
	fb.Add("referrer", i.Referrer)
	fb.Add("browser", i.Browser)
	fb.Add("os", i.OS)
	fb.Add("device", i.Device)
	fb.Add("country", i.Country)
	fb.Add("language", i.Language)
	fb.Add("utm_source", i.UTMSource)
	fb.Add("utm_medium", i.UTMMedium)
	fb.Add("utm_campaign", i.UTMCampaign)
	return fb
}

// resolveFilters builds the FilterBuilder and, if CohortID is set and
// the StatsService has a cohort resolver wired, expands the cohort
// into a distinct_id IN (...) clause. Used by every analytics route
// handler so cohort filtering is uniform across pages, sessions,
// channels, etc.
//
// A cohort with zero members short-circuits to a "1 = 0" clause via
// FilterBuilder.AddIn, so the chart returns empty rather than the
// (mistaken) full unfiltered result.
//
// Fail closed: when the resolver errors, the request fails with a mapped
// HTTP error instead of silently dropping the cohort filter. Falling back
// to "unfiltered" painted a chart of ALL traffic under a cohort label,
// which is a wrong answer, not a degraded one. See cohortResolveError for
// the status mapping.
func (i StatsInput) resolveFilters(ctx context.Context, svc *StatsService) (*FilterBuilder, error) {
	fb := i.Filters()
	if i.Channel != "" {
		if svc == nil {
			return nil, neutron.ErrBadRequest("channel filtering requires stats service")
		}
		from, to := i.TimeRange()
		ids, err := svc.channelEventIDs(ctx, i.SiteID, i.Channel, from, to)
		if err != nil {
			return nil, err
		}
		fb.AddIn("event_id", ids)
	}
	if i.CohortID == "" || svc == nil {
		return fb, nil
	}
	ids, err := svc.resolveCohortAdmitted(ctx, i.SiteID, i.CohortID)
	if err != nil {
		return nil, cohortResolveFailure(ctx, i.SiteID, i.CohortID, err)
	}
	if ids == nil {
		// Resolver wired but cohort_id wasn't found / returned nil.
		// Treat as zero-members so the chart shows nothing (truthful
		// rather than "your cohort doesn't exist; here's all data").
		ids = []string{}
	}
	fb.AddIn("distinct_id", ids)
	return fb, nil
}

// cohortResolveFailure logs a cohort resolution failure (the raw error stays
// in the log, never in the response) and returns its HTTP mapping.
func cohortResolveFailure(ctx context.Context, siteID, cohortID string, err error) error {
	if !errors.Is(err, cohorts.ErrNotFound) {
		slog.Error("cohort resolve failed", "err", err, "site", siteID, "cohort", cohortID)
	}
	return cohortResolveError(err)
}

// cohortResolveError maps a cohort resolver failure to an HTTP error:
//   - cohort missing for this site (including another site's cohort id):
//     404, indistinguishable from a cohort that never existed;
//   - cohort larger than the filter can express (one SQL parameter per
//     member): 422, the caller must narrow the cohort;
//   - query-admission refusal (429 / 504): the refusal's own status;
//   - anything else (store failure, unreadable stored rule): 503, a
//     retryable server-side condition. The raw error is logged by the
//     caller, never echoed to the client.
func cohortResolveError(err error) error {
	if errors.Is(err, cohorts.ErrNotFound) {
		return neutron.ErrNotFound("cohort not found")
	}
	if errors.Is(err, cohorts.ErrTooLarge) {
		return neutron.ErrValidation(
			"cohort has more members than a chart filter supports; narrow the cohort definition",
			[]neutron.ValidationError{{Field: "cohort_id", Message: "cohort too large for chart filtering"}})
	}
	if mapped := guardmap.HTTPError(err); mapped != err {
		return mapped
	}
	return neutron.ErrServiceUnavailable("cohort filter could not be resolved; retry later")
}

// UTMInput extends StatsInput with a UTM type selector.
type UTMInput struct {
	StatsInput
	Type string `query:"type"`
}

// RealtimeInput queries real-time visitors.
type RealtimeInput struct {
	SiteID  string `query:"site_id"`
	Minutes int    `query:"minutes"`
}

// SessionsInput is used for the session browser list endpoint.
type SessionsInput struct {
	SiteID string `query:"site_id"`
	From   string `query:"from"`
	To     string `query:"to"`
	Limit  int    `query:"limit"`
}

func (i SessionsInput) TimeRange() (time.Time, time.Time) {
	from, _ := time.Parse(time.RFC3339, i.From)
	to, _ := time.Parse(time.RFC3339, i.To)
	if from.IsZero() {
		from = time.Now().UTC().Add(-24 * time.Hour)
	}
	if to.IsZero() {
		to = time.Now().UTC()
	}
	return from, to
}

// SessionDetailInput is used for the session detail endpoint.
type SessionDetailInput struct {
	ID     string `path:"id"`
	SiteID string `query:"site_id"`
}

// EventPropertyKeysInput queries property keys for a custom event type.
type EventPropertyKeysInput struct {
	Name   string `path:"name"`
	SiteID string `query:"site_id"`
	From   string `query:"from"`
	To     string `query:"to"`
}

func (i EventPropertyKeysInput) TimeRange() (time.Time, time.Time) {
	from, _ := time.Parse(time.RFC3339, i.From)
	to, _ := time.Parse(time.RFC3339, i.To)
	if from.IsZero() {
		from = time.Now().UTC().Add(-24 * time.Hour)
	}
	if to.IsZero() {
		to = time.Now().UTC()
	}
	return from, to
}

// EventPropertyValuesInput queries values for a specific property key.
type EventPropertyValuesInput struct {
	Name   string `path:"name"`
	Key    string `path:"key"`
	SiteID string `query:"site_id"`
	From   string `query:"from"`
	To     string `query:"to"`
}

func (i EventPropertyValuesInput) TimeRange() (time.Time, time.Time) {
	from, _ := time.Parse(time.RFC3339, i.From)
	to, _ := time.Parse(time.RFC3339, i.To)
	if from.IsZero() {
		from = time.Now().UTC().Add(-24 * time.Hour)
	}
	if to.IsZero() {
		to = time.Now().UTC()
	}
	return from, to
}

// FunnelInput is the request body for funnel analysis.
type FunnelInput struct {
	SiteID string       `json:"site_id"`
	From   string       `json:"from"`
	To     string       `json:"to"`
	Steps  []FunnelStep `json:"steps"`
	// Entity selects the grouped entity: "visit", "person", or
	// "visitor-estimate" (O03 ADR vocabulary). Empty = visitor-estimate.
	Entity string `json:"entity"`
	// ConversionWindowMs bounds the traversal from the first step's event
	// (edge inclusive). 0/absent = unbounded within the query range.
	ConversionWindowMs int64 `json:"conversion_window_ms"`
	// Exclusions are disqualifying steps (see FunnelOptions).
	Exclusions []FunnelStep `json:"exclusions"`
	// CohortID restricts the funnel to the cohort's members (see
	// FunnelOptions.CohortID).
	CohortID string `json:"cohort_id"`
}

// FunnelBreakdownInput augments FunnelInput with a breakdown dimension.
type FunnelBreakdownInput struct {
	ConversionWindowMs int64        `json:"conversion_window_ms"`
	Exclusions         []FunnelStep `json:"exclusions"`
	SiteID             string       `json:"site_id"`
	From               string       `json:"from"`
	To                 string       `json:"to"`
	Steps              []FunnelStep `json:"steps"`
	BreakdownBy        string       `json:"breakdown_by"`
	MinSize            int          `json:"min_size"`
	Entity             string       `json:"entity"`
	CohortID           string       `json:"cohort_id"`
}

func (i FunnelBreakdownInput) TimeRange() (time.Time, time.Time) {
	from, _ := time.Parse(time.RFC3339, i.From)
	to, _ := time.Parse(time.RFC3339, i.To)
	if from.IsZero() {
		from = time.Now().UTC().Add(-24 * time.Hour)
	}
	if to.IsZero() {
		to = time.Now().UTC()
	}
	return from, to
}

func (i FunnelInput) TimeRange() (time.Time, time.Time) {
	from, _ := time.Parse(time.RFC3339, i.From)
	to, _ := time.Parse(time.RFC3339, i.To)
	if from.IsZero() {
		from = time.Now().UTC().Add(-24 * time.Hour)
	}
	if to.IsZero() {
		to = time.Now().UTC()
	}
	return from, to
}

// RetentionInput is the query params for retention cohort analysis.
type RetentionInput struct {
	SiteID     string `query:"site_id"`
	From       string `query:"from"`
	To         string `query:"to"`
	PeriodDays int    `query:"period_days"`
	// Entity selects the grouped entity (see FunnelInput.Entity).
	Entity string `query:"entity"`
	// CohortEvent restricts cohort entry to entities whose first matching
	// event_type falls in the period (empty = first activity).
	CohortEvent string `query:"cohort_event"`
	// ReturnEvent restricts return-activity bucketing to an event_type
	// (empty = any event).
	ReturnEvent string `query:"return_event"`
	// CohortID restricts retention to the cohort's members (see
	// RetentionOptions.CohortID).
	CohortID string `query:"cohort_id"`
}

func (i RetentionInput) TimeRange() (time.Time, time.Time) {
	from, _ := time.Parse(time.RFC3339, i.From)
	to, _ := time.Parse(time.RFC3339, i.To)
	if from.IsZero() {
		from = time.Now().UTC().Add(-30 * 24 * time.Hour)
	}
	if to.IsZero() {
		to = time.Now().UTC()
	}
	return from, to
}

// RegisterRoutes registers all dashboard query API endpoints.
// Optional middleware is applied to the stats route group (e.g. JWT auth).
func RegisterRoutes(r *neutron.Router, svc *StatsService, mw ...neutron.Middleware) {
	api := r.Group("/api/v1/stats", mw...)

	neutron.Get(api, "/realtime", func(ctx context.Context, input RealtimeInput) (RealtimeResult, error) {
		if input.Minutes <= 0 {
			input.Minutes = 5
		}
		count, err := svc.RealtimeVisitors(ctx, input.SiteID, input.Minutes)
		if err != nil {
			slog.Error("realtime query failed", "err", err, "site", input.SiteID)
			return RealtimeResult{}, err
		}
		return RealtimeResult{ActiveVisitors: count}, nil
	}, neutron.WithTags("stats"))

	// What the visitor panels can honestly claim for this range: the retention
	// configuration and the range's start decide whether anything COULD be
	// missing, and the site's own earliest data — cached per site — decides
	// whether it actually is. A range inside retention runs no query at all.
	// The dashboard reads this to label the panels when the unique counts cover
	// a shorter window than the one the user picked.
	neutron.Get(api, "/unique-coverage", func(ctx context.Context, input StatsInput) (UniqueCoverage, error) {
		from, to := input.TimeRange()
		filters, err := input.resolveFilters(ctx, svc)
		if err != nil {
			return UniqueCoverage{}, err
		}
		return svc.UniqueCoverageFor(ctx, input.SiteID, from, to, filters), nil
	}, neutron.WithTags("stats"),
		neutron.WithSummary("Which table answers this range's unique counts, and how much of it they cover"))

	neutron.Get(api, "/overview", func(ctx context.Context, input StatsInput) (any, error) {
		from, to := input.TimeRange()
		filters, err := input.resolveFilters(ctx, svc)
		if err != nil {
			return nil, err
		}
		if input.Compare != "" {
			result, err := svc.OverviewWithComparison(ctx, input.SiteID, from, to, input.Compare, filters)
			if err != nil {
				slog.Error("overview comparison query failed", "err", err, "site", input.SiteID, "from", from, "to", to)
			}
			return result, err
		}
		result, err := svc.Overview(ctx, input.SiteID, from, to, filters)
		if err != nil {
			slog.Error("overview query failed", "err", err, "site", input.SiteID, "from", from, "to", to)
		}
		return result, err
	}, neutron.WithTags("stats"))

	neutron.Get(api, "/timeseries", func(ctx context.Context, input StatsInput) ([]TimeSeriesPoint, error) {
		from, to := input.TimeRange()
		filters, err := input.resolveFilters(ctx, svc)
		if err != nil {
			return nil, err
		}
		result, err := svc.PageviewTimeSeries(ctx, input.SiteID, from, to, input.Interval, filters)
		if err != nil {
			slog.Error("timeseries query failed", "err", err, "site", input.SiteID, "from", from, "to", to)
		}
		return result, err
	}, neutron.WithTags("stats"))

	neutron.Get(api, "/pages", func(ctx context.Context, input StatsInput) ([]TopPage, error) {
		from, to := input.TimeRange()
		filters, err := input.resolveFilters(ctx, svc)
		if err != nil {
			return nil, err
		}
		result, err := svc.TopPages(ctx, input.SiteID, from, to, input.Limit, filters)
		if err != nil {
			slog.Error("pages query failed", "err", err, "site", input.SiteID, "from", from, "to", to)
		}
		return result, err
	}, neutron.WithTags("stats"))

	neutron.Get(api, "/referrers", func(ctx context.Context, input StatsInput) ([]TopReferrer, error) {
		from, to := input.TimeRange()
		filters, err := input.resolveFilters(ctx, svc)
		if err != nil {
			return nil, err
		}
		result, err := svc.TopReferrers(ctx, input.SiteID, from, to, input.Limit, filters)
		if err != nil {
			slog.Error("referrers query failed", "err", err, "site", input.SiteID, "from", from, "to", to)
		}
		return result, err
	}, neutron.WithTags("stats"))

	neutron.Get(api, "/browsers", func(ctx context.Context, input StatsInput) ([]BrowserStat, error) {
		from, to := input.TimeRange()
		filters, err := input.resolveFilters(ctx, svc)
		if err != nil {
			return nil, err
		}
		result, err := svc.TopBrowsers(ctx, input.SiteID, from, to, input.Limit, filters)
		if err != nil {
			slog.Error("browsers query failed", "err", err, "site", input.SiteID, "from", from, "to", to)
		}
		return result, err
	}, neutron.WithTags("stats"))

	neutron.Get(api, "/countries", func(ctx context.Context, input StatsInput) ([]CountryStat, error) {
		from, to := input.TimeRange()
		filters, err := input.resolveFilters(ctx, svc)
		if err != nil {
			return nil, err
		}
		result, err := svc.TopCountries(ctx, input.SiteID, from, to, input.Limit, filters)
		if err != nil {
			slog.Error("countries query failed", "err", err, "site", input.SiteID, "from", from, "to", to)
		}
		return result, err
	}, neutron.WithTags("stats"))

	neutron.Get(api, "/os", func(ctx context.Context, input StatsInput) ([]OSStat, error) {
		from, to := input.TimeRange()
		filters, err := input.resolveFilters(ctx, svc)
		if err != nil {
			return nil, err
		}
		result, err := svc.TopOS(ctx, input.SiteID, from, to, input.Limit, filters)
		if err != nil {
			slog.Error("os query failed", "err", err, "site", input.SiteID, "from", from, "to", to)
		}
		return result, err
	}, neutron.WithTags("stats"))

	neutron.Get(api, "/devices", func(ctx context.Context, input StatsInput) ([]DeviceStat, error) {
		from, to := input.TimeRange()
		filters, err := input.resolveFilters(ctx, svc)
		if err != nil {
			return nil, err
		}
		result, err := svc.TopDevices(ctx, input.SiteID, from, to, input.Limit, filters)
		if err != nil {
			slog.Error("devices query failed", "err", err, "site", input.SiteID, "from", from, "to", to)
		}
		return result, err
	}, neutron.WithTags("stats"))

	neutron.Get(api, "/channels", func(ctx context.Context, input StatsInput) ([]ChannelStat, error) {
		from, to := input.TimeRange()
		filters, err := input.resolveFilters(ctx, svc)
		if err != nil {
			return nil, err
		}
		result, err := svc.TopChannels(ctx, input.SiteID, from, to, input.Limit, filters)
		if err != nil {
			slog.Error("channels query failed", "err", err, "site", input.SiteID, "from", from, "to", to)
		}
		return result, err
	}, neutron.WithTags("stats"))

	neutron.Get(api, "/languages", func(ctx context.Context, input StatsInput) ([]LanguageStat, error) {
		from, to := input.TimeRange()
		filters, err := input.resolveFilters(ctx, svc)
		if err != nil {
			return nil, err
		}
		result, err := svc.TopLanguages(ctx, input.SiteID, from, to, input.Limit, filters)
		if err != nil {
			slog.Error("languages query failed", "err", err, "site", input.SiteID, "from", from, "to", to)
		}
		return result, err
	}, neutron.WithTags("stats"))

	neutron.Get(api, "/screens", func(ctx context.Context, input StatsInput) ([]ScreenStat, error) {
		from, to := input.TimeRange()
		filters, err := input.resolveFilters(ctx, svc)
		if err != nil {
			return nil, err
		}
		result, err := svc.TopScreens(ctx, input.SiteID, from, to, input.Limit, filters)
		if err != nil {
			slog.Error("screens query failed", "err", err, "site", input.SiteID, "from", from, "to", to)
		}
		return result, err
	}, neutron.WithTags("stats"))

	neutron.Get(api, "/utm", func(ctx context.Context, input UTMInput) ([]UTMStat, error) {
		from, to := input.TimeRange()
		utmType := input.Type
		if utmType == "" {
			utmType = "source"
		}
		filters, err := input.resolveFilters(ctx, svc)
		if err != nil {
			return nil, err
		}
		result, err := svc.TopUTM(ctx, input.SiteID, from, to, utmType, input.Limit, filters)
		if err != nil {
			slog.Error("utm query failed", "err", err, "site", input.SiteID, "type", utmType, "from", from, "to", to)
		}
		return result, err
	}, neutron.WithTags("stats"))

	neutron.Get(api, "/entry-pages", func(ctx context.Context, input StatsInput) ([]EntryPageStat, error) {
		from, to := input.TimeRange()
		filters, err := input.resolveFilters(ctx, svc)
		if err != nil {
			return nil, err
		}
		result, err := svc.TopEntryPages(ctx, input.SiteID, from, to, input.Limit, filters)
		if err != nil {
			slog.Error("entry-pages query failed", "err", err, "site", input.SiteID, "from", from, "to", to)
		}
		return result, err
	}, neutron.WithTags("stats"))

	neutron.Get(api, "/exit-pages", func(ctx context.Context, input StatsInput) ([]ExitPageStat, error) {
		from, to := input.TimeRange()
		filters, err := input.resolveFilters(ctx, svc)
		if err != nil {
			return nil, err
		}
		result, err := svc.TopExitPages(ctx, input.SiteID, from, to, input.Limit, filters)
		if err != nil {
			slog.Error("exit-pages query failed", "err", err, "site", input.SiteID, "from", from, "to", to)
		}
		return result, err
	}, neutron.WithTags("stats"))

	neutron.Get(api, "/events", func(ctx context.Context, input StatsInput) ([]CustomEventStat, error) {
		from, to := input.TimeRange()
		filters, err := input.resolveFilters(ctx, svc)
		if err != nil {
			return nil, err
		}
		result, err := svc.CustomEvents(ctx, input.SiteID, from, to, input.Limit, filters)
		if err != nil {
			slog.Error("custom events query failed", "err", err, "site", input.SiteID, "from", from, "to", to)
		}
		return result, err
	}, neutron.WithTags("stats"))

	type eventPropsInput struct {
		SiteID    string `query:"site_id"`
		From      string `query:"from"`
		To        string `query:"to"`
		EventType string `query:"event_type"`
		Limit     int    `query:"limit"`
	}
	neutron.Get(api, "/event-properties", func(ctx context.Context, input eventPropsInput) ([]PropertyStat, error) {
		from, _ := time.Parse(time.RFC3339, input.From)
		to, _ := time.Parse(time.RFC3339, input.To)
		if from.IsZero() {
			from = time.Now().UTC().Add(-24 * time.Hour)
		}
		if to.IsZero() {
			to = time.Now().UTC()
		}
		return svc.EventProperties(ctx, input.SiteID, from, to, input.EventType, input.Limit)
	}, neutron.WithTags("stats"), neutron.WithSummary("Property breakdown for a custom event"))

	// Session browser
	neutron.Get(api, "/sessions", func(ctx context.Context, input SessionsInput) ([]SessionSummary, error) {
		from, to := input.TimeRange()
		result, err := svc.Sessions(ctx, input.SiteID, from, to, input.Limit)
		if err != nil {
			slog.Error("sessions query failed", "err", err, "site", input.SiteID, "from", from, "to", to)
		}
		return result, err
	}, neutron.WithTags("stats"))

	neutron.Get(api, "/sessions/{id}", func(ctx context.Context, input SessionDetailInput) ([]SessionEvent, error) {
		result, err := svc.SessionDetail(ctx, input.ID, input.SiteID)
		if err != nil {
			slog.Error("session detail query failed", "err", err, "session", input.ID, "site", input.SiteID)
		}
		return result, err
	}, neutron.WithTags("stats"))

	// Event property drill-down
	neutron.Get(api, "/events/{name}/properties", func(ctx context.Context, input EventPropertyKeysInput) ([]PropertyKeyStat, error) {
		from, to := input.TimeRange()
		result, err := svc.EventPropertyKeys(ctx, input.SiteID, input.Name, from, to)
		if err != nil {
			slog.Error("event property keys query failed", "err", err, "event", input.Name, "site", input.SiteID)
		}
		return result, err
	}, neutron.WithTags("stats"))

	neutron.Get(api, "/events/{name}/properties/{key}", func(ctx context.Context, input EventPropertyValuesInput) ([]PropertyValueStat, error) {
		from, to := input.TimeRange()
		result, err := svc.EventPropertyValues(ctx, input.SiteID, input.Name, input.Key, from, to)
		if err != nil {
			slog.Error("event property values query failed", "err", err, "event", input.Name, "key", input.Key, "site", input.SiteID)
		}
		return result, err
	}, neutron.WithTags("stats"))

	// Correlation analysis
	neutron.Get(api, "/correlations", func(ctx context.Context, input struct {
		StatsInput
		Target string `query:"target"`
	}) ([]Correlation, error) {
		from, to := input.TimeRange()
		target := input.Target
		if target == "" {
			target = "signup"
		}
		filters, err := input.resolveFilters(ctx, svc)
		if err != nil {
			return nil, err
		}
		return svc.CorrelationAnalysis(ctx, input.SiteID, target, from, to, filters)
	}, neutron.WithTags("stats"))

	// User journeys
	neutron.Get(api, "/journeys", func(ctx context.Context, input StatsInput) (*JourneyResult, error) {
		from, to := input.TimeRange()
		filters, err := input.resolveFilters(ctx, svc)
		if err != nil {
			return nil, err
		}
		return svc.Journeys(ctx, input.SiteID, from, to, input.Limit, filters)
	}, neutron.WithTags("stats"))

	// Funnel analysis
	neutron.Post(api, "/funnel", func(ctx context.Context, input FunnelInput) ([]FunnelResult, error) {
		from, to := input.TimeRange()
		return svc.FunnelWithOptions(ctx, input.SiteID, from, to, input.Steps, FunnelOptions{
			Entity:             input.Entity,
			ConversionWindowMs: input.ConversionWindowMs,
			Exclusions:         input.Exclusions,
			CohortID:           input.CohortID,
		})
	}, neutron.WithTags("stats"))

	// Funnel analysis with breakdown by a property (browser, country, device, os).
	neutron.Post(api, "/funnel/breakdown", func(ctx context.Context, input FunnelBreakdownInput) ([]FunnelBreakdownResult, error) {
		from, to := input.TimeRange()
		min := input.MinSize
		if min <= 0 {
			min = 5
		}
		return svc.FunnelByBreakdownWithOptions(ctx, input.SiteID, from, to, input.Steps, input.BreakdownBy, min, FunnelOptions{
			Entity:             input.Entity,
			CohortID:           input.CohortID,
			ConversionWindowMs: input.ConversionWindowMs,
			Exclusions:         input.Exclusions,
		})
	}, neutron.WithTags("stats"))

	// Retention cohort analysis
	neutron.Get(api, "/retention", func(ctx context.Context, input RetentionInput) ([]RetentionCohort, error) {
		from, to := input.TimeRange()
		return svc.RetentionWithOptions(ctx, input.SiteID, from, to, input.PeriodDays, RetentionOptions{
			Entity:      input.Entity,
			CohortEvent: input.CohortEvent,
			ReturnEvent: input.ReturnEvent,
			CohortID:    input.CohortID,
		})
	}, neutron.WithTags("stats"))
}
