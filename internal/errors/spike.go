package errors

// Spike protection: per-site and per-issue event rate caps applied at
// flush time (insertErrorEvent), BEFORE issue resolution and the
// error_events insert.
//
// Behaviour: fixed one-minute windows. Within a window, events past a
// cap are thinned to a sampled 1-in-N keep (so issue counts stay
// approximately right: the stored count under-reports by about the
// factor N for the over-cap part, and the exact number discarded is the
// spike_dropped counter at /healthz errors). A dropped event is a FINAL
// labeled disposition ("spike_dropped_issue" / "spike_dropped_site"),
// never a silent loss: it is counted, logged at debug, and an
// identified event still claims its inbox ledger row so a producer
// retry dedupes.
//
// Never dropped, regardless of the caps:
//   - the FIRST event of a new issue (no cached issue entry yet),
//   - a REGRESSION event (the cached issue entry is marked resolved).
// When the cache cannot say (entry evicted) the event is kept: the
// guard fails open towards keeping data.
//
// Defaults are conservative (a healthy app never reaches them). Env:
//   OBSERVE_ERROR_SPIKE_ISSUE_PER_MIN  per-issue cap per minute (default 1000, 0 = off)
//   OBSERVE_ERROR_SPIKE_SITE_PER_MIN   per-site cap per minute  (default 10000, 0 = off)
//   OBSERVE_ERROR_SPIKE_SAMPLE         keep 1/N past a cap      (default 10, min 1)
//   OBSERVE_ERROR_SPIKE_DISABLE=true   turn protection off

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ErrSpikeDropped is the internal sentinel for a spike-sampled event.
var ErrSpikeDropped = errors.New("error event dropped by spike protection")

const (
	DefaultSpikeIssuePerMin = 1000
	DefaultSpikeSitePerMin  = 10000
	DefaultSpikeSample      = 10
	spikeWindow             = time.Minute
	spikeMaxTrackedIssues   = 50000
)

// SpikeLimiter holds the window counters.
type SpikeLimiter struct {
	issueCap, siteCap, sampleN int
	disabled                   bool
	window                     time.Duration
	now                        func() time.Time

	mu       sync.Mutex
	winStart time.Time
	site     map[string]int
	issue    map[string]int

	DroppedIssue atomic.Int64
	DroppedSite  atomic.Int64
}

// NewSpikeLimiter builds a limiter; a cap <= 0 disables that dimension,
// sample < 1 is treated as 1 (drop everything past the cap).
func NewSpikeLimiter(issueCap, siteCap, sample int) *SpikeLimiter {
	if sample < 1 {
		sample = 1
	}
	return &SpikeLimiter{issueCap: issueCap, siteCap: siteCap, sampleN: sample,
		window: spikeWindow, now: time.Now, site: map[string]int{}, issue: map[string]int{}}
}

// NewSpikeLimiterFromEnv reads the OBSERVE_ERROR_SPIKE_* variables.
func NewSpikeLimiterFromEnv() *SpikeLimiter {
	l := NewSpikeLimiter(
		envInt("OBSERVE_ERROR_SPIKE_ISSUE_PER_MIN", DefaultSpikeIssuePerMin),
		envInt("OBSERVE_ERROR_SPIKE_SITE_PER_MIN", DefaultSpikeSitePerMin),
		envInt("OBSERVE_ERROR_SPIKE_SAMPLE", DefaultSpikeSample))
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("OBSERVE_ERROR_SPIKE_DISABLE"))); v == "true" || v == "1" {
		l.disabled = true
	}
	return l
}

func envInt(name string, def int) int {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return def
	}
	return n
}

// Over counts one event and reports whether it is past a cap and not in
// the sampled keep set. reason is "issue" or "site" when drop is true.
// The caller applies the new-issue / regression exemption BEFORE acting
// on drop.
func (l *SpikeLimiter) Over(siteID, groupHash string) (drop bool, reason string) {
	if l == nil || l.disabled {
		return false, ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if l.winStart.IsZero() || now.Sub(l.winStart) >= l.window {
		l.winStart = now
		l.site = map[string]int{}
		l.issue = map[string]int{}
	}
	l.site[siteID]++
	siteN := l.site[siteID]
	ikey := siteID + "\x00" + groupHash
	issueN := 0
	if _, tracked := l.issue[ikey]; tracked || len(l.issue) < spikeMaxTrackedIssues {
		l.issue[ikey]++
		issueN = l.issue[ikey]
	}
	if l.issueCap > 0 && issueN > l.issueCap {
		if (issueN-l.issueCap)%l.sampleN != 0 {
			return true, "issue"
		}
		return false, ""
	}
	if l.siteCap > 0 && siteN > l.siteCap {
		if (siteN-l.siteCap)%l.sampleN != 0 {
			return true, "site"
		}
	}
	return false, ""
}

// spikeCheck decides whether the event must be dropped. exempt reports
// whether the event is the first of a new issue or a regression (only
// consulted when the limiter would drop).
func (l *SpikeLimiter) spikeCheck(siteID, groupHash string, exempt func() bool) bool {
	drop, reason := l.Over(siteID, groupHash)
	if !drop {
		return false
	}
	if exempt != nil && exempt() {
		return false
	}
	if reason == "issue" {
		l.DroppedIssue.Add(1)
	} else {
		l.DroppedSite.Add(1)
	}
	return true
}

// spikeExempt reports whether an event for (site, hash) is the first of a
// new issue or a regression, from the grouphash cache entry alone (no
// SQL on the flood path). Unknown = exempt (fail open towards keeping).
func (s *IssueService) spikeExempt(ctx context.Context, siteID, groupHash string) bool {
	if s == nil || s.db == nil {
		return true
	}
	data, err := s.db.KV().Get(ctx, kvCacheKey(siteID, groupHash))
	if err != nil || data == nil {
		return true
	}
	var ci cachedIssue
	if json.Unmarshal(data, &ci) != nil || ci.IssueID == "" {
		return true
	}
	return ci.Resolved
}
