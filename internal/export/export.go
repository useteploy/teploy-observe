package export

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
)

// maxExportWindow bounds how wide a single export range may be, so a request
// can't ask the server to scan an unbounded slice of history. Memory stays O(1)
// per row via streaming regardless; this just caps total work.
const maxExportWindow = 186 * 24 * time.Hour

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
		if to.Sub(from) > maxExportWindow {
			http.Error(w, "export range too wide (max ~6 months); narrow from/to", http.StatusBadRequest)
			return
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

	// Stream rows from a cursor so memory stays O(1) per row; the previous
	// implementation buffered the entire result set, so one wide export could
	// OOM the server and take down ingestion for every site.
	rows, err := s.db.Pool().Query(ctx,
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
		pgx.QueryExecModeSimpleProtocol, siteID, fromMs, toMs,
	)
	if err != nil {
		queryError(w, err)
		return
	}
	defer rows.Close()

	header := []string{"event_id", "session_id", "event_type", "timestamp", "url", "pathname", "referrer", "browser", "os", "device", "country", "language", "utm_source", "utm_medium", "properties"}
	scan := func() (eventRow, error) {
		var r eventRow
		err := rows.Scan(&r.EventID, &r.SiteID, &r.SessionID, &r.EventType, &r.Timestamp,
			&r.URL, &r.Pathname, &r.Referrer, &r.Browser, &r.OS, &r.Device,
			&r.Country, &r.Language, &r.UTMSource, &r.UTMMedium, &r.Properties)
		return r, err
	}
	toRow := func(r eventRow) []string {
		return []string{r.EventID, r.SessionID, r.EventType, strconv.FormatInt(r.Timestamp, 10),
			r.URL, r.Pathname, r.Referrer, r.Browser, r.OS, r.Device,
			r.Country, r.Language, r.UTMSource, r.UTMMedium, r.Properties}
	}

	if format == "csv" {
		setCSVHeaders(w, "events")
		streamCSV(w, rows, header, scan, toRow, "events")
	} else {
		setJSONHeaders(w, "events")
		streamJSON(w, rows, scan, "events")
	}
}

func (s *ExportService) exportSessions(ctx context.Context, w http.ResponseWriter, format, siteID string, from, to time.Time) {
	fromMs := dbutil.IntParam(from.UnixMilli())
	toMs := dbutil.IntParam(to.UnixMilli())

	rows, err := s.db.Pool().Query(ctx,
		`SELECT session_id, site_id, first_ts, last_ts, pageviews,
			COALESCE(entry_url, '') AS entry_url, COALESCE(exit_url, '') AS exit_url,
			COALESCE(browser, '') AS browser, COALESCE(os, '') AS os,
			COALESCE(device, '') AS device, COALESCE(country, '') AS country,
			COALESCE(is_bounce, 'false') AS is_bounce
		 FROM sessions
		 WHERE site_id = $1 AND first_ts >= $2 AND first_ts < $3
		 ORDER BY first_ts DESC`,
		pgx.QueryExecModeSimpleProtocol, siteID, fromMs, toMs,
	)
	if err != nil {
		queryError(w, err)
		return
	}
	defer rows.Close()

	header := []string{"session_id", "first_ts", "last_ts", "pageviews", "entry_url", "exit_url", "browser", "os", "device", "country", "is_bounce"}
	scan := func() (sessionRow, error) {
		var r sessionRow
		err := rows.Scan(&r.SessionID, &r.SiteID, &r.FirstTS, &r.LastTS, &r.Pageviews,
			&r.EntryURL, &r.ExitURL, &r.Browser, &r.OS, &r.Device, &r.Country, &r.IsBounce)
		return r, err
	}
	toRow := func(r sessionRow) []string {
		return []string{r.SessionID, strconv.FormatInt(r.FirstTS, 10), strconv.FormatInt(r.LastTS, 10),
			strconv.FormatInt(r.Pageviews, 10), r.EntryURL, r.ExitURL,
			r.Browser, r.OS, r.Device, r.Country, r.IsBounce}
	}

	if format == "csv" {
		setCSVHeaders(w, "sessions")
		streamCSV(w, rows, header, scan, toRow, "sessions")
	} else {
		setJSONHeaders(w, "sessions")
		streamJSON(w, rows, scan, "sessions")
	}
}

