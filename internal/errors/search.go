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

// IndexError indexes an error event's message for full-text search.
// Uses a KV counter for sequential FTS doc IDs, with a reverse mapping
// from doc_id to error_id stored in KV.
func (s *SearchService) IndexError(ctx context.Context, errorID, errorType, errorValue string) error {
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

	// Index the searchable text
	text := errorType + ": " + errorValue
	if _, err := fts.Index(ctx, docID, text); err != nil {
		return fmt.Errorf("fts index: %w", err)
	}

	return nil
}

// SearchResult represents a search hit with the matched error event.
type SearchResult struct {
	ErrorID string  `json:"error_id"`
	Score   float64 `json:"score"`
}

// Search performs a BM25-ranked full-text search across error messages.
func (s *SearchService) Search(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	if limit <= 0 {
		limit = 20
	}
	fts := s.db.FTS()
	kv := s.db.KV()

	results, err := fts.Search(ctx, query, nucleus.WithFTSLimit(int64(limit)), nucleus.WithFuzzy(1))
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
	hits, err := s.Search(ctx, query, limit)
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
			`SELECT error_id, tenant_id, site_id, session_id, issue_id, group_hash,
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
	hits, err := s.Search(ctx, query, limit*2) // fetch more to account for grouping
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

// ErrorCount returns the total number of error events for a site in a time range.
func (s *SearchService) ErrorCount(ctx context.Context, siteID string, fromMs, toMs int64) (int64, error) {
	type countResult struct {
		Count int64 `db:"count"`
	}
	rows, err := nucleus.Query[countResult](ctx, s.db.SQL(),
		`SELECT COUNT(*) AS count FROM error_events
		 WHERE site_id = $1 AND timestamp >= CAST($2 AS BIGINT) AND timestamp < CAST($3 AS BIGINT)`,
		siteID, strconv.FormatInt(fromMs, 10), strconv.FormatInt(toMs, 10),
	)
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	return rows[0].Count, nil
}
