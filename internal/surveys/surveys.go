package surveys

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"
)

type SurveyService struct {
	db *nucleus.Client
}

func NewSurveyService(db *nucleus.Client) *SurveyService {
	return &SurveyService{db: db}
}

type Survey struct {
	SurveyID   string `json:"survey_id" db:"survey_id"`
	TenantID   string `json:"-" db:"tenant_id"`
	SiteID     string `json:"site_id" db:"site_id"`
	Name       string `json:"name" db:"name"`
	Questions  string `json:"questions" db:"questions"`   // JSONB
	Appearance string `json:"appearance" db:"appearance"` // JSONB
	Targeting  string `json:"targeting" db:"targeting"`   // JSONB
	Status     string `json:"status" db:"status"`         // draft, active, closed
	CreatedAt  string `json:"created_at" db:"created_at"`
	Version    string `json:"-" db:"version"`
}

// Question defines a survey question.
type Question struct {
	ID          string   `json:"id"`
	Type        string   `json:"type"` // text, rating, nps, choice
	Text        string   `json:"text"`
	Required    bool     `json:"required"`
	Choices     []string `json:"choices,omitempty"`
	Placeholder string   `json:"placeholder,omitempty"`
}

type SurveyResponse struct {
	ResponseID string `json:"response_id" db:"response_id"`
	TenantID   string `json:"-" db:"tenant_id"`
	SurveyID   string `json:"survey_id" db:"survey_id"`
	SiteID     string `json:"site_id" db:"site_id"`
	UserID     string `json:"user_id" db:"user_id"`
	Answers    string `json:"answers" db:"answers"` // JSONB
	Timestamp  int64  `json:"timestamp" db:"timestamp"`
}

func (s *SurveyService) Create(ctx context.Context, siteID, name, questions, appearance, targeting string) (*Survey, error) {
	id := genID()
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO surveys (survey_id, tenant_id, site_id, name, questions, appearance, targeting, status, created_at, version)
		 VALUES ($1, 'default', $2, $3, $4, $5, $6, 'draft', $7, $8)`,
		id, siteID, name, questions, appearance, targeting, now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("create survey: %w", err)
	}
	return &Survey{SurveyID: id, SiteID: siteID, Name: name, Questions: questions, Status: "draft", CreatedAt: now}, nil
}

func (s *SurveyService) List(ctx context.Context, siteID string) ([]Survey, error) {
	return nucleus.Query[Survey](ctx, s.db.SQL(),
		`SELECT survey_id, tenant_id, site_id, name, COALESCE(questions, '') AS questions,
			COALESCE(appearance, '') AS appearance, COALESCE(targeting, '') AS targeting,
			status, created_at, version
		 FROM surveys WHERE site_id = $1 ORDER BY created_at DESC`, siteID)
}

func (s *SurveyService) Activate(ctx context.Context, surveyID string) error {
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO surveys (survey_id, tenant_id, site_id, name, questions, appearance, targeting, status, created_at, version)
		 SELECT survey_id, tenant_id, site_id, name, questions, appearance, targeting, 'active', created_at, $2
		 FROM surveys WHERE survey_id = $1`,
		surveyID, now)
	return err
}

func (s *SurveyService) Close(ctx context.Context, surveyID string) error {
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO surveys (survey_id, tenant_id, site_id, name, questions, appearance, targeting, status, created_at, version)
		 SELECT survey_id, tenant_id, site_id, name, questions, appearance, targeting, 'closed', created_at, $2
		 FROM surveys WHERE survey_id = $1`,
		surveyID, now)
	return err
}

// GetActive returns active surveys for a site (used by the JS SDK to show surveys).
func (s *SurveyService) GetActive(ctx context.Context, siteID string) ([]Survey, error) {
	return nucleus.Query[Survey](ctx, s.db.SQL(),
		`SELECT survey_id, tenant_id, site_id, name, COALESCE(questions, '') AS questions,
			COALESCE(appearance, '') AS appearance, COALESCE(targeting, '') AS targeting,
			status, created_at, version
		 FROM surveys WHERE site_id = $1 AND status = 'active'`, siteID)
}

// SubmitResponse records a survey response.
func (s *SurveyService) SubmitResponse(ctx context.Context, surveyID, siteID, userID string, answers map[string]any) (string, error) {
	// Public endpoint (untrusted browsers): the survey must exist, be active,
	// and belong to the claimed site — otherwise anyone could write responses to
	// any site's survey or to an arbitrary survey_id.
	type svRow struct {
		SiteID string `db:"site_id"`
		Status string `db:"status"`
	}
	sv, err := nucleus.Query[svRow](ctx, s.db.SQL(),
		"SELECT site_id, status FROM surveys WHERE survey_id = $1", surveyID)
	if err != nil || len(sv) == 0 {
		return "", fmt.Errorf("survey not found")
	}
	if sv[0].SiteID != siteID {
		return "", fmt.Errorf("survey does not belong to this site")
	}
	if sv[0].Status != "active" {
		return "", fmt.Errorf("survey is not active")
	}

	id := genID()
	answersJSON := "null"
	if answers != nil {
		raw, _ := json.Marshal(answers)
		answersJSON = string(raw)
	}
	if len(answersJSON) > 16384 {
		return "", fmt.Errorf("answers payload too large")
	}
	_, err = s.db.SQL().Exec(ctx,
		`INSERT INTO survey_responses (response_id, tenant_id, survey_id, site_id, user_id, answers, timestamp)
		 VALUES ($1, 'default', $2, $3, $4, $5, $6)`,
		id, surveyID, siteID, userID, answersJSON, time.Now().UTC().UnixMilli(),
	)
	if err != nil {
		return "", fmt.Errorf("submit response: %w", err)
	}
	return id, nil
}

// ListResponses returns responses for a survey.
func (s *SurveyService) ListResponses(ctx context.Context, surveyID, siteID string, limit int) ([]SurveyResponse, error) {
	if limit <= 0 {
		limit = 50
	}
	return nucleus.Query[SurveyResponse](ctx, s.db.SQL(),
		fmt.Sprintf(`SELECT response_id, tenant_id, survey_id, site_id, user_id, COALESCE(answers, '') AS answers, timestamp
		 FROM survey_responses WHERE survey_id = $1 AND site_id = $2
		 ORDER BY timestamp DESC LIMIT %d`, limit),
		surveyID, siteID,
	)
}

func genID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
