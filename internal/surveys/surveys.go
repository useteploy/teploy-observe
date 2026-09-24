package surveys

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/identity"
	"github.com/useteploy/teploy-observe/internal/session"
	"github.com/useteploy/teploy-observe/internal/sites"
)

type SurveyService struct {
	db      *nucleus.Client
	salt    string
	siteSvc *sites.SiteService
}

// NewSurveyService wires the survey store. salt is the global session salt
// (cfg.SessionSalt); siteSvc supplies the per-site privacy config so the
// person entity is derived with the same salt the analytics ingest path
// uses (O03: one identity model, not a survey-specific one). Both are
// optional - nil/empty degrades to the global salt / estimate-only.
func NewSurveyService(db *nucleus.Client, salt string, siteSvc *sites.SiteService) *SurveyService {
	return &SurveyService{db: db, salt: salt, siteSvc: siteSvc}
}

// O03 entity types (IDENTITY_MODEL_ADR vocabulary, the same set the O04
// funnel/retention entity modes pin).
const (
	EntityPerson          = "person"
	EntityVisitorEstimate = "visitor-estimate"
)

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
		 VALUES ($1, 'default', $2, $3, NULLIF($4, ''), NULLIF($5, ''), NULLIF($6, ''), 'draft', $7, $8)`,
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
		 FROM `+surveysLatest("site_id = $1")+`
		 ORDER BY created_at DESC`, siteID)
}

func (s *SurveyService) Activate(ctx context.Context, surveyID string) error {
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	// Strictly-monotonic version (the 70f6eff version-tie defect): a
	// same-millisecond create+activate must not tie, or argMax resolves the
	// superseded status arbitrarily.
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO surveys (survey_id, tenant_id, site_id, name, questions, appearance, targeting, status, created_at, version)
		 SELECT survey_id, tenant_id, site_id, name, NULLIF(CAST(questions AS TEXT), ''), NULLIF(CAST(appearance AS TEXT), ''), NULLIF(CAST(targeting AS TEXT), ''), 'active', created_at,
		        GREATEST(CAST($2 AS BIGINT), version + 1)
		 FROM `+surveysLatest("survey_id = $1"),
		surveyID, now)
	return err
}

func (s *SurveyService) Close(ctx context.Context, surveyID string) error {
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	// Same monotonic stamp as Activate: an activate+close inside one
	// millisecond must resolve closed.
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO surveys (survey_id, tenant_id, site_id, name, questions, appearance, targeting, status, created_at, version)
		 SELECT survey_id, tenant_id, site_id, name, NULLIF(CAST(questions AS TEXT), ''), NULLIF(CAST(appearance AS TEXT), ''), NULLIF(CAST(targeting AS TEXT), ''), 'closed', created_at,
		        GREATEST(CAST($2 AS BIGINT), version + 1)
		 FROM `+surveysLatest("survey_id = $1"),
		surveyID, now)
	return err
}

// GetActive returns active surveys for a site (used by the JS SDK to show surveys).
func (s *SurveyService) GetActive(ctx context.Context, siteID string) ([]Survey, error) {
	return nucleus.Query[Survey](ctx, s.db.SQL(),
		`SELECT survey_id, tenant_id, site_id, name, COALESCE(questions, '') AS questions,
			COALESCE(appearance, '') AS appearance, COALESCE(targeting, '') AS targeting,
			status, created_at, version
		 FROM `+surveysLatest("site_id = $1")+`
		 WHERE status = 'active'`, siteID)
}

// clientIDPattern is the bounded alphabet for producer-minted response
// identity (the same class as the events event_id / errors event_id - the
// F12 ladder).
var clientIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

// entity resolves the O03 identity for one survey interaction: the
// identified person when the SDK sent an identify value (HMAC under the
// per-site salt, or raw per the site opt-out - exactly the analytics
// ingest derivation), else the anonymous visitor estimate derived with
// the same session.ID fingerprint the analytics path uses. Deliberately
// NOT a visit id: an hourly visit would re-count the same reader every
// clock hour; the estimate is the stable unique-reader unit.
func (s *SurveyService) entity(ctx context.Context, siteID, userID, ip, userAgent string) (string, string) {
	if userID != "" {
		salt := s.salt
		rawOptIn := false
		if s.siteSvc != nil {
			if ps, raw, ok := s.siteSvc.PrivacyConfig(ctx, siteID); ok {
				salt = ps
				rawOptIn = raw
			}
		}
		return EntityPerson, identity.MaybeHashDistinctID(userID, salt, rawOptIn)
	}
	return EntityVisitorEstimate, session.ID(siteID, ip, userAgent, s.salt)
}

