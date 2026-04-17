package logs

import (
	"sync"
	"time"
)

// Broadcaster fans out freshly-ingested logs to live-tail subscribers per site.
// Subscribers get a bounded channel; slow consumers lose events rather than block ingest.
type Broadcaster struct {
	mu   sync.RWMutex
	subs map[string]map[*Subscriber]struct{} // site_id -> subscriber set
}

type Subscriber struct {
	SiteID string
	Ch     chan Log
}

func NewBroadcaster() *Broadcaster {
	return &Broadcaster{subs: make(map[string]map[*Subscriber]struct{})}
}

// Subscribe returns a channel that receives logs for the given site.
// The caller must call Close when done (e.g. on SSE client disconnect).
func (b *Broadcaster) Subscribe(siteID string) *Subscriber {
	sub := &Subscriber{SiteID: siteID, Ch: make(chan Log, 32)}
	b.mu.Lock()
	defer b.mu.Unlock()
	m, ok := b.subs[siteID]
	if !ok {
		m = make(map[*Subscriber]struct{})
		b.subs[siteID] = m
	}
	m[sub] = struct{}{}
	return sub
}

// Close removes the subscriber and closes its channel.
func (b *Broadcaster) Close(sub *Subscriber) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if m, ok := b.subs[sub.SiteID]; ok {
		delete(m, sub)
		if len(m) == 0 {
			delete(b.subs, sub.SiteID)
		}
	}
	close(sub.Ch)
}

// Publish sends the log to every subscriber of that site. Drops for full channels.
func (b *Broadcaster) Publish(log Log) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	m, ok := b.subs[log.SiteID]
	if !ok {
		return
	}
	for sub := range m {
		select {
		case sub.Ch <- log:
		default:
			// subscriber too slow — drop
		}
	}
}

// SubscriberCount returns how many subscribers are attached (mainly for tests).
func (b *Broadcaster) SubscriberCount(siteID string) int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs[siteID])
}

// logToPublish builds a Log from an ingested input + timestamp, for broadcast.
func logToPublish(id string, input LogInput, ts time.Time) Log {
	attrs := ""
	if len(input.Attributes) > 0 {
		// best-effort; matches storage format
		attrs = mapJSON(input.Attributes)
	}
	return Log{
		LogID:       id,
		SiteID:      input.SiteID,
		Timestamp:   ts,
		Level:       input.Level,
		Message:     input.Message,
		ServiceName: input.ServiceName,
		TraceID:     input.TraceID,
		SpanID:      input.SpanID,
		Attributes:  attrs,
	}
}

func mapJSON(m map[string]any) string {
	// kept simple: fall back to empty on any marshal issue
	if m == nil {
		return ""
	}
	raw, err := jsonMarshalCompat(m)
	if err != nil {
		return ""
	}
	return raw
}
