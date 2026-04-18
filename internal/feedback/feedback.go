package feedback

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
)

type FeedbackService struct {
	db *nucleus.Client
}

func NewFeedbackService(db *nucleus.Client) *FeedbackService {
	return &FeedbackService{db: db}
}

type FeedbackEntry struct {
	FeedbackID string `json:"feedback_id" db:"feedback_id"`
	TenantID   string `json:"-" db:"tenant_id"`
	SiteID     string `json:"site_id" db:"site_id"`
	SessionID  string `json:"session_id" db:"session_id"`
	URL        string `json:"url" db:"url"`
	Message    string `json:"message" db:"message"`
	Email      string `json:"email" db:"email"`
	Category   string `json:"category" db:"category"`
	Timestamp  int64  `json:"timestamp" db:"timestamp"`
}

type FeedbackInput struct {
	SiteID    string `json:"site_id"`
	SessionID string `json:"session_id"`
	URL       string `json:"url"`
	Message   string `json:"message"`
	Email     string `json:"email"`
	Category  string `json:"category"`
}

func (s *FeedbackService) Submit(ctx context.Context, input FeedbackInput) (string, error) {
	id := genID()
	now := time.Now().UTC().UnixMilli()
	if input.Category == "" {
		input.Category = "general"
	}
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO feedback (feedback_id, tenant_id, site_id, session_id, url, message, email, category, timestamp)
		 VALUES ($1, 'default', $2, $3, $4, $5, $6, $7, $8)`,
		id, input.SiteID, input.SessionID, input.URL, input.Message, input.Email, input.Category, now,
	)
	if err != nil {
		return "", fmt.Errorf("submit feedback: %w", err)
	}
	return id, nil
}

func (s *FeedbackService) List(ctx context.Context, siteID string, from, to time.Time, limit int) ([]FeedbackEntry, error) {
	if limit <= 0 {
		limit = 20
	}
	fromMs := dbutil.IntParam(from.UnixMilli())
	toMs := dbutil.IntParam(to.UnixMilli())
	return nucleus.Query[FeedbackEntry](ctx, s.db.SQL(),
		fmt.Sprintf(`SELECT feedback_id, tenant_id, site_id, session_id, url, message, email, category, timestamp
		 FROM feedback WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3
		 ORDER BY timestamp DESC LIMIT %d`, limit),
		siteID, fromMs, toMs,
	)
}

func genID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
