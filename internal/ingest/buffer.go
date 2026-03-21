package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
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
	for i, e := range batch {
		var propsJSON string
		if e.Properties != nil {
			if raw, err := json.Marshal(e.Properties); err == nil {
				propsJSON = string(raw)
			}
		}
		// Use SQLModel.Exec which has SimpleProtocol — Nucleus extended protocol
		// silently drops INSERT data.
		_, err := sql.Exec(ctx, insertSQL,
			e.EventID, e.TenantID, e.SiteID, e.SessionID, e.VisitID, e.EventType,
			e.Timestamp, e.URL, e.Referrer, e.Title, e.Hostname, e.Pathname,
			e.Language, e.Country, e.Region, e.City,
			e.Browser, e.BrowserVersion, e.OS, e.OSVersion, e.Device,
			e.ScreenWidth, e.ScreenHeight,
			e.UTMSource, e.UTMMedium, e.UTMCampaign, e.UTMTerm, e.UTMContent,
			propsJSON,
		)
		if err != nil {
			return fmt.Errorf("insert event %d/%d: %w", i+1, len(batch), err)
		}
		// Recent events table
		_, err = sql.Exec(ctx, insertRecentSQL,
			e.EventID, e.TenantID, e.SiteID, e.SessionID, e.EventType,
			e.Timestamp, e.Pathname, e.Referrer, e.Browser, e.OS, e.Country,
			propsJSON,
		)
		if err != nil {
			return fmt.Errorf("insert recent %d/%d: %w", i+1, len(batch), err)
		}
	}
	return nil
}
