package sentrycompat

import (
	"encoding/json"
	"fmt"
	obserrors "github.com/useteploy/teploy-observe/internal/errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestOBS132NearLimitBreadcrumbAggregateAdmitted(t *testing.T) {
	crumbs := make([]any, 100)
	for i := range crumbs {
		crumbs[i] = map[string]any{"message": strings.Repeat("x", 1024), "data": map[string]any{"k": strings.Repeat("y", 2035)}}
	}
	body, _ := json.Marshal(map[string]any{"message": "bounded event", "breadcrumbs": crumbs})
	in, _, err := mapEvent(body, "")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(in)
	if len(raw) > 192<<10 || len(in.Breadcrumbs) == 100 {
		t.Fatalf("aggregate not bounded: bytes=%d crumbs=%d", len(raw), len(in.Breadcrumbs))
	}
	b := obserrors.NewErrorBuffer(nil, 10, 100, time.Hour, slog.Default())
	if err := b.Push(siteA, in); err != nil {
		t.Fatalf("bounded mapped record cannot be admitted: %v", err)
	}
	again, _, _ := mapEvent(body, "")
	againRaw, _ := json.Marshal(again)
	if string(raw) != string(againRaw) {
		t.Fatal("budgeting is nondeterministic")
	}
}
func TestOBS133WrappedAdmissionErrorsPermanent(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
	}{{obserrors.ErrBadTimestamp, 400}, {obserrors.ErrErrorRecordTooLarge, 413}, {obserrors.ErrErrorBufferFull, 429}} {
		h := newH(&fakeSink{err: fmt.Errorf("admission: %w", tc.err)}, nil)
		rr := post(h, "/api/1/envelope/", envelope(evItem(`{"message":"test"}`)), map[string]string{"X-Sentry-Auth": "Sentry sentry_key=obs_siteA"})
		if rr.Code != tc.status {
			t.Fatalf("%v: HTTP %d body=%s", tc.err, rr.Code, rr.Body.String())
		}
		if tc.status != 429 && rr.Header().Get("Retry-After") != "" {
			t.Fatal("permanent rejection advertised retry")
		}
	}
	b := obserrors.NewErrorBuffer(nil, 10, 100, time.Hour, slog.Default())
	h := &Handler{Keys: fakeKeys{}, Sink: b}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/{project_id}/store/{$}", h.Store)
	body := []byte(fmt.Sprintf(`{"message":"future","timestamp":%f}`, float64(time.Now().Add(48*time.Hour).UnixMilli())/1000))
	rr := post(mux, "/api/1/store/", body, map[string]string{"X-Sentry-Auth": "Sentry sentry_key=obs_siteA"})
	if rr.Code != 400 {
		t.Fatalf("real admission future: %d %s", rr.Code, rr.Body.String())
	}
}
