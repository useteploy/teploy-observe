package surveys

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// PublicSurvey is the shape /surveys/active returns to the browser widget:
// what is needed to render, plus the evaluated `once` flag. The stored
// targeting is NOT echoed (it is server-side policy, and sample_percent would
// let a reader infer the bucketing). Questions/Appearance stay as the stored
// JSON strings, the same shape the endpoint has always returned.
type PublicSurvey struct {
	SurveyID   string `json:"survey_id"`
	SiteID     string `json:"site_id"`
	Name       string `json:"name"`
	Questions  string `json:"questions"`
	Appearance string `json:"appearance"`
	Once       bool   `json:"once"`
}

// maxActiveSurveys bounds the response; the widget shows one at a time.
const maxActiveSurveys = 10

// FilterActive applies each survey's stored targeting to one page view and
// returns the survivors newest-first, capped. Pure (no I/O) so the targeting
// matrix is table-testable.
func FilterActive(all []Survey, c ClientContext) []PublicSurvey {
	sort.SliceStable(all, func(i, j int) bool { return all[i].CreatedAt > all[j].CreatedAt })
	out := make([]PublicSurvey, 0, len(all))
	for _, sv := range all {
		t := lenientTargeting(sv.Targeting)
		if !t.Matches(sv.SurveyID, c) {
			continue
		}
		out = append(out, PublicSurvey{
			SurveyID: sv.SurveyID, SiteID: sv.SiteID, Name: sv.Name,
			Questions: sv.Questions, Appearance: sv.Appearance, Once: t.Once,
		})
		if len(out) == maxActiveSurveys {
			break
		}
	}
	return out
}

// ActiveFor returns the active surveys of a site that target this page view.
// ip/userAgent derive the anonymous visitor used for sample_percent bucketing
// (the same visitor-estimate the exposure path records); no client id is
// minted or stored.
func (s *SurveyService) ActiveFor(ctx context.Context, siteID string, c ClientContext, ip string) ([]PublicSurvey, error) {
	all, err := s.GetActive(ctx, siteID)
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return []PublicSurvey{}, nil
	}
	_, c.Visitor = s.entity(ctx, siteID, "", ip, c.UserAgent)
	return FilterActive(all, c), nil
}

var answerKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

const (
	maxAnswers        = 50
	maxAnswerRunes    = 2000
	maxAnswerChoices  = 20
	maxAnswerChoiceLn = 500
)

// ValidateAnswers bounds a public response before it is stored: key shape,
// count, value types and lengths. Numbers/bools/null cover rating and nps;
// strings cover text and choice; a short list of strings covers multi-choice.
func ValidateAnswers(a map[string]any) error {
	if len(a) > maxAnswers {
		return fmt.Errorf("too many answers (max %d)", maxAnswers)
	}
	for k, v := range a {
		if !answerKeyPattern.MatchString(k) {
			return fmt.Errorf("invalid answer key (must match [A-Za-z0-9_-]{1,64})")
		}
		switch x := v.(type) {
		case nil, bool, float64:
		case string:
			if n := len([]rune(x)); n > maxAnswerRunes {
				return fmt.Errorf("answer %q too long (max %d characters)", k, maxAnswerRunes)
			}
		case []any:
			if len(x) > maxAnswerChoices {
				return fmt.Errorf("answer %q has too many choices (max %d)", k, maxAnswerChoices)
			}
			for _, e := range x {
				str, ok := e.(string)
				if !ok || len([]rune(str)) > maxAnswerChoiceLn || strings.ContainsRune(str, 0) {
					return fmt.Errorf("answer %q: list entries must be strings up to %d characters", k, maxAnswerChoiceLn)
				}
			}
		default:
			return fmt.Errorf("answer %q has an unsupported value type", k)
		}
	}
	return nil
}
