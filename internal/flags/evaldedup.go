package flags

import (
	"container/list"
	"crypto/sha256"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Evaluation-write dedupe.
//
// POST /flags/evaluate is a public, per-pageview SDK call. Every enabled
// decision used to append a flag_evaluations row, so one busy user (or a
// polling client) wrote a row per call. Nothing reads flag_evaluations:
// experiment exposure and conversion counts come from the separate
// experiment_exposures / experiment_conversions tables, which only the
// explicit RecordExposure / RecordConversion endpoints write. The table is
// an audit/volume trail, so it is safe to write at most one row per
// (site, flag, variant, user) per window.
//
// The window is measured from the last row actually written (a hit does not
// extend it), so a continuously-evaluating user still leaves one row per
// window rather than one forever. A different variant is a different key: a
// variant change is always recorded. The cache is a bounded LRU; evicting a
// key can only cause one extra row, never a lost one. A failed write
// forgets its key so the next evaluation retries. Dedup state is
// per-process, so N replicas can write up to N rows per window - acceptable
// for an unread trail.

const (
	// DefaultEvalDedupWindow is the safe default: short enough that the
	// trail still shows ongoing activity, long enough to collapse a
	// polling client or a page that evaluates a flag on every render.
	DefaultEvalDedupWindow = 5 * time.Minute
	// DefaultEvalDedupMax bounds the dedupe cache (one 32-byte hash key plus
	// list bookkeeping per entry, so roughly 10 MB at the default).
	DefaultEvalDedupMax = 50000

	envEvalDedupWindowSeconds = "OBSERVE_FLAG_EVAL_DEDUP_SECONDS"
	envEvalDedupMax           = "OBSERVE_FLAG_EVAL_DEDUP_MAX"
)

// LoadEvalDedupFromEnv reads the dedupe knobs. OBSERVE_FLAG_EVAL_DEDUP_SECONDS
// is the window in seconds (0 disables dedupe: every evaluation writes a row,
// the pre-existing behaviour); OBSERVE_FLAG_EVAL_DEDUP_MAX bounds the cache.
// Unset or malformed values fall back to the defaults.
func LoadEvalDedupFromEnv(getenv func(string) string) (window time.Duration, max int) {
	window, max = DefaultEvalDedupWindow, DefaultEvalDedupMax
	if raw := strings.TrimSpace(getenv(envEvalDedupWindowSeconds)); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n >= 0 && n <= 86400 {
			window = time.Duration(n) * time.Second
		}
	}
	if raw := strings.TrimSpace(getenv(envEvalDedupMax)); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 5_000_000 {
			max = n
		}
	}
	return window, max
}

// evalDedup is a bounded LRU with a fixed TTL. Safe for concurrent use.
type evalDedup struct {
	mu      sync.Mutex
	window  time.Duration
	max     int
	order   *list.List // front = most recently touched
	entries map[[32]byte]*list.Element
	now     func() time.Time

	evicted int64
}

type evalDedupEntry struct {
	key        [32]byte
	recordedAt time.Time
}

func newEvalDedup(window time.Duration, max int) *evalDedup {
	if max <= 0 {
		max = DefaultEvalDedupMax
	}
	return &evalDedup{
		window:  window,
		max:     max,
		order:   list.New(),
		entries: make(map[[32]byte]*list.Element),
		now:     time.Now,
	}
}

// evalKey hashes the tuple so memory per entry is fixed regardless of how
// long an attacker-chosen user_id is. NUL separators prevent field-boundary
// ambiguity ("a","bc" vs "ab","c").
func evalKey(siteID, flagKey, variant, userID string) [32]byte {
	h := sha256.New()
	for _, p := range [...]string{siteID, flagKey, variant, userID} {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	var k [32]byte
	copy(k[:], h.Sum(nil))
	return k
}

// admit reports whether a row should be written for key now. True means the
// caller must write (and call forget on failure); false means a row for this
// key was already written within the window.
func (d *evalDedup) admit(key [32]byte) bool {
	if d == nil || d.window <= 0 {
		return true
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	if el, ok := d.entries[key]; ok {
		e := el.Value.(*evalDedupEntry)
		if now.Sub(e.recordedAt) < d.window {
			d.order.MoveToFront(el)
			return false
		}
		// Window elapsed: record a fresh row and restart the window.
		e.recordedAt = now
		d.order.MoveToFront(el)
		return true
	}
	d.entries[key] = d.order.PushFront(&evalDedupEntry{key: key, recordedAt: now})
	for d.order.Len() > d.max {
		back := d.order.Back()
		d.order.Remove(back)
		delete(d.entries, back.Value.(*evalDedupEntry).key)
		d.evicted++
	}
	return true
}

// forget drops key so the next evaluation writes again (used when the write
// failed).
func (d *evalDedup) forget(key [32]byte) {
	if d == nil || d.window <= 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if el, ok := d.entries[key]; ok {
		d.order.Remove(el)
		delete(d.entries, key)
	}
}

func (d *evalDedup) size() int {
	if d == nil {
		return 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.order.Len()
}

func (d *evalDedup) evictedTotal() int64 {
	if d == nil {
		return 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.evicted
}
