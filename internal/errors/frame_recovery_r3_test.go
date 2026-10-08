package errors

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/useteploy/teploy-observe/internal/ingest"
)

func r3Queue(t *testing.T, root string) *ingest.DiskQueue {
	t.Helper()
	q, err := ingest.NewDiskQueue(root, "errors", time.Hour, 1<<20, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	return q
}
func r3Envelope(t *testing.T, kind string) json.RawMessage {
	t.Helper()
	if kind == "poison" {
		return json.RawMessage(`{"site_id":"","body":null}`)
	}
	body, err := json.Marshal(ErrorInput{SiteID: "site", ErrorType: kind})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(errorRecord{SiteID: "site", Body: body})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func r3Remaining(t *testing.T, q *ingest.DiskQueue) int {
	t.Helper()
	n := 0
	if err := q.StreamRecordFrames(func(rs []json.RawMessage, _ int64) error { n += len(rs); return nil }); err != nil {
		t.Fatal(err)
	}
	return n
}

// Real WAL/checkpoint/reopen tests; Close models a process boundary without
// buffer drain. These do not claim OS power-loss/fsync fault acceptance.
func TestSR202WholeFrameSurvivesTwoReopensAndPartialRuntimeFinals(t *testing.T) {
	for _, kinds := range [][]string{{"final", "pending-a"}, {"poison", "pending-a"}, {"final", "pending-a", "pending-b"}} {
		t.Run(kinds[0]+kinds[len(kinds)-1], func(t *testing.T) {
			root := t.TempDir()
			q := r3Queue(t, root)
			// A separate complete prefix may advance, but never the mixed frame.
			if _, err := q.AppendRecords([]json.RawMessage{r3Envelope(t, "final")}); err != nil {
				t.Fatal(err)
			}
			rs := make([]json.RawMessage, len(kinds))
			for i, k := range kinds {
				rs[i] = r3Envelope(t, k)
			}
			off, err := q.AppendRecords(rs)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := q.AppendRecords([]json.RawMessage{r3Envelope(t, "final")}); err != nil {
				t.Fatal(err)
			}
			allowA, allowAll := false, false
			makeBuffer := func() *ErrorBuffer {
				b := NewErrorBuffer(nil, 10, 100, time.Hour, slog.Default())
				b.apply = func(_ context.Context, ev bufferedError) bool {
					var in ErrorInput
					if err := json.Unmarshal(ev.Body, &in); err != nil {
						return b.quarantine(ev, err)
					}
					return in.ErrorType == "final" || allowAll || (in.ErrorType == "pending-a" && allowA)
				}
				return b
			}
			b := makeBuffer()
			if err := b.AttachQueue(q); err != nil {
				t.Fatal(err)
			}
			for _, ev := range b.events {
				if ev.Offset != off {
					t.Fatalf("lost frame grouping: %+v", ev)
				}
			}
			if got := r3Remaining(t, q); got != len(kinds)+1 {
				t.Fatalf("startup checkpoint crossed mixed frame: %d", got)
			}
			if err := q.Close(); err != nil {
				t.Fatal(err)
			}
			q = r3Queue(t, root)
			b = makeBuffer()
			if err := b.AttachQueue(q); err != nil {
				t.Fatal(err)
			}
			// First member final, last still pending within the SAME runtime batch.
			if len(kinds) == 3 {
				allowA = true
			}
			b.Flush()
			if got := r3Remaining(t, q); got != len(kinds)+1 {
				t.Fatalf("runtime checkpoint crossed pending sibling: %d", got)
			}
			if err := q.Close(); err != nil {
				t.Fatal(err)
			}
			q = r3Queue(t, root)
			defer q.Close()
			b = makeBuffer()
			if err := b.AttachQueue(q); err != nil {
				t.Fatal(err)
			}
			if len(b.events) != 1 {
				t.Fatalf("second reopen lost pending obligation: %+v", b.Stats())
			}
			allowAll = true
			b.Flush()
			if b.Stats().Pending != 0 || b.Stats().Bytes != 0 {
				t.Fatalf("reservations not released: %+v", b.Stats())
			}
			// Later already-final frames may remain until restart; every record is
			// final now, so recovery can advance to the actual final WAL offset.
			if err := q.Close(); err != nil {
				t.Fatal(err)
			}
			q = r3Queue(t, root)
			defer q.Close()
			b = makeBuffer()
			if err := b.AttachQueue(q); err != nil {
				t.Fatal(err)
			}
			if got := r3Remaining(t, q); got != 0 {
				t.Fatalf("final tail did not checkpoint: %d", got)
			}
		})
	}
}
