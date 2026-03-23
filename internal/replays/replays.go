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

type ReplaySession struct {
	ReplayID  string `json:"replay_id" db:"replay_id"`
	TenantID  string `json:"-" db:"tenant_id"`
	SiteID    string `json:"site_id" db:"site_id"`
	SessionID string `json:"session_id" db:"session_id"`
	StartTime int64  `json:"start_time" db:"start_time"`
	Duration  string `json:"duration_ms" db:"duration_ms"`
	PageCount string `json:"page_count" db:"page_count"`
	URL       string `json:"url" db:"url"`
	Browser   string `json:"browser" db:"browser"`
	OS        string `json:"os" db:"os"`
	Device    string `json:"device" db:"device"`
	HasError  string `json:"has_error" db:"has_error"`
}

type ReplayEvent struct {
	EventID   string `json:"event_id" db:"event_id"`
	TenantID  string `json:"-" db:"tenant_id"`
	ReplayID  string `json:"replay_id" db:"replay_id"`
	Timestamp int64  `json:"timestamp" db:"timestamp"`
	EventType string `json:"event_type" db:"event_type"`
	Data      string `json:"data" db:"data"`
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
	now := time.Now().UTC()
	startTime := input.Events[0].Timestamp
	if startTime == 0 {
		startTime = now.UnixMilli()
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

	// Insert replay session
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

	// Insert replay events
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
func (s *ReplayService) ListReplays(ctx context.Context, siteID string, from, to time.Time, limit int) ([]ReplaySession, error) {
	if limit <= 0 {
		limit = 20
	}
	fromMs := dbutil.IntParam(from.UnixMilli())
	toMs := dbutil.IntParam(to.UnixMilli())

	return nucleus.Query[ReplaySession](ctx, s.db.SQL(),
		fmt.Sprintf(`SELECT replay_id, tenant_id, site_id, session_id, start_time,
			duration_ms, page_count, url, browser, os, device, has_error
		 FROM replay_sessions
		 WHERE site_id = $1 AND start_time >= $2 AND start_time < $3
		 ORDER BY start_time DESC
		 LIMIT %d`, limit),
		siteID, fromMs, toMs,
	)
}

// GetReplayEvents returns all events for a replay session.
func (s *ReplayService) GetReplayEvents(ctx context.Context, replayID string) ([]ReplayEvent, error) {
	return nucleus.Query[ReplayEvent](ctx, s.db.SQL(),
		`SELECT event_id, tenant_id, replay_id, timestamp, event_type,
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
