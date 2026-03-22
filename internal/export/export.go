package export

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/teploy/observe/internal/dbutil"
)

type ExportService struct {
	db *nucleus.Client
}

func NewExportService(db *nucleus.Client) *ExportService {
	return &ExportService{db: db}
}

type eventRow struct {
	EventID    string `db:"event_id"`
	SiteID     string `db:"site_id"`
	SessionID  string `db:"session_id"`
	EventType  string `db:"event_type"`
	Timestamp  int64  `db:"timestamp"`
	URL        string `db:"url"`
	Pathname   string `db:"pathname"`
	Referrer   string `db:"referrer"`
	Browser    string `db:"browser"`
	OS         string `db:"os"`
	Device     string `db:"device"`
	Country    string `db:"country"`
	Language   string `db:"language"`
	UTMSource  string `db:"utm_source"`
	UTMMedium  string `db:"utm_medium"`
	Properties string `db:"properties"`
}

type sessionRow struct {
	SessionID string `db:"session_id"`
	SiteID    string `db:"site_id"`
	FirstTS   int64  `db:"first_ts"`
	LastTS    int64  `db:"last_ts"`
	Pageviews int64  `db:"pageviews"`
	EntryURL  string `db:"entry_url"`
	ExitURL   string `db:"exit_url"`
	Browser   string `db:"browser"`
	OS        string `db:"os"`
	Device    string `db:"device"`
	Country   string `db:"country"`
	IsBounce  string `db:"is_bounce"`
}

// Handler returns an http.HandlerFunc for the export endpoint.
// Query params: format (csv|json), type (events|sessions), site_id, from, to
func (s *ExportService) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		format := q.Get("format")
		dataType := q.Get("type")
		siteID := q.Get("site_id")
		fromStr := q.Get("from")
		toStr := q.Get("to")

		if siteID == "" {
			http.Error(w, "site_id required", http.StatusBadRequest)
			return
		}
		if format == "" {
			format = "json"
		}
		if dataType == "" {
			dataType = "events"
		}
		if format != "csv" && format != "json" {
			http.Error(w, "format must be csv or json", http.StatusBadRequest)
			return
		}
		if dataType != "events" && dataType != "sessions" {
			http.Error(w, "type must be events or sessions", http.StatusBadRequest)
			return
		}

		from, err := time.Parse(time.RFC3339, fromStr)
		if err != nil {
			from = time.Now().UTC().Add(-24 * time.Hour)
		}
		to, err := time.Parse(time.RFC3339, toStr)
		if err != nil {
			to = time.Now().UTC()
		}

		ctx := r.Context()

		switch dataType {
		case "events":
			s.exportEvents(ctx, w, format, siteID, from, to)
		case "sessions":
			s.exportSessions(ctx, w, format, siteID, from, to)
		}
	}
}

func (s *ExportService) exportEvents(ctx context.Context, w http.ResponseWriter, format, siteID string, from, to time.Time) {
	fromMs := dbutil.IntParam(from.UnixMilli())
	toMs := dbutil.IntParam(to.UnixMilli())

	rows, err := nucleus.Query[eventRow](ctx, s.db.SQL(),
		`SELECT event_id, site_id, session_id, event_type, timestamp,
			COALESCE(url, '') AS url, COALESCE(pathname, '') AS pathname,
			COALESCE(referrer, '') AS referrer, COALESCE(browser, '') AS browser,
			COALESCE(os, '') AS os, COALESCE(device, '') AS device,
			COALESCE(country, '') AS country, COALESCE(language, '') AS language,
			COALESCE(utm_source, '') AS utm_source, COALESCE(utm_medium, '') AS utm_medium,
			COALESCE(properties, '') AS properties
		 FROM events
		 WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3
		 ORDER BY timestamp DESC`,
		siteID, fromMs, toMs,
	)
	if err != nil {
		http.Error(w, fmt.Sprintf("query failed: %v", err), http.StatusInternalServerError)
		return
	}

	if format == "csv" {
		setCSVHeaders(w, "events")
		cw := csv.NewWriter(w)
		cw.Write([]string{"event_id", "session_id", "event_type", "timestamp", "url", "pathname", "referrer", "browser", "os", "device", "country", "language", "utm_source", "utm_medium", "properties"})
		for _, r := range rows {
			cw.Write([]string{
				r.EventID, r.SessionID, r.EventType, strconv.FormatInt(r.Timestamp, 10),
				r.URL, r.Pathname, r.Referrer, r.Browser, r.OS, r.Device,
				r.Country, r.Language, r.UTMSource, r.UTMMedium, r.Properties,
			})
		}
		cw.Flush()
	} else {
		setJSONHeaders(w, "events")
		writeJSONArray(w, rows)
	}
}

func (s *ExportService) exportSessions(ctx context.Context, w http.ResponseWriter, format, siteID string, from, to time.Time) {
	fromMs := dbutil.IntParam(from.UnixMilli())
	toMs := dbutil.IntParam(to.UnixMilli())

	rows, err := nucleus.Query[sessionRow](ctx, s.db.SQL(),
		`SELECT session_id, site_id, first_ts, last_ts, pageviews,
			COALESCE(entry_url, '') AS entry_url, COALESCE(exit_url, '') AS exit_url,
			COALESCE(browser, '') AS browser, COALESCE(os, '') AS os,
			COALESCE(device, '') AS device, COALESCE(country, '') AS country,
			COALESCE(is_bounce, 'false') AS is_bounce
		 FROM sessions
		 WHERE site_id = $1 AND first_ts >= $2 AND first_ts < $3
		 ORDER BY first_ts DESC`,
		siteID, fromMs, toMs,
	)
	if err != nil {
		http.Error(w, fmt.Sprintf("query failed: %v", err), http.StatusInternalServerError)
		return
	}

	if format == "csv" {
		setCSVHeaders(w, "sessions")
		cw := csv.NewWriter(w)
		cw.Write([]string{"session_id", "first_ts", "last_ts", "pageviews", "entry_url", "exit_url", "browser", "os", "device", "country", "is_bounce"})
		for _, r := range rows {
			cw.Write([]string{
				r.SessionID, strconv.FormatInt(r.FirstTS, 10), strconv.FormatInt(r.LastTS, 10),
				strconv.FormatInt(r.Pageviews, 10), r.EntryURL, r.ExitURL,
				r.Browser, r.OS, r.Device, r.Country, r.IsBounce,
			})
		}
		cw.Flush()
	} else {
		setJSONHeaders(w, "sessions")
		writeJSONArray(w, rows)
	}
}

func setCSVHeaders(w http.ResponseWriter, name string) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="observe-%s.csv"`, name))
}

func setJSONHeaders(w http.ResponseWriter, name string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="observe-%s.json"`, name))
}

func writeJSONArray(w io.Writer, data any) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(data)
}
