package query

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/neutron-build/neutron/go/nucleus"
)

// RetentionCohort represents one row of the retention grid. Entity and
// EntityLimitation are the O03 D2/D11 labels for the grouped entity;
// IncompletePeriods counts grid columns that exist in the capped grid
// but had not fully elapsed by `to` — their values are excluded from
// Periods (the current, still-accumulating period is never shown as a
// misleadingly-low retention rate).
type RetentionCohort struct {
	CohortDate        string    `json:"cohort_date"` // e.g. "2026-03-15"
	CohortSize        int       `json:"cohort_size"` // entities first seen in this period
	Periods           []float64 `json:"periods"`     // retention % for complete periods 0, 1, 2, ...
	IncompletePeriods int       `json:"incomplete_periods"`
	Entity            string    `json:"entity,omitempty"`
	EntityLimitation  string    `json:"entity_limitation,omitempty"`
}

// RetentionOptions is the O04 retention semantics surface. Zero value =
// pre-O04 behavior with two pinned exceptions (see RetentionWithOptions):
// the entity defaults to visitor-estimate, and incomplete periods are
// excluded + flagged (previously the current period rendered as a
// truncated, misleadingly-low column).
type RetentionOptions struct {
	// Entity is the grouped entity (entity.go). "" = visitor-estimate.
	// visit/person modes derive cohort entry from raw events, so their
	// first-seen history is bounded by raw-events retention.
	Entity string
	// CohortEvent, when non-empty, is the event_type that defines cohort
	// ENTRY: an entity's cohort is the period bucket of its FIRST
	// matching event within the query window; entities without a
	// matching event are not cohort members. Empty = first activity.
	CohortEvent string
	// ReturnEvent, when non-empty, restricts return-activity bucketing
	// to matching event_types. Empty = any event.
	ReturnEvent string
}

// sessionFirstLast is the minimal session data for retention's default
// (visitor-estimate) cohort entry. FirstTS scans as BIGINT natively
// (same as the Sessions browser read in stats.go).
type sessionFirstLast struct {
	SessionID string `db:"session_id"`
	FirstTS   int64  `db:"first_ts"`
}

// retentionEntity is one entity's cohort entry and return activity.
type retentionEntity struct {
	firstTS int64          // cohort entry instant (ms)
	buckets map[int64]bool // period buckets with qualifying activity
}

// Retention computes cohort retention over the given time range on the
// visitor-estimate entity.
func (s *StatsService) Retention(ctx context.Context, siteID string, from, to time.Time, periodDays int) ([]RetentionCohort, error) {
	return s.RetentionWithOptions(ctx, siteID, from, to, periodDays, RetentionOptions{})
}

