package errors

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"
)

// SearchService provides full-text search over error messages using Nucleus FTS.
type SearchService struct {
	db *nucleus.Client
	mu sync.Mutex
	// Instance-local boundary fault injection; nil in production.
	before func(stage string) error
}

func NewSearchService(db *nucleus.Client) *SearchService {
	return &SearchService{db: db}
}

// IndexError indexes an error event's message for full-text search, tagged
// with its site_id as a facet so Search can rank within a single site's
// documents. Uses a KV counter for globally-sequential FTS doc IDs (unique
// across sites), with a reverse mapping from doc_id to error_id stored in KV.
func (s *SearchService) IndexError(ctx context.Context, siteID, errorID, errorType, errorValue string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.indexErrorLocked(ctx, siteID, errorID, errorType, errorValue)
}

func (s *SearchService) step(ctx context.Context, stage string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.before != nil {
		return s.before(stage)
	}
	return nil
}

func (s *SearchService) indexErrorLocked(ctx context.Context, siteID, errorID, errorType, errorValue string) error {
	kv := s.db.KV()
	fts := s.db.FTS()

	// Keep one durable document identity per site/event, including retries.
	key := eventDocumentKey(siteID, errorID)
	data, err := kv.Get(ctx, key)
	if err != nil {
		return fmt.Errorf("fts identity get: %w", err)
	}
	var docID int64
	if data != nil {
		docID, err = strconv.ParseInt(string(data), 10, 64)
		if err != nil {
			return fmt.Errorf("fts identity decode: %w", err)
		}
	} else {
		docID, err = kv.Incr(ctx, "fts:error:seq")
		if err != nil {
			return fmt.Errorf("fts seq incr: %w", err)
		}
		if err := s.step(ctx, "forward"); err != nil {
			return err
		}
		if err := kv.Set(ctx, key, []byte(strconv.FormatInt(docID, 10))); err != nil {
			return fmt.Errorf("fts identity set: %w", err)
		}
	}

	if docID <= 0 {
		return fmt.Errorf("fts identity must be positive")
	}
	// Store reverse mapping: doc_id -> error_id
	if err := s.step(ctx, "reverse"); err != nil {
		return err
	}
	if err := kv.Set(ctx, fmt.Sprintf("fts:error:%d", docID), []byte(errorID)); err != nil {
		return fmt.Errorf("fts mapping set: %w", err)
	}

	// Index the searchable text, partitioned by site so BM25 ranks per-site.
	text := errorType + ": " + errorValue
	if err := s.step(ctx, "index"); err != nil {
		return err
	}
	ok, err := fts.IndexFaceted(ctx, docID, text, "site_id", siteID)
	if err != nil {
		return fmt.Errorf("fts index: %w", err)
	}
	if !ok {
		return fmt.Errorf("fts index was not accepted")
	}

	return nil
}

// SearchResult represents a search hit with the matched error event.
type SearchResult struct {
	ErrorID string  `json:"error_id"`
	Score   float64 `json:"score"`
}

// Search performs a BM25-ranked full-text search across a single site's error
// messages. Scoping by the site_id facet keeps one busy site's hits from
// crowding another site out of the result budget (the whole-instance index is
// shared). Note: the site-scoped path does not fuzzy-match (the engine's
// faceted search is exact-term) — a deliberate trade of fuzziness for
// per-site completeness.
func (s *SearchService) Search(ctx context.Context, siteID, query string, limit int) ([]SearchResult, error) {
	if limit <= 0 {
		limit = 20
	}
	fts := s.db.FTS()
	kv := s.db.KV()

	// Legacy/retry survivors may share an event identity. Grow the ranked
	// site-local prefix until the requested UNIQUE event budget is filled or
	// the engine exhausts it. Never claim a complete budget after a read fault.
	budget := int64(limit)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		results, err := fts.SearchFilter(ctx, query, "site_id", siteID, nucleus.WithFTSLimit(budget))
		if err != nil {
			return nil, fmt.Errorf("fts search: %w", err)
		}
		var searchResults []SearchResult
		seen := map[string]bool{}
		for _, r := range results {
			data, err := kv.Get(ctx, fmt.Sprintf("fts:error:%d", r.DocID))
			if err != nil {
				return nil, fmt.Errorf("fts reverse lookup: %w", err)
			}
			if data == nil || seen[string(data)] {
				continue
			}
			seen[string(data)] = true
			searchResults = append(searchResults, SearchResult{ErrorID: string(data), Score: r.Score})
			if len(searchResults) == limit {
				return searchResults, nil
			}
		}
		if int64(len(results)) < budget {
			return searchResults, nil
		}
		if budget > (1<<63-1)/2 {
			return nil, fmt.Errorf("fts unique result budget overflow")
		}
		budget *= 2
	}
}

