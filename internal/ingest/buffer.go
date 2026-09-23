package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"
	"github.com/useteploy/teploy-observe/internal/dbutil"
)

// Event represents a single analytics event ready for storage.
type Event struct {
	EventID        string         `json:"event_id"`
	TenantID       string         `json:"tenant_id"`
	SiteID         string         `json:"site_id"`
	SessionID      string         `json:"session_id"`
	VisitID        string         `json:"visit_id"`
	EventType      string         `json:"event_type"`
	Timestamp      int64          `json:"timestamp"`
	URL            string         `json:"url"`
	Referrer       string         `json:"referrer"`
	Title          string         `json:"title"`
	Hostname       string         `json:"hostname"`
	Pathname       string         `json:"pathname"`
	Language       string         `json:"language"`
	Country        string         `json:"country"`
	Region         string         `json:"region"`
	City           string         `json:"city"`
	Browser        string         `json:"browser"`
	BrowserVersion string         `json:"browser_version"`
	OS             string         `json:"os"`
	OSVersion      string         `json:"os_version"`
	Device         string         `json:"device"`
	ScreenWidth    int            `json:"screen_width"`
	ScreenHeight   int            `json:"screen_height"`
	UTMSource      string         `json:"utm_source"`
	UTMMedium      string         `json:"utm_medium"`
	UTMCampaign    string         `json:"utm_campaign"`
	UTMTerm        string         `json:"utm_term"`
	UTMContent     string         `json:"utm_content"`
	Properties     map[string]any `json:"properties,omitempty"`
	// DistinctID is the hashed user identifier (per identify() SDK call).
	// Empty string means an anonymous event. The hashing happens in the
	// ingest handler before the event reaches this buffer; callers should
	// never put a raw user ID here.
	DistinctID string `json:"distinct_id,omitempty"`
	// ReleaseTag is the application release the SDK was initialized
	// with (git sha, semver, etc.). Empty means the SDK didn't supply
	// one. Used by the session rollup to stamp sessions.release_tag.
	ReleaseTag string `json:"release_tag,omitempty"`
}

// Buffer is a ring buffer that accumulates events and batch-inserts them
// into Nucleus on a time or size trigger. A DiskQueue (if attached) provides
// crash recovery: events pushed since the last successful flush are replayed
// after a restart.
type Buffer struct {
	mu               sync.Mutex
	events           []Event
	eventBytes       []int
	bufferedBytes    int64
	inFlightBytes    int64
	maxSize          int
	maxBufferedBytes int64
	flushSize        int
	flushInterval    time.Duration
	db               *nucleus.Client
	logger           *slog.Logger
	stopCh           chan struct{}
	// flushCh coalesces size-triggered flush wakeups: capacity one, so any
	// number of above-threshold Pushes while an insertion is in flight
	// collapse into a single pending wakeup instead of piling up goroutines
	// that all block on flushMu to perform the same drain.
	flushCh chan struct{}
	wg      sync.WaitGroup
	queue   *DiskQueue
	// Lifecycle state (AUD-016, round 2), all guarded by mu:
	// started latches on the first Start so a duplicate Start cannot race
	// wg.Add against Stop's wg.Wait; stopped rejects admissions after
	// shutdown; workerErr latches a dead flush goroutine so admission stops
	// acknowledging events nothing will ever flush.
	started   bool
	stopped   bool
	workerErr error
	// lastOffset is the WAL offset after the most recently appended event,
	// guarded by mu. It is the checkpoint target for the current batch: every
	// event in the buffer was WAL-appended at or below it, and no later event
	// exists yet, so checkpointing it covers exactly the flushed batch.
	lastOffset int64
	// flushMu serializes Flush so batches insert and checkpoint in WAL order;
	// the monotonic checkpoint clamp then can't advance past an un-inserted
	// earlier batch.
	flushMu sync.Mutex
	// stopOnce makes Stop idempotent (audit F10): a second Stop (lifecycle
	// hook plus a test defer, or concurrent shutdown paths) must not close
	// an already-closed channel.
	stopOnce sync.Once
	// O01 counters (ADR §5.10), atomics — the admission path must not take
	// a second lock for accounting. Accepted counts events given a 200
	// (both modes); DurablyAcked counts events whose group-commit fsync
	// completed before the ack (durable mode only — in lossy mode it stays
	// 0 and the gap is the mode's declared loss budget); ReplayedOnRestart
	// counts events recovered from the WAL at attach.
	accepted          atomic.Int64
	durablyAcked      atomic.Int64
	replayedOnRestart atomic.Int64
}