// streamCSV writes the header then each scanned row, sanitizing every cell
// against CSV/formula injection, flushing periodically to bound buffering.
//
// OBS-028: a scan/write/flush failure used to `break` silently — no log, no
// error surfaced, and the response simply stopped, indistinguishable from a
// row count that happened to end there. By the time any of this runs, HTTP
// headers are already committed 200 OK (streaming to bound memory means we
// can't buffer the whole export to validate it first), so the status code
// itself can never signal a mid-stream failure — the two things that CAN
// still signal it are logs and the data itself. On any failure this appends
// a sentinel row a consumer can check for, and always logs what happened
// server-side with the row count reached, so an incomplete export is
// detectable instead of silently accepted as complete.
func streamCSV[T any](w http.ResponseWriter, rows pgx.Rows, header []string, scan func() (T, error), toRow func(T) []string, kind string) {
	cw := csv.NewWriter(w)
	if err := cw.Write(header); err != nil {
		slog.Error("export: writing CSV header failed", "kind", kind, "err", err)
		return
	}
	n := 0
	var failErr error
	for rows.Next() {
		v, err := scan()
		if err != nil {
			failErr = err
			break
		}
		rec := toRow(v)
		for i := range rec {
			rec[i] = csvSafe(rec[i])
		}
		if err := cw.Write(rec); err != nil {
			failErr = err
			break
		}
		n++
		if n%1000 == 0 {
			cw.Flush()
			if err := cw.Error(); err != nil {
				failErr = err
				break
			}
		}
	}
	if failErr == nil {
		if err := rows.Err(); err != nil {
			failErr = err
		}
	}
	if failErr != nil {
		// Grep-able sentinel: a consumer that cares about completeness can
		// check the last row; nothing changes for one that doesn't.
		_ = cw.Write([]string{"__export_incomplete__", "streaming failed after this row"})
	}
	cw.Flush()
	if failErr != nil {
		slog.Error("export: CSV stream failed partway through", "kind", kind, "rows_written", n, "err", failErr)
	}
}

// streamJSON writes a JSON array incrementally so memory stays O(1) per row.
//
// OBS-028: on a scan failure this used to still close the array with `]`,
// so a truncated export was syntactically valid JSON and looked complete —
// the worst version of this bug, since nothing about the file itself hints
// at the missing rows. Now a failure leaves the array deliberately
// unclosed: any conforming JSON parser rejects the response outright rather
// than silently accepting a partial result, and the failure (with the row
// count reached) is logged server-side, matching streamCSV's contract.
func streamJSON[T any](w http.ResponseWriter, rows pgx.Rows, scan func() (T, error), kind string) {
	enc := json.NewEncoder(w)
	io.WriteString(w, "[")
	first := true
	n := 0
	var failErr error
	for rows.Next() {
		v, err := scan()
		if err != nil {
			failErr = err
			break
		}
		if !first {
			io.WriteString(w, ",")
		}
		first = false
		if err := enc.Encode(v); err != nil { // Encode appends a newline; acceptable inside the array
			failErr = err
			break
		}
		n++
	}
	if failErr == nil {
		if err := rows.Err(); err != nil {
			failErr = err
		}
	}
	if failErr != nil {
		slog.Error("export: JSON stream failed partway through — response left as invalid/truncated JSON on purpose", "kind", kind, "rows_written", n, "err", failErr)
		return
	}
	io.WriteString(w, "]")
}

// csvSafe neutralizes spreadsheet formula injection: a cell beginning with one
// of = + - @ (or a control char) is prefixed with an apostrophe so it is not
// interpreted as a formula on open.
func csvSafe(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

// queryError logs the detail server-side and returns a generic message so raw
// (possibly attacker-influenced) SQL errors aren't reflected to the client.
func queryError(w http.ResponseWriter, err error) {
	slog.Error("export query failed", "err", err)
	http.Error(w, "export failed", http.StatusInternalServerError)
}

func setCSVHeaders(w http.ResponseWriter, name string) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="observe-%s.csv"`, name))
}

func setJSONHeaders(w http.ResponseWriter, name string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="observe-%s.json"`, name))
}