// SearchErrors performs FTS and then fetches the full error events.
func (s *SearchService) SearchErrors(ctx context.Context, siteID, query string, limit int) ([]ErrorEvent, error) {
	hits, err := s.Search(ctx, siteID, query, limit)
	if err != nil {
		return nil, err
	}
	if len(hits) == 0 {
		return nil, nil
	}

	// Fetch error events by ID — no IN clause support in Nucleus SimpleProtocol,
	// so query one at a time (acceptable for search result sets < 50)
	var events []ErrorEvent
	for _, hit := range hits {
		rows, err := nucleus.Query[ErrorEvent](ctx, s.db.SQL(),
			`SELECT error_id, tenant_id, site_id, session_id,
				COALESCE(replay_id, '') AS replay_id,
				issue_id, group_hash,
				CAST(timestamp AS TEXT) AS timestamp,
				error_type, error_value, mechanism,
				COALESCE(handled, 'true') AS handled, level, release_tag, environment, url,
				browser, os, device,
				COALESCE(stack_trace, '') AS stack_trace,
				COALESCE(breadcrumbs, '') AS breadcrumbs,
				COALESCE(contexts, '') AS contexts,
				COALESCE(extra, '') AS extra
			 FROM error_events
			 WHERE error_id = $1 AND site_id = $2`,
			hit.ErrorID, siteID,
		)
		if err != nil {
			return nil, fmt.Errorf("fts hydrate event: %w", err)
		}
		if len(rows) > 0 {
			events = append(events, rows[0])
		}
	}

	return events, nil
}

// SearchIssues performs FTS and groups results by issue_id, returning matching issues.
func (s *SearchService) SearchIssues(ctx context.Context, siteID, query string, limit int) ([]Issue, error) {
	if limit <= 0 {
		limit = 20
	}
	budget := limit
	var issueIDs []string
	for {
		hits, err := s.Search(ctx, siteID, query, budget)
		if err != nil {
			return nil, err
		}
		seenIssues := make(map[string]bool)
		issueIDs = nil
		for _, hit := range hits {
			rows, err := nucleus.Query[struct {
				IssueID string `db:"issue_id"`
			}](ctx, s.db.SQL(),
				`SELECT issue_id FROM error_events WHERE error_id = $1 AND site_id = $2`, hit.ErrorID, siteID)
			if err != nil {
				return nil, fmt.Errorf("fts hydrate issue identity: %w", err)
			}
			if len(rows) == 0 || seenIssues[rows[0].IssueID] {
				continue
			}
			seenIssues[rows[0].IssueID] = true
			issueIDs = append(issueIDs, rows[0].IssueID)
			if len(issueIDs) == limit {
				break
			}
		}
		if len(issueIDs) == limit || len(hits) < budget {
			break
		}
		if budget > int(^uint(0)>>1)/2 {
			return nil, fmt.Errorf("fts issue result budget overflow")
		}
		budget *= 2
	}

	// Fetch full issue objects
	var issues []Issue
	for _, id := range issueIDs {
		rows, err := nucleus.Query[issueScan](ctx, s.db.SQL(),
			`SELECT `+issueSelectCols+`
			 FROM `+issuesLatest("issue_id = $1 AND site_id = $2"),
			id, siteID,
		)
		if err != nil {
			return nil, fmt.Errorf("fts hydrate issue: %w", err)
		}
		if len(rows) > 0 {
			issues = append(issues, rows[0].toIssue(time.Now().UTC()))
		}
	}

	return issues, nil
}