// ErrBufferFull is a capacity-class admission refusal: count cap, byte
// budget, or the buffer being stopped/shut down. Maps to HTTP 429 (today's
// consumer contract).
var ErrBufferFull = errors.New("ingest buffer full or closed")

// ErrDurabilityUnavailable is a durability-class admission refusal (O01
// §5.1/§5.2): the WAL latched a write/sync failure (F14), or the group
// commit for this batch failed. Maps to HTTP 503 — retryable, never a
// 200-over-undurable-bytes.
var ErrDurabilityUnavailable = errors.New("ingest durability unavailable")

// BufferStats is the events-signal counter block surfaced at /healthz
// (O01 §5.10). The high-water refusal and potentially-lost gauges live on
// the WAL's own stats block — one source of truth each.
type BufferStats struct {
	Accepted          int64 `json:"accepted"`
	DurablyAcked      int64 `json:"durably_acked"`
	ReplayedOnRestart int64 `json:"replayed_on_restart"`
}

// Stats returns a snapshot of the admission counters.
func (b *Buffer) Stats() BufferStats {
	return BufferStats{
		Accepted:          b.accepted.Load(),
		DurablyAcked:      b.durablyAcked.Load(),
		ReplayedOnRestart: b.replayedOnRestart.Load(),
	}
}

// defaultMaxBufferedBytes bounds the queued+in-flight serialized bytes
// (AUD-015, round 2). Count-based caps alone let many individually legal
// large events exhaust memory while the database stalls.
const defaultMaxBufferedBytes = 256 << 20

// WithMaxBufferedBytes overrides the serialized-byte budget (queued plus
// in-flight). Zero or negative restores the default.
func (b *Buffer) WithMaxBufferedBytes(n int64) *Buffer {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n > 0 {
		b.maxBufferedBytes = n
	}
	return b
}

// NewBuffer creates a new ingestion buffer. If queue is non-nil, every Push
// is also written to the WAL and any events surviving a crash are replayed
// into memory at construction time.
func NewBuffer(db *nucleus.Client, maxSize, flushSize int, flushInterval time.Duration, logger *slog.Logger) *Buffer {
	return &Buffer{
		events:           make([]Event, 0, flushSize),
		maxSize:          maxSize,
		maxBufferedBytes: defaultMaxBufferedBytes,
		flushSize:        flushSize,
		flushInterval:    flushInterval,
		db:               db,
		logger:           logger,
		stopCh:           make(chan struct{}),
		flushCh:          make(chan struct{}, 1),
	}
}

// AttachQueue enables WAL-backed durability. Must be called before Start;
// any surviving events from the previous process are recovered immediately.
//
// Audit F15: the queue is installed ONLY after recovery succeeds.
//
// TO-014: recovery is WRITE-THROUGH and bounded — each streamed frame
// chunk is committed to the database through insertBatch (whose per-chunk
// transaction already runs the committed-event-id dedup) before the next
// chunk is read, and the WAL is checkpointed once at the end. Peak replay
// memory is one chunk, not the backlog; a corrupt late frame fails the
// attach with NOTHING partially staged in the buffer (the events already
// committed durably stay committed — the checkpoint simply doesn't
// advance, and the remaining tail replays after the file is repaired).
// The in-memory events buffer is never touched here, so a failed attach
// cannot leave a half-attached prefix behind a memory-only fallback.
func (b *Buffer) AttachQueue(q *DiskQueue) error {
	const recoverChunk = 500
	var chunk []Event
	recovered, dropped := 0, 0
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	flushChunk := func() error {
		if len(chunk) == 0 {
			return nil
		}
		committed, err := b.insertBatch(ctx, chunk)
		if err != nil {
			return fmt.Errorf("WAL recovery commit failed (recovered-and-committed prefix: %d events, failing chunk starts at %d): %w", recovered, committed, err)
		}
		recovered += committed
		dropped += len(chunk) - committed
		chunk = chunk[:0]
		return nil
	}
	if err := q.StreamPending(func(events []Event) error {
		chunk = append(chunk, events...)
		if len(chunk) >= recoverChunk {
			return flushChunk()
		}
		return nil
	}); err != nil {
		return fmt.Errorf("WAL replay failed: %w", err)
	}
	if err := flushChunk(); err != nil {
		return fmt.Errorf("WAL replay failed: %w", err)
	}
	if dropped > 0 {
		b.logger.Info("ingest queue: skipped already-committed events on recovery", "dropped", dropped)
	}
	b.replayedOnRestart.Add(int64(recovered))

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.queue != nil {
		return fmt.Errorf("WAL already attached")
	}
	// Everything recovered is now durable in the database: advance the
	// checkpoint to the WAL end so the recovered region never replays
	// again, and seed the high-water mark for the next flush.
	if err := q.Checkpoint(q.Offset()); err != nil {
		return fmt.Errorf("WAL recovery checkpoint failed (events are committed; the backlog will replay again after a restart): %w", err)
	}
	b.lastOffset = q.Offset()
	if recovered > 0 {
		b.logger.Info("ingest queue: recovered events from WAL", "count", recovered)
	}
	b.queue = q
	return nil
}

