package cohorts

// Static cohorts (C2 depth): an explicit list of entity ids stored in
// cohort_members (migration 060) instead of a rule. The cohorts row carries
// the rule {"op":"static"} and a maintained member_count.
//
// Storage shape: cohort_members is a ReplacingMergeTree keyed on
// (tenant, site, cohort, entity) and versioned. Add writes removed=0 rows,
// remove writes removed=1 tombstones at a higher version; reads collapse to
// the highest version per entity in Go (finding #10) over a row-bounded
// SELECT. Versions are taken from max(now, cohort.updated_at+1) and the
// cohort row is rewritten at the same value, so each mutation batch is
// strictly newer than the last one on that cohort.
//
// Everything is scoped by (site_id, cohort_id): the cohort is first resolved
// through Get(siteID, cohortID), so another site's cohort id is
// ErrNotFound, and member rows are only ever read/written under the
// caller's site_id.

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
)

const (
	// MaxStaticMembers is the most members one static cohort may hold, and
	// the most distinct ids one bulk create / import / add call may carry.
	MaxStaticMembers = 100000
	// MaxEntityIDLen is the longest accepted entity id, in bytes.
	MaxEntityIDLen = 256
	// staticInsertChunk is rows per multi-row INSERT: 1000 x 7 columns =
	// 7000 parameters, far under the 65535 wire cap.
	staticInsertChunk = 1000
	staticInsertCols  = 7
	// maxStaticRows bounds the raw rows (all versions, tombstones
	// included) one read pulls. Past it the read fails (ErrTooLarge)
	// rather than truncating membership.
	maxStaticRows = 600000
)

// ErrTooManyIDs is returned (wrapped) when a request carries, or would
// leave a static cohort with, more than MaxStaticMembers distinct ids.
var ErrTooManyIDs = errors.New("too many member ids")

// ErrNotStatic is returned (wrapped) when a member edit targets a rule
// cohort.
var ErrNotStatic = errors.New("cohort is not static")

// NormalizeIDs trims, drops blanks, validates and de-duplicates a list of
// entity ids, preserving first-seen order. It refuses an id longer than
// MaxEntityIDLen or containing control characters, and refuses to hold more
// than MaxStaticMembers distinct ids (ErrTooManyIDs) - checked while
// building so an oversized list is never fully materialised.
func NormalizeIDs(ids []string) ([]string, error) {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, raw := range ids {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		if len(id) > MaxEntityIDLen {
			return nil, invalidf("member id longer than %d bytes", MaxEntityIDLen)
		}
		for i := 0; i < len(id); i++ {
			if id[i] < 0x20 || id[i] == 0x7f {
				return nil, invalidf("member id contains a control character")
			}
		}
		if _, dup := seen[id]; dup {
			continue
		}
		if len(out) >= MaxStaticMembers {
			return nil, fmt.Errorf("%w: more than %d distinct ids", ErrTooManyIDs, MaxStaticMembers)
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out, nil
}

// bomPrefix is the UTF-8 byte order mark some spreadsheet exports prepend.
const bomPrefix = "\xef\xbb\xbf"

// csvHeaderNames are first-cell values treated as a header row, not an id.
var csvHeaderNames = map[string]struct{}{
	"id": {}, "ids": {}, "entity_id": {}, "distinct_id": {}, "user_id": {}, "person": {},
}

// ParseCSVIDs reads entity ids from a CSV body: the FIRST column of each
// record, one id per row, an optional header row (id / entity_id /
// distinct_id / user_id) skipped, blank rows ignored. It stops with
// ErrTooManyIDs as soon as the distinct count passes MaxStaticMembers.
func ParseCSVIDs(r io.Reader) ([]string, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true
	cr.ReuseRecord = true
	var ids []string
	seen := make(map[string]struct{})
	first := true
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			var perr *csv.ParseError
			if errors.As(err, &perr) {
				return nil, invalidf("csv: %v", err)
			}
			return nil, err // read failure (for example an oversized body)
		}
		if len(rec) == 0 {
			continue
		}
		cell := strings.TrimSpace(strings.TrimPrefix(rec[0], bomPrefix))
		if first {
			first = false
			if _, hdr := csvHeaderNames[strings.ToLower(cell)]; hdr {
				continue
			}
		}
		if cell == "" {
			continue
		}
		if _, dup := seen[cell]; dup {
			continue
		}
		if len(seen) >= MaxStaticMembers {
			return nil, fmt.Errorf("%w: more than %d distinct ids", ErrTooManyIDs, MaxStaticMembers)
		}
		seen[cell] = struct{}{}
		ids = append(ids, cell)
	}
	return NormalizeIDs(ids)
}

