package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"
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
	mu            sync.Mutex
	events        []Event
	maxSize       int
	flushSize     int
	flushInterval time.Duration
	db            *nucleus.Client
	logger        *slog.Logger
	stopCh        chan struct{}
	wg            sync.WaitGroup
	queue         *DiskQueue
	// lastOffset is the WAL offset after the most recently appended event,
	// guarded by mu. It is the checkpoint target for the current batch: every
	// event in the buffer was WAL-appended at or below it, and no later event
	// exists yet, so checkpointing it covers exactly the flushed batch.
	lastOffset int64
	// flushMu serializes Flush so batches insert and checkpoint in WAL order;
	// the monotonic checkpoint clamp then can't advance past an un-inserted
	// earlier batch.
	flushMu sync.Mutex
}

// NewBuffer creates a new ingestion buffer. If queue is non-nil, every Push
// is also written to the WAL and any events surviving a crash are replayed
// into memory at construction time.
func NewBuffer(db *nucleus.Client, maxSize, flushSize int, flushInterval time.Duration, logger *slog.Logger) *Buffer {
	return &Buffer{
		events:        make([]Event, 0, flushSize),
		maxSize:       maxSize,
		flushSize:     flushSize,
		flushInterval: flushInterval,
		db:            db,
		logger:        logger,
		stopCh:        make(chan struct{}),
	}
}

// AttachQueue enables WAL-backed durability. Must be called before Start;
// any surviving events from the previous process are replayed immediately.
func (b *Buffer) AttachQueue(q *DiskQueue) error {
	b.queue = q
	pending, err := q.Pending()
	if err != nil {
		return err
	}
	// Exactly-once: a crash between a flush's DB commit and its WAL checkpoint
	// leaves committed events still in the WAL, so replay would re-insert (and
	// double-count) them. Drop any pending event whose event_id is already in
	// the DB before replaying. This is a one-time, startup-only cost on the rare
	// post-crash path; the hot ingest path is untouched.
	if len(pending) > 0 {
		before := len(pending)
		pending = b.dropAlreadyCommitted(pending)
		if dropped := before - len(pending); dropped > 0 {
			b.logger.Info("ingest queue: skipped already-committed events on replay", "dropped", dropped)
		}
	}

	b.mu.Lock()
	// Seed the high-water mark to the WAL end so the first flush after replay
	// checkpoints past the replayed region; otherwise those events (already on
	// disk below the new writes) would replay again on the next crash.
	b.lastOffset = q.Offset()
	if len(pending) > 0 {
		b.events = append(b.events, pending...)
	}
	b.mu.Unlock()
	if len(pending) > 0 {
		b.logger.Info("ingest queue: replayed", "count", len(pending))
	}
	return nil
}

// dropAlreadyCommitted returns the subset of events whose event_id is NOT
// already present in the events table — so a WAL replay of committed-but-not-
// checkpointed events doesn't double-count them. Bounded by a timestamp floor
// so the lookup uses the (…, timestamp, …) sort-key prefix instead of a full
// scan. On any error it FAILS OPEN (keeps all events) — preserving durability
// at the cost of a possible duplicate, which is the pre-existing behavior.
func (b *Buffer) dropAlreadyCommitted(pending []Event) []Event {
	if len(pending) == 0 {
		return pending
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Collect ids and the earliest timestamp to bound the scan.
	ids := make([]string, 0, len(pending))
	minTS := pending[0].Timestamp
	for _, e := range pending {
		ids = append(ids, e.EventID)
		if e.Timestamp < minTS {
			minTS = e.Timestamp
		}
	}

	existing := make(map[string]struct{}, len(ids))
	type idRow struct {
		EventID string `db:"event_id"`
	}
	const chunk = 500
	for i := 0; i < len(ids); i += chunk {
		end := i + chunk
		if end > len(ids) {
			end = len(ids)
		}
		batch := ids[i:end]
		ph := make([]string, len(batch))
		args := make([]any, 0, len(batch)+1)
		args = append(args, dbutil.IntParam(minTS))
		for j, id := range batch {
			ph[j] = fmt.Sprintf("$%d", j+2)
			args = append(args, id)
		}
		q := fmt.Sprintf(
			"SELECT event_id FROM events WHERE timestamp >= $1 AND event_id IN (%s)",
			strings.Join(ph, ","))
		rows, err := nucleus.Query[idRow](ctx, b.db.SQL(), q, args...)
		if err != nil {
			// Fail open: keep all pending (durability over dedup).
			b.logger.Warn("ingest queue: replay dedup lookup failed, keeping all pending", "err", err)
			return pending
		}
		for _, r := range rows {
			existing[r.EventID] = struct{}{}
		}
	}
	if len(existing) == 0 {
		return pending
	}
	out := pending[:0]
	for _, e := range pending {
		if _, dup := existing[e.EventID]; !dup {
			out = append(out, e)
		}
	}
	return out
}

// Start begins the periodic flush loop.
func (b *Buffer) Start() {
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		defer func() {
			if r := recover(); r != nil {
				b.logger.Error("buffer flush goroutine panicked", "err", r)
			}
		}()
		ticker := time.NewTicker(b.flushInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				b.Flush()
			case <-b.stopCh:
				b.Flush() // final flush
				return
			}
		}
	}()
}