// approxEventBytes returns the serialized size of one event for the byte
// budget. Marshal failures count as a floor so accounting never undercounts
// a storable event.
func approxEventBytes(e Event) int {
	raw, err := json.Marshal(e)
	if err != nil {
		return 256
	}
	return len(raw)
}

// eventKey is the durable-dedupe identity of one event (TO-018): the
// producer-assigned event id is only unique within its OWNER SITE — two
// sites choosing the same id are two events, and a cross-site collision
// must not make one of them vanish.
type eventKey struct {
	SiteID  string
	EventID string
}

// existingEventKeys returns which of the given (site, event_id) pairs are
// already stored in events, with a timestamp floor so the lookup stays
// bounded (see flushDedupeHorizon). Used by the flush-time dedup
// (filterUncommitted), which also covers WAL recovery (TO-014).
func existingEventKeys(ctx context.Context, sqlc *nucleus.SQLModel, events []Event, minTS int64, logger *slog.Logger) map[eventKey]struct{} {
	existing := make(map[eventKey]struct{}, len(events))
	type keyRow struct {
		SiteID  string `db:"site_id"`
		EventID string `db:"event_id"`
	}
	const chunk = 500
	for i := 0; i < len(events); i += chunk {
		end := i + chunk
		if end > len(events) {
			end = len(events)
		}
		batch := events[i:end]
		ph := make([]string, len(batch))
		args := make([]any, 0, len(batch)+1)
		args = append(args, dbutil.IntParam(minTS))
		for j, e := range batch {
			ph[j] = fmt.Sprintf("$%d", j+2)
			args = append(args, e.EventID)
		}
		q := fmt.Sprintf(
			"SELECT site_id, event_id FROM events WHERE timestamp >= $1 AND event_id IN (%s)",
			strings.Join(ph, ","))
		rows, err := nucleus.Query[keyRow](ctx, sqlc, q, args...)
		if err != nil {
			// Fail open: keep all pending (durability over dedup).
			if logger != nil {
				logger.Warn("ingest: event-id dedup lookup failed, keeping all candidates", "err", err)
			}
			return nil
		}
		for _, r := range rows {
			existing[eventKey{SiteID: r.SiteID, EventID: r.EventID}] = struct{}{}
		}
	}
	return existing
}

// Start begins the periodic flush loop. Idempotent-safe: a second Start is
// refused so it cannot race Stop's wg.Wait with a late wg.Add (AUD-016).
func (b *Buffer) Start() {
	b.mu.Lock()
	if b.started || b.stopped {
		b.mu.Unlock()
		return
	}
	b.started = true
	b.wg.Add(1) // before Stop can begin waiting
	b.mu.Unlock()
	go func() {
		defer b.wg.Done()
		defer func() {
			if r := recover(); r != nil {
				// AUD-016: latch the death instead of logging it away — a
				// panicked flush worker left an apparently running process
				// that acknowledged events nothing would ever flush.
				b.mu.Lock()
				b.workerErr = fmt.Errorf("buffer flush goroutine panicked: %v", r)
				b.mu.Unlock()
				b.logger.Error("buffer flush goroutine panicked — admission refused until restart", "err", r)
			}
		}()
		ticker := time.NewTicker(b.flushInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				b.Flush()
			case <-b.flushCh:
				// Audit F10: ONE bounded attempt per wakeup. The old inner
				// loop re-flushed while len(events) >= flushSize without
				// selecting on stopCh — a failing flush requeues its batch,
				// the threshold stayed true, and the worker spun in a tight
				// livelock no Stop could interrupt. A failed flush is now
				// retried by the next coalesced wakeup (a Push above
				// threshold re-signals) or by the ticker, which bounds the
				// retry rate at the configured flush interval.
				b.Flush()
			case <-b.stopCh:
				b.Flush() // final flush
				return
			}
		}
	}()
}