// RetentionWithOptions is Retention under the O04 semantics. Pinned
// meaning of every knob:
//
//   - Period: periodDays days (default 1, or 7 when the range exceeds 30
//     days and no explicit period was given). Buckets are absolute
//     UTC-epoch multiples of the period length (floor(ts/periodMs)):
//     daily buckets start at UTC midnight; 7-day buckets start Thursday
//     (1970-01-01). Timezone is UTC only — the bucket math is
//     timezone-free epoch flooring, and a timezone parameter is future
//     work.
//   - Cohort entry: RetentionOptions.CohortEvent when set; otherwise the
//     entity's first activity — for visitor-estimate that is the
//     sessions rollup's first_ts (the estimate's true first event,
//     bounded by rollup coverage and sessions retention); for visit and
//     person entities it is the entity's first event inside the query
//     window (bounded by raw-events retention — an entity first seen
//     before `from` re-enters as a cohort member at its first in-window
//     activity; documented limitation, not a backfill).
//   - Return activity: RetentionOptions.ReturnEvent when set, else any
//     event; bucketed on the STORED timestamp (= ingestion time, era-1:
//     the wire protocol carries no event time — ADR D8).
//   - Incomplete periods: a grid column is complete only once the period
//     has fully elapsed by `to`; incomplete columns (including the
//     current one) are excluded from Periods and counted in
//     IncompletePeriods.
//   - Bounds: the window is clamped to 186 days and the grid to 12
//     columns (pre-O04 behavior, unchanged).
func (s *StatsService) RetentionWithOptions(ctx context.Context, siteID string, from, to time.Time, periodDays int, opts RetentionOptions) ([]RetentionCohort, error) {
	entity := opts.Entity
	if err := ValidateEntity(entity); err != nil {
		return nil, err
	}

	if periodDays <= 0 {
		periodDays = 1
		if to.Sub(from) > 30*24*time.Hour {
			periodDays = 7
		}
	}

	// Clamp the window so a single request can't trigger an unbounded events
	// scan / map build. The declared MaxWindow budget (default 186 days,
	// matching this pinned clamp) governs; the grid stays capped at 12
	// columns (pre-O04 behavior, unchanged).
	from = s.clampWindow(from, to)

	qctx, finish, err := s.beginQuery(ctx, siteID)
	if err != nil {
		return nil, err
	}
	defer finish()

	fromMs := from.UnixMilli()
	toMs := to.UnixMilli()
	periodMs := int64(periodDays) * 86400000

	entities := make(map[string]*retentionEntity)
	getEntity := func(id string) *retentionEntity {
		e, ok := entities[id]
		if !ok {
			e = &retentionEntity{firstTS: -1, buckets: make(map[int64]bool)}
			entities[id] = e
		}
		return e
	}

	// Default cohort entry for visitor-estimate: the sessions rollup's
	// first_ts. Superseded by CohortEvent below when that is set.
	usingSessionsCohorts := entity == "" && opts.CohortEvent == ""
	if usingSessionsCohorts {
		rows, err := nucleus.Query[sessionFirstLast](qctx, s.db.SQL(),
			`SELECT session_id, first_ts
		 FROM `+LatestRows("sessions", []string{"first_ts"},
				`site_id = $1 AND first_ts >= $2 AND first_ts < $3`)+` AS s`,
			siteID, fromMs, toMs,
		)
		if err != nil {
			return nil, fmt.Errorf("retention query sessions: %w", err)
		}
		for _, r := range rows {
			getEntity(r.SessionID).firstTS = r.FirstTS
		}
	}

	// Activity (and, outside the sessions path, cohort entry) come from
	// raw events, streamed in the pinned (timestamp, event_id) order
	// under the O12 row/time budgets (guard.go) instead of one unbounded
	// whole-range read. All three entity columns ride the row so the
	// dispatch is a Go-side grouping decision; memory is O(entities).
	err = s.streamEvents(qctx, siteID, fromMs, toMs, "", func(row pgx.Row) error {
		r, err := scanFunnelEvent(row)
		if err != nil {
			return fmt.Errorf("retention scan: %w", err)
		}
		id, ok := entityKeyOf(entity, r)
		if !ok {
			return nil
		}
		e := getEntity(id)
		isReturn := opts.ReturnEvent == "" || r.EventType == opts.ReturnEvent
		isEntry := opts.CohortEvent == "" || r.EventType == opts.CohortEvent
		// In the default visitor-estimate path, cohort membership comes
		// from the sessions rollup alone (pre-O04 behavior): events
		// supply activity buckets, never cohort entry. Every other mode
		// (or an explicit CohortEvent) derives entry from raw events.
		if isEntry && !usingSessionsCohorts && (e.firstTS < 0 || r.Timestamp < e.firstTS) {
			e.firstTS = r.Timestamp
		}
		if isReturn {
			e.buckets[(r.Timestamp/periodMs)*periodMs] = true
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("retention query events: %w", err)
	}

	// Entities that never got a cohort entry (a CohortEvent that never
	// fired, or — impossible in practice — a sessions row without events)
	// are not cohort members.
	for id, e := range entities {
		if e.firstTS < 0 {
			delete(entities, id)
		}
	}

	return buildRetentionCohorts(entities, from, to, periodMs, entity), nil
}

// buildRetentionCohorts is the deterministic grid math (storage-free;
// pinned by funnel_semantics_test.go and the internal/session O04 oracle
// tables). Cohorts group by the bucket of firstTS; period p of cohort
// bucket b is complete iff b+(p+1)*periodMs <= to. Values are the share
// of the cohort active in each complete bucket; incomplete grid columns
// are excluded and counted.
func buildRetentionCohorts(entities map[string]*retentionEntity, from, to time.Time, periodMs int64, entity string) []RetentionCohort {
	cohortMap := make(map[int64][]*retentionEntity)
	for _, e := range entities {
		bucket := (e.firstTS / periodMs) * periodMs
		cohortMap[bucket] = append(cohortMap[bucket], e)
	}

	var buckets []int64
	for b := range cohortMap {
		buckets = append(buckets, b)
	}
	sortInt64(buckets)

	// The grid spans the period buckets that START in [from, to): a
	// bucket starting exactly at `to` can never hold an event (the read
	// is timestamp < to) nor complete, so the pre-O04 trailing +1 column
	// is structurally dead and is not a grid column.
	totalPeriods := int((to.UnixMilli()-1-from.UnixMilli())/periodMs) + 1
	if totalPeriods > 12 {
		totalPeriods = 12
	}

	var result []RetentionCohort
	for _, bucket := range buckets {
		members := cohortMap[bucket]
		cohort := RetentionCohort{
			CohortDate:       time.UnixMilli(bucket).UTC().Format("2006-01-02"),
			CohortSize:       len(members),
			Entity:           entityLabel(entity),
			EntityLimitation: EntityLimitation(entity),
		}

		// A cohort's real columns are the grid columns whose buckets start
		// strictly before `to`; complete ones have fully elapsed by `to`.
		// IncompletePeriods therefore flags exactly the cohort's current,
		// still-accumulating period (the dead trailing bucket is in
		// nobody's grid).
		realColumns := int((to.UnixMilli()-1-bucket)/periodMs) + 1
		if realColumns > totalPeriods {
			realColumns = totalPeriods
		}
		complete := int((to.UnixMilli() - bucket) / periodMs)
		if complete > realColumns {
			complete = realColumns
		}
		if complete < 0 {
			complete = 0
		}
		cohort.Periods = make([]float64, complete)
		for p := 0; p < complete; p++ {
			targetBucket := bucket + int64(p)*periodMs
			active := 0
			for _, e := range members {
				if e.buckets[targetBucket] {
					active++
				}
			}
			if cohort.CohortSize > 0 {
				cohort.Periods[p] = float64(active) / float64(cohort.CohortSize) * 100
			}
		}
		cohort.IncompletePeriods = realColumns - complete

		result = append(result, cohort)
	}

	return result
}

func sortInt64(s []int64) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
