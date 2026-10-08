package errors

import (
	"context"
	stderrors "errors"
	"testing"

	"github.com/neutron-build/neutron/go/neutron"
)

func TestOBS79ErrorPolicyFailurePrecedesIssueAndEventWrites(t *testing.T) {
	for _, found := range []bool{false, true} {
		svc := NewService(nil, nil, nil, nil).WithPrivacyChecked(func(context.Context, string) (string, bool, bool, error) {
			return "stale-salt", true, found, stderrors.New("policy store failed")
		}, "fallback-salt")
		// Nil dependencies catch any issue, spike or event side effects before refusal.
		_, _, err := svc.insertErrorEvent(context.Background(), nil, ErrorInput{SiteID: "site", DistinctID: "private-person"})
		var appErr *neutron.AppError
		if !stderrors.As(err, &appErr) || appErr.Status != 503 {
			t.Fatalf("policy refusal = %v", err)
		}
	}
}
