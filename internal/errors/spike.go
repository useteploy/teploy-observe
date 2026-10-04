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
// Exempt from the rate caps (but NOT from the new-issue cap below):
//   - the FIRST event of a new issue (no cached issue entry yet),
//   - a REGRESSION event (the cached issue entry is marked resolved).
// When the cache cannot say (entry evicted) the event is kept: the
// guard fails open towards keeping data.
//
// The exemption itself is capped per site per minute (new-issue cap):
// otherwise a producer varying the fingerprint on every event makes every
// event "the first of a new issue" and walks around every other cap. A
// new-issue admission past that cap is dropped (counted as
// spike_dropped_new_issue) and the fingerprint stays denied for the rest
// of the window, so its follow-up events cannot create the issue either.
//
// Site-wide thinning is proportional, not blanket: between siteCap and
// 2*siteCap only issues past their fair share (siteCap / issues seen for
// the site this window) are thinned, so one flood issue cannot spend the
// whole site budget and starve the steady ones; past 2*siteCap everything
// is thinned.
//
// Defaults are conservative (a healthy app never reaches them). Env:
//   OBSERVE_ERROR_SPIKE_ISSUE_PER_MIN  per-issue cap per minute (default 1000, 0 = off)
//   OBSERVE_ERROR_SPIKE_SITE_PER_MIN   per-site cap per minute  (default 10000, 0 = off)
//   OBSERVE_ERROR_SPIKE_NEW_ISSUE_CAP  new-issue/regression admissions per site per minute (default 300, 0 = off)
//   OBSERVE_ERROR_SPIKE_SAMPLE         keep 1/N past a cap      (default 10, min 1)
//   OBSERVE_ERROR_SPIKE_DISABLE=true   turn protection off

import (
	"context"
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
	DefaultSpikeNewIssueCap = 300
	spikeWindow             = time.Minute
	spikeMaxTrackedIssues   = 50000
)

// SpikeLimiter holds the window counters.
type SpikeLimiter struct {
	issueCap, siteCap, sampleN int
	newIssueCap                int
	disabled                   bool
	window                     time.Duration
	now                        func() time.Time

	mu       sync.Mutex
	winStart time.Time
	site     map[string]int
	issue    map[string]int
	siteKeys map[string]int      // distinct issues seen per site this window
	exempt   map[string]int      // new-issue/regression admissions per site
	denied   map[string]struct{} // fingerprints refused by the new-issue cap

	DroppedIssue    atomic.Int64
	DroppedSite     atomic.Int64
	DroppedNewIssue atomic.Int64
	// AdmittedNewIssue counts exemption admissions (observability only).
	AdmittedNewIssue atomic.Int64
}

// NewSpikeLimiter builds a limiter; a cap <= 0 disables that dimension,
// sample < 1 is treated as 1 (drop everything past the cap).
func NewSpikeLimiter(issueCap, siteCap, sample int) *SpikeLimiter {
	if sample < 1 {
		sample = 1
	}
	return &SpikeLimiter{issueCap: issueCap, siteCap: siteCap, sampleN: sample,
		window: spikeWindow, now: time.Now, newIssueCap: DefaultSpikeNewIssueCap,
		site: map[string]int{}, issue: map[string]int{}, siteKeys: map[string]int{},
		exempt: map[string]int{}, denied: map[string]struct{}{}}
}