// gateSurvey is the shared public-endpoint check: the survey must exist
// (through the version collapse - the replacing.go lesson), belong to the
// claimed site, and be ACTIVE. Both exposure and response recording refuse
// anything else, so junk pings cannot pollute either table.
func (s *SurveyService) gateSurvey(ctx context.Context, surveyID, siteID string) error {
	type svRow struct {
		SiteID string `db:"site_id"`
		Status string `db:"status"`
	}
	sv, err := nucleus.Query[svRow](ctx, s.db.SQL(),
		"SELECT site_id, status FROM "+surveysLatest("survey_id = $1"), surveyID)
	if err != nil || len(sv) == 0 {
		return fmt.Errorf("survey not found")
	}
	if sv[0].SiteID != siteID {
		return fmt.Errorf("survey does not belong to this site")
	}
	if sv[0].Status != "active" {
		return fmt.Errorf("survey is not active")
	}
	return nil
}

// RecordExposure records that a survey was OFFERED to one entity. The
// exposure row is an append, not a claim: exposure counts derive DISTINCT
// entity_id (see Stats), so a retried exposure ping adds a row without
// inflating unique-exposed. This is the denominator of the response rate
// the O14 spec asks surveys to carry.
func (s *SurveyService) RecordExposure(ctx context.Context, surveyID, siteID, userID, ip, userAgent string) error {
	if err := s.gateSurvey(ctx, surveyID, siteID); err != nil {
		return err
	}
	entityType, entityID := s.entity(ctx, siteID, userID, ip, userAgent)
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO survey_exposures (exposure_id, tenant_id, survey_id, site_id, entity_type, entity_id, timestamp)
		 VALUES ($1, 'default', $2, $3, $4, $5, $6)`,
		genID(), surveyID, siteID, entityType, entityID, time.Now().UTC().UnixMilli(),
	)
	if err != nil {
		return fmt.Errorf("record exposure: %w", err)
	}
	return nil
}

// SubmitResponse records a survey response. clientID is the OPTIONAL
// producer-minted response identity: when present and well-formed, a
// submit that retries it (lost HTTP response, redelivery) returns the
// original response_id with deduped=true instead of inserting a second
// row. Absent clientID keeps the v1 posture (no dedupe) - the same ladder
// the events and errors paths climb. entity_type/entity_id attribute the
// response the same way exposures are attributed, so the response-rate
// numerator counts the same kind of unit as its denominator.
//
// Residual, documented: the dedupe is a check-then-insert on a synchronous
// path - two identical client_ids submitted in the same instant can both
// insert. The covered case is the retry (seconds apart, the real-world
// duplicate); a same-millisecond concurrent double-submit is the same
// window class the events admission cache documents pre-serialization.
func (s *SurveyService) SubmitResponse(ctx context.Context, surveyID, siteID, userID string, clientID string, answers map[string]any, ip, userAgent string) (SubmitResult, error) {
	if err := s.gateSurvey(ctx, surveyID, siteID); err != nil {
		return SubmitResult{}, err
	}
	if clientID != "" && !clientIDPattern.MatchString(clientID) {
		return SubmitResult{}, fmt.Errorf("invalid response identity (must match [A-Za-z0-9_-]{8,64})")
	}

	// Response-identity dedupe: a retry of an already-recorded client id
	// acks the original row instead of inserting again.
	if clientID != "" {
		existing, err := nucleus.Query[struct {
			ResponseID string `db:"response_id"`
		}](ctx, s.db.SQL(),
			`SELECT response_id FROM survey_responses
			 WHERE site_id = $1 AND client_id = $2 LIMIT 1`,
			siteID, clientID)
		if err != nil {
			return SubmitResult{}, fmt.Errorf("submit response: dedupe lookup: %w", err)
		}
		if len(existing) > 0 {
			return SubmitResult{ResponseID: existing[0].ResponseID, Deduped: true}, nil
		}
	}

	id := genID()
	answersJSON := "null"
	if answers != nil {
		raw, _ := json.Marshal(answers)
		answersJSON = string(raw)
	}
	if len(answersJSON) > 16384 {
		return SubmitResult{}, fmt.Errorf("answers payload too large")
	}
	entityType, entityID := s.entity(ctx, siteID, userID, ip, userAgent)
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO survey_responses (response_id, tenant_id, survey_id, site_id, user_id, answers, timestamp, entity_type, entity_id, client_id)
		 VALUES ($1, 'default', $2, $3, $4, $5, $6, $7, $8, $9)`,
		id, surveyID, siteID, userID, answersJSON, time.Now().UTC().UnixMilli(),
		entityType, entityID, clientID,
	)
	if err != nil {
		return SubmitResult{}, fmt.Errorf("submit response: %w", err)
	}
	return SubmitResult{ResponseID: id}, nil
}

