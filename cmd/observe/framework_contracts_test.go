package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/neutron-build/neutron/go/neutron"
	"github.com/useteploy/teploy-observe/internal/guardmap"
	"github.com/useteploy/teploy-observe/internal/queryguard"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestQueryRefusalMetadataOnActualFrameworkResponse(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/v1/stats/overview", nil)
	w := httptest.NewRecorder()
	queryRefusalMetadata(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		neutron.WriteError(w, r, guardmap.HTTPError(queryguard.RowBudgetRefusal(nil, 10)))
	})).ServeHTTP(w, r)
	var got struct {
		Extensions map[string]any `json:"extensions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if w.Code != 429 || got.Extensions["refusal_code"] != queryguard.CodeBudgetRows {
		t.Fatalf("%d %+v", w.Code, got)
	}
}
func TestDynamicOutputOpenAPI(t *testing.T) {
	app := neutron.New()
	neutron.Get(app.Router(), "/api/v1/logs/search", func(_ context.Context, _ neutron.Empty) (any, error) { return []string{"log"}, nil })
	spec := observeOpenAPI(app.OpenAPI())
	op := spec.Paths["/api/v1/logs/search"]["get"]
	if _, ok := op.Responses["204"]; ok {
		t.Fatal("dynamic JSON response documented empty")
	}
	if op.Responses["200"].Content["application/json"].Schema == nil {
		t.Fatal("missing JSON schema")
	}
}

func TestStopOnlyHookDoesNotShiftFailedStartRollback(t *testing.T) {
	var stopped []string
	app := neutron.New(neutron.WithLifecycle(
		observeLifecycleHook(neutron.LifecycleHook{Name: "acquired", OnStop: func(context.Context) error { stopped = append(stopped, "acquired"); return nil }}),
		neutron.LifecycleHook{Name: "worker", OnStart: func(context.Context) error { return nil }, OnStop: func(context.Context) error { stopped = append(stopped, "worker"); return nil }},
		neutron.LifecycleHook{Name: "failure", OnStart: func(context.Context) error { return fmt.Errorf("intentional start refusal") }},
	))
	if err := app.Run("127.0.0.1:0"); err == nil {
		t.Fatal("failed hook accepted")
	}
	if !reflect.DeepEqual(stopped, []string{"worker", "acquired"}) {
		t.Fatalf("rollback skipped active worker: %v", stopped)
	}
}
