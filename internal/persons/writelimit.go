package persons

import (
	"sync"
	"time"
)

// WriteLimiter caps property writes per (site, person) per window with a
// fixed-window counter. It is in-process (multi-replica deployments get the
// limit per replica) and memory-bounded: when the table reaches max entries
// expired windows are swept, and if it is still full new keys are refused
// rather than growing it.
type WriteLimiter struct {
	limit  int
	window time.Duration
	max    int
	now    func() time.Time

	mu sync.Mutex
	m  map[string]*writeWindow
}

type writeWindow struct {
	start time.Time
	n     int
}

// NewWriteLimiter allows limit writes per window per person.
func NewWriteLimiter(limit int, window time.Duration) *WriteLimiter {
	return &WriteLimiter{limit: limit, window: window, max: 100000, now: time.Now, m: map[string]*writeWindow{}}
}

// Allow charges one write to (siteID, key) and reports whether it fits.
func (l *WriteLimiter) Allow(siteID, key string) bool {
	k := siteID + "\x00" + key
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	w, ok := l.m[k]
	if ok && now.Sub(w.start) < l.window {
		if w.n >= l.limit {
			return false
		}
		w.n++
		return true
	}
	if !ok && len(l.m) >= l.max {
		for kk, ww := range l.m {
			if now.Sub(ww.start) >= l.window {
				delete(l.m, kk)
			}
		}
		if len(l.m) >= l.max {
			return false
		}
	}
	l.m[k] = &writeWindow{start: now, n: 1}
	return true
}
