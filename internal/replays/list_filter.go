package replays

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/neutron-build/neutron/go/neutron"
	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
	"github.com/useteploy/teploy-observe/internal/queryguard"
)

// Filter bounds for the replay list.
const (
	maxFilterURLContains = 200
	maxFilterDistinctID  = 256
	// maxFilterMinDurationMS caps min_duration at 7 days; anything larger
	// can never match a recorded session.
	maxFilterMinDurationMS = 7 * 24 * 3600 * 1000
	// errorLinkSlackMS extends the error_events lookup window past the
	// listing's upper bound: an error is raised after its session starts.
	errorLinkSlackMS = 3600 * 1000
)

// ReplayFilter narrows ListReplaysFiltered. The zero value filters nothing.
type ReplayFilter struct {
	// HasErrors keeps sessions flagged has_error by the tracker or linked
	// to an error event by session id or replay id.
	HasErrors bool
	// MinDurationMS keeps sessions at least this long (milliseconds).
	MinDurationMS int64
	// URLContains is a case-insensitive substring match on the stored
	// (query-stripped) session URL. Wildcards are matched literally.
	URLContains string
	// DistinctID is the RAW identifier; it is hashed with the site's ingest
	// derivation before comparing, so the raw value never touches SQL.
	DistinctID string
}

func (f ReplayFilter) active() bool {
	return f.HasErrors || f.MinDurationMS > 0 || f.URLContains != "" || f.DistinctID != ""
}

// escapeLike makes s match literally inside a LIKE/ILIKE pattern
// (backslash is the default escape character).
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// WithQueryGuard installs O12 query admission for the list: a concurrency
// slot per call plus the wall-time budget and window clamp. A nil limiter
// disables concurrency admission; zero budget fields fall back to defaults.
func (s *ReplayService) WithQueryGuard(l *queryguard.Limiter, b queryguard.Budgets) *ReplayService {
	d := queryguard.DefaultBudgets()
	if b.Timeout <= 0 {
		b.Timeout = d.Timeout
	}
	if b.MaxWindow <= 0 {
		b.MaxWindow = d.MaxWindow
	}
	if b.MaxScanRows <= 0 {
		b.MaxScanRows = d.MaxScanRows
	}
	s.guard = l
	s.budgets = b
	return s
}

// buildListQuery renders the list SQL and its bound arguments. Pure (no
// store access) so the text can be asserted. Every user value is a bound
// parameter; the only interpolated numbers are the clamped LIMIT/OFFSET.
// distinctHash is the already-hashed distinct id ("" when unfiltered).
func buildListQuery(siteID string, from, to time.Time, limit, offset int, f ReplayFilter, distinctHash string) (string, []any) {
	if limit <= 0 {
		limit = 20
	}
	if limit > maxListReplaysLimit {
		limit = maxListReplaysLimit
	}
	if offset < 0 {
		offset = 0
	}
	args := []any{siteID, dbutil.IntParam(from.UnixMilli()), dbutil.IntParam(to.UnixMilli())}
	var outer []string
	add := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	// Non-key columns are compared on the COLLAPSED row (outer WHERE): a
	// predicate inside the argMax grouping would match stale versions.
	if f.DistinctID != "" {
		outer = append(outer, "distinct_id = "+add(distinctHash))
	}
	if f.MinDurationMS > 0 {
		outer = append(outer, "CAST(duration_ms AS BIGINT) >= CAST("+add(dbutil.IntParam(f.MinDurationMS))+" AS BIGINT)")
	}
	if f.URLContains != "" {
		// Precedent: internal/logs/logs.go message ILIKE '%' || $n || '%'.
		outer = append(outer, "url ILIKE '%' || "+add(escapeLike(f.URLContains))+" || '%'")
	}
	if f.HasErrors {
		hi := add(dbutil.IntParam(to.UnixMilli() + errorLinkSlackMS))
		// Precedent for IN (SELECT ...): internal/groups/groups.go. The
		// error_events lookups are site- and time-bounded.
		outer = append(outer, "(has_error = 'true'"+
			" OR (session_id <> '' AND session_id IN (SELECT session_id FROM error_events WHERE site_id = $1 AND timestamp >= $2 AND timestamp < "+hi+" AND session_id <> ''))"+
			" OR replay_id IN (SELECT replay_id FROM error_events WHERE site_id = $1 AND timestamp >= $2 AND timestamp < "+hi+" AND replay_id <> ''))")
	}
	where := ""
	if len(outer) > 0 {
		where = "\n\t\t WHERE " + strings.Join(outer, " AND ")
	}
	q := `SELECT replay_id, tenant_id, site_id, session_id,
			CAST(start_time AS TEXT) AS start_time,
			duration_ms, page_count, url, browser, os, device, has_error
		 FROM ` + replaySessionsLatest("site_id = $1 AND start_time >= $2 AND start_time < $3") + where +
		fmt.Sprintf(`
		 ORDER BY start_time DESC
		 LIMIT %d OFFSET %d`, limit, offset)
	return q, args
}

