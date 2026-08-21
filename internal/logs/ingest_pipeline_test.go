package logs

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
)

func logsTestDB(t *testing.T) (*nucleus.Client, func()) {
	t.Helper()
	dsn := nucleustest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		cancel()
		t.Skipf("nucleus not reachable at %s — skipping", dsn)
	}
	return db, func() { db.Close(); cancel() }
}

// TestIngestLog_AppliesMaskPipeline is the regression for the HIGH finding that
// log pipelines were never executed on ingest: a mask rule must rewrite the
// stored message, not be silently inert.
func TestIngestLog_AppliesMaskPipeline(t *testing.T) {
	db, done := logsTestDB(t)
	defer done()
	ctx := context.Background()

	site := fmt.Sprintf("test-mask-%d", time.Now().UnixNano())
	pipes := NewPipelineService(db)
	if _, err := pipes.Create(ctx, site, "mask-tokens",
		`[{"type":"mask","pattern":"secret-[a-z0-9]+"}]`, 0); err != nil {
		t.Fatalf("create pipeline: %v", err)
	}

	svc := NewLogService(db)
	svc.SetPipelines(pipes)
	if _, err := svc.IngestLog(ctx, LogInput{SiteID: site, Level: "info", Message: "token is secret-abc123 ok"}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	got := searchOne(t, svc, site)
	if strings.Contains(got, "secret-abc123") {
		t.Fatalf("mask rule not applied; stored message still contains the secret: %q", got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("expected redaction marker in stored message, got %q", got)
	}
}

// TestIngestLog_DropPipeline confirms a drop rule yields no stored row and no
// error to the caller.
func TestIngestLog_DropPipeline(t *testing.T) {
	db, done := logsTestDB(t)
	defer done()
	ctx := context.Background()

	site := fmt.Sprintf("test-drop-%d", time.Now().UnixNano())
	pipes := NewPipelineService(db)
	if _, err := pipes.Create(ctx, site, "drop-noise",
		`[{"type":"drop","pattern":"healthcheck"}]`, 0); err != nil {
		t.Fatalf("create pipeline: %v", err)
	}

	svc := NewLogService(db)
	svc.SetPipelines(pipes)
	id, err := svc.IngestLog(ctx, LogInput{SiteID: site, Level: "info", Message: "healthcheck ping"})
	if err != nil {
		t.Fatalf("ingest returned error for dropped log: %v", err)
	}
	if id != "" {
		t.Fatalf("dropped log should not be inserted, got id %q", id)
	}

	logs, err := svc.SearchLogs(ctx, site, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), "", "", "", 10, 0)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(logs) != 0 {
		t.Fatalf("expected no stored logs after drop, got %d", len(logs))
	}
}

func searchOne(t *testing.T, svc *LogService, site string) string {
	t.Helper()
	logs, err := svc.SearchLogs(context.Background(), site, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), "", "", "", 10, 0)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("expected exactly 1 stored log, got %d", len(logs))
	}
	return logs[0].Message
}
