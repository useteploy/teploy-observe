package logs

import (
	"context"
	"errors"
	"fmt"
	logspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	otlplogs "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"
	"strconv"
	"testing"
	"time"
)

func TestOBS99PolicyFailureRefusesSingleAndBatch(t *testing.T) {
	for _, expired := range []bool{false, true} {
		p := NewPipelineService(nil)
		p.list = func(context.Context, string) ([]Pipeline, error) {
			return nil, errors.New("isolated policy read failure")
		}
		if expired {
			p.cache["site"] = pipelineCacheEntry{pipelines: []Pipeline{{Rules: `[{"type":"mask","pattern":"FAKE_ONLY_SECRET"}]`}}, expires: time.Now().Add(-time.Second)}
		}
		s := NewLogService(nil)
		s.SetPipelines(p)
		if row, err := s.prepareLog(context.Background(), LogInput{SiteID: "site", Message: "FAKE_ONLY_SECRET"}); err == nil || row != nil {
			t.Fatalf("policy failed open: %v %v", row, err)
		}
		if r, err := s.IngestLogs(context.Background(), []LogInput{{SiteID: "site", Message: "FAKE_ONLY_SECRET"}}); !errors.Is(err, ErrPipelineUnavailable) || r.Accepted != 0 {
			t.Fatalf("batch failed open: %+v %v", r, err)
		}
	}
}
func TestOBS101SampleBoundariesAndPattern(t *testing.T) {
	for _, pct := range []int{0, 1, 100} {
		p := NewPipelineService(nil)
		p.cache["site"] = pipelineCacheEntry{pipelines: []Pipeline{{Rules: fmt.Sprintf(`[{"type":"sample","pattern":"match","value":"%d"}]`, pct)}}, expires: time.Now().Add(time.Hour)}
		kept := 0
		for i := 0; i < 1000; i++ {
			_, _, keep := p.ProcessLog(context.Background(), "site", fmt.Sprintf("match-%d", i), nil)
			if keep {
				kept++
			}
		}
		if pct == 0 && kept != 0 || pct == 100 && kept != 1000 || pct == 1 && (kept == 0 || kept == 1000) {
			t.Fatalf("sample %d retained %d", pct, kept)
		}
		_, _, keep := p.ProcessLog(context.Background(), "site", "unrelated", nil)
		if !keep {
			t.Fatal("sample dropped a nonmatch")
		}
	}
}
func TestOBS100OTLPEventTimeAndObservedFallback(t *testing.T) {
	old := time.Now().Add(-48 * time.Hour).UnixNano()
	for _, event := range []int64{0, old} {
		wire := fmt.Sprintf(`{"resourceLogs":[{"scopeLogs":[{"logRecords":[{"timeUnixNano":"%d","observedTimeUnixNano":"%d"}]}]}]}`, event, old+1000000)
		jsonInputs, err := jsonLogInputs([]byte(wire), "site")
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := proto.Marshal(&logspb.ExportLogsServiceRequest{ResourceLogs: []*otlplogs.ResourceLogs{{ScopeLogs: []*otlplogs.ScopeLogs{{LogRecords: []*otlplogs.LogRecord{{TimeUnixNano: uint64(event), ObservedTimeUnixNano: uint64(old + 1000000)}}}}}}})
		pbInputs, err := protoLogInputs(raw, "site")
		if err != nil {
			t.Fatal(err)
		}
		want := event
		if want == 0 {
			want = old + 1000000
		}
		for _, in := range []LogInput{jsonInputs[0], pbInputs[0]} {
			p, err := NewLogService(nil).prepareLog(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			if p.tsMs != strconv.FormatInt(want/1000000, 10) || p.now.UnixNano() != want {
				t.Fatalf("event time replaced: %+v", p)
			}
		}
	}
	for _, bad := range []string{"-1", "18446744073709551615", "bad"} {
		if _, err := logEventTime(bad, "0"); err == nil {
			t.Fatalf("accepted invalid timestamp %q", bad)
		}
	}
	if _, err := NewLogService(nil).prepareLog(context.Background(), LogInput{TimestampNs: time.Now().Add(48 * time.Hour).UnixNano()}); err == nil {
		t.Fatal("accepted future timestamp")
	}
}
func TestOBS21SparseCandidateWindowStillHasContinuation(t *testing.T) {
	plan := compileFor(t, `attr.k:match`)
	for _, matches := range []int{0, 1, 49} {
		rows := make([]Log, 100)
		for i := range rows {
			attrs := `{"k":"miss"}`
			if i < matches {
				attrs = `{"k":"match"}`
			}
			rows[i] = Log{LogID: fmt.Sprintf("id%03d", i), Timestamp: time.UnixMilli(int64(1000 - i)), Attributes: attrs}
		}
		r := pageResult(rows, plan, 50, 100)
		if len(r.Logs) != matches || r.NextCursor == "" || !r.Truncated {
			t.Fatalf("sparse window lost continuation: matches=%d result=%+v", matches, r)
		}
	}
}