// Stop signals the flush loop to exit and waits for the final flush.
// Idempotent — safe under concurrent or repeated shutdown paths.
//
// The final Flush is SKIPPED when the worker died (workerErr latched,
// AUD-016): whatever killed it (in these tests a nil db panicking
// insertBatch) would panic the same call on the STOP CALLER's goroutine,
// which has no recover — a rare race where the worker died with events
// still buffered turned Stop into a process panic instead of a clean
// unhealthy-shutdown report.
func (b *Buffer) Stop() {
	b.stopOnce.Do(func() {
		b.mu.Lock()
		b.stopped = true
		close(b.stopCh)
		b.mu.Unlock()
		b.wg.Wait()
		if b.WorkerErr() == nil {
			b.Flush() // covers a buffer that was never Started
		}
		if b.queue != nil {
			if err := b.queue.Close(); err != nil {
				b.logger.Warn("ingest queue: close failed", "err", err)
			}
		}
	})
}

// WorkerErr reports a latched flush-worker failure (nil while healthy), for
// readiness surfacing (AUD-016).
func (b *Buffer) WorkerErr() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.workerErr
}

// Push adds an event to the buffer. Returns nil when admitted (and, with a
// WAL attached in durable mode, group-commit fsynced — the condition the
// HTTP handler turns into 200 OK, O01 §5.1); ErrBufferFull for
// capacity-class refusals (429); ErrDurabilityUnavailable when the WAL
// cannot back an ack (503 — audit F14 posture).
func (b *Buffer) Push(e Event) error {
	return b.PushBatch([]Event{e})
}

// PushBatch admits a WHOLE batch under one lock hold — the reservation the
// old Avail-then-Push loop only pretended to make (AUD-010, round 2). Two
// concurrent requests can no longer both pass a capacity snapshot and then
// interleave pushes that accept a prefix and refuse the tail: either every
// event in events is admitted (memory + ONE WAL frame) or none is.
//
// O01 §5.1 (durable mode): the WAL frame is appended under the buffer lock
// (batch atomicity), but the group-commit WAIT happens after releasing it —
// waiting under b.mu would serialize every request behind each 25 ms
// window instead of letting them share one. PushBatch returns nil only
// after the shared fsync covering this batch's frame completed; the ack is
// durable. In lossy mode (ADR §5.3) there is no wait — the admission is
// fast and the fsyncInterval backstop bounds the declared loss window.
func (b *Buffer) PushBatch(events []Event) error {
	if len(events) == 0 {
		return nil
	}
	b.mu.Lock()
	if b.stopped || b.workerErr != nil {
		b.mu.Unlock()
		return ErrBufferFull
	}
	if len(events) > b.maxSize-len(b.events) {
		b.mu.Unlock()
		return ErrBufferFull
	}
	// AUD-015: acquire the whole batch's serialized-byte credit (queued +
	// in-flight) before journaling it.
	var cost int64
	for _, e := range events {
		cost += int64(approxEventBytes(e))
	}
	if b.bufferedBytes+b.inFlightBytes+cost > b.maxBufferedBytes {
		b.mu.Unlock()
		return ErrBufferFull
	}
	var waitOff int64 = -1
	if b.queue != nil {
		off, err := b.queue.AppendBatch(events)
		if err != nil {
			// Nothing was appended to memory: the WAL is the durability
			// contract, and accepting the events anyway would ack data the
			// process promised is crash-safe but is not. The queue latches
			// the error (see DiskQueue.AppendBatch); admission stays refused
			// until it recovers, and /healthz reports the degradation.
			b.mu.Unlock()
			b.logger.Error("ingest queue: append failed — refusing admission (WAL-backed durability unavailable)", "err", err)
			return fmt.Errorf("%w: %w", ErrDurabilityUnavailable, err)
		}
		b.lastOffset = off
		if !b.queue.Lossy() {
			waitOff = off
		}
	}
	b.events = append(b.events, events...)
	for _, e := range events {
		b.eventBytes = append(b.eventBytes, approxEventBytes(e))
	}
	b.bufferedBytes += cost
	shouldFlush := len(b.events) >= b.flushSize
	b.mu.Unlock()

	if waitOff >= 0 {
		// O01 §5.1: the ack boundary. An error here means the fsync backing
		// this batch failed (or the queue closed first) — refuse so the
		// producer retries. The events stay admitted (memory + WAL buffer):
		// they will flush or replay, and the flush-time event-id dedupe
		// absorbs the retry — at-least-once, never a lossy 200.
		if err := b.queue.WaitCommit(waitOff); err != nil {
			b.logger.Error("ingest queue: group commit failed — refusing to acknowledge", "err", err)
			return fmt.Errorf("%w: %w", ErrDurabilityUnavailable, err)
		}
		b.durablyAcked.Add(int64(len(events)))
	}
	b.accepted.Add(int64(len(events)))

	if shouldFlush {
		// Non-blocking wakeup of the owned flush loop: one outstanding
		// signal is enough — the loop drains until below threshold.
		select {
		case b.flushCh <- struct{}{}:
		default:
		}
	}
	return nil
}