// memberPlaceholders renders "($1,...,$7),($8,...)" for n rows.
func memberPlaceholders(n int) string {
	var b strings.Builder
	p := 1
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('(')
		for c := 0; c < staticInsertCols; c++ {
			if c > 0 {
				b.WriteByte(',')
			}
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(p))
			p++
		}
		b.WriteByte(')')
	}
	return b.String()
}

const memberInsertPrefix = `INSERT INTO cohort_members (cohort_id, tenant_id, site_id, entity_id, added_at, removed, version) VALUES `

// memberRow is one cohort_members write.
type memberRow struct {
	id      string
	addedAt int64
	removed int64
}

// insertMembers writes rows in chunked multi-row INSERTs at one version.
func (s *Service) insertMembers(ctx context.Context, siteID, cohortID string, rows []memberRow, version int64) error {
	for start := 0; start < len(rows); start += staticInsertChunk {
		end := start + staticInsertChunk
		if end > len(rows) {
			end = len(rows)
		}
		chunk := rows[start:end]
		args := make([]any, 0, len(chunk)*staticInsertCols)
		for _, r := range chunk {
			args = append(args, cohortID, "default", siteID, r.id,
				dbutil.IntParam(r.addedAt), dbutil.IntParam(r.removed), dbutil.IntParam(version))
		}
		if _, err := s.db.SQL().Exec(ctx, memberInsertPrefix+memberPlaceholders(len(chunk)), args...); err != nil {
			return fmt.Errorf("insert cohort_members %d-%d: %w", start+1, end, err)
		}
	}
	return nil
}

type memberRecord struct {
	EntityID string `db:"entity_id"`
	AddedAt  int64  `db:"added_at"`
	Removed  int64  `db:"removed"`
	Version  int64  `db:"version"`
}

// staticMembersSQL is the bounded read of one cohort's member rows. Both
// site_id and cohort_id are bound; the LIMIT is one past maxStaticRows so
// an overrun is detected.
func staticMembersSQL() string {
	return `SELECT entity_id, added_at, removed, version
	 FROM cohort_members
	 WHERE site_id = $1 AND cohort_id = $2
	 LIMIT ` + strconv.Itoa(maxStaticRows+1)
}

// collapseMembers reduces versioned rows to the live members (entity ->
// first added_at): the highest version per entity wins, and a winning
// tombstone drops the entity. Ties on version prefer the tombstone, the
// conservative reading of a same-batch race.
func collapseMembers(rows []memberRecord) map[string]int64 {
	best := make(map[string]memberRecord, len(rows))
	for _, r := range rows {
		cur, ok := best[r.EntityID]
		if !ok || r.Version > cur.Version || (r.Version == cur.Version && r.Removed > cur.Removed) {
			best[r.EntityID] = r
		}
	}
	live := make(map[string]int64, len(best))
	for id, r := range best {
		if r.Removed == 0 && id != "" {
			live[id] = r.AddedAt
		}
	}
	return live
}

func (s *Service) readStatic(ctx context.Context, siteID, cohortID string) (map[string]int64, error) {
	rows, err := nucleus.Query[memberRecord](ctx, s.db.SQL(), staticMembersSQL(), siteID, cohortID)
	if err != nil {
		return nil, fmt.Errorf("read cohort_members: %w", err)
	}
	if len(rows) > maxStaticRows {
		return nil, fmt.Errorf("%w: static cohort has more than %d stored rows", ErrTooLarge, maxStaticRows)
	}
	return collapseMembers(rows), nil
}