// SubmitResult carries the durable response identity and whether this
// call acked an already-recorded one.
type SubmitResult struct {
	ResponseID string
	Deduped    bool
}

// Stats is the survey's exposure/response math: the counts a survey owner
// reads off one card. Exposed/Responding are DISTINCT entity ids (retries
// and repeat exposures do not inflate them); Responses counts rows
// (multiple submits from distinct client ids are real rows). The entity
// split is carried per O03 type because a person and an estimate are
// different kinds of unit - a reader exposed anonymously who responds
// identified shows up in different halves, and the totals row is the
// honest mixed number, labeled as such by carrying the split.
type Stats struct {
	SurveyID string `json:"survey_id"`

	ExposedEntities     int64 `json:"exposed_entities"`
	ExposedPersons      int64 `json:"exposed_persons"`
	ExposedEstimates    int64 `json:"exposed_estimates"`
	RespondingEntities  int64 `json:"responding_entities"`
	RespondingPersons   int64 `json:"responding_persons"`
	RespondingEstimates int64 `json:"responding_estimates"`
	ResponseRows        int64 `json:"response_rows"`
	ExposureRows        int64 `json:"exposure_rows"`
}

func (s *SurveyService) Stats(ctx context.Context, surveyID, siteID string) (*Stats, error) {
	out := &Stats{SurveyID: surveyID}

	exp, err := nucleus.Query[struct {
		Entities  int64 `db:"entities"`
		Persons   int64 `db:"persons"`
		Estimates int64 `db:"estimates"`
		Rows      int64 `db:"rows"`
	}](ctx, s.db.SQL(),
		`SELECT COUNT(DISTINCT entity_id) AS entities,
			COUNT(DISTINCT CASE WHEN entity_type = 'person' THEN entity_id ELSE NULL END) AS persons,
			COUNT(DISTINCT CASE WHEN entity_type = 'visitor-estimate' THEN entity_id ELSE NULL END) AS estimates,
			COUNT(*) AS rows
		 FROM survey_exposures WHERE survey_id = $1 AND site_id = $2`,
		surveyID, siteID)
	if err != nil {
		return nil, fmt.Errorf("survey exposure stats: %w", err)
	}
	if len(exp) > 0 {
		out.ExposedEntities = exp[0].Entities
		out.ExposedPersons = exp[0].Persons
		out.ExposedEstimates = exp[0].Estimates
		out.ExposureRows = exp[0].Rows
	}

	resp, err := nucleus.Query[struct {
		Entities  int64 `db:"entities"`
		Persons   int64 `db:"persons"`
		Estimates int64 `db:"estimates"`
		Rows      int64 `db:"rows"`
	}](ctx, s.db.SQL(),
		`SELECT COUNT(DISTINCT entity_id) AS entities,
			COUNT(DISTINCT CASE WHEN entity_type = 'person' THEN entity_id ELSE NULL END) AS persons,
			COUNT(DISTINCT CASE WHEN entity_type = 'visitor-estimate' THEN entity_id ELSE NULL END) AS estimates,
			COUNT(*) AS rows
		 FROM survey_responses WHERE survey_id = $1 AND site_id = $2 AND entity_id <> ''`,
		surveyID, siteID)
	if err != nil {
		return nil, fmt.Errorf("survey response stats: %w", err)
	}
	if len(resp) > 0 {
		out.RespondingEntities = resp[0].Entities
		out.RespondingPersons = resp[0].Persons
		out.RespondingEstimates = resp[0].Estimates
		out.ResponseRows = resp[0].Rows
	}
	return out, nil
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
