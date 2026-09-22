package errors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/useteploy/teploy-observe/internal/ingest"
)

// ErrorBuffer accumulates error events and batch-processes them
// asynchronously, decoupling the HTTP response from issue resolution and
// storage.
//
// O01 implementation slice 2 (ADR §5.6, §5.4) — the durable error path:
// with a WAL attached (durable mode, the default), Push appends the
// record's frame to the errors WAL and the 200-equivalent admission
// returns only after the group-commit fsync covering it — the same
// discipline the events buffer gained in slice 1, on the same
// generalized DiskQueue machinery (a second instance, not a second WAL
// implementation). OBSERVE_WAL_LOSSY=true opts the errors queue into the
// same declared loss budget as events.
//
// Flush dispositions (§5.4 — no silent drop):
//   - applied / deduped / conflict-id / quarantined: final. The WAL
//     checkpoint advances past them.
//   - storage failure: the record is requeued at the head as PENDING and
//     retried on the next flush tick — never dropped. The checkpoint
//     stops at the first pending gap so a crash re-replays exactly the
//     unapplied records; the flush-time inbox dedupe absorbs anything the
//     database already committed.
//   - poison (undecodable after admission): diverted to a bounded
//     quarantine spool beside the WAL, counted, and skipped without
//     blocking the stream.
//
// History: F11 (single owned flush worker), R13 (count+byte budgets,
// frozen snapshots, reservations retired only on final disposition),
// R15 (worker panic latches and closes admission) — all preserved.
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
	// O01 slice 2 state, guarded by mu:
	queue     *ingest.DiskQueue // nil = memory-only (attach failed; healthz shows it)
	lastOff   int64             // WAL offset after the most recent append
	flushFail bool              // the last flush left PENDING records (healthz degraded)
	// adm is the in-process admission cache (request-time dedupe/conflict
	// fast path; see inbox.go). Nil-safe: only used for identified records.
	adm *admissionCache
	// quarantineDir hosts the poison spool when a queue is attached.
	quarantineDir string
	// O01 §5.10 counters (atomics — the admission path must not take a
	// second lock for accounting).
	accepted          atomic.Int64
	durablyAcked      atomic.Int64
	applied           atomic.Int64
	deduped           atomic.Int64
	conflicting       atomic.Int64
	quarantined       atomic.Int64
	replayedOnRestart atomic.Int64
}

type bufferedError struct {
	// Body is the serialized ErrorInput captured at admission (R13): the
	// record is immutable from the moment Push returns. Cost stays counted
	// in usedBytes until the record's final disposition, so a detached
	// in-flight batch — or a PENDING requeue — keeps its reservation.
	Body       []byte
	Site       string
	Cost       int64
	ProducerID string
	EventID    string
	Digest     string
	// Offset is the WAL position after this record's frame (0 without a
	// queue). The flush checkpoint target: advancing past it is only
	// correct once the record reaches a FINAL disposition.
	Offset int64
}

// DefaultErrorBufferBytes is the retained-memory budget for queued plus
// in-flight error records.
const DefaultErrorBufferBytes = 64 << 20

// maxErrorRecordBytes caps one serialized error record at admission. Larger
// inputs are rejected (429) rather than buffered.
const maxErrorRecordBytes = 256 << 10

// ErrErrorBufferFull is the capacity-class admission refusal (429).
var ErrErrorBufferFull = errors.New("error buffer full or closed")

// admissionCacheTTL/capacity mirror the events BatchDeduper constants: the
// cache is the fast path only; the durable arbiter is the error_inbox
// ledger at flush.
const (
	admissionCacheTTL      = 10 * time.Minute
	admissionCacheCapacity = 16384
)

// maxQuarantineBytes bounds the poison spool (§5.9's per-signal quarantine
// budget; the fuller WAL-fence + K-attempt diversion design is slice 4).
const maxQuarantineBytes = 64 << 20

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
		adm:           newAdmissionCache(admissionCacheTTL, admissionCacheCapacity),
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