// ReindexProgress reports how much of the reindex pass has run.
type ReindexProgress struct {
	Scanned int64 // rows read from error_events
	Indexed int64 // rows submitted to FTS (always == Scanned in non-dry-run mode)
}

// ReindexAll rebuilds the FTS index from error_events, in batches of `batch`
// rows ordered by (site_id, timestamp). If `siteID` is empty, every site is
// scanned. If `dryRun` is true, the function reads but does NOT call
// IndexError — useful for verifying that the source rows look sane before
// reindexing in earnest.
//
// The progress callback fires every `reportEvery` rows so callers can log
// without re-tailing the error_events table.
//
// Idempotent: each event reuses its durable FTS document identity.
func (s *SearchService) ReindexAll(
	ctx context.Context,
	siteID string,
	batch int,
	dryRun bool,
	reportEvery int64,
	progress func(p ReindexProgress),
) (ReindexProgress, error) {
	if batch <= 0 {
		batch = 1000
	}
	if reportEvery <= 0 {
		reportEvery = int64(batch)
	}
	if !dryRun {
		if err := s.adoptLegacyDocuments(ctx, siteID); err != nil {
			return ReindexProgress{}, err
		}
	}

	type row struct {
		ErrorID    string `db:"error_id"`
		ErrorType  string `db:"error_type"`
		ErrorValue string `db:"error_value"`
		SiteID     string `db:"site_id"`
		Timestamp  int64  `db:"timestamp"`
	}

	var p ReindexProgress
	var lastTS int64 = -1
	var lastID string = ""
	var lastSite string

	for {
		if err := s.step(ctx, "cursor"); err != nil {
			return p, err
		}
		var rows []row
		var err error
		// Use (timestamp, error_id, site_id) as a keyset cursor so we don't paginate
		// with OFFSET (which costs more on every page in Nucleus). Empty
		// cursor on first iteration.
		if siteID == "" {
			if lastTS < 0 {
				rows, err = nucleus.Query[row](ctx, s.db.SQL(),
					fmt.Sprintf(`SELECT error_id, error_type, error_value, site_id,
						CAST(timestamp AS BIGINT) AS timestamp
						FROM error_events
						ORDER BY timestamp ASC, error_id ASC, site_id ASC
						LIMIT %d`, batch))
			} else {
				rows, err = nucleus.Query[row](ctx, s.db.SQL(),
					fmt.Sprintf(`SELECT error_id, error_type, error_value, site_id,
						CAST(timestamp AS BIGINT) AS timestamp
						FROM error_events
						WHERE (timestamp > $1)
						   OR (timestamp = $1 AND error_id > $2)
					   OR (timestamp = $1 AND error_id = $2 AND site_id > $3)
						ORDER BY timestamp ASC, error_id ASC, site_id ASC
						LIMIT %d`, batch),
					strconv.FormatInt(lastTS, 10), lastID, lastSite)
			}
		} else {
			if lastTS < 0 {
				rows, err = nucleus.Query[row](ctx, s.db.SQL(),
					fmt.Sprintf(`SELECT error_id, error_type, error_value, site_id,
						CAST(timestamp AS BIGINT) AS timestamp
						FROM error_events
						WHERE site_id = $1
						ORDER BY timestamp ASC, error_id ASC, site_id ASC
						LIMIT %d`, batch),
					siteID)
			} else {
				rows, err = nucleus.Query[row](ctx, s.db.SQL(),
					fmt.Sprintf(`SELECT error_id, error_type, error_value, site_id,
						CAST(timestamp AS BIGINT) AS timestamp
						FROM error_events
						WHERE site_id = $1
						  AND ((timestamp > $2)
						       OR (timestamp = $2 AND error_id > $3))
						ORDER BY timestamp ASC, error_id ASC, site_id ASC
						LIMIT %d`, batch),
					siteID, strconv.FormatInt(lastTS, 10), lastID)
			}
		}
		if err != nil {
			return p, fmt.Errorf("reindex: scan: %w", err)
		}
		if len(rows) == 0 {
			break
		}

		for _, r := range rows {
			p.Scanned++
			if !dryRun {
				if err := s.IndexError(ctx, r.SiteID, r.ErrorID, r.ErrorType, r.ErrorValue); err != nil {
					return p, fmt.Errorf("reindex: index error_id=%s: %w", r.ErrorID, err)
				}
				p.Indexed++
			}
			if progress != nil && p.Scanned%reportEvery == 0 {
				progress(p)
			}
		}

		last := rows[len(rows)-1]
		lastTS = last.Timestamp
		lastID = last.ErrorID
		lastSite = last.SiteID

		// Short batch means we drained the cursor.
		if len(rows) < batch {
			break
		}
	}

	if progress != nil && p.Scanned%reportEvery != 0 {
		progress(p)
	}
	return p, nil
}