// ListReplays returns recent replay sessions for a site, read through the
// version collapse so multi-batch upserts surface as one row per replay.
func (s *ReplayService) ListReplays(ctx context.Context, siteID string, from, to time.Time, limit, offset int) ([]ReplaySession, error) {
	return s.ListReplaysFiltered(ctx, siteID, from, to, limit, offset, ReplayFilter{})
}

// validateFilter bounds the filter values (400s).
func validateFilter(f ReplayFilter) error {
	if len(f.URLContains) > maxFilterURLContains || strings.ContainsRune(f.URLContains, 0) {
		return neutron.ErrBadRequest(fmt.Sprintf("url_contains must be at most %d bytes", maxFilterURLContains))
	}
	if len(f.DistinctID) > maxFilterDistinctID || strings.ContainsRune(f.DistinctID, 0) {
		return neutron.ErrBadRequest(fmt.Sprintf("distinct_id must be at most %d bytes", maxFilterDistinctID))
	}
	if f.MinDurationMS < 0 || f.MinDurationMS > maxFilterMinDurationMS {
		return neutron.ErrBadRequest("min_duration must be between 0 and 604800000 milliseconds")
	}
	return nil
}

// ListReplaysFiltered is ListReplays with optional filters. Invalid filter
// values are 400s; the guard (when installed) admits and bounds the query.
func (s *ReplayService) ListReplaysFiltered(ctx context.Context, siteID string, from, to time.Time, limit, offset int, f ReplayFilter) ([]ReplaySession, error) {
	if err := validateFilter(f); err != nil {
		return nil, err
	}

	distinctHash := ""
	if f.DistinctID != "" {
		salt, rawOptIn := s.salt, false
		if s.privacy != nil {
			if siteSalt, raw, ok := s.privacy(ctx, siteID); ok {
				salt, rawOptIn = siteSalt, raw
			}
		}
		if salt == "" && !rawOptIn {
			// Ingest drops the id in this state, so nothing can match.
			return []ReplaySession{}, nil
		}
		distinctHash = hashDistinctID(f.DistinctID, salt, rawOptIn)
	}

	qctx := ctx
	if s.guard != nil {
		release, err := s.guard.Acquire(ctx, siteID)
		if err != nil {
			return nil, err
		}
		defer release()
	}
	if s.budgets.Timeout > 0 {
		if max := s.budgets.MaxWindow; max > 0 && to.Sub(from) > max {
			from = to.Add(-max)
		}
		var cancel context.CancelFunc
		qctx, cancel = context.WithTimeout(ctx, s.budgets.Timeout)
		defer cancel()
	}

	q, args := buildListQuery(siteID, from, to, limit, offset, f, distinctHash)
	rows, err := nucleus.Query[ReplaySession](qctx, s.db.SQL(), q, args...)
	if err != nil && errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		return nil, queryguard.TimeBudgetRefusal(s.guard, s.budgets.Timeout)
	}
	return rows, err
}
