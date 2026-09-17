package replays

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
	"github.com/useteploy/teploy-observe/internal/heatmaps"
	"github.com/useteploy/teploy-observe/internal/identity"
	"github.com/useteploy/teploy-observe/internal/query"
)

// ErrCrossSiteReplay indicates the caller tried to append events to a replay
// owned by a different site (audit F08). Handlers map it to 403.
var ErrCrossSiteReplay = errors.New("replay_id belongs to a different site")

// replaySessionCols are the non-key columns of replay_sessions, in the order
// the collapse helpers expect (see internal/query/replacing.go). The ORDER BY
// key is (tenant_id, site_id, start_time, replay_id); key columns are selected
// verbatim by LatestRows and must not appear here.
var replaySessionCols = []string{
	"session_id", "duration_ms", "page_count", "url", "browser", "os",
	"device", "has_error", "distinct_id",
}

func replaySessionsLatest(where string) string {
	return query.LatestRows("replay_sessions", replaySessionCols, where) + " AS replay_sessions"
}

// hashDistinctID is a local alias so the call site reads cleanly. The
// real impl lives in internal/identity.
func hashDistinctID(raw, salt string, rawOptIn bool) string {
	return identity.MaybeHashDistinctID(raw, salt, rawOptIn)
}

// PrivacyLookup mirrors errors.PrivacyLookup — see that doc for shape.
// Duplicated here to avoid a replays -> errors import cycle (errors
// already depends on sourcemaps; both packages need the same lookup).
type PrivacyLookup func(ctx context.Context, siteID string) (salt string, rawOptIn bool, ok bool)

type ReplayService struct {
	db       *nucleus.Client
	heatmaps *heatmaps.Service
	logger   *slog.Logger
	privacy  PrivacyLookup
	salt     string
}

func NewReplayService(db *nucleus.Client) *ReplayService {
	return &ReplayService{
		db:       db,
		heatmaps: heatmaps.NewService(db),
		logger:   slog.Default(),
	}
}

// WithPrivacy installs the per-site distinct_id hashing lookup and a
// fallback global salt for sites the lookup doesn't know about.
func (s *ReplayService) WithPrivacy(lookup PrivacyLookup, fallbackSalt string) *ReplayService {
	s.privacy = lookup
	s.salt = fallbackSalt
	return s
}

// WithLogger threads a custom logger so heatmap-rollup write failures
// surface under the same handler context as the replay ingest itself.
func (s *ReplayService) WithLogger(logger *slog.Logger) *ReplayService {
	if logger != nil {
		s.logger = logger
	}
	return s
}

// ReplaySession is the domain type with typed fields.
type ReplaySession struct {
	ReplayID  string    `json:"replay_id"`
	SiteID    string    `json:"site_id"`
	SessionID string    `json:"session_id"`
	StartTime time.Time `json:"start_time"`
	Duration  int64     `json:"duration_ms"`
	PageCount int       `json:"page_count"`
	URL       string    `json:"url"`
	Browser   string    `json:"browser"`
	OS        string    `json:"os"`
	Device    string    `json:"device"`
	HasError  bool      `json:"has_error"`
}

// ReplayEvent is the domain type for replay events.
type ReplayEvent struct {
	EventID   string    `json:"event_id"`
	ReplayID  string    `json:"replay_id"`
	Timestamp time.Time `json:"timestamp"`
	EventType string    `json:"event_type"`
	Data      string    `json:"data"`
}

// IngestInput is the JSON body from the replay SDK.
//
// ViewportWidth is optional; when set it seeds the heatmap aggregator with
// a vw_bucket for clicks that occur before any `resize` event in the
// batch. The replay SDK populates it from `window.innerWidth` at flush
// time (see cmd/observe/tracker/observe-replay.js).
type IngestInput struct {
	SiteID    string `json:"site_id"`
	SessionID string `json:"session_id"`
	// ReplayID is generated client-side so observe-errors.js can attach
	// errors to the same replay before the first batch reaches the server.
	// Empty -> the server assigns a fresh id.
	ReplayID      string `json:"replay_id"`
	URL           string `json:"url"`
	Browser       string `json:"browser"`
	OS            string `json:"os"`
	Device        string `json:"device"`
	HasError      bool   `json:"has_error"`
	ViewportWidth int    `json:"viewport_width"`
	// DistinctID, when present, is the user identifier the SDK passed
	// via identify(userId). Hashed with the per-site session_salt
	// before storage.
	DistinctID string `json:"distinct_id,omitempty"`
	Events     []struct {
		Type      string `json:"type"`
		Timestamp int64  `json:"timestamp"`
		Data      any    `json:"data"`
	} `json:"events"`
}

