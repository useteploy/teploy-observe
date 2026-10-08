package experiments

import (
	"context"
	"errors"
	"testing"
)

func TestOBS79ExperimentPolicyFailureNeverChangesIdentitySalt(t *testing.T) {
	for _, tc := range []struct {
		found bool
		err   error
	}{{false, nil}, {false, errors.New("policy unavailable")}, {true, errors.New("stale policy unavailable")}} {
		svc := NewExperimentService(nil).WithPrivacyChecked(func(context.Context, string) (string, bool, bool, error) { return "stale-salt", true, tc.found, tc.err }, "fallback-salt")
		id, err := svc.EventDistinctIDChecked(context.Background(), "site", "private-person")
		if err == nil || id != "" {
			t.Fatalf("policy failure produced identity: %q %v", id, err)
		}
		if id := svc.EventDistinctID(context.Background(), "site", "private-person"); id != "" {
			t.Fatalf("compatibility helper produced identity: %q", id)
		}
	}
}
