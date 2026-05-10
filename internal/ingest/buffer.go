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
	if len(pending) == 0 {
		return nil
	}
	b.mu.Lock()
	b.events = append(b.events, pending...)
	b.mu.Unlock()
	b.logger.Info("ingest queue: replayed", "count", len(pending))
	return nil
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
	shouldFlush := len(b.events) >= b.flushSize
	b.mu.Unlock()

	// WAL before signaling flush. Failure is logged, not fatal, so a disk
	// problem never drops an event from the in-memory path.
	if b.queue != nil {
		if err := b.queue.Append(e); err != nil {
			b.logger.Warn("ingest queue: append failed", "err", err)
		}
	}

	if shouldFlush {
		go b.Flush()
	}
	return true
}

// Flush drains the buffer and batch-inserts into Nucleus.
func (b *Buffer) Flush() {
	b.mu.Lock()
	if len(b.events) == 0 {
		b.mu.Unlock()
		return
	}
	batch := b.events
	b.events = make([]Event, 0, b.flushSize)
	b.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	b.logger.Info("flushing events", "count", len(batch))
	if err := b.insertBatch(ctx, batch); err != nil {
		b.logger.Error("flush failed", "count", len(batch), "err", err)
		// Re-queue events that failed to insert (best effort, may drop under pressure)
		b.mu.Lock()
		remaining := b.maxSize - len(b.events)
		if remaining > 0 {
			if len(batch) > remaining {
				batch = batch[:remaining]
			}
			b.events = append(b.events, batch...)
		}
		b.mu.Unlock()
		return
	}
	b.logger.Info("flushed events OK", "count", len(batch))
	if b.queue != nil {
		if err := b.queue.Checkpoint(); err != nil {
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

func (b *Buffer) insertBatch(ctx context.Context, batch []Event) error {
	sql := b.db.SQL()

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

		if _, err := sql.Exec(ctx, eventsQuery, eventsArgs...); err != nil {
			return fmt.Errorf("batch insert events %d-%d: %w", start+1, end, err)
		}
		if _, err := sql.Exec(ctx, recentQuery, recentArgs...); err != nil {
			return fmt.Errorf("batch insert recent %d-%d: %w", start+1, end, err)
		}
	}
	return nil
}