// existingSession is the collapsed current state of one replay's session row,
// read before writing a new version (see upsertSession).
type existingSession struct {
	SiteID     string `db:"site_id"`
	StartTime  int64  `db:"start_time"`
	DurationMS int64  `db:"duration_ms"`
	PageCount  int64  `db:"page_count"`
	HasError   string `db:"has_error"`
	Version    int64  `db:"version"`
}

func (e existingSession) exists() bool   { return e.SiteID != "" }
func (e existingSession) hasError() bool { return e.HasError == "true" }

// batchAggregate is the metadata one batch contributes to its session row.
// Duration is min..max over the batch's timestamped events (not first/last
// positional — batches are not guaranteed timestamp-ordered); pages count
// navigation events plus the initial page, not raw event count (audit F20:
// 100 mouse events must not become 100 pages).
type batchAggregate struct {
	StartMS     int64
	EndMS       int64
	Navigations int64
	Initialized bool
}

func aggregateBatch(input *IngestInput) batchAggregate {
	var agg batchAggregate
	for _, ev := range input.Events {
		if ev.Timestamp <= 0 {
			continue
		}
		if !agg.Initialized {
			agg.StartMS, agg.EndMS, agg.Initialized = ev.Timestamp, ev.Timestamp, true
		}
		if ev.Timestamp < agg.StartMS {
			agg.StartMS = ev.Timestamp
		}
		if ev.Timestamp > agg.EndMS {
			agg.EndMS = ev.Timestamp
		}
		if ev.Type == "navigation" {
			agg.Navigations++
		}
	}
	return agg
}

// replayOwner resolves the site that owns replayID through the collapsed
// session table. Empty siteID means no session exists yet. A transport error
// is returned (fail closed) — an unavailable store must not read as "no
// owner" and let a cross-site append through.
func (s *ReplayService) replayOwner(ctx context.Context, replayID string) (string, error) {
	rows, err := nucleus.Query[struct {
		SiteID string `db:"site_id"`
	}](ctx, s.db.SQL(),
		`SELECT site_id FROM `+replaySessionsLatest("replay_id = $1"), replayID)
	if err != nil {
		return "", fmt.Errorf("replays: ownership lookup: %w", err)
	}
	for _, r := range rows {
		if r.SiteID != "" {
			return r.SiteID, nil
		}
	}
	return "", nil
}

