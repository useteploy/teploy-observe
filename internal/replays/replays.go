package replays

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
	"github.com/useteploy/teploy-observe/internal/heatmaps"
	"github.com/useteploy/teploy-observe/internal/identity"
)

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

// Ingest stores a batch of replay events.
func (s *ReplayService) Ingest(ctx context.Context, input IngestInput) (string, error) {
	if len(input.Events) == 0 {
		return "", nil
	}

	replayID := input.ReplayID
	if replayID == "" {
		replayID = genID()
	}
	startTime := input.Events[0].Timestamp
	if startTime == 0 {
		startTime = time.Now().UTC().UnixMilli()
	}
	endTime := input.Events[len(input.Events)-1].Timestamp
	duration := endTime - startTime
	if duration < 0 {
		duration = 0
	}

	hasError := "false"
	if input.HasError {
		hasError = "true"
	}

	sql := s.db.SQL()

	// When the SDK supplies a stable client-side replay_id, multiple batches
	// share the same id. Insert the session row only on the first batch we
	// see for that id (KV-backed dedupe).
	insertSession := true
	if input.ReplayID != "" {
		kv := s.db.KV()
		key := "replay_seen:" + input.SiteID + ":" + replayID
		// Atomic claim: only the batch that wins SetNX inserts the session row,
		// closing the check-then-set race that produced duplicate sessions. The
		// dedupe key self-expires (keys are 1-byte; TTL bounds growth).
		claimed, err := kv.SetNX(ctx, key, []byte("1"))
		if err == nil {
			insertSession = claimed
			if claimed {
				_, _ = kv.Expire(ctx, key, 6*time.Hour)
			}
		}
	}

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
			distinctID = input.DistinctID
		} else {
			distinctID = hashDistinctID(input.DistinctID, salt, rawOptIn)
		}
	}

	if insertSession {
		_, err := sql.Exec(ctx,
			`INSERT INTO replay_sessions (replay_id, tenant_id, site_id, session_id, start_time,
				duration_ms, page_count, url, browser, os, device, has_error, distinct_id)
			 VALUES ($1, 'default', $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
			replayID, input.SiteID, input.SessionID, startTime,
			strconv.FormatInt(duration, 10), strconv.Itoa(len(input.Events)),
			input.URL, input.Browser, input.OS, input.Device, hasError, distinctID,
		)
		if err != nil {
			return "", fmt.Errorf("insert replay session: %w", err)
		}
	}

	// Track the most recent viewport width seen in this batch so click
	// events can carry a vw_bucket without requiring the tracker to
	// re-emit window size on every click. ViewportWidth defaults to the
	// session's `viewport_width` field if the SDK supplied it, else 0.
	currentVW := input.ViewportWidth
	clickEvents := make([]heatmaps.RawEvent, 0)

	for _, ev := range input.Events {
		eventID := genID()
		dataJSON := ""
		if ev.Data != nil {
			if raw, err := json.Marshal(ev.Data); err == nil {
				dataJSON = string(raw)
			}
		}
		_, err := sql.Exec(ctx,
			`INSERT INTO replay_events (event_id, tenant_id, replay_id, timestamp, event_type, data)
			 VALUES ($1, 'default', $2, $3, $4, $5)`,
			eventID, replayID, ev.Timestamp, ev.Type, dataJSON,
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

// ListReplays returns recent replay sessions for a site.
func (s *ReplayService) ListReplays(ctx context.Context, siteID string, from, to time.Time, limit, offset int) ([]ReplaySession, error) {
	if limit <= 0 {
		limit = 20
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
		 FROM replay_sessions
		 WHERE site_id = $1 AND start_time >= $2 AND start_time < $3
		 ORDER BY start_time DESC
		 LIMIT %d OFFSET %d`, limit, offset),
		siteID, fromMs, toMs,
	)
}

// GetReplayEvents returns all events for a replay session.
func (s *ReplayService) GetReplayEvents(ctx context.Context, replayID string) ([]ReplayEvent, error) {
	return nucleus.Query[ReplayEvent](ctx, s.db.SQL(),
		`SELECT event_id, tenant_id, replay_id,
			CAST(timestamp AS TEXT) AS timestamp,
			event_type,
			COALESCE(data, '') AS data
		 FROM replay_events
		 WHERE replay_id = $1
		 ORDER BY timestamp ASC`,
		replayID,
	)
}

func genID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
