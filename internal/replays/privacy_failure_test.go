package replays

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/neutron-build/neutron/go/neutron"
)

func TestReplayPrivacyFailureRefusesBeforeStorage(t *testing.T) {
	for _, tc := range []struct {
		name  string
		found bool
		err   error
	}{
		{"unavailable", false, errors.New("policy store unavailable")},
		{"unknown site", false, nil},
		{"failure with stale policy", true, errors.New("policy read failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A nil database makes any attempted session/event write fail the test.
			calls := 0
			svc := NewReplayService(nil).WithPrivacyChecked(func(ctx context.Context, site string) (string, bool, bool, error) {
				calls++
				if site != "privacy-site" {
					t.Fatalf("policy site = %q", site)
				}
				return "stale-salt", true, tc.found, tc.err
			}, "fallback-salt")
			var input IngestInput
			if err := json.Unmarshal([]byte(`{"site_id":"privacy-site","session_id":"session","distinct_id":"private-person","events":[{"type":"navigation","timestamp":1,"data":{}}]}`), &input); err != nil {
				t.Fatal(err)
			}
			_, err := svc.Ingest(context.Background(), input)
			var appErr *neutron.AppError
			if !errors.As(err, &appErr) || appErr.Status != 503 || calls != 1 {
				t.Fatalf("result = %v, calls = %d; want policy refusal", err, calls)
			}
		})
	}
}
