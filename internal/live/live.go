package live

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
)

// LiveService provides real-time event streaming via SSE.
type LiveService struct {
	db     *nucleus.Client
	logger *slog.Logger
}

// NewLiveService creates a new LiveService.
func NewLiveService(db *nucleus.Client, logger *slog.Logger) *LiveService {
	return &LiveService{db: db, logger: logger}
}

// liveEvent is a single event pushed over SSE.
type liveEvent struct {
	EventID   string `json:"event_id" db:"event_id"`
	EventType string `json:"event_type" db:"event_type"`
	Pathname  string `json:"pathname" db:"pathname"`
	Browser   string `json:"browser" db:"browser"`
	OS        string `json:"os" db:"os"`
	Country   string `json:"country" db:"country"`
	Timestamp int64  `json:"timestamp" db:"timestamp"`
}

// Handler returns an http.HandlerFunc that streams live events via SSE.
// Register as: r.HandleFunc("GET /api/v1/stats/live", liveSvc.Handler())
func (s *LiveService) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		siteID := r.URL.Query().Get("site_id")
		if siteID == "" {
			http.Error(w, "site_id required", http.StatusBadRequest)
			return
		}

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		ctx := r.Context()
		lastSeen := time.Now().UTC().Add(-30 * time.Second)
		pollTicker := time.NewTicker(2 * time.Second)
		heartbeat := time.NewTicker(15 * time.Second)
		defer pollTicker.Stop()
		defer heartbeat.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-heartbeat.C:
				if _, err := fmt.Fprint(w, ":ping\n\n"); err != nil {
					return
				}
				flusher.Flush()
			case <-pollTicker.C:
				events, err := s.pollEvents(ctx, siteID, lastSeen)
				if err != nil {
					s.logger.Error("live poll failed", "err", err, "site", siteID)
					continue
				}
				for _, ev := range events {
					data, err := json.Marshal(ev)
					if err != nil {
						continue
					}
					if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
						return
					}
					evTS := time.UnixMilli(ev.Timestamp)
					if evTS.After(lastSeen) {
						lastSeen = evTS
					}
				}
				if len(events) > 0 {
					flusher.Flush()
				}
			}
		}
	}
}

func (s *LiveService) pollEvents(ctx context.Context, siteID string, after time.Time) ([]liveEvent, error) {
	afterMs := dbutil.IntParam(after.UnixMilli())
	rows, err := nucleus.Query[liveEvent](ctx, s.db.SQL(),
		fmt.Sprintf(`SELECT event_id, event_type, pathname, browser, os, country, timestamp
		 FROM events_recent
		 WHERE site_id = $1 AND timestamp > $2
		 ORDER BY timestamp DESC
		 LIMIT %d`, 20),
		siteID, afterMs,
	)
	if err != nil {
		return nil, fmt.Errorf("live poll: %w", err)
	}
	return rows, nil
}