// Avail returns how many more events the buffer can accept before backpressure.
// BatchHandler uses it to admit a batch atomically (all-or-nothing) instead of
// accepting a prefix and then refusing the tail (audit F12).
func (b *Buffer) Avail() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.maxSize - len(b.events)
}

// Flush drains the buffer and batch-inserts into Nucleus. It is serialized by
// flushMu so batches are inserted and checkpointed in WAL order — without that,
// a later batch finishing first could checkpoint past an earlier, still
// un-inserted batch and drop it on crash.
func (b *Buffer) Flush() {
	b.flushMu.Lock()
	defer b.flushMu.Unlock()

	b.mu.Lock()
	if len(b.events) == 0 {
		b.mu.Unlock()
		return
	}
	batch := b.events
	batchBytes := b.eventBytes
	batchCost := b.bufferedBytes
	// AUD-015: detached batches stay on the byte budget as in-flight credit
	// until their commit outcome is known — released on success, returned to
	// the queued side on requeue.
	b.bufferedBytes = 0
	b.inFlightBytes += batchCost
	b.eventBytes = nil
	// Checkpoint target captured with the batch: every buffered event was
	// WAL-appended at or below lastOffset, so this offset covers exactly this
	// batch. New pushes after we release mu get a higher offset and a later batch.
	target := b.lastOffset
	b.events = make([]Event, 0, b.flushSize)
	b.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	b.logger.Info("flushing events", "count", len(batch))
	if committed, err := b.insertBatch(ctx, batch); err != nil {
		// Only the unsubmitted tail is re-queued; already-committed chunks are
		// dropped from the retry set so a later flush cannot double-insert them.
		unsent := batch[committed:]
		var unsentBytes int64
		for _, n := range batchBytes[committed:] {
			unsentBytes += int64(n)
		}
		b.logger.Error("flush failed", "committed", committed, "requeue", len(unsent), "err", err)
		// We deliberately do NOT checkpoint, so these events stay in the WAL and
		// a later successful flush checkpoints them via lastOffset.
		b.requeueFailed(unsent, unsentBytes, batchCost)
		return
	}
	b.mu.Lock()
	b.inFlightBytes -= batchCost
	if b.inFlightBytes < 0 {
		b.inFlightBytes = 0
	}
	b.mu.Unlock()
	b.logger.Info("flushed events OK", "count", len(batch))
	if b.queue != nil {
		if err := b.queue.Checkpoint(target); err != nil {
			b.logger.Warn("ingest queue: checkpoint failed", "err", err)
		}
	}
}