func (s *Service) staticMembers(ctx context.Context, siteID, cohortID string) ([]string, error) {
	live, err := s.readStatic(ctx, siteID, cohortID)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(live))
	for id := range live {
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

func (s *Service) staticCount(ctx context.Context, siteID, cohortID string) (int64, error) {
	live, err := s.readStatic(ctx, siteID, cohortID)
	if err != nil {
		return 0, err
	}
	return int64(len(live)), nil
}

// resolveMembers is the single membership entry point for an existing
// cohort: static cohorts read their list, rule cohorts evaluate their tree.
func (s *Service) resolveMembers(ctx context.Context, c *Cohort) ([]string, error) {
	def, err := ParseDefinition(c.Rule)
	if err != nil {
		return nil, err
	}
	if def.IsStatic() {
		return s.staticMembers(ctx, c.SiteID, c.CohortID)
	}
	return s.EvaluateCohort(ctx, c.SiteID, def)
}

const staticRule = `{"op":"static"}`

// putCohortRow inserts a new version of a cohorts row.
func (s *Service) putCohortRow(ctx context.Context, c Cohort) error {
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO cohorts (cohort_id, tenant_id, site_id, name, description, rule, member_count, created_at, updated_at)
		 VALUES ($1, 'default', $2, $3, $4, $5, $6, $7, $8)`,
		c.CohortID, c.SiteID, c.Name, c.Description, c.Rule,
		dbutil.IntParam(c.MemberCount), dbutil.IntParam(c.CreatedAt), dbutil.IntParam(c.UpdatedAt),
	)
	if err != nil {
		return fmt.Errorf("write cohort: %w", err)
	}
	return nil
}

// CreateStatic creates a static cohort from a list of ids (at most
// MaxStaticMembers distinct). Members are written first, then the cohort
// row: a failure part-way leaves only unreachable member rows under a
// random id, never a visible cohort with a partial list.
func (s *Service) CreateStatic(ctx context.Context, siteID, name, description string, ids []string) (*Cohort, error) {
	if siteID == "" || strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("site_id and name required")
	}
	norm, err := NormalizeIDs(ids)
	if err != nil {
		return nil, err
	}
	id := genID()
	now := time.Now().UTC().UnixMilli()
	rows := make([]memberRow, len(norm))
	for i, e := range norm {
		rows[i] = memberRow{id: e, addedAt: now}
	}
	if err := s.insertMembers(ctx, siteID, id, rows, now); err != nil {
		return nil, err
	}
	c := Cohort{
		CohortID: id, TenantID: "default", SiteID: siteID,
		Name: name, Description: description, Rule: staticRule,
		MemberCount: int64(len(norm)), CreatedAt: now, UpdatedAt: now,
	}
	if err := s.putCohortRow(ctx, c); err != nil {
		return nil, err
	}
	return &c, nil
}

// getStatic loads a cohort and requires it to be static.
func (s *Service) getStatic(ctx context.Context, siteID, cohortID string) (*Cohort, error) {
	c, err := s.Get(ctx, siteID, cohortID)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, ErrNotFound
	}
	def, err := ParseDefinition(c.Rule)
	if err != nil {
		return nil, err
	}
	if !def.IsStatic() {
		return nil, fmt.Errorf("%w: members of a rule cohort come from its rule", ErrNotStatic)
	}
	return c, nil
}

// AddMembers adds ids to a static cohort and returns the updated cohort and
// how many were new. The resulting list may not exceed MaxStaticMembers.
func (s *Service) AddMembers(ctx context.Context, siteID, cohortID string, ids []string) (*Cohort, int, error) {
	c, err := s.getStatic(ctx, siteID, cohortID)
	if err != nil {
		return nil, 0, err
	}
	norm, err := NormalizeIDs(ids)
	if err != nil {
		return nil, 0, err
	}
	live, err := s.readStatic(ctx, siteID, cohortID)
	if err != nil {
		return nil, 0, err
	}
	version := time.Now().UTC().UnixMilli()
	if version <= c.UpdatedAt {
		version = c.UpdatedAt + 1
	}
	var fresh []memberRow
	for _, e := range norm {
		if _, ok := live[e]; !ok {
			fresh = append(fresh, memberRow{id: e, addedAt: version})
		}
	}
	if len(live)+len(fresh) > MaxStaticMembers {
		return nil, 0, fmt.Errorf("%w: cohort would hold %d members, limit %d", ErrTooManyIDs, len(live)+len(fresh), MaxStaticMembers)
	}
	if len(fresh) == 0 {
		return c, 0, nil
	}
	if err := s.insertMembers(ctx, siteID, cohortID, fresh, version); err != nil {
		return nil, 0, err
	}
	next := *c
	next.MemberCount = int64(len(live) + len(fresh))
	next.UpdatedAt = version
	if err := s.putCohortRow(ctx, next); err != nil {
		return nil, 0, err
	}
	return &next, len(fresh), nil
}

// RemoveMembers removes ids from a static cohort (tombstone rows at a newer
// version) and returns the updated cohort and how many were removed.
func (s *Service) RemoveMembers(ctx context.Context, siteID, cohortID string, ids []string) (*Cohort, int, error) {
	c, err := s.getStatic(ctx, siteID, cohortID)
	if err != nil {
		return nil, 0, err
	}
	norm, err := NormalizeIDs(ids)
	if err != nil {
		return nil, 0, err
	}
	live, err := s.readStatic(ctx, siteID, cohortID)
	if err != nil {
		return nil, 0, err
	}
	version := time.Now().UTC().UnixMilli()
	if version <= c.UpdatedAt {
		version = c.UpdatedAt + 1
	}
	var gone []memberRow
	for _, e := range norm {
		if added, ok := live[e]; ok {
			gone = append(gone, memberRow{id: e, addedAt: added, removed: 1})
		}
	}
	if len(gone) == 0 {
		return c, 0, nil
	}
	if err := s.insertMembers(ctx, siteID, cohortID, gone, version); err != nil {
		return nil, 0, err
	}
	next := *c
	next.MemberCount = int64(len(live) - len(gone))
	next.UpdatedAt = version
	if err := s.putCohortRow(ctx, next); err != nil {
		return nil, 0, err
	}
	return &next, len(gone), nil
}
