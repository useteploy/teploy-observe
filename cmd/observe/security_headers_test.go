package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSecurityHeadersSetIfAbsent(t *testing.T) {
	h := securityHeadersMiddleware(true)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	hdr := rec.Header()
	for k, want := range map[string]string{
		"X-Content-Type-Options":    "nosniff",
		"Referrer-Policy":           "strict-origin-when-cross-origin",
		"X-Frame-Options":           "SAMEORIGIN",
		"Strict-Transport-Security": "max-age=31536000",
	} {
		if hdr.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, hdr.Get(k), want)
		}
	}
}

func TestSecurityHeadersDoNotClobberHandlerPolicies(t *testing.T) {
	h := securityHeadersMiddleware(false)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/share/x", nil))
	if got := rec.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Fatalf("handler policy clobbered: %q", got)
	}
	if got := rec.Header().Get("Strict-Transport-Security"); got != "" {
		t.Fatalf("HSTS must not be set when hsts=false: %q", got)
	}
}
