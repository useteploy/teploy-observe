package main

import (
	"errors"
	"strconv"

	"github.com/neutron-build/neutron/go/neutron"

	"github.com/useteploy/teploy-observe/internal/queryguard"
	"github.com/useteploy/teploy-observe/internal/replays"
)

// parseReplayFilter turns the replay-list query parameters into a
// replays.ReplayFilter. has_errors is strict ("true"/"false"/empty) and
// min_duration is whole milliseconds; anything else is a 400. Length
// bounds on url_contains/distinct_id are enforced by the service.
func parseReplayFilter(hasErrors, minDuration, urlContains, distinctID string) (replays.ReplayFilter, error) {
	f := replays.ReplayFilter{URLContains: urlContains, DistinctID: distinctID}
	switch hasErrors {
	case "", "false":
	case "true":
		f.HasErrors = true
	default:
		return f, neutron.ErrBadRequest("has_errors must be true or false")
	}
	if minDuration != "" {
		n, err := strconv.ParseInt(minDuration, 10, 64)
		if err != nil || n < 0 {
			return f, neutron.ErrBadRequest("min_duration must be a non-negative integer (milliseconds)")
		}
		f.MinDurationMS = n
	}
	return f, nil
}

// mapGuardRefusal converts an O12 admission refusal into the matching
// HTTP error so a shed query reads as 429/504, not a bare 500.
func mapGuardRefusal(err error) error {
	var ref *queryguard.Refusal
	if errors.As(err, &ref) {
		if ref.Status == 429 {
			return neutron.ErrRateLimited(ref.Error())
		}
		return neutron.ErrServiceUnavailable(ref.Error())
	}
	return err
}
