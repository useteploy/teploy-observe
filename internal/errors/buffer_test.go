package errors

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// R13 (round 4): admission must bound retained bytes, not just count, and
// the buffered record must be a frozen snapshot of the input.
func TestErrorBufferByteBudgetBounded(t *testing.T) {
	b := NewErrorBuffer(nil, 50000, 100, time.Hour, slog.New(slog.DiscardHandler))
	// Shrink the budget so the test does not need 64 MiB of payloads.
	b.maxBytes = 8192

	big := strings.Repeat("x", 3000)
	accepted := 0
	for i := 0; i < 20; i++ {
		in := ErrorInput{SiteID: "s", ErrorType: "T", ErrorValue: big}
		if b.Push("s", in) != nil {
			break
		}
		accepted++
	}
	if accepted == 0 {
		t.Fatal("first push was rejected — budget too small for the test payload")
	}
	if accepted == 20 {
		t.Fatal("byte budget never rejected admission")
	}
	st := b.Stats()
	if st.Queued != accepted {
		t.Fatalf("queued=%d accepted=%d", st.Queued, accepted)
	}
	if st.Bytes > b.maxBytes {
		t.Fatalf("usedBytes %d exceeds budget %d", st.Bytes, b.maxBytes)
	}
	// Count budget still enforced independently.
	small := NewErrorBuffer(nil, 2, 100, time.Hour, slog.New(slog.DiscardHandler))
	if err := small.Push("s", ErrorInput{ErrorType: "T"}); err != nil {
		t.Fatalf("count budget rejected early: %v", err)
	}
	if err := small.Push("s", ErrorInput{ErrorType: "T"}); err != nil {
		t.Fatalf("count budget rejected second record: %v", err)
	}
	if err := small.Push("s", ErrorInput{ErrorType: "T"}); err == nil {
		t.Fatal("count budget not enforced")
	}
}

// R13: mutation of the caller's input after Push cannot change the buffered
// record.
func TestErrorBufferPushFreezesInput(t *testing.T) {
	b := NewErrorBuffer(nil, 10, 100, time.Hour, slog.New(slog.DiscardHandler))
	inner := map[string]string{"k": "original"}
	in := ErrorInput{SiteID: "s", ErrorType: "T", Extra: inner}
	if err := b.Push("s", in); err != nil {
		t.Fatalf("push rejected: %v", err)
	}
	inner["k"] = "mutated"
	in.ErrorType = "MutatedAfterCapture"

	var frozen ErrorInput
	if err := json.Unmarshal(b.events[0].Body, &frozen); err != nil {
		t.Fatalf("frozen record does not decode: %v", err)
	}
	extra, _ := frozen.Extra.(map[string]any)
	if extra["k"] != "original" {
		t.Fatalf("buffered record mutated after capture: %v", extra)
	}
	if frozen.ErrorType != "T" {
		t.Fatalf("buffered field mutated after capture: %q", frozen.ErrorType)
	}
	if frozen.SiteID != "s" {
		t.Fatalf("site not stamped into the frozen record: %q", frozen.SiteID)
	}
}

// R13: an unserializable input is rejected at admission (json.Marshal of
// ErrorInput cannot fail for plain fields, but a >cap record must reject).
func TestErrorBufferRejectsOversizedRecord(t *testing.T) {
	b := NewErrorBuffer(nil, 50000, 100, time.Hour, slog.New(slog.DiscardHandler))
	huge := strings.Repeat("x", maxErrorRecordBytes+1)
	if err := b.Push("s", ErrorInput{ErrorValue: huge}); err == nil {
		t.Fatal("oversized record admitted")
	}
}

// R15: a latched worker failure closes admission and is observable.
func TestErrorBufferWorkerFailureClosesAdmission(t *testing.T) {
	b := NewErrorBuffer(nil, 10, 100, time.Hour, slog.New(slog.DiscardHandler))
	if err := b.Push("s", ErrorInput{ErrorType: "T"}); err != nil {
		t.Fatalf("healthy buffer rejected admission: %v", err)
	}
	b.fail(errWorkerPanicForTest)
	if err := b.Push("s", ErrorInput{ErrorType: "T"}); err == nil {
		t.Fatal("admission accepted after fatal worker failure")
	}
	if b.WorkerErr() == nil {
		t.Fatal("WorkerErr did not surface the latched failure")
	}
}

// R13: release retires the reservation, re-admitting after a drain.
func TestErrorBufferReleaseRestoresBudget(t *testing.T) {
	b := NewErrorBuffer(nil, 10, 100, time.Hour, slog.New(slog.DiscardHandler))
	in := ErrorInput{SiteID: "s", ErrorType: "T", ErrorValue: strings.Repeat("x", 400)}
	if err := b.Push("s", in); err != nil {
		t.Fatalf("push rejected: %v", err)
	}
	usedBefore := b.Stats().Bytes
	ev := b.events[0]
	b.mu.Lock()
	b.events = nil
	b.mu.Unlock()
	b.release(ev)
	usedAfter := b.Stats().Bytes
	if usedAfter >= usedBefore {
		t.Fatalf("release did not retire the reservation: before=%d after=%d", usedBefore, usedAfter)
	}
}

type workerPanicErr struct{}

func (workerPanicErr) Error() string { return "panic" }

var errWorkerPanicForTest = workerPanicErr{}
