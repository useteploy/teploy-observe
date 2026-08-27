package platform

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// The outbound alert webhook had no test at all until this file: its signature
// scheme, its headers and its payload shape are a wire contract that another
// service verifies byte for byte (teploy-ship's /hooks/observe receiver), and a
// silent change to any of them fails as "Ship stopped opening incidents", far
// from the line that caused it.

func testWebhookService(t *testing.T) *WebhookService {
	t.Helper()
	svc := NewWebhookService(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// The production client refuses private and CGNAT destinations
	// (netsafe.go:41-56), and httptest binds 127.0.0.1. Swap in a plain client
	// so this exercises the signing path rather than the dial guard.
	svc.client = &http.Client{Timeout: 5 * time.Second}
	return svc
}

func samplePayload() AlertPayload {
	return AlertPayload{
		AlertID:   "al_7c1f",
		RuleID:    "rule_42",
		RuleName:  "error rate over 5%",
		Metric:    "error_rate",
		Value:     12.5,
		Threshold: "5",
		SiteID:    "site_fylun",
		Timestamp: "2026-08-26T21:04:00Z",
	}
}

func TestFireHTTPSignsBodyWithTimestamp(t *testing.T) {
	const secret = "s3cr3t"
	var (
		gotBody []byte
		gotSig  string
		gotTS   string
		gotID   string
		gotType string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotSig = r.Header.Get("X-Observe-Signature")
		gotTS = r.Header.Get("X-Observe-Timestamp")
		gotID = r.Header.Get("X-Observe-Delivery")
		gotType = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := testWebhookService(t).fireHTTP(srv.URL, secret, samplePayload()); err != nil {
		t.Fatalf("fireHTTP: %v", err)
	}

	if gotType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotType)
	}
	if gotTS == "" {
		t.Fatal("X-Observe-Timestamp is empty")
	}
	if _, err := strconv.ParseInt(gotTS, 10, 64); err != nil {
		t.Errorf("X-Observe-Timestamp = %q, want unix seconds: %v", gotTS, err)
	}
	// The receiver recomputes exactly this. hex, lowercase, "sha256=" prefixed,
	// over "<timestamp>.<raw body>" — not over the body alone.
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(gotTS))
	mac.Write([]byte("."))
	mac.Write(gotBody)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if gotSig != want {
		t.Errorf("X-Observe-Signature = %q, want %q", gotSig, want)
	}
	if len(gotID) != 32 {
		t.Errorf("X-Observe-Delivery = %q, want a 32-char id", gotID)
	}
}

func TestFireHTTPDeliveryIDIsUniquePerAttempt(t *testing.T) {
	var ids []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ids = append(ids, r.Header.Get("X-Observe-Delivery"))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	svc := testWebhookService(t)
	for i := 0; i < 2; i++ {
		if err := svc.fireHTTP(srv.URL, "", samplePayload()); err != nil {
			t.Fatalf("fireHTTP: %v", err)
		}
	}
	if len(ids) != 2 || ids[0] == ids[1] {
		t.Errorf("delivery ids %v, want two distinct values", ids)
	}
	// An unsigned webhook still gets one: replay protection does not depend on
	// whether a secret happens to be configured.
	if ids[0] == "" {
		t.Error("unsigned delivery carries no X-Observe-Delivery")
	}
}

func TestFireHTTPOmitsSignatureHeadersWithoutASecret(t *testing.T) {
	var sig, ts string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sig = r.Header.Get("X-Observe-Signature")
		ts = r.Header.Get("X-Observe-Timestamp")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := testWebhookService(t).fireHTTP(srv.URL, "", samplePayload()); err != nil {
		t.Fatalf("fireHTTP: %v", err)
	}
	if sig != "" || ts != "" {
		t.Errorf("unsigned delivery sent sig=%q ts=%q, want both empty", sig, ts)
	}
}

func TestAlertPayloadWireShape(t *testing.T) {
	// The receiver parses these names. value is a JSON number and threshold is
	// a JSON string in the same object — deliberate, and easy to "tidy" into a
	// break, so it is asserted here.
	body, err := json.Marshal(samplePayload())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"alert_id", "rule_id", "rule_name", "metric", "value", "threshold", "site_id", "timestamp"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("payload is missing %q", key)
		}
	}
	if _, ok := raw["value"].(float64); !ok {
		t.Errorf("value = %T, want a JSON number", raw["value"])
	}
	if _, ok := raw["threshold"].(string); !ok {
		t.Errorf("threshold = %T, want a JSON string", raw["threshold"])
	}
}

func TestFireHTTPReturnsErrorOnFailureStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	if err := testWebhookService(t).fireHTTP(srv.URL, "s", samplePayload()); err == nil {
		t.Error("fireHTTP returned nil for a 500 response")
	}
}
