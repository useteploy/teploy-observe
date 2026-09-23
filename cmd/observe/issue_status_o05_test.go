package main

// O05 slice 1: the snooze parameter on POST /api/v1/issues/{id}/status.
// `until` (RFC3339) is legal only with status=resolved, must parse, and
// must lie in the future — the validation is the handler's, so these
// run storage-free against the handler closure directly.

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/neutron-build/neutron/go/neutron"
)

func TestO05_UpdateStatusUntilValidation(t *testing.T) {
	h := updateIssueStatusHandler(nil) // validation precedes any svc use

	cases := []struct {
		name string
		in   updateStatusInput
		want int
	}{
		{"until requires resolved", updateStatusInput{IssueID: "i", SiteID: "s", Status: "open",
			Until: time.Now().Add(time.Hour).Format(time.RFC3339)}, http.StatusBadRequest},
		{"until requires resolved (ignored)", updateStatusInput{IssueID: "i", SiteID: "s", Status: "ignored",
			Until: time.Now().Add(time.Hour).Format(time.RFC3339)}, http.StatusBadRequest},
		{"until must parse", updateStatusInput{IssueID: "i", SiteID: "s", Status: "resolved",
			Until: "next tuesday"}, http.StatusBadRequest},
		{"until must be future", updateStatusInput{IssueID: "i", SiteID: "s", Status: "resolved",
			Until: time.Now().Add(-time.Minute).Format(time.RFC3339)}, http.StatusBadRequest},
	}
	for _, tc := range cases {
		_, err := h(t.Context(), tc.in)
		var appErr *neutron.AppError
		if !errors.As(err, &appErr) || appErr.Status != tc.want {
			t.Fatalf("%s: want %d, got %v", tc.name, tc.want, err)
		}
	}
	// The legal shape (resolved + future until) is exercised end to end
	// against live Nucleus in internal/errors (o05_lifecycle_test.go);
	// here only the 400 boundary is storage-free.
}