// Stop signals the flush loop to exit and waits for the final flush.
func (b *Buffer) Stop() {
	close(b.stopCh)
	b.wg.Wait()
	if b.queue != nil {
		if err := b.queue.Close(); err != nil {
			b.logger.Warn("ingest queue: close failed", "err", err)
		}
	}
}

// Push adds an event to the buffer. Returns false if the buffer is full
// (backpressure signal).
func (b *Buffer) Push(e Event) bool {
	b.mu.Lock()
	if len(b.events) >= b.maxSize {
		b.mu.Unlock()
		return false
	}
	b.events = append(b.events, e)
	// WAL under mu so the on-disk order matches b.events order; lastOffset then
	// tracks the offset of the final buffered event. A failed append is logged,
	// not fatal — the in-memory path still flushes it, it just isn't crash-safe.
	// The bufio write is in-memory (fsync is on the background loop), so holding
	// mu here does not block on disk I/O.
	if b.queue != nil {
		if off, err := b.queue.Append(e); err != nil {
			b.logger.Warn("ingest queue: append failed", "err", err)
		} else {
			b.lastOffset = off
		}
	}
	shouldFlush := len(b.events) >= b.flushSize
	b.mu.Unlock()

	if shouldFlush {
		go b.Flush()
	}
	return true
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
		b.logger.Error("flush failed", "committed", committed, "requeue", len(unsent), "err", err)
		// We deliberately do NOT checkpoint, so these events stay in the WAL and
		// a later successful flush checkpoints them via lastOffset.
		b.mu.Lock()
		remaining := b.maxSize - len(b.events)
		if remaining > 0 {
			if len(unsent) > remaining {
				unsent = unsent[:remaining]
			}
			b.events = append(b.events, unsent...)
		}
		b.mu.Unlock()
		return
	}
	b.logger.Info("flushed events OK", "count", len(batch))
	if b.queue != nil {
		if err := b.queue.Checkpoint(target); err != nil {
			b.logger.Warn("ingest queue: checkpoint failed", "err", err)
		}
	}
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

// propertiesJSON returns the event's properties as a JSON string, or "" if
// there are none. Empty-string is stored as an empty JSON object by the caller.
func propertiesJSON(p map[string]any) string {
	if p == nil {
		return ""
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return ""
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

// insertBatch inserts events in chunks. Each chunk's events + events_recent
// inserts run inside a single transaction so a failure of the second can never
// leave the first committed (which previously let events_recent diverge from
// events). It returns the number of events successfully committed so the caller
// re-queues only the unsubmitted tail rather than the whole batch — that whole-
// batch re-queue was the main way a flush retry double-counted already-inserted
// events.
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

		tx, txErr := b.db.Begin(ctx)
		if txErr != nil {
			return committed, fmt.Errorf("batch begin tx %d-%d: %w", start+1, end, txErr)
		}
		txSQL := tx.SQL()
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