// upsertSession writes the session row as a new version of the replacing
// table, merging this batch's aggregates into whatever is already recorded
// (audit F20: duration grows to the max seen, has_error is sticky, page_count
// accumulates navigations). Collapsing by (tenant, site, start_time,
// replay_id) keeps one visible row per replay, so there is no claim-then-
// insert window to orphan (audit F19 — the old KV SetNX guard is gone).
func (s *ReplayService) upsertSession(ctx context.Context, input *IngestInput, replayID string, agg batchAggregate, distinctID string) error {
	rows, err := nucleus.Query[existingSession](ctx, s.db.SQL(),
		`SELECT site_id, start_time,
		        CAST(duration_ms AS BIGINT) AS duration_ms,
		        CAST(page_count AS BIGINT) AS page_count,
		        has_error,
		        MAX(version) AS version
		 FROM replay_sessions
		 WHERE replay_id = $1 AND site_id = $2
		 GROUP BY tenant_id, site_id, start_time, replay_id`, replayID, input.SiteID)
	if err != nil {
		// Fail the batch rather than guess at stored aggregates: a fresh
		// insert under a read error could fork a second visible session
		// (different start_time -> different ORDER BY key -> no collapse).
		return fmt.Errorf("replays: read session state: %w", err)
	}
	var existing existingSession
	if len(rows) > 0 {
		existing = rows[0]
	}

	startTime := agg.StartMS
	if !agg.Initialized {
		startTime = time.Now().UTC().UnixMilli()
	}
	if existing.exists() && existing.StartTime > 0 {
		// Preserve the session's original start_time — it is part of the
		// ORDER BY key, so a version that moved it would not collapse with
		// its predecessors.
		startTime = existing.StartTime
	}
	duration := agg.EndMS - agg.StartMS
	if !agg.Initialized || duration < 0 {
		duration = 0
	}
	if existing.exists() && existing.DurationMS > duration {
		duration = existing.DurationMS
	}
	pages := int64(1) + agg.Navigations
	if existing.exists() {
		pages = existing.PageCount + agg.Navigations
	}
	hasError := input.HasError || (existing.exists() && existing.hasError())

	version := time.Now().UTC().UnixMilli()
	if existing.exists() && existing.Version >= version {
		version = existing.Version + 1
	}

	hasErrStr := "false"
	if hasError {
		hasErrStr = "true"
	}

	_, err = s.db.SQL().Exec(ctx,
		`INSERT INTO replay_sessions (replay_id, tenant_id, site_id, session_id, start_time,
			duration_ms, page_count, url, browser, os, device, has_error, distinct_id, version)
		 VALUES ($1, 'default', $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		replayID, input.SiteID, input.SessionID, dbutil.IntParam(startTime),
		strconv.FormatInt(duration, 10), strconv.FormatInt(pages, 10),
		input.URL, input.Browser, input.OS, input.Device, hasErrStr, distinctID,
		dbutil.IntParam(version),
	)
	if err != nil {
		return fmt.Errorf("upsert replay session: %w", err)
	}
	return nil
}

// Ingest stores a batch of replay events.
func (s *ReplayService) Ingest(ctx context.Context, input IngestInput) (string, error) {
	if len(input.Events) == 0 {
		return "", nil
	}

	replayID := input.ReplayID
	if replayID == "" {
		replayID = genID()
	}

	// Audit F08: the replay's owning site (from its session row) is
	// authoritative. A key valid for site A must not append child events to
	// a replay recorded under site B just by naming its (client-generated)
	// replay ID.
	if input.ReplayID != "" {
		owner, err := s.replayOwner(ctx, replayID)
		if err != nil {
			return "", err
		}
		if owner != "" && owner != input.SiteID {
			return "", fmt.Errorf("%w: replay %s is owned by another site", ErrCrossSiteReplay, replayID)
		}
	}

	agg := aggregateBatch(&input)

	sql := s.db.SQL()

	// Resolve and hash the user-supplied distinct_id (if any).
	distinctID := ""
	if input.DistinctID != "" {
		salt := s.salt
		rawOptIn := false
		if s.privacy != nil {
			if siteSalt, raw, ok := s.privacy(ctx, input.SiteID); ok {
				salt = siteSalt
				rawOptIn = raw
			}
		}
		if salt == "" && !rawOptIn {
			// Fail closed (OBS-030): this is a privacy control, not a
			// best-effort one — never fall through to raw storage just
			// because no salt was available. main.go always seeds a random
			// fallback salt at startup so this path isn't reachable through
			// normal wiring today, but the package must not depend on the
			// caller continuing to do that correctly.
			//
			// The identifier is dropped, not the whole batch: the session
			// and its events are still real, valuable data, and rejecting
			// the entire ingest over a hashing-config gap would lose them
			// too. Logged loudly (not silently swallowed) so a persistent
			// salt-configuration gap is actually visible operationally.
			s.logger.Warn("replays: dropping distinct_id — no salt available and site has not opted into raw storage",
				"site", input.SiteID)
		} else {
			distinctID = hashDistinctID(input.DistinctID, salt, rawOptIn)
		}
	}

	if err := s.upsertSession(ctx, &input, replayID, agg, distinctID); err != nil {
		return "", err
	}

	// Track the most recent viewport width seen in this batch so click
	// events can carry a vw_bucket without requiring the tracker to
	// re-emit window size on every click. ViewportWidth defaults to the
	// session's `viewport_width` field if the SDK supplied it, else 0.
	currentVW := input.ViewportWidth
	clickEvents := make([]heatmaps.RawEvent, 0)

	for _, ev := range input.Events {
		eventID := genID()
		dataJSON := "null"
		if ev.Data != nil {
			if raw, err := json.Marshal(ev.Data); err == nil {
				dataJSON = string(raw)
			}
		}
		// Audit F08: child events carry the authenticated site so two sites
		// reusing the same client-generated replay ID stay disjoint.
		_, err := sql.Exec(ctx,
			`INSERT INTO replay_events (event_id, tenant_id, site_id, replay_id, timestamp, event_type, data)
			 VALUES ($1, 'default', $2, $3, $4, $5, $6)`,
			eventID, input.SiteID, replayID, ev.Timestamp, ev.Type, dataJSON,
		)
		if err != nil {
			return replayID, fmt.Errorf("insert replay event: %w", err)
		}

		switch ev.Type {
		case "resize":
			if w, ok := readIntField(ev.Data, "w"); ok {
				currentVW = w
			}
		case "click":
			clickEvents = append(clickEvents, heatmaps.RawEvent{
				Type:          ev.Type,
				Data:          ev.Data,
				ViewportWidth: currentVW,
			})
		}
	}

	// Write the per-bucket heatmap rollups. Best-effort: a heatmap write
	// failure must not fail the underlying replay ingest because the raw
	// event rows are already durable. Pattern matches tracing rollups
	// (see internal/tracing/ingest.go).
	if len(clickEvents) > 0 && input.URL != "" {
		clicks := heatmaps.ExtractClicks(clickEvents)
		if len(clicks) > 0 {
			if err := s.heatmaps.Aggregate(ctx, input.SiteID, input.URL, clicks); err != nil {
				s.logger.Warn("heatmaps: aggregate failed",
					"site", input.SiteID, "url", input.URL, "err", err)
			}
		}
	}

	return replayID, nil
}

// readIntField is the same defensive numeric extractor used by the
// heatmaps package, kept here so the resize-tracking shortcut doesn't
// need to import a parser. Returns false on missing or non-numeric
// values.
func readIntField(data any, key string) (int, bool) {
	m, ok := data.(map[string]any)
	if !ok {
		return 0, false
	}
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	}
	return 0, false
}

// maxListReplaysLimit bounds the listing page size (audit F28: limit had a
// default but no cap, so an extreme value produced an extreme query).
const maxListReplaysLimit = 200

// ListReplays returns recent replay sessions for a site, read through the
// version collapse so multi-batch upserts surface as one row per replay.
func (s *ReplayService) ListReplays(ctx context.Context, siteID string, from, to time.Time, limit, offset int) ([]ReplaySession, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > maxListReplaysLimit {
		limit = maxListReplaysLimit
	}
	if offset < 0 {
		offset = 0
	}
	fromMs := dbutil.IntParam(from.UnixMilli())
	toMs := dbutil.IntParam(to.UnixMilli())

	return nucleus.Query[ReplaySession](ctx, s.db.SQL(),
		fmt.Sprintf(`SELECT replay_id, tenant_id, site_id, session_id,
			CAST(start_time AS TEXT) AS start_time,
			duration_ms, page_count, url, browser, os, device, has_error
		 FROM `+replaySessionsLatest("site_id = $1 AND start_time >= $2 AND start_time < $3")+`
		 ORDER BY start_time DESC
		 LIMIT %d OFFSET %d`, limit, offset),
		siteID, fromMs, toMs,
	)
}

// GetReplayEvents returns the events of one replay, scoped to the site that
// owns it (audit F08). Legacy rows written before the site column existed
// carry site_id=” and still belong to the owning session's site.
func (s *ReplayService) GetReplayEvents(ctx context.Context, replayID string) ([]ReplayEvent, error) {
	if replayID == "" {
		return nil, fmt.Errorf("replays: replay_id is required")
	}
	owner, err := s.replayOwner(ctx, replayID)
	if err != nil {
		return nil, err
	}
	return nucleus.Query[ReplayEvent](ctx, s.db.SQL(),
		`SELECT event_id, tenant_id, replay_id,
			CAST(timestamp AS TEXT) AS timestamp,
			event_type,
			COALESCE(data, '') AS data
		 FROM replay_events
		 WHERE replay_id = $1 AND (site_id = $2 OR site_id = '')
		 ORDER BY timestamp ASC`,
		replayID, owner,
	)
}

func genID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