// AttachQueue enables the WAL-backed durable error path (O01 §5.6) and
// replays the previous process's uncheckpointed records WRITE-THROUGH, the
// errors twin of the events Buffer.AttachQueue discipline (TO-014):
// pending frames are applied through ApplyInbox in bounded chunks before
// the queue is installed, so replay memory is one frame and a corrupt
// frame fails the attach with nothing partially staged. Records whose
// apply fails during replay (storage still down at boot) are re-enqueued
// as PENDING — their frames stay uncheckpointed, so a later crash
// re-replays them and the inbox dedupe guards the committed ones.
func (b *ErrorBuffer) AttachQueue(q *ingest.DiskQueue) error {
	b.mu.Lock()
	if b.queue != nil {
		b.mu.Unlock()
		return fmt.Errorf("errors WAL already attached")
	}
	b.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	var pending []bufferedError
	var pendingCost int64
	recovered, failed := 0, 0
	target := int64(-1)
	gapOpen := false
	err := q.StreamRecordFrames(func(recs []json.RawMessage, endOffset int64) error {
		for _, raw := range recs {
			var rec errorRecord
			if uerr := json.Unmarshal(raw, &rec); uerr != nil || len(rec.Body) == 0 || rec.SiteID == "" {
				// Frame decodes, envelope does not: poison by inspection —
				// quarantine and keep replaying.
				b.quarantineRaw(raw, fmt.Errorf("replay: undecodable error envelope: %v", uerr))
				if !gapOpen {
					target = endOffset
				}
				recovered++
				continue
			}
			ev := bufferedError{
				Body:       rec.Body,
				Site:       rec.SiteID,
				Cost:       int64(len(rec.Body) + len(rec.SiteID) + 128),
				ProducerID: rec.ProducerID,
				EventID:    rec.EventID,
				Digest:     rec.Digest,
				Offset:     endOffset,
			}
			if b.applyOne(ctx, ev) {
				// Final disposition (applied/deduped/conflict/quarantined).
				if !gapOpen {
					target = endOffset
				}
				recovered++
			} else {
				// Storage unavailable at boot: PENDING, checkpoint stops
				// here, the record keeps its WAL frame.
				gapOpen = true
				failed++
				pending = append(pending, ev)
				pendingCost += ev.Cost
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("errors WAL replay failed: %w", err)
	}
	if failed > 0 {
		b.logger.Warn("errors WAL replay: storage unavailable for some records — retained as PENDING", "pending", failed)
	}

	b.mu.Lock()
	if b.queue != nil {
		b.mu.Unlock()
		return fmt.Errorf("errors WAL already attached")
	}
	b.events = append(pending, b.events...)
	b.usedBytes += pendingCost
	b.quarantineDir = q.Dir()
	// Everything final is durable in the database; advance the checkpoint
	// past it so it never replays again. A gap leaves the tail alone.
	if target >= 0 {
		if cerr := q.Checkpoint(target); cerr != nil {
			b.mu.Unlock()
			return fmt.Errorf("errors WAL recovery checkpoint failed (final records are committed and will replay-dedupe after a restart): %w", cerr)
		}
	}
	b.lastOff = q.Offset()
	b.queue = q
	b.mu.Unlock()
	b.replayedOnRestart.Add(int64(recovered))
	if recovered > 0 {
		b.logger.Info("errors WAL: recovered records on restart", "count", recovered)
	}
	return nil
}

// Stop closes admission, wakes the worker, and waits for the final flush.
// Idempotent; safe to call concurrently with Push. Closes the attached
// queue after the drain (the buffer owns it from AttachQueue on).
func (b *ErrorBuffer) Stop() {
	b.stopOnce.Do(func() {
		b.mu.Lock()
		b.closing = true
		b.mu.Unlock()
		close(b.stopCh)
	})
	b.wg.Wait()
	if b.WorkerErr() == nil {
		b.Flush() // covers a buffer that was never Started
	}
	b.mu.Lock()
	q := b.queue
	b.mu.Unlock()
	if q != nil {
		if err := q.Close(); err != nil {
			b.logger.Warn("errors WAL close failed", "err", err)
		}
	}
}

// Push admits an error record. Nil return = admitted — and, with a WAL in
// durable mode, group-commit fsynced (the condition errorIngestHandler
// turns into 200 OK, O01 §5.1/§5.6). Refusals:
//   - ErrErrorBufferFull: capacity class (429 — unchanged contract)
//   - ingest.ErrDurabilityUnavailable (wrapped): the WAL cannot back an
//     ack (503 + Retry-After via the ingest group middleware)
//   - ErrAdmittedDuplicate: retry of an admission this process already
//     made (200 {ok, deduped:true})
//   - ErrEventIDConflict: same identity, different payload (409)
//   - ErrInvalidEventID: malformed producer identity (400)
//
// A durability refusal leaves the record admitted (memory + WAL buffer):
// it will flush or replay, and the inbox dedupe absorbs the producer's
// retry — at-least-once, never a lossy 200.
func (b *ErrorBuffer) Push(siteID string, input ErrorInput) error {
	input.SiteID = siteID
	if err := ValidateEventIdentity(input.EventID, input.ProducerID); err != nil {
		return err
	}
	raw, err := json.Marshal(input)
	if err != nil || len(raw) > maxErrorRecordBytes {
		return ErrErrorBufferFull
	}
	digest := digestBytes(raw)
	if input.EventID != "" {
		duplicate, conflict := b.adm.lookup(siteID, input.ProducerID, input.EventID, digest)
		if duplicate {
			b.deduped.Add(1)
			b.accepted.Add(1)
			return ErrAdmittedDuplicate
		}
		if conflict {
			b.conflicting.Add(1)
			return fmt.Errorf("%w (site %s event %s)", ErrEventIDConflict, siteID, input.EventID)
		}
	}
	// Payload + site key + envelope allowance.
	cost := int64(len(raw) + len(siteID) + 128)

	rec := errorRecord{SiteID: siteID, ProducerID: input.ProducerID, EventID: input.EventID, Digest: digest, Body: raw}
	frame, err := json.Marshal(rec)
	if err != nil {
		return ErrErrorBufferFull
	}

	b.mu.Lock()
	if b.closing || len(b.events) >= b.maxSize || cost > b.maxBytes-b.usedBytes {
		b.mu.Unlock()
		return ErrErrorBufferFull
	}
	var waitOff int64 = -1
	off := int64(0)
	if b.queue != nil {
		off, err = b.queue.AppendRecords([]json.RawMessage{frame})
		if err != nil {
			b.mu.Unlock()
			b.logger.Error("errors WAL append failed — refusing admission (durable error path unavailable)", "err", err)
			return fmt.Errorf("%w: %w", ingest.ErrDurabilityUnavailable, err)
		}
		b.lastOff = off
		if !b.queue.Lossy() {
			waitOff = off
		}
	}
	b.events = append(b.events, bufferedError{Body: raw, Site: siteID, Cost: cost,
		ProducerID: input.ProducerID, EventID: input.EventID, Digest: digest, Offset: off})
	b.usedBytes += cost
	full := len(b.events) >= b.flushSize
	b.mu.Unlock()

	if waitOff >= 0 {
		// O01 §5.1: the durable-ack boundary. An error here means the
		// fsync backing this record failed — refuse so the producer
		// retries; the record stays admitted and the inbox dedupe makes
		// the retry safe.
		if err := b.queue.WaitCommit(waitOff); err != nil {
			b.logger.Error("errors WAL group commit failed — refusing to acknowledge", "err", err)
			return fmt.Errorf("%w: %w", ingest.ErrDurabilityUnavailable, err)
		}
		b.durablyAcked.Add(1)
	}
	b.accepted.Add(1)
	if input.EventID != "" {
		// Recorded only after successful admission: a refused first
		// attempt stays retryable.
		b.adm.record(siteID, input.ProducerID, input.EventID, digest)
	}

	if full {
		select {
		case b.wake <- struct{}{}:
		default:
		}
	}
	return nil
}

// release retires a record's reservation on its FINAL disposition (R13):
// applied, deduped, conflicting, or quarantined — never a PENDING requeue.
func (b *ErrorBuffer) release(ev bufferedError) {
	b.mu.Lock()
	b.usedBytes -= ev.Cost
	b.mu.Unlock()
}

// applyOne gives one record its disposition. Returns true when FINAL
// (applied / deduped / conflict / quarantined — the checkpoint may advance
// past it), false when PENDING (transient failure; requeue, never drop).
func (b *ErrorBuffer) applyOne(ctx context.Context, ev bufferedError) bool {
	var input ErrorInput
	if err := json.Unmarshal(ev.Body, &input); err != nil {
		// A record that no longer decodes cannot ever succeed: quarantine
		// (counted, spooled) and let the stream continue — O01 §5.4/§5.9.
		b.quarantine(ev, fmt.Errorf("record undecodable after admission: %w", err))
		return true
	}
	actx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	outcome, _, err := b.handler.ApplyInbox(actx, input, ev.ProducerID, ev.EventID, ev.Digest)
	if err != nil {
		// Transient by assumption (storage unavailable, timeout): PENDING.
		// K-attempt SQL-layer diversion is slice 4 (ADR §5.9).
		b.logger.Error("error flush failed", "err", err)
		return false
	}
	switch outcome {
	case InboxApplied:
		b.applied.Add(1)
	case InboxDeduped:
		b.deduped.Add(1)
	case InboxConflict:
		// A conflicting reuse that slipped past the admission cache
		// (restart, TTL): counted, never applied, never merged.
		b.conflicting.Add(1)
		b.logger.Warn("errors: conflicting event_id at flush — record counted and skipped",
			"site", ev.Site, "event_id", ev.EventID)
	}
	return true
}

// Flush processes all buffered errors on the caller's goroutine. The
// worker is the only routine that calls it in normal operation (audit
// F11). Per-record bounded contexts (R14); failures requeue as PENDING
// (O01 §5.4 — never dropped); the WAL checkpoint advances only past the
// contiguous final prefix, so a crash re-replays exactly the records the
// database never committed (their retries then hit the inbox dedupe).
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
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()

	var pending []bufferedError
	target := int64(-1)
	gapOpen := false
	final := 0
	for _, ev := range batch {
		if b.applyOne(ctx, ev) {
			final++
			b.release(ev)
			if !gapOpen {
				// The checkpoint may cover this record's frame.
				target = ev.Offset
			}
			continue
		}
		gapOpen = true
		pending = append(pending, ev)
	}

	b.mu.Lock()
	if len(pending) > 0 {
		// PENDING: requeue at the head, reservations retained (R13),
		// retried by the next tick — never dropped.
		b.events = append(pending, b.events...)
		b.flushFail = true
	} else {
		b.flushFail = false
	}
	q := b.queue
	b.mu.Unlock()

	if len(pending) > 0 {
		b.logger.Error("error flush left records PENDING — retained and retried, never dropped", "pending", len(pending), "final", final)
	} else {
		b.logger.Info("flushed errors", "final", final, "total", len(batch))
	}
	if q != nil && target >= 0 {
		if err := q.Checkpoint(target); err != nil {
			b.logger.Warn("errors WAL checkpoint failed", "err", err)
		}
	}
}

// quarantine diverts a poison record (permanently unapplicable by
// inspection — undecodable after admission) to the bounded spool beside
// the WAL and counts it. Quarantine is visible through counters, never a
// silent 200-as-applied (ADR §5.9).
func (b *ErrorBuffer) quarantine(ev bufferedError, reason error) {
	b.quarantined.Add(1)
	b.logger.Error("errors: record quarantined (permanently unapplicable)", "site", ev.Site, "event_id", ev.EventID, "err", reason)
	b.quarantineRaw(ev.Body, reason)
}

func (b *ErrorBuffer) quarantineRaw(body []byte, reason error) {
	dir := b.quarantineDir
	if dir == "" {
		return
	}
	entry := struct {
		TS     string `json:"ts"`
		Reason string `json:"reason"`
		Record []byte `json:"record"`
	}{TS: time.Now().UTC().Format(time.RFC3339), Reason: reason.Error(), Record: body}
	line, err := json.Marshal(entry)
	if err != nil {
		return
	}
	path := filepath.Join(dir, "quarantine.log")
	if info, err := os.Stat(path); err == nil && info.Size()+int64(len(line))+1 > maxQuarantineBytes {
		b.logger.Error("errors: quarantine spool full — record counted but not spooled", "path", path)
		return
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		b.logger.Error("errors: quarantine spool write failed", "err", err)
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}

// ErrorBufferStats is the errors counter block at /healthz (O01 §5.10 —
// independent counts). Accepted = 200s given (deduped re-acks included);
// the balance is applied + pending + quarantined + deduped-flush +
// conflicting (a legacy drop-on-flush-failure disposition no longer
// exists — its counter, dropped_post_ack, would be identically zero and
// is deliberately not carried).
type ErrorBufferStats struct {
	Accepted          int64 `json:"accepted"`
	DurablyAcked      int64 `json:"durably_acked"`
	Applied           int64 `json:"applied"`
	Deduped           int64 `json:"deduped"`
	Quarantined       int64 `json:"quarantined"`
	ConflictingID     int64 `json:"conflicting_id"`
	ReplayedOnRestart int64 `json:"replayed_on_restart"`
	Pending           int   `json:"pending"`
	FlushFailing      bool  `json:"flush_failing"`
	// Queued/Bytes keep the pre-O01 operator field names (R15 backlog
	// visibility) — queued is the same number as pending.
	Queued int   `json:"queued"`
	Bytes  int64 `json:"bytes"`
}

// Stats reports the errors counter block (operator surface).
func (b *ErrorBuffer) Stats() ErrorBufferStats {
	b.mu.Lock()
	defer b.mu.Unlock()
	return ErrorBufferStats{
		Accepted:          b.accepted.Load(),
		DurablyAcked:      b.durablyAcked.Load(),
		Applied:           b.applied.Load(),
		Deduped:           b.deduped.Load(),
		Quarantined:       b.quarantined.Load(),
		ConflictingID:     b.conflicting.Load(),
		ReplayedOnRestart: b.replayedOnRestart.Load(),
		Pending:           len(b.events),
		FlushFailing:      b.flushFail,
		Queued:            len(b.events),
		Bytes:             b.usedBytes,
	}
}
