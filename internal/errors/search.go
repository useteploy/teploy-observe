package errors

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/neutron-dev/neutron-go/nucleus"
)

// SearchService provides full-text search over error messages using Nucleus FTS.
type SearchService struct {
	db *nucleus.Client
}

func NewSearchService(db *nucleus.Client) *SearchService {
	return &SearchService{db: db}
}

// IndexError indexes an error event's message for full-text search, tagged
// with its site_id as a facet so Search can rank within a single site's
// documents. Uses a KV counter for globally-sequential FTS doc IDs (unique
// across sites), with a reverse mapping from doc_id to error_id stored in KV.
func (s *SearchService) IndexError(ctx context.Context, siteID, errorID, errorType, errorValue string) error {
	kv := s.db.KV()
	fts := s.db.FTS()

	// Generate sequential doc_id
	docID, err := kv.Incr(ctx, "fts:error:seq")
	if err != nil {
		return fmt.Errorf("fts seq incr: %w", err)
	}

	// Store reverse mapping: doc_id -> error_id
	if err := kv.Set(ctx, fmt.Sprintf("fts:error:%d", docID), []byte(errorID)); err != nil {
		return fmt.Errorf("fts mapping set: %w", err)
	}

	// Index the searchable text, partitioned by site so BM25 ranks per-site.
	text := errorType + ": " + errorValue
	if _, err := fts.IndexFaceted(ctx, docID, text, "site_id", siteID); err != nil {
		return fmt.Errorf("fts index: %w", err)
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

	results, err := fts.SearchFilter(ctx, query, "site_id", siteID, nucleus.WithFTSLimit(int64(limit)))
	if err != nil {
		return nil, fmt.Errorf("fts search: %w", err)
	}

	var searchResults []SearchResult
	for _, r := range results {
		// Reverse lookup: doc_id -> error_id
		data, err := kv.Get(ctx, fmt.Sprintf("fts:error:%d", r.DocID))
		if err != nil || data == nil {
			continue
		}
		searchResults = append(searchResults, SearchResult{
			ErrorID: string(data),
			Score:   r.Score,
		})
	}

	return searchResults, nil
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
		if err == nil && len(rows) > 0 {
			events = append(events, rows[0])
		}
	}

	return events, nil
}

// SearchIssues performs FTS and groups results by issue_id, returning matching issues.
func (s *SearchService) SearchIssues(ctx context.Context, siteID, query string, limit int) ([]Issue, error) {
	hits, err := s.Search(ctx, siteID, query, limit*2) // fetch more to account for grouping
	if err != nil {
		return nil, err
	}
	if len(hits) == 0 {
		return nil, nil
	}

	// Collect unique issue_ids from the matched error events
	seenIssues := make(map[string]bool)
	var issueIDs []string
	for _, hit := range hits {
		rows, err := nucleus.Query[struct {
			IssueID string `db:"issue_id"`
		}](ctx, s.db.SQL(),
			`SELECT issue_id FROM error_events WHERE error_id = $1 AND site_id = $2`,
			hit.ErrorID, siteID,
		)
		if err != nil || len(rows) == 0 {
			continue
		}
		id := rows[0].IssueID
		if !seenIssues[id] {
			seenIssues[id] = true
			issueIDs = append(issueIDs, id)
			if len(issueIDs) >= limit {
				break
			}
		}
	}

	// Fetch full issue objects
	var issues []Issue
	for _, id := range issueIDs {
		rows, err := nucleus.Query[Issue](ctx, s.db.SQL(),
			`SELECT issue_id, tenant_id, site_id, group_hash, title, culprit, level, status,
				first_seen, last_seen, event_count, user_count, release_tag, version
			 FROM issues
			 WHERE issue_id = $1 AND site_id = $2`,
			id, siteID,
		)
		if err == nil && len(rows) > 0 {
			issues = append(issues, rows[0])
		}
	}

	return issues, nil
}

// helper used by json scanning
func unmarshalJSON(data string, v any) error {
	if data == "" {
		return nil
	}
	return json.Unmarshal([]byte(data), v)
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
// Idempotent: IndexError uses Incr-derived doc IDs, so re-running creates
// new FTS rows that supersede earlier ones (the lookup map points to the
// latest mapping). For a clean rebuild, drop the FTS files first.
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

	for {
		var rows []row
		var err error
		// Use (timestamp, error_id) as a keyset cursor so we don't paginate
		// with OFFSET (which costs more on every page in Nucleus). Empty
		// cursor on first iteration.
		if siteID == "" {
			if lastTS < 0 {
				rows, err = nucleus.Query[row](ctx, s.db.SQL(),
					fmt.Sprintf(`SELECT error_id, error_type, error_value, site_id,
						CAST(timestamp AS BIGINT) AS timestamp
						FROM error_events
						ORDER BY timestamp ASC, error_id ASC
						LIMIT %d`, batch))
			} else {
				rows, err = nucleus.Query[row](ctx, s.db.SQL(),
					fmt.Sprintf(`SELECT error_id, error_type, error_value, site_id,
						CAST(timestamp AS BIGINT) AS timestamp
						FROM error_events
						WHERE (timestamp > $1)
						   OR (timestamp = $1 AND error_id > $2)
						ORDER BY timestamp ASC, error_id ASC
						LIMIT %d`, batch),
					strconv.FormatInt(lastTS, 10), lastID)
			}
		} else {
			if lastTS < 0 {
				rows, err = nucleus.Query[row](ctx, s.db.SQL(),
					fmt.Sprintf(`SELECT error_id, error_type, error_value, site_id,
						CAST(timestamp AS BIGINT) AS timestamp
						FROM error_events
						WHERE site_id = $1
						ORDER BY timestamp ASC, error_id ASC
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
						ORDER BY timestamp ASC, error_id ASC
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
