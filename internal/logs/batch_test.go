package logs

import (
	"context"
	"testing"
)

// IngestLogs over the batch cap must reject before touching the DB — no
// LogService fields are read on this path, so a zero-value service is safe.
func TestIngestLogs_RejectsOversizedBatch(t *testing.T) {
	svc := &LogService{}
	inputs := make([]LogInput, maxLogBatchSize+1)

	_, err := svc.IngestLogs(context.Background(), inputs)
	if err == nil {
		t.Fatal("expected an error for a batch over maxLogBatchSize, got nil")
	}
}

func TestIngestLogs_EmptyBatchIsNotAnError(t *testing.T) {
	svc := &LogService{}

	result, err := svc.IngestLogs(context.Background(), nil)
	if err != nil {
		t.Fatalf("empty batch should not error: %v", err)
	}
	if result.Accepted != 0 || result.Rejected != 0 {
		t.Fatalf("expected zero accepted/rejected for empty batch, got %+v", result)
	}
}
