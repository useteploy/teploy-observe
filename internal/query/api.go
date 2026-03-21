package query

import (
	"context"
	"log/slog"
	"time"

	"github.com/neutron-dev/neutron-go/neutron"
)

// StatsInput is the common query input for dashboard API endpoints.
type StatsInput struct {
	SiteID      string `query:"site_id"`
	From        string `query:"from"`
	To          string `query:"to"`
	Limit       int    `query:"limit"`
	Interval    string `query:"interval"`
	Compare     string `query:"compare"`
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

	neutron.Get(api, "/overview", func(ctx context.Context, input StatsInput) (any, error) {
		from, to := input.TimeRange()
		filters := input.Filters()
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
		result, err := svc.PageviewTimeSeries(ctx, input.SiteID, from, to, input.Interval, input.Filters())
		if err != nil {
			slog.Error("timeseries query failed", "err", err, "site", input.SiteID, "from", from, "to", to)
		}
		return result, err
	}, neutron.WithTags("stats"))

	neutron.Get(api, "/pages", func(ctx context.Context, input StatsInput) ([]TopPage, error) {
		from, to := input.TimeRange()
		result, err := svc.TopPages(ctx, input.SiteID, from, to, input.Limit, input.Filters())
		if err != nil {
			slog.Error("pages query failed", "err", err, "site", input.SiteID, "from", from, "to", to)
		}
		return result, err
	}, neutron.WithTags("stats"))

	neutron.Get(api, "/referrers", func(ctx context.Context, input StatsInput) ([]TopReferrer, error) {
		from, to := input.TimeRange()
		result, err := svc.TopReferrers(ctx, input.SiteID, from, to, input.Limit, input.Filters())
		if err != nil {
			slog.Error("referrers query failed", "err", err, "site", input.SiteID, "from", from, "to", to)
		}
		return result, err
	}, neutron.WithTags("stats"))

	neutron.Get(api, "/browsers", func(ctx context.Context, input StatsInput) ([]BrowserStat, error) {
		from, to := input.TimeRange()
		result, err := svc.TopBrowsers(ctx, input.SiteID, from, to, input.Limit, input.Filters())
		if err != nil {
			slog.Error("browsers query failed", "err", err, "site", input.SiteID, "from", from, "to", to)
		}
		return result, err
	}, neutron.WithTags("stats"))

	neutron.Get(api, "/countries", func(ctx context.Context, input StatsInput) ([]CountryStat, error) {
		from, to := input.TimeRange()
		result, err := svc.TopCountries(ctx, input.SiteID, from, to, input.Limit, input.Filters())
		if err != nil {
			slog.Error("countries query failed", "err", err, "site", input.SiteID, "from", from, "to", to)
		}
		return result, err
	}, neutron.WithTags("stats"))

	neutron.Get(api, "/os", func(ctx context.Context, input StatsInput) ([]OSStat, error) {
		from, to := input.TimeRange()
		result, err := svc.TopOS(ctx, input.SiteID, from, to, input.Limit, input.Filters())
		if err != nil {
			slog.Error("os query failed", "err", err, "site", input.SiteID, "from", from, "to", to)
		}
		return result, err
	}, neutron.WithTags("stats"))

	neutron.Get(api, "/devices", func(ctx context.Context, input StatsInput) ([]DeviceStat, error) {
		from, to := input.TimeRange()
		result, err := svc.TopDevices(ctx, input.SiteID, from, to, input.Limit, input.Filters())
		if err != nil {
			slog.Error("devices query failed", "err", err, "site", input.SiteID, "from", from, "to", to)
		}
		return result, err
	}, neutron.WithTags("stats"))

	neutron.Get(api, "/channels", func(ctx context.Context, input StatsInput) ([]ChannelStat, error) {
		from, to := input.TimeRange()
		result, err := svc.TopChannels(ctx, input.SiteID, from, to, input.Filters())
		if err != nil {
			slog.Error("channels query failed", "err", err, "site", input.SiteID, "from", from, "to", to)
		}
		return result, err
	}, neutron.WithTags("stats"))

	neutron.Get(api, "/languages", func(ctx context.Context, input StatsInput) ([]LanguageStat, error) {
		from, to := input.TimeRange()
		result, err := svc.TopLanguages(ctx, input.SiteID, from, to, input.Limit, input.Filters())
		if err != nil {
			slog.Error("languages query failed", "err", err, "site", input.SiteID, "from", from, "to", to)
		}
		return result, err
	}, neutron.WithTags("stats"))

	neutron.Get(api, "/screens", func(ctx context.Context, input StatsInput) ([]ScreenStat, error) {
		from, to := input.TimeRange()
		result, err := svc.TopScreens(ctx, input.SiteID, from, to, input.Limit, input.Filters())
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
		result, err := svc.TopUTM(ctx, input.SiteID, from, to, utmType, input.Limit, input.Filters())
		if err != nil {
			slog.Error("utm query failed", "err", err, "site", input.SiteID, "type", utmType, "from", from, "to", to)
		}
		return result, err
	}, neutron.WithTags("stats"))

	neutron.Get(api, "/entry-pages", func(ctx context.Context, input StatsInput) ([]EntryPageStat, error) {
		from, to := input.TimeRange()
		result, err := svc.TopEntryPages(ctx, input.SiteID, from, to, input.Limit, input.Filters())
		if err != nil {
			slog.Error("entry-pages query failed", "err", err, "site", input.SiteID, "from", from, "to", to)
		}
		return result, err
	}, neutron.WithTags("stats"))

	neutron.Get(api, "/exit-pages", func(ctx context.Context, input StatsInput) ([]ExitPageStat, error) {
		from, to := input.TimeRange()
		result, err := svc.TopExitPages(ctx, input.SiteID, from, to, input.Limit, input.Filters())
		if err != nil {
			slog.Error("exit-pages query failed", "err", err, "site", input.SiteID, "from", from, "to", to)
		}
		return result, err
	}, neutron.WithTags("stats"))

	neutron.Get(api, "/events", func(ctx context.Context, input StatsInput) ([]CustomEventStat, error) {
		from, to := input.TimeRange()
		result, err := svc.CustomEvents(ctx, input.SiteID, from, to, input.Limit, input.Filters())
		if err != nil {
			slog.Error("custom events query failed", "err", err, "site", input.SiteID, "from", from, "to", to)
		}
		return result, err
	}, neutron.WithTags("stats"))

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
}
