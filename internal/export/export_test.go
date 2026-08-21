package export

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
)

func TestCSVSafe(t *testing.T) {
	cases := map[string]string{
		"hello":           "hello",
		"=cmd|'/c calc'":  "'=cmd|'/c calc'",
		"+1":              "'+1",
		"-2":              "'-2",
		"@x":              "'@x",
		"normal text":     "normal text",
		"":                "",
		"https://ok.test": "https://ok.test",
	}
	for in, want := range cases {
		if got := csvSafe(in); got != want {
			t.Fatalf("csvSafe(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExport_SiteScopedCSV(t *testing.T) {
	dsn := nucleustest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("nucleus not reachable at %s — skipping", dsn)
	}
	defer db.Close()

	siteA := fmt.Sprintf("exp-a-%d", time.Now().UnixNano())
	siteB := fmt.Sprintf("exp-b-%d", time.Now().UnixNano())
	ts := time.Now().UTC().Add(-1 * time.Hour).UnixMilli()
	ins := func(site, eid, url string) {
		_, err := db.SQL().Exec(ctx,
			`INSERT INTO events (event_id, tenant_id, site_id, session_id, visit_id, event_type, timestamp, url)
			 VALUES ($1,'default',$2,$3,$3,'pageview',$4,$5)`, eid, site, "s-"+eid, ts, url)
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	ins(siteA, "ea1-"+siteA, "=danger()") // also exercises CSV injection sanitization
	ins(siteB, "eb1-"+siteB, "https://b.test")

	svc := NewExportService(db)
	from := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
	to := time.Now().UTC().Format(time.RFC3339)
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/export?format=csv&type=events&site_id="+siteA+"&from="+from+"&to="+to, nil)
	rec := httptest.NewRecorder()
	svc.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "event_id,session_id,event_type") {
		t.Fatalf("missing CSV header: %q", body)
	}
	if !strings.Contains(body, "ea1-"+siteA) {
		t.Fatalf("expected site A event in export")
	}
	if strings.Contains(body, "eb1-"+siteB) {
		t.Fatalf("site B event leaked into site A export")
	}
	if !strings.Contains(body, "'=danger()") {
		t.Fatalf("formula injection not neutralized: %q", body)
	}
}

func TestExport_ValidatesParams(t *testing.T) {
	svc := NewExportService(nil) // no DB needed: validation happens before any query
	req := httptest.NewRequest(http.MethodGet, "/api/v1/export?format=xml&type=events&site_id=s", nil)
	rec := httptest.NewRecorder()
	svc.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad format should be 400, got %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/export?format=csv&type=events", nil)
	rec = httptest.NewRecorder()
	svc.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing site_id should be 400, got %d", rec.Code)
	}
}
