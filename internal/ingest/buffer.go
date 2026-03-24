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
}

// Buffer is a ring buffer that accumulates events and batch-inserts them
// into Nucleus on a time or size trigger.
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
}

// NewBuffer creates a new ingestion buffer.
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
}

// Len returns the current number of buffered events.
func (b *Buffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.events)
}

const insertSQL = `INSERT INTO events (
	event_id, tenant_id, site_id, session_id, visit_id, event_type,
	timestamp, url, referrer, title, hostname, pathname,
	language, country, region, city,
	browser, browser_version, os, os_version, device,
	screen_width, screen_height,
	utm_source, utm_medium, utm_campaign, utm_term, utm_content,
	properties
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29)`

const insertRecentSQL = `INSERT INTO events_recent (
	event_id, tenant_id, site_id, session_id, event_type,
	timestamp, pathname, referrer, browser, os, country, properties
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`

func (b *Buffer) insertBatch(ctx context.Context, batch []Event) error {
	sql := b.db.SQL()

	// Build multi-row INSERT for events table
	// Nucleus supports multi-row VALUES via SimpleProtocol
	const batchSize = 50 // rows per INSERT statement
	for start := 0; start < len(batch); start += batchSize {
		end := start + batchSize
		if end > len(batch) {
			end = len(batch)
		}
		chunk := batch[start:end]

		// Build events INSERT
		eventsQuery := `INSERT INTO events (
			event_id, tenant_id, site_id, session_id, visit_id, event_type,
			timestamp, url, referrer, title, hostname, pathname,
			language, country, region, city,
			browser, browser_version, os, os_version, device,
			screen_width, screen_height,
			utm_source, utm_medium, utm_campaign, utm_term, utm_content,
			properties) VALUES `

		recentQuery := `INSERT INTO events_recent (
			event_id, tenant_id, site_id, session_id, event_type,
			timestamp, pathname, referrer, browser, os, country, properties) VALUES `

		var eventsValues, recentValues []string
		for _, e := range chunk {
			propsJSON := "''"
			if e.Properties != nil {
				if raw, err := json.Marshal(e.Properties); err == nil {
					propsJSON = "'" + escapeSQL(string(raw)) + "'"
				}
			}
			eventsValues = append(eventsValues, fmt.Sprintf(
				"('%s','%s','%s','%s','%s','%s',%d,'%s','%s','%s','%s','%s','%s','%s','%s','%s','%s','%s','%s','%s','%s',%d,%d,'%s','%s','%s','%s','%s',%s)",
				escapeSQL(e.EventID), escapeSQL(e.TenantID), escapeSQL(e.SiteID),
				escapeSQL(e.SessionID), escapeSQL(e.VisitID), escapeSQL(e.EventType),
				e.Timestamp, escapeSQL(e.URL), escapeSQL(e.Referrer), escapeSQL(e.Title),
				escapeSQL(e.Hostname), escapeSQL(e.Pathname),
				escapeSQL(e.Language), escapeSQL(e.Country), escapeSQL(e.Region), escapeSQL(e.City),
				escapeSQL(e.Browser), escapeSQL(e.BrowserVersion), escapeSQL(e.OS),
				escapeSQL(e.OSVersion), escapeSQL(e.Device),
				e.ScreenWidth, e.ScreenHeight,
				escapeSQL(e.UTMSource), escapeSQL(e.UTMMedium), escapeSQL(e.UTMCampaign),
				escapeSQL(e.UTMTerm), escapeSQL(e.UTMContent), propsJSON,
			))
			recentValues = append(recentValues, fmt.Sprintf(
				"('%s','%s','%s','%s','%s',%d,'%s','%s','%s','%s','%s',%s)",
				escapeSQL(e.EventID), escapeSQL(e.TenantID), escapeSQL(e.SiteID),
				escapeSQL(e.SessionID), escapeSQL(e.EventType),
				e.Timestamp, escapeSQL(e.Pathname), escapeSQL(e.Referrer),
				escapeSQL(e.Browser), escapeSQL(e.OS), escapeSQL(e.Country), propsJSON,
			))
		}

		fullEventsQuery := eventsQuery + strings.Join(eventsValues, ",")
		fullRecentQuery := recentQuery + strings.Join(recentValues, ",")

		if _, err := sql.Exec(ctx, fullEventsQuery); err != nil {
			return fmt.Errorf("batch insert events %d-%d: %w", start+1, end, err)
		}
		if _, err := sql.Exec(ctx, fullRecentQuery); err != nil {
			return fmt.Errorf("batch insert recent %d-%d: %w", start+1, end, err)
		}
	}
	return nil
}

// escapeSQL escapes single quotes for SQL string literals.
func escapeSQL(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}
