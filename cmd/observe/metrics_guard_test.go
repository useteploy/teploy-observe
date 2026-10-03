package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy-observe/internal/queryguard"
)

// Guard refusals keep their labeled status through the metrics handlers;
// other errors keep the handler's historical status.
func TestWriteMetricsErrorMapsRefusals(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		fallback int
		want     int
		body     string
	}{
		{"rows", queryguard.RowBudgetRefusal(nil, 5), http.StatusBadRequest, http.StatusTooManyRequests, "query_budget_rows"},
		{"time", queryguard.TimeBudgetRefusal(nil, time.Second), http.StatusBadRequest, http.StatusGatewayTimeout, "query_budget_time"},
		{"plain-400", errors.New("bad agg"), http.StatusBadRequest, http.StatusBadRequest, "bad agg"},
		{"plain-500", errors.New("boom"), http.StatusInternalServerError, http.StatusInternalServerError, "boom"},
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/api/v1/metrics/series", nil)
		writeMetricsError(w, r, c.err, c.fallback)
		if w.Code != c.want {
			t.Errorf("%s: status %d, want %d", c.name, w.Code, c.want)
		}
		if !strings.Contains(w.Body.String(), c.body) {
			t.Errorf("%s: body %q missing %q", c.name, w.Body.String(), c.body)
		}
	}
}
