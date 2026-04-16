package replays

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/teploy/observe/internal/dbutil"
)

type ReplayService struct {
	db *nucleus.Client
}

func NewReplayService(db *nucleus.Client) *ReplayService {
	return &ReplayService{db: db}
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

type replaySessionRow struct {
	ReplayID  string `db:"replay_id"`
	TenantID  string `db:"tenant_id"`
	SiteID    string `db:"site_id"`
	SessionID string `db:"session_id"`
	StartTime string `db:"start_time"`
	Duration  string `db:"duration_ms"`
	PageCount string `db:"page_count"`
	URL       string `db:"url"`
	Browser   string `db:"browser"`
	OS        string `db:"os"`
	Device    string `db:"device"`
	HasError  string `db:"has_error"`
}

func parseEpochMillis(s string) time.Time {
	if s == "" || s == "0" {
		return time.Time{}
	}
	if ms, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.UnixMilli(ms).UTC()
	}
	return time.Time{}
}

func parseBool(s string) bool {
	switch s {
	case "true", "1", "TRUE":
		return true
	}
	return false
}

func (r replaySessionRow) toDomain() ReplaySession {
	dur, _ := strconv.ParseInt(r.Duration, 10, 64)
	pc, _ := strconv.Atoi(r.PageCount)
	return ReplaySession{
		ReplayID: r.ReplayID, SiteID: r.SiteID, SessionID: r.SessionID,
		StartTime: parseEpochMillis(r.StartTime), Duration: dur, PageCount: pc,
		URL: r.URL, Browser: r.Browser, OS: r.OS, Device: r.Device,
		HasError: parseBool(r.HasError),
	}
}

// ReplayEvent is the domain type for replay events.
type ReplayEvent struct {
	EventID   string    `json:"event_id"`
	ReplayID  string    `json:"replay_id"`
	Timestamp time.Time `json:"timestamp"`
	EventType string    `json:"event_type"`
	Data      string    `json:"data"`
}

type replayEventRow struct {
	EventID   string `db:"event_id"`
	TenantID  string `db:"tenant_id"`
	ReplayID  string `db:"replay_id"`
	Timestamp string `db:"timestamp"`
	EventType string `db:"event_type"`
	Data      string `db:"data"`
}

func (r replayEventRow) toDomain() ReplayEvent {
	return ReplayEvent{
		EventID: r.EventID, ReplayID: r.ReplayID,
		Timestamp: parseEpochMillis(r.Timestamp),
		EventType: r.EventType, Data: r.Data,
	}
}

// IngestInput is the JSON body from the replay SDK.
type IngestInput struct {
	SiteID    string `json:"site_id"`
	SessionID string `json:"session_id"`
	URL       string `json:"url"`
	Browser   string `json:"browser"`
	OS        string `json:"os"`
	Device    string `json:"device"`
	HasError  bool   `json:"has_error"`
	Events    []struct {
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

	replayID := genID()
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

	_, err := sql.Exec(ctx,
		`INSERT INTO replay_sessions (replay_id, tenant_id, site_id, session_id, start_time,
			duration_ms, page_count, url, browser, os, device, has_error)
		 VALUES ($1, 'default', $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		replayID, input.SiteID, input.SessionID, startTime,
		strconv.FormatInt(duration, 10), strconv.Itoa(len(input.Events)),
		input.URL, input.Browser, input.OS, input.Device, hasError,
	)
	if err != nil {
		return "", fmt.Errorf("insert replay session: %w", err)
	}

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
	}

	return replayID, nil
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

	rows, err := nucleus.Query[replaySessionRow](ctx, s.db.SQL(),
		fmt.Sprintf(`SELECT replay_id, tenant_id, site_id, session_id,
			CAST(start_time AS TEXT) AS start_time,
			duration_ms, page_count, url, browser, os, device, has_error
		 FROM replay_sessions
		 WHERE site_id = $1 AND start_time >= $2 AND start_time < $3
		 ORDER BY start_time DESC
		 LIMIT %d OFFSET %d`, limit, offset),
		siteID, fromMs, toMs,
	)
	if err != nil {
		return nil, err
	}
	out := make([]ReplaySession, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.toDomain())
	}
	return out, nil
}

// GetReplayEvents returns all events for a replay session.
func (s *ReplayService) GetReplayEvents(ctx context.Context, replayID string) ([]ReplayEvent, error) {
	rows, err := nucleus.Query[replayEventRow](ctx, s.db.SQL(),
		`SELECT event_id, tenant_id, replay_id,
			CAST(timestamp AS TEXT) AS timestamp,
			event_type,
			COALESCE(data, '') AS data
		 FROM replay_events
		 WHERE replay_id = $1
		 ORDER BY timestamp ASC`,
		replayID,
	)
	if err != nil {
		return nil, err
	}
	out := make([]ReplayEvent, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.toDomain())
	}
	return out, nil
}

func genID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