// requeueFailed puts a failed batch's uncommitted tail back at the FRONT of
// the buffer, untruncated. The old code appended only what fit below maxSize
// and silently dropped the rest under pressure — but the dropped events were
// already WAL-appended below lastOffset, so the next successful flush
// checkpointed PAST them and they were lost for good, restart included.
// Retaining them can temporarily push the buffer over maxSize; Push refuses
// new events until the retry drains it back down.
func (b *Buffer) requeueFailed(unsent []Event, unsentBytes, batchCost int64) {
	b.mu.Lock()
	b.events = append(unsent, b.events...)
	sizes := make([]int, len(unsent))
	for i := range unsent {
		sizes[i] = approxEventBytes(unsent[i])
	}
	b.eventBytes = append(sizes, b.eventBytes...)
	// In-flight credit for the whole detached batch returns to the queued
	// side: unsent becomes queued again, committed cost is released.
	b.inFlightBytes -= batchCost
	if b.inFlightBytes < 0 {
		b.inFlightBytes = 0
	}
	b.bufferedBytes += unsentBytes
	b.mu.Unlock()
}

// Len returns the current number of buffered events.
func (b *Buffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.events)
}

const (
	eventsCols       = 31 // keep in sync with eventsColList / eventRow args
	eventsRecentCols = 12 // keep in sync with eventsRecentColList / eventsRecentRow args
)

const eventsColList = `event_id, tenant_id, site_id, session_id, visit_id, event_type,
	timestamp, url, referrer, title, hostname, pathname,
	language, country, region, city,
	browser, browser_version, os, os_version, device,
	screen_width, screen_height,
	utm_source, utm_medium, utm_campaign, utm_term, utm_content,
	properties, distinct_id, release_tag`

const eventsRecentColList = `event_id, tenant_id, site_id, session_id, event_type,
	timestamp, pathname, referrer, browser, os, country, properties`

// buildPlaceholders returns "($1,$2,...),($N+1,...)..." for rows*cols placeholders.
func buildPlaceholders(rows, cols int) string {
	var b strings.Builder
	b.Grow(rows * cols * 5)
	n := 1
	for r := 0; r < rows; r++ {
		if r > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('(')
		for c := 0; c < cols; c++ {
			if c > 0 {
				b.WriteByte(',')
			}
			b.WriteByte('$')
			b.WriteString(fmt.Sprintf("%d", n))
			n++
		}
		b.WriteByte(')')
	}
	return b.String()
}

