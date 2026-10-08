package dashboards

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/neutron-build/neutron/go/nucleus"
	"github.com/useteploy/teploy-observe/internal/nucleustest"
	"testing"
	"time"
)

func TestAuditExecuteErrorSeries(t *testing.T) {
	dsn := nucleustest.DSN(t)
	ctx := context.Background()
	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	site := fmt.Sprintf("audit_error_panel_%d", time.Now().UnixNano())
	from := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	to := from.Add(2 * time.Hour)
	for i, ts := range []int64{from.UnixMilli(), from.UnixMilli() + 1, from.Add(time.Hour).UnixMilli(), to.UnixMilli()} {
		_, err := db.SQL().Exec(ctx, `INSERT INTO error_events (error_id,site_id,group_hash,timestamp) VALUES ($1,$2,'audit-test',$3)`, fmt.Sprintf("%s-%d", site, i), site, ts)
		if err != nil {
			t.Fatal(err)
		}
	}
	result, err := NewDashboardService(db).ExecutePanel(ctx, site, Panel{PanelType: "timeseries", QueryType: "errors"}, from.Format(time.RFC3339), to.Format(time.RFC3339))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		Bucket int64 `json:"bucket"`
		Errors int64 `json:"errors"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Bucket != from.UnixMilli() || rows[0].Errors != 2 || rows[1].Errors != 1 {
		t.Fatalf("half-open ordered error series: %s", raw)
	}
}