// ErrorCount returns the total number of error events for a site in a time range.
func (s *SearchService) ErrorCount(ctx context.Context, siteID string, fromMs, toMs int64) (int64, error) {
	type countResult struct {
		Count int64 `db:"count"`
	}
	rows, err := nucleus.Query[countResult](ctx, s.db.SQL(),
		`SELECT COUNT(*) AS count FROM error_events
		 WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3`,
		siteID, strconv.FormatInt(fromMs, 10), strconv.FormatInt(toMs, 10),
	)
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	return rows[0].Count, nil
}

func eventDocumentKey(siteID, errorID string) string {
	return "fts:error:event:" + strconv.Itoa(len(siteID)) + ":" + siteID + ":" + errorID
}

// Reconcile the old append-only reverse mappings before a rebuild. Work is
// streamed, one identity at a time; no whole-index map is retained. A scoped
// rebuild only touches source events in that site. Establish the canonical
// indexed document before removing any redundant document, so interruption
// and restart retain at least one searchable document per event.
func (s *SearchService) adoptLegacyDocuments(ctx context.Context, siteID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	kv, fts := s.db.KV(), s.db.FTS()
	raw, err := kv.Get(ctx, "fts:error:seq")
	if err != nil {
		return fmt.Errorf("fts legacy sequence: %w", err)
	}
	if raw == nil {
		return nil
	}
	end, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		return fmt.Errorf("fts legacy sequence decode: %w", err)
	}
	for docID := int64(1); docID <= end; docID++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		eventID, err := kv.Get(ctx, fmt.Sprintf("fts:error:%d", docID))
		if err != nil {
			return err
		}
		if eventID == nil {
			continue
		}
		query := `SELECT site_id, error_type, error_value FROM error_events WHERE error_id = $1`
		args := []any{string(eventID)}
		if siteID != "" {
			query += ` AND site_id = $2`
			args = append(args, siteID)
		}
		rows, err := nucleus.Query[struct {
			SiteID     string `db:"site_id"`
			ErrorType  string `db:"error_type"`
			ErrorValue string `db:"error_value"`
		}](ctx, s.db.SQL(), query+` LIMIT 1`, args...)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			continue
		}
		key := eventDocumentKey(rows[0].SiteID, string(eventID))
		canonical, err := kv.Get(ctx, key)
		if err != nil {
			return err
		}
		if canonical == nil {
			if err := s.step(ctx, "forward"); err != nil {
				return err
			}
			if err := kv.Set(ctx, key, []byte(strconv.FormatInt(docID, 10))); err != nil {
				return err
			}
			canonical = []byte(strconv.FormatInt(docID, 10))
		}
		id, err := strconv.ParseInt(string(canonical), 10, 64)
		if err != nil {
			return err
		}
		// A durable forward/reverse KV identity is not proof of an indexed
		// document: IndexError may have stopped before IndexFaceted. Rebuild
		// canonical text from SQL and require successful indexing FIRST.
		if err := s.indexErrorLocked(ctx, rows[0].SiteID, string(eventID), rows[0].ErrorType, rows[0].ErrorValue); err != nil {
			return fmt.Errorf("fts legacy canonical replacement: %w", err)
		}
		if id != docID {
			if err := s.step(ctx, "remove"); err != nil {
				return err
			}
			if _, err := fts.Remove(ctx, docID); err != nil {
				return fmt.Errorf("fts legacy duplicate removal: %w", err)
			}
		}
	}
	return nil
}
