package errors

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// ErrorBuffer accumulates error events and batch-processes them
// asynchronously, decoupling HTTP response from KV+FTS+issue resolution.
type ErrorBuffer struct {
	mu            sync.Mutex
	events        []bufferedError
	maxSize       int
	flushSize     int
	flushInterval time.Duration
	handler       *ErrorHandler
	logger        *slog.Logger
	stopCh        chan struct{}
	wg            sync.WaitGroup
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
			case <-b.stopCh:
				b.Flush()
				return
			}
		}
	}()
}

func (b *ErrorBuffer) Stop() {
	close(b.stopCh)
	b.wg.Wait()
}

// Push adds an error to the buffer. Returns false if buffer is full.
func (b *ErrorBuffer) Push(siteID string, input ErrorInput) bool {
	b.mu.Lock()
	if len(b.events) >= b.maxSize {
		b.mu.Unlock()
		return false
	}
	b.events = append(b.events, bufferedError{Input: input, SiteID: siteID})
	shouldFlush := len(b.events) >= b.flushSize
	b.mu.Unlock()

	if shouldFlush {
		go b.Flush()
	}
	return true
}

// Flush processes all buffered errors.
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
