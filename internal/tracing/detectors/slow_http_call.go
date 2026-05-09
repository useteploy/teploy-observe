package detectors

import "fmt"

// SlowHTTPCallThresholdMs is the minimum duration for a single outbound HTTP
// span to be flagged. 3s catches genuinely slow third-party calls without
// drowning the issue list in slightly-slow API gateways.
const SlowHTTPCallThresholdMs int64 = 3000

// SlowHTTPCall detects single outbound HTTP client spans whose duration
// exceeds a threshold.
type SlowHTTPCall struct {
	ThresholdMs int64
}

func NewSlowHTTPCall() *SlowHTTPCall {
	return &SlowHTTPCall{ThresholdMs: SlowHTTPCallThresholdMs}
}

func (d *SlowHTTPCall) Name() string { return "slow_http_call" }

func (d *SlowHTTPCall) Detect(spans []Span) []Issue {
	threshold := d.ThresholdMs
	if threshold <= 0 {
		threshold = SlowHTTPCallThresholdMs
	}

	var issues []Issue
	for _, s := range spans {
		if !isHTTPClientSpan(s) {
			continue
		}
		if s.DurationMs < threshold {
			continue
		}

		target := s.Attributes["http.url"]
		if target == "" {
			target = s.Attributes["http.host"]
		}
		if target == "" {
			target = s.OperationName
		}
		fp := hashFingerprint("slow_http_call", s.ServiceName, target)
		issues = append(issues, Issue{
			TraceID:      s.TraceID,
			DetectorName: "slow_http_call",
			Fingerprint:  fp,
			Title:        fmt.Sprintf("Slow HTTP call: %dms", s.DurationMs),
			Description:  fmt.Sprintf("%s → %s", s.ServiceName, truncate(target, 200)),
			Severity:     severityForDuration(s.DurationMs, threshold),
			FirstSeen:    s.StartMs,
			LastSeen:     s.EndMs,
		})
	}
	return issues
}