// NewSpikeLimiterFromEnv reads the OBSERVE_ERROR_SPIKE_* variables.
func NewSpikeLimiterFromEnv() *SpikeLimiter {
	l := NewSpikeLimiter(
		envInt("OBSERVE_ERROR_SPIKE_ISSUE_PER_MIN", DefaultSpikeIssuePerMin),
		envInt("OBSERVE_ERROR_SPIKE_SITE_PER_MIN", DefaultSpikeSitePerMin),
		envInt("OBSERVE_ERROR_SPIKE_SAMPLE", DefaultSpikeSample))
	l.newIssueCap = envInt("OBSERVE_ERROR_SPIKE_NEW_ISSUE_CAP", DefaultSpikeNewIssueCap)
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

// overResult is one event's window accounting.
type overResult struct {
	drop   bool
	reason string // "issue" or "site" when drop
	first  bool   // first sighting of this fingerprint in the window
	denied bool   // fingerprint refused earlier by the new-issue cap
}

// Over counts one event and reports whether it is past a cap and not in
// the sampled keep set. reason is "issue" or "site" when drop is true.
// The caller applies the new-issue / regression exemption BEFORE acting
// on drop.
func (l *SpikeLimiter) Over(siteID, groupHash string) (drop bool, reason string) {
	r := l.count(siteID, groupHash)
	return r.drop, r.reason
}

func (l *SpikeLimiter) rollWindow(now time.Time) {
	if l.winStart.IsZero() || now.Sub(l.winStart) >= l.window {
		l.winStart = now
		l.site = map[string]int{}
		l.issue = map[string]int{}
		l.siteKeys = map[string]int{}
		l.exempt = map[string]int{}
		l.denied = map[string]struct{}{}
	}
}

func (l *SpikeLimiter) count(siteID, groupHash string) overResult {
	if l == nil || l.disabled {
		return overResult{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rollWindow(l.now())
	return l.countLocked(siteID, groupHash)
}

func (l *SpikeLimiter) countLocked(siteID, groupHash string) overResult {
	var res overResult
	l.site[siteID]++
	siteN := l.site[siteID]
	ikey := siteID + "\x00" + groupHash
	issueN := 0
	_, tracked := l.issue[ikey]
	if tracked || len(l.issue) < spikeMaxTrackedIssues {
		l.issue[ikey]++
		issueN = l.issue[ikey]
		if !tracked {
			l.siteKeys[siteID]++
		}
	}
	res.first = !tracked
	if _, d := l.denied[ikey]; d {
		res.denied = true
	}
	if l.issueCap > 0 && issueN > l.issueCap {
		if (issueN-l.issueCap)%l.sampleN != 0 {
			res.drop, res.reason = true, "issue"
		}
		return res
	}
	if l.siteCap > 0 && siteN > l.siteCap {
		// Fairness: inside the protected band an issue at or under its fair
		// share of the site budget is not thinned.
		if siteN <= 2*l.siteCap && issueN > 0 {
			share := l.siteCap / max(l.siteKeys[siteID], 1)
			if issueN <= max(share, 1) {
				return res
			}
		}
		if (siteN-l.siteCap)%l.sampleN != 0 {
			res.drop, res.reason = true, "site"
		}
	}
	return res
}

// admitExempt charges one new-issue/regression admission to the site and
// reports whether it fits under the cap. A refusal denies the fingerprint
// for the rest of the window.
func (l *SpikeLimiter) admitExempt(siteID, groupHash string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.newIssueCap <= 0 {
		return true
	}
	l.exempt[siteID]++
	if l.exempt[siteID] > l.newIssueCap {
		if len(l.denied) < spikeMaxTrackedIssues {
			l.denied[siteID+"\x00"+groupHash] = struct{}{}
		}
		return false
	}
	return true
}

// spikeCheck decides whether the event must be dropped. exempt reports
// whether the event is the first of a new issue or a regression; it is
// consulted on a fingerprint's first sighting in the window (so new
// issues are charged to the new-issue cap even under the rate caps) and
// whenever a rate cap would drop.
func (l *SpikeLimiter) spikeCheck(siteID, groupHash string, exempt func() bool) bool {
	if l == nil || l.disabled {
		return false
	}
	r := l.count(siteID, groupHash)
	if r.denied {
		l.DroppedNewIssue.Add(1)
		return true
	}
	if !r.drop && !r.first {
		return false
	}
	if exempt != nil && exempt() {
		if !l.admitExempt(siteID, groupHash) {
			l.DroppedNewIssue.Add(1)
			return true
		}
		l.AdmittedNewIssue.Add(1)
		return false
	}
	if !r.drop {
		return false
	}
	if r.reason == "issue" {
		l.DroppedIssue.Add(1)
	} else {
		l.DroppedSite.Add(1)
	}
	return true
}

// spikeExempt reports whether an event for (site, hash) is the first of a
// new issue or a regression, judged against the issue the event will be
// ATTRIBUTED to (the merge target when the fingerprint maps to a merged
// source). Snoozed issues are status=resolved with a snooze deadline, so a
// resolved status covers them. Unknown = exempt (fail open towards keeping).
func (s *IssueService) spikeExempt(ctx context.Context, siteID, groupHash string) bool {
	if s == nil || (s.db == nil && s.store == nil) {
		return true
	}
	lc := s.lifecycle()
	ci, ok := lc.cacheGet(ctx, siteID, groupHash)
	if !ok {
		return true
	}
	if target := s.ResolveMerged(ctx, siteID, ci.IssueID); target != ci.IssueID {
		t, err := lc.issueByID(ctx, siteID, target)
		if err != nil || t == nil {
			return true
		}
		return t.Status == "resolved"
	}
	return ci.Resolved
}
