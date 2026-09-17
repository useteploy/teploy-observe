package errors

import (
	"context"
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
// drain, and Stop waits for the worker. Events whose storage still fails
// during shutdown are reported in the stop log — a durable retry spool needs
// an idempotent sink (IngestErrorEvent mints fresh IDs per call, so blindly
// requeueing would double-count issue counters); that design is deferred and
// recorded in AUDIT_OPEN.md.
type ErrorBuffer struct {
	mu            sync.Mutex
	events        []bufferedError
	maxSize       int
	flushSize     int
	flushInterval time.Duration
	handler       *ErrorHandler
	logger        *slog.Logger
	stopCh        chan struct{}
	// wake coalesces size-triggered flush wakeups: capacity one, so any
	// number of above-threshold Pushes while a flush is in flight collapse
	// into a single pending wakeup instead of piling up goroutines.
	wake     chan struct{}
	wg       sync.WaitGroup
	closing  bool
	stopOnce sync.Once
}

type bufferedError struct {
	Input  ErrorInput
	SiteID string
}

func NewErrorBuffer(handler *ErrorHandler, maxSize, flushSize int, flushInterval time.Duration, logger *slog.Logger) *ErrorBuffer {
	return &ErrorBuffer{
		events:        make([]bufferedError, 0, flushSize),
		maxSize:       maxSize,
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
				b.logger.Error("error buffer goroutine panicked", "err", r)
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

// Push adds an error to the buffer. Returns false if the buffer is full or
// shutdown has begun.
func (b *ErrorBuffer) Push(siteID string, input ErrorInput) bool {
	b.mu.Lock()
	if b.closing || len(b.events) >= b.maxSize {
		b.mu.Unlock()
		return false
	}
	b.events = append(b.events, bufferedError{Input: input, SiteID: siteID})
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

// Flush processes all buffered errors on the caller's goroutine. The worker
// is the only routine that calls it in normal operation; the HTTP ingest
// path no longer spawns per-push flushes (audit F11).
func (b *ErrorBuffer) Flush() {
	b.mu.Lock()
	if len(b.events) == 0 {
		b.mu.Unlock()
		return
	}
	batch := b.events
	b.events = make([]bufferedError, 0, b.flushSize)
	b.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	b.logger.Info("flushing errors", "count", len(batch))
	success := 0
	for _, ev := range batch {
		ev.Input.SiteID = ev.SiteID
		if _, err := b.handler.Handle(ctx, ev.Input); err != nil {
			b.logger.Error("error flush failed", "err", err)
		} else {
			success++
		}
	}
	b.logger.Info("flushed errors", "success", success, "total", len(batch))
}
