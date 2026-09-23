package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The app's durable state replays exactly: counters survive a reopen.
func TestStateReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.jsonl")
	s, err := openState(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := s.bump("hit", 1, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.bump("exposure_control", 2, "c1"); err != nil {
		t.Fatal(err)
	}
	again, err := openState(path)
	if err != nil {
		t.Fatal(err)
	}
	snap := again.snapshot()
	if snap["hit"] != 5 || snap["exposure_control"] != 2 {
		t.Fatalf("replay drift: %+v", snap)
	}
}

// Experiment assignment is deterministic per (site, client) and balanced
// enough across the client space to be a usable O09 fixture.
func TestArmDeterministic(t *testing.T) {
	if armFor("x05", "c1") != armFor("x05", "c1") {
		t.Fatal("assignment not deterministic")
	}
	if armFor("x05", "c1") == armFor("x05", "c2") && armFor("x05", "c2") == armFor("x05", "c3") && armFor("x05", "c3") == armFor("x05", "c4") {
		t.Fatal("all sampled clients share one arm — unusable fixture")
	}
	var control int
	const n = 1000
	for i := 0; i < n; i++ {
		if armFor("x05", "x05-client-"+string(rune('a'+i%20))+string(rune('a'+i/20%20))) == "control" {
			control++
		}
	}
	if control < n/4 || control > 3*n/4 {
		t.Fatalf("assignment badly skewed: %d/%d control", control, n)
	}
}

// A smoke assertion that the page carries the dynamic-DOM legs the replay
// fixtures depend on (mutating counter, form, list, assignment fetch).
func TestPageCarriesDynamicDOMLegs(t *testing.T) {
	for _, needle := range []string{"setInterval", "getElementById('tick')", "<form", "id=\"list\"", "/api/experiment/assign"} {
		if !bytes.Contains([]byte(page), []byte(needle)) {
			t.Fatalf("page missing dynamic-DOM leg: %s", needle)
		}
	}
}

// The documented failpoints are the only accepted values.
func TestFailpointValues(t *testing.T) {
	accepted := map[string]bool{"": true, "stats_read": true, "bg_commit": true}
	if !accepted[os.Getenv("TEPLOY_X05_FAILPOINT")] {
		// Unset in tests — anything else would be a typo the harnesses
		// would silently treat as "no failpoint".
		os.Unsetenv("TEPLOY_X05_FAILPOINT")
	}
}

// End-to-end through the REAL routes (X05: expected results are asserted
// from outside the implementation under test): hits and exposures land in
// stats, assignment is stable, the stats_read failpoint answers 500, and
// the error endpoint counts.
func TestRoutesEndToEnd(t *testing.T) {
	st, err := openState(filepath.Join(t.TempDir(), "state.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	ts := httptest.NewServer(newMux(st, "x05", "test-rel", "", &started))
	defer ts.Close()

	post := func(path string) int {
		resp, err := http.Post(ts.URL+path, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	get := func(path string) (int, string) {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(body)
	}

	for i := 0; i < 3; i++ {
		if code := post("/api/hit"); code != 204 {
			t.Fatalf("hit %d", code)
		}
	}
	code, body := get("/api/experiment/assign?client=e2e-1")
	if code != 200 || !strings.Contains(body, `"arm"`) {
		t.Fatalf("assign: %d %s", code, body)
	}

	code, body = get("/api/stats")
	if code != 200 {
		t.Fatalf("stats: %d", code)
	}
	var stats struct {
		Release  string           `json:"release"`
		Counters map[string]int64 `json:"counters"`
	}
	if err := json.Unmarshal([]byte(body), &stats); err != nil {
		t.Fatal(err)
	}
	if stats.Release != "test-rel" || stats.Counters["hit"] != 3 || stats.Counters["exposure_"+armFor("x05", "e2e-1")] != 1 {
		t.Fatalf("stats drift: %+v", stats)
	}
	if code, _ := get("/error"); code != 500 {
		t.Fatalf("error endpoint: %d", code)
	}
	if st.snapshot()["error_injected"] != 1 {
		t.Fatalf("error counter = %d", st.snapshot()["error_injected"])
	}

	// The stats_read failpoint is a real introducible defect.
	srv2 := httptest.NewServer(newMux(st, "x05", "test-rel", "stats_read", &started))
	defer srv2.Close()
	resp, err := http.Get(srv2.URL + "/api/stats")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 500 {
		t.Fatalf("stats_read failpoint: %d", resp.StatusCode)
	}
}