// propertiesJSON returns the event's properties as a JSON string. Returns "{}"
// (not "") for nil/empty properties — Nucleus rejects empty-string for JSONB
// columns and makes the row invisible to WHERE-clause queries.
func propertiesJSON(p map[string]any) string {
	if p == nil {
		return "{}"
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

// eventArgs appends the 31 parameter values for one event row (in column order).
func eventArgs(dst []any, e *Event) []any {
	return append(dst,
		e.EventID, e.TenantID, e.SiteID, e.SessionID, e.VisitID, e.EventType,
		dbutil.IntParam(e.Timestamp), e.URL, e.Referrer, e.Title, e.Hostname, e.Pathname,
		e.Language, e.Country, e.Region, e.City,
		e.Browser, e.BrowserVersion, e.OS, e.OSVersion, e.Device,
		dbutil.IntParam(int64(e.ScreenWidth)), dbutil.IntParam(int64(e.ScreenHeight)),
		e.UTMSource, e.UTMMedium, e.UTMCampaign, e.UTMTerm, e.UTMContent,
		propertiesJSON(e.Properties),
		e.DistinctID,
		e.ReleaseTag,
	)
}

// eventsRecentArgs appends the 12 parameter values for one events_recent row.
func eventsRecentArgs(dst []any, e *Event) []any {
	return append(dst,
		e.EventID, e.TenantID, e.SiteID, e.SessionID, e.EventType,
		dbutil.IntParam(e.Timestamp), e.Pathname, e.Referrer, e.Browser, e.OS, e.Country,
		propertiesJSON(e.Properties),
	)
}

// flushDedupeHorizon bounds how far back the flush-time event-id dedup
// searches for an already-committed original. A retried batch is re-prepared
// with fresh server timestamps, so the original copy it would duplicate is
// OLDER than the retry's own timestamps - the floor reaches down past them
// by this margin. A producer retrying the same batch more than a horizon
// later (and after an admission-cache miss) can still double-count; the
// SDKs' bounded retention queues make that window unreachable in practice.
const flushDedupeHorizon = 24 * time.Hour

// filterUncommitted drops chunk events whose event_id already exists in the
// events table (F12: the durable dedupe boundary for producer-stable event
// ids — and, since TO-014, the WAL-recovery dedup too: recovery commits
// through insertBatch, so already-committed survivors of a crash between DB
// commit and WAL checkpoint are dropped instead of double-counted). Runs
// inside the chunk's own transaction; Flush is serialized by flushMu, so
// within one process the existence check and the INSERT it guards cannot
// interleave with another flush of the same ids. Fails OPEN on a lookup
// error (a possible duplicate beats certain data loss).
func (b *Buffer) filterUncommitted(ctx context.Context, sqlc *nucleus.SQLModel, chunk []Event) []Event {
	minTS := chunk[0].Timestamp
	for _, e := range chunk {
		if e.Timestamp < minTS {
			minTS = e.Timestamp
		}
	}
	existing := existingEventKeys(ctx, sqlc, chunk, minTS-flushDedupeHorizon.Milliseconds(), b.logger)
	if len(existing) == 0 {
		return chunk
	}
	out := chunk[:0]
	for _, e := range chunk {
		if _, dup := existing[eventKey{SiteID: e.SiteID, EventID: e.EventID}]; !dup {
			out = append(out, e)
		}
	}
	return out
}

// insertBatch inserts events in chunks. Each chunk's events + events_recent
// inserts run inside a single transaction so a failure of the second can never
// leave the first committed (which previously let events_recent diverge from
// events). It returns the number of events successfully committed so the caller
// re-queues only the unsubmitted tail rather than the whole batch — that whole-
// batch re-queue was the main way a flush retry double-counted already-inserted
// events.
//
// F12: each chunk is first filtered against already-committed event ids, so a
// client retry that slipped past the admission cache (restart, TTL, concurrent
// submit) is dropped here instead of double-counted. `committed` still counts
// the chunk's whole span once its transaction commits: dropped duplicates were
// acknowledged to their producer by the FIRST successful insert of those ids.
func (b *Buffer) insertBatch(ctx context.Context, batch []Event) (committed int, err error) {
	// Chunk size chosen so each statement stays well under protocol limits
	// even for wide rows (29 cols * 50 rows = 1450 placeholders).
	const batchSize = 50
	for start := 0; start < len(batch); start += batchSize {
		end := start + batchSize
		if end > len(batch) {
			end = len(batch)
		}
		chunk := batch[start:end]

		tx, txErr := b.db.Begin(ctx)
		if txErr != nil {
			return committed, fmt.Errorf("batch begin tx %d-%d: %w", start+1, end, txErr)
		}
		txSQL := tx.SQL()

		chunk = b.filterUncommitted(ctx, txSQL, chunk)
		if len(chunk) == 0 {
			// Every event in this span is already stored; committing the
			// empty transaction is fine (and keeps rollback handling
			// uniform), the span still counts as processed.
			_ = tx.Commit(ctx)
			committed = end
			continue
		}

		eventsQuery := "INSERT INTO events (" + eventsColList + ") VALUES " +
			buildPlaceholders(len(chunk), eventsCols)
		recentQuery := "INSERT INTO events_recent (" + eventsRecentColList + ") VALUES " +
			buildPlaceholders(len(chunk), eventsRecentCols)

		eventsArgs := make([]any, 0, len(chunk)*eventsCols)
		recentArgs := make([]any, 0, len(chunk)*eventsRecentCols)
		for i := range chunk {
			eventsArgs = eventArgs(eventsArgs, &chunk[i])
			recentArgs = eventsRecentArgs(recentArgs, &chunk[i])
		}

		if _, e := txSQL.Exec(ctx, eventsQuery, eventsArgs...); e != nil {
			_ = tx.Rollback(ctx)
			return committed, fmt.Errorf("batch insert events %d-%d: %w", start+1, end, e)
		}
		if _, e := txSQL.Exec(ctx, recentQuery, recentArgs...); e != nil {
			_ = tx.Rollback(ctx)
			return committed, fmt.Errorf("batch insert recent %d-%d: %w", start+1, end, e)
		}
		if e := tx.Commit(ctx); e != nil {
			return committed, fmt.Errorf("batch commit %d-%d: %w", start+1, end, e)
		}
		committed = end
	}
	return committed, nil
}
