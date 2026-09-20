package errors

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// ErrorBuffer accumulates error events and batch-processes them
// asynchronously, decoupling HTTP response from KV+FTS+issue resolution.
//
// Audit F11: size-triggered flushes used to launch `go b.Flush()` goroutines
// that Stop never waited for, and a failed Handle dropped the event with a
// log line. Flushes now run on ONE owned worker woken by a coalesced channel
// (same lifecycle as the analytics buffer), admission closes before the
// drain, and Stop waits for the worker.
//
// R13 (round 4): admission is bounded by count AND retained bytes, reserved
// across queued AND in-flight records; each buffered record is a frozen
// serialized snapshot, so a caller mutating its input map after Push cannot
// change what gets stored. The byte budget (default 64 MiB) bounds the
// retained-memory product of the 2 MiB route cap and the queue depth.
//
// R15 (round 4): a worker failure latches (workerErr) and closes admission —
// /healthz reports 503 so supervisors see a dead pipeline instead of a
// healthy process that acknowledges and drops.
//
// Events whose storage still fails are dropped with a log line. A durable
// retry spool needs an idempotent sink (IngestErrorEvent mints fresh IDs per
// call, so blindly requeueing would double-count issue counters); that design
// is deferred and recorded in AUDIT_OPEN.md (R14's durable half).
type ErrorBuffer struct {
	mu            sync.Mutex
	events        []bufferedError
	maxSize       int
	maxBytes      int64
	usedBytes     int64
	flushSize     int
	flushInterval time.Duration
	handler       *ErrorHandler
	logger        *slog.Logger
	stopCh        chan struct{}
	// wake coalesces size-triggered flush wakeups: capacity one, so any
	// number of above-threshold Pushes while a flush is in flight collapse
	// into a single pending wakeup instead of piling up goroutines.
	wake      chan struct{}
	wg        sync.WaitGroup
	closing   bool
	stopOnce  sync.Once
	workerErr error
}

type bufferedError struct {
	// Body is the serialized ErrorInput captured at admission (R13): the
	// record is immutable from the moment Push returns. Cost stays counted
	// in usedBytes until the record's final disposition, so a detached
	// in-flight batch keeps its reservation.
	Body []byte
	Site string
	Cost int64
}

// DefaultErrorBufferBytes is the retained-memory budget for queued plus
// in-flight error records.
const DefaultErrorBufferBytes = 64 << 20

// maxErrorRecordBytes caps one serialized error record at admission. Larger
// inputs are rejected (429) rather than buffered.
const maxErrorRecordBytes = 256 << 10

func NewErrorBuffer(handler *ErrorHandler, maxSize, flushSize int, flushInterval time.Duration, logger *slog.Logger) *ErrorBuffer {
	return &ErrorBuffer{
		events:        make([]bufferedError, 0, flushSize),
		maxSize:       maxSize,
		maxBytes:      DefaultErrorBufferBytes,
		flushSize:     flushSize,
		flushInterval: flushInterval,
		handler:       handler,
		logger:        logger,
		stopCh:        make(chan struct{}),
		wake:          make(chan struct{}, 1),
	}
}

func (b *ErrorBuffer) Start() {
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		defer func() {
			if r := recover(); r != nil {
				// R15: a panicked worker latches fatal and closes admission
				// instead of leaving a silent ack-and-drop pipeline.
				b.fail(fmt.Errorf("error worker panic: %v", r))
				b.logger.Error("error buffer goroutine panicked; admission closed", "err", r)
			}
		}()
		ticker := time.NewTicker(b.flushInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				b.Flush()
			case <-b.wake:
				// One bounded attempt per wakeup (audit F10 pattern): a
				// failing flush is retried by the ticker, not by a tight
				// loop the stop channel cannot interrupt.
				b.Flush()
			case <-b.stopCh:
				b.Flush()
				return
			}
		}
	}()
}

// fail latches a fatal worker error and closes admission (R15).
func (b *ErrorBuffer) fail(err error) {
	b.mu.Lock()
	b.workerErr = err
	b.closing = true
	b.mu.Unlock()
}

// WorkerErr returns the latched fatal worker error, if any (R15 — surfaced
// by /healthz).
func (b *ErrorBuffer) WorkerErr() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.workerErr
}

// Stop closes admission, wakes the worker, and waits for the final flush.
// Idempotent; safe to call concurrently with Push.
func (b *ErrorBuffer) Stop() {
	b.stopOnce.Do(func() {
		b.mu.Lock()
		b.closing = true
		b.mu.Unlock()
		close(b.stopCh)
	})
	b.wg.Wait()
}

// Push adds an error to the buffer. Returns false if the buffer is full
// (count or byte budget), the record is too large, shutdown has begun, or a
// fatal worker failure has closed admission.
func (b *ErrorBuffer) Push(siteID string, input ErrorInput) bool {
	input.SiteID = siteID
	raw, err := json.Marshal(input)
	if err != nil || len(raw) > maxErrorRecordBytes {
		return false
	}
	// Payload + site key + envelope allowance.
	cost := int64(len(raw) + len(siteID) + 128)

	b.mu.Lock()
	if b.closing || len(b.events) >= b.maxSize || cost > b.maxBytes-b.usedBytes {
		b.mu.Unlock()
		return false
	}
	b.events = append(b.events, bufferedError{Body: raw, Site: siteID, Cost: cost})
	b.usedBytes += cost
	full := len(b.events) >= b.flushSize
	b.mu.Unlock()

	if full {
		select {
		case b.wake <- struct{}{}:
		default:
		}
	}
	return true
}

// release retires a record's reservation on its FINAL disposition (R13):
// successful store, dropped failure, or unserializable tail.
func (b *ErrorBuffer) release(ev bufferedError) {
	b.mu.Lock()
	b.usedBytes -= ev.Cost
	b.mu.Unlock()
}

// Flush processes all buffered errors on the caller's goroutine. The worker
// is the only routine that calls it in normal operation; the HTTP ingest
// path no longer spawns per-push flushes (audit F11).
//
// R14 contained half (round 4): each record gets its own bounded context, so
// one slow storage call can no longer exhaust a shared deadline and take the
// whole remaining batch tail with it. The durable idempotent-inbox half of
// R14 stays deferred (see the package comment).
func (b *ErrorBuffer) Flush() {
	b.mu.Lock()
	if len(b.events) == 0 {
		b.mu.Unlock()
		return
	}
	batch := b.events
	b.events = make([]bufferedError, 0, b.flushSize)
	b.mu.Unlock()

	b.logger.Info("flushing errors", "count", len(batch))
	success := 0
	for _, ev := range batch {
		var input ErrorInput
		if err := json.Unmarshal(ev.Body, &input); err != nil {
			// A record that no longer decodes cannot ever succeed; retire
			// its reservation and move on.
			b.logger.Error("error flush failed", "err", fmt.Errorf("record undecodable: %w", err))
			b.release(ev)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_, herr := b.handler.Handle(ctx, input)
		cancel()
		if herr != nil {
			b.logger.Error("error flush failed", "err", herr)
		} else {
			success++
		}
		b.release(ev)
	}
	b.logger.Info("flushed errors", "success", success, "total", len(batch))
}

// Stats reports queued record count and retained bytes (operator surface).
func (b *ErrorBuffer) Stats() (queued int, bytes int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.events), b.usedBytes
}
