package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/useteploy/teploy-observe/internal/ingest"
)

type failedPersonsPrivacy struct {
	found bool
	err   error
}

func (p failedPersonsPrivacy) PrivacyConfig(context.Context, string) (string, bool, bool) {
	panic("checked policy must take precedence over the legacy fallback")
}

func (p failedPersonsPrivacy) PrivacyConfigChecked(context.Context, string) (string, bool, bool, error) {
	return "stale-salt", true, p.found, p.err
}

func TestPersonsPolicyFailureRefusesBeforePropertyWrite(t *testing.T) {
	for _, p := range []failedPersonsPrivacy{
		{err: errors.New("policy store unavailable")},
		{},
		{found: true, err: errors.New("stale policy read failed")},
	} {
		// A nil service catches any fallthrough into a property write.
		h := personsPropertiesHandler(nil, p, "fallback-salt", nil)
		r := httptest.NewRequest(http.MethodPost, propsPath, strings.NewReader(`{"distinct_id":"private-person","properties":{"plan":"test"}}`))
		r = r.WithContext(ingest.WithSiteID(r.Context(), "privacy-site"))
		w := httptest.NewRecorder()
		h(w, r)
		if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "private-person") {
			t.Fatalf("policy refusal = %d %s", w.Code, w.Body.String())
		}
	}
}
