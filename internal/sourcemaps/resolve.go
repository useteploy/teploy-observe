package sourcemaps

import (
	"context"
	"log/slog"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// kvGetter is the one KV method resolution needs; *nucleus.KVModel satisfies
// it and tests substitute an in-memory store.
type kvGetter interface {
	Get(ctx context.Context, key string) ([]byte, error)
}

// Resolution outcome counters. Process-wide (the service is a singleton in
// practice and /healthz is process-wide), restart-reset like the other
// in-memory counters.
var (
	resolveHit       atomic.Int64
	resolveNoMap     atomic.Int64
	resolveNoMapping atomic.Int64
	resolveError     atomic.Int64
	lastErrLogNanos  atomic.Int64
)

// ResolveStats is the source-map resolution counter block surfaced at
// /healthz. Counts are per frame. A rising miss_no_map with maps uploaded
// means filename or release do not line up with what the SDK reports; a
// rising error means the store or a stored map is unreadable.
type ResolveStats struct {
	Hit           int64 `json:"hit"`
	MissNoMap     int64 `json:"miss_no_map"`
	MissNoMapping int64 `json:"miss_no_mapping"`
	Error         int64 `json:"error"`
}

// Stats returns a snapshot of the resolution counters.
func Stats() ResolveStats {
	return ResolveStats{
		Hit:           resolveHit.Load(),
		MissNoMap:     resolveNoMap.Load(),
		MissNoMapping: resolveNoMapping.Load(),
		Error:         resolveError.Load(),
	}
}

// errLogInterval rate-limits the debug log for resolution errors so a
// failing store cannot turn every ingested frame into a log line.
const errLogInterval = 30 * time.Second

func noteResolveError(site, release, file string, err error) {
	resolveError.Add(1)
	now := time.Now().UnixNano()
	last := lastErrLogNanos.Load()
	if now-last < int64(errLogInterval) || !lastErrLogNanos.CompareAndSwap(last, now) {
		return
	}
	slog.Debug("sourcemaps: resolution failed (rate limited; see /healthz sourcemaps.error)",
		"site", site, "release", release, "file", file, "err", err)
}

const (
	maxFilenameLen    = 2048
	maxLookupCands    = 8
	maxSuffixSegments = 5
)

// canonicalName reduces a filename or URL to the form maps are stored under:
// no query, no fragment, no scheme/host, no leading "/", "./" or "~/".
// "https://cdn.x/static/app.js?v=3#a" and "/static/app.js" both become
// "static/app.js", so an upload keyed by path matches a frame carrying the
// full URL and vice versa.
func canonicalName(name string) string {
	return trimLead(pathOf(stripQF(name)))
}

func stripQF(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "#?"); i >= 0 {
		s = s[:i]
	}
	return s
}

// pathOf returns the path of an absolute URL, or s unchanged.
func pathOf(s string) string {
	if strings.Contains(s, "://") {
		if u, err := url.Parse(s); err == nil && u.Host != "" {
			return u.Path
		}
	}
	return s
}

func trimLead(s string) string {
	for {
		switch {
		case strings.HasPrefix(s, "~/"):
			s = s[2:]
		case strings.HasPrefix(s, "./"):
			s = s[2:]
		case strings.HasPrefix(s, "/"):
			s = s[1:]
		default:
			return s
		}
	}
}

// lookupCandidates lists, in deterministic most-specific-first order, the
// stored names a frame filename may have been uploaded under: the exact
// string (pre-normalisation behaviour, so existing maps still resolve), the
// query/fragment-stripped full URL, the canonical path, shorter path
// suffixes, and finally the basename. Bounded so an attacker-supplied
// filename cannot fan out into many KV reads.
func lookupCandidates(filename string) []string {
	if filename == "" || len(filename) > maxFilenameLen {
		return nil
	}
	var out []string
	seen := map[string]struct{}{}
	add := func(c string) {
		if c == "" || len(out) >= maxLookupCands {
			return
		}
		if _, ok := seen[c]; ok {
			return
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	add(filename)
	add(stripQF(filename))
	canon := canonicalName(filename)
	add(canon)
	segs := strings.Split(canon, "/")
	for i := 1; i < len(segs) && i <= maxSuffixSegments && len(out) < maxLookupCands-1; i++ {
		add(strings.Join(segs[i:], "/"))
	}
	add(segs[len(segs)-1])
	return out
}

// findSourceMap tries each candidate name for (site, release). Returns the
// first map found; a nil meta with nil error means no candidate has a map.
func findSourceMap(ctx context.Context, kv kvGetter, siteID, release, filename string) (*SourceMapMeta, error) {
	for _, cand := range lookupCandidates(filename) {
		meta, err := loadSourceMap(ctx, kv, siteID, release, cand)
		if err != nil || meta != nil {
			return meta, err
		}
	}
	return nil, nil
}
