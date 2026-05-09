package detectors

import "testing"

func TestSlowHTTPCall(t *testing.T) {
	tests := []struct {
		name      string
		duration  int64
		wantFires bool
	}{
		{"1s below threshold", 1000, false},
		{"2999ms boundary below", 2999, false},
		{"3s boundary fires", 3000, true},
		{"15s severe", 15000, true},
	}

	d := NewSlowHTTPCall()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			span := Span{
				TraceID:       "t1",
				SpanID:        "s1",
				ParentSpanID:  "p1",
				ServiceName:   "api",
				OperationName: "HTTP GET",
				SpanKind:      "client",
				DurationMs:    tc.duration,
				Attributes:    map[string]string{"http.url": "https://stripe.com/v1/charges", "http.method": "POST"},
				StartMs:       1000,
				EndMs:         1000 + tc.duration,
			}
			got := d.Detect([]Span{span})
			if tc.wantFires && len(got) != 1 {
				t.Fatalf("want 1 issue, got %d", len(got))
			}
			if !tc.wantFires && len(got) != 0 {
				t.Fatalf("want 0 issues, got %d", len(got))
			}
		})
	}
}

func TestSlowHTTPCallServerSpanIgnored(t *testing.T) {
	// A slow server-kind span (inbound) is not an outbound HTTP issue.
	got := NewSlowHTTPCall().Detect([]Span{{
		TraceID: "t1", SpanID: "s1", ParentSpanID: "p1",
		ServiceName: "api", OperationName: "GET /users",
		SpanKind:   "server",
		DurationMs: 5000, StartMs: 1, EndMs: 5001,
		Attributes: map[string]string{"http.url": "https://demo.local/users", "http.method": "GET"},
	}})
	if len(got) != 0 {
		t.Fatalf("server-kind span should not fire, got %d", len(got))
	}
}
