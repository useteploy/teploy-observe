package surveys

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestValidateUserID(t *testing.T) {
	for _, ok := range []string{"", "user-42", strings.Repeat("a", MaxUserIDLen), "café"} {
		if err := ValidateUserID(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for name, bad := range map[string]string{
		"long":    strings.Repeat("a", MaxUserIDLen+1),
		"control": "a\x00b",
		"newline": "a\nb",
		"utf8":    "a\xffb",
	} {
		err := ValidateUserID(bad)
		var pe *PublicError
		if !errors.As(err, &pe) {
			t.Errorf("%s: want PublicError, got %v", name, err)
		}
	}
}

// Validation runs before any store access, so a service with no database
// proves the boundary checks (and that they are public errors).
func TestSubmitResponseValidatesBeforeStore(t *testing.T) {
	svc := &SurveyService{}
	ctx := context.Background()
	cases := map[string]struct {
		user, client string
		answers      map[string]any
	}{
		"user_id":   {strings.Repeat("u", MaxUserIDLen+1), "", nil},
		"client_id": {"", "bad!", nil},
		"answers":   {"", "", map[string]any{"bad key": 1.0}},
	}
	for name, c := range cases {
		_, err := svc.SubmitResponse(ctx, "sv", "site", c.user, c.client, c.answers, "1.2.3.4", "ua")
		var pe *PublicError
		if !errors.As(err, &pe) {
			t.Errorf("%s: want PublicError, got %v", name, err)
		}
	}
}

func TestSubmitLockStable(t *testing.T) {
	svc := &SurveyService{}
	if a, b := svc.submitLock("s", "sv1", "client-abc12345"), svc.submitLock("s", "sv1", "client-abc12345"); a != b {
		t.Fatal("same key must map to the same lock")
	}
}

func TestSiteKnownNilSiteService(t *testing.T) {
	if !(&SurveyService{}).SiteKnown(context.Background(), "x") {
		t.Fatal("nil site service must not reject")
	}
}

// A client id reused against a DIFFERENT survey of the same site is a
// different response, not a dedupe hit; a same-survey retry still dedupes,
// including under concurrency (striped submit lock). Needs a live Nucleus.
func TestResponseDedupeIsPerSurvey(t *testing.T) {
	db := exposureFixture(t)
	ctx := context.Background()
	site := surveySite(t)
	svc := NewSurveyService(db, "test-global-salt", nil)
	mk := func(name string) string {
		s, err := svc.Create(ctx, site, name, `[{"id":"q1","type":"rating","text":"?"}]`, "", "")
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if err := svc.Activate(ctx, s.SurveyID); err != nil {
			t.Fatalf("activate: %v", err)
		}
		return s.SurveyID
	}
	a, b := mk("A"), mk("B")
	const cid = "shared-client-id-1"
	ra, err := svc.SubmitResponse(ctx, a, site, "", cid, map[string]any{"q1": 1}, "1.2.3.4", "ua")
	if err != nil || ra.Deduped {
		t.Fatalf("a: %+v %v", ra, err)
	}
	rb, err := svc.SubmitResponse(ctx, b, site, "", cid, map[string]any{"q1": 2}, "1.2.3.4", "ua")
	if err != nil || rb.Deduped || rb.ResponseID == ra.ResponseID {
		t.Fatalf("b must be a new response: %+v %v", rb, err)
	}
	done := make(chan SubmitResult, 8)
	for i := 0; i < 8; i++ {
		go func() {
			r, _ := svc.SubmitResponse(ctx, a, site, "", "race-client-id-1", map[string]any{"q1": 3}, "1.2.3.4", "ua")
			done <- r
		}()
	}
	ids := map[string]bool{}
	for i := 0; i < 8; i++ {
		ids[(<-done).ResponseID] = true
	}
	if len(ids) != 1 {
		t.Fatalf("concurrent identical client ids produced %d rows", len(ids))
	}
}
