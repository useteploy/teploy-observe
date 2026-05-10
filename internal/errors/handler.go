package errors

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/identity"
	"github.com/useteploy/teploy-observe/internal/sourcemaps"
)

// ErrorInput is the JSON body for POST /api/v1/errors (SDK envelope format).
type ErrorInput struct {
	SiteID      string       `json:"site_id"`
	SessionID   string       `json:"session_id"`
	ReplayID    string       `json:"replay_id"`
	// DistinctID, when present, is the user identifier from identify().
	// The server hashes it with the site's session_salt before storage.
	DistinctID  string       `json:"distinct_id,omitempty"`
	ErrorType   string       `json:"error_type"`
	ErrorValue  string       `json:"error_value"`
	Mechanism   string       `json:"mechanism"`
	Handled     bool         `json:"handled"`
	Level       string       `json:"level"`
	ReleaseTag  string       `json:"release"`
	Environment string       `json:"environment"`
	URL         string       `json:"url"`
	Browser     string       `json:"browser"`
	OS          string       `json:"os"`
	Device      string       `json:"device"`
	StackTrace  []StackFrame `json:"stack_trace"`
	Breadcrumbs []Breadcrumb `json:"breadcrumbs"`
	Contexts    any          `json:"contexts"`
	Extra       any          `json:"extra"`
	Fingerprint []string     `json:"fingerprint"`
	// Selector identifies the synthetic source of the error (e.g. the DOM
	// selector for a RageClick auto-issue). Used by grouping for non-stack
	// errors so multiple rage clicks on the same target collapse together.
	Selector string `json:"selector"`
}

// Breadcrumb is a user action that preceded the error.
type Breadcrumb struct {
	Type      string `json:"type"`
	Category  string `json:"category"`
	Message   string `json:"message"`
	Data      any    `json:"data,omitempty"`
	Timestamp int64  `json:"timestamp"`
	Level     string `json:"level,omitempty"`
}

// ErrorResponse is returned to the SDK.
type ErrorResponse struct {
	OK      bool   `json:"ok"`
	IssueID string `json:"issue_id,omitempty"`
}

// PrivacyLookup resolves the per-site distinct-id hashing config without
// the errors package needing to import internal/sites (which would create
// an import cycle in tests). Returned (salt, rawOptIn, ok); ok=false
// means "site unknown — caller should not hash".
type PrivacyLookup func(ctx context.Context, siteID string) (salt string, rawOptIn bool, ok bool)

// Service is the canonical entry point for inserting error events.
//
// Every code path that records an error (live HTTP ingest via ErrorBuffer,
// the demo seed, ops tooling) MUST call IngestErrorEvent. Direct INSERTs
// into error_events are forbidden because they bypass:
//   - issue resolve/create (so /api/v1/issues misses the row),
//   - FTS indexing (so /api/v1/issues/search returns 500 on "no docs"),
//   - source-map resolution,
//   - distinct_id hashing (so raw user IDs would land in error_events
//     verbatim — privacy regression).
//
// Pre-refactor, the seed inserted directly and search broke on fresh installs.
// Funneling through one method makes the bug class structurally impossible.
type Service struct {
	db        *nucleus.Client
	issueSvc  *IssueService
	searchSvc *SearchService
	srcmapSvc *sourcemaps.SourceMapService
	// privacy is optional. If nil, distinct_id is stored as-is. cmd/observe
	// wires this from sites.SiteService.PrivacyConfig at boot.
	privacy PrivacyLookup
	// fallbackSalt is the global session salt used when a per-site lookup
	// returns ok=false. Empty means "do not hash" — leave distinct_id raw.
	fallbackSalt string
}

// WithPrivacy installs the per-site distinct_id hashing lookup and
// fallback salt. Called once at boot from cmd/observe.
func (s *Service) WithPrivacy(lookup PrivacyLookup, fallbackSalt string) *Service {
	s.privacy = lookup
	s.fallbackSalt = fallbackSalt
	return s
}

// ErrorHandler is the legacy alias retained for callers that still reference
// the old name (HTTP ingest, error buffer). New code should use *Service.
type ErrorHandler = Service

// NewService constructs the canonical ingest service.
func NewService(db *nucleus.Client, issueSvc *IssueService, searchSvc *SearchService, srcmapSvc *sourcemaps.SourceMapService) *Service {
	return &Service{db: db, issueSvc: issueSvc, searchSvc: searchSvc, srcmapSvc: srcmapSvc}
}

// NewErrorHandler is the legacy constructor, kept so existing callers
// don't need to be touched in the same commit. Forwards to NewService.
func NewErrorHandler(db *nucleus.Client, issueSvc *IssueService, searchSvc *SearchService, srcmapSvc *sourcemaps.SourceMapService) *Service {
	return NewService(db, issueSvc, searchSvc, srcmapSvc)
}

// IngestErrorEvent inserts an error event end-to-end:
//   1. compute grouphash (custom fingerprint, rage-click special-case, or stack-based),
//   2. resolve or create the parent issue,
//   3. (optionally) resolve a minified stack via source maps,
//   4. INSERT into error_events,
//   5. index the error message in FTS for BM25 search.
//
// Returns the issue_id so callers can present a link to the user.
func (s *Service) IngestErrorEvent(ctx context.Context, input ErrorInput) (string, error) {
	now := time.Now().UTC()

	if input.ErrorType == "" && input.ErrorValue == "" {
		input.ErrorType = "Error"
		input.ErrorValue = "Unknown error"
	}
	if input.Level == "" {
		input.Level = "error"
	}

	// Compute grouphash
	var groupHash string
	if len(input.Fingerprint) > 0 {
		groupHash = customFingerprint(input.Fingerprint)
	} else if input.ErrorType == "RageClick" {
		// Rage clicks have no stack trace — group by (type + URL + selector)
		// so repeated rage clicks on the same element merge into one issue.
		groupHash = GroupHashRageClick(input.URL, input.Selector)
	} else {
		groupHash = GroupHash(input.ErrorType, input.ErrorValue, input.StackTrace)
	}

	title := IssueTitle(input.ErrorType, input.ErrorValue)
	culprit := IssueCulprit(input.StackTrace)

	// Resolve or create issue
	issueID, err := s.issueSvc.ResolveIssue(ctx, input.SiteID, groupHash, title, culprit, input.Level, input.ReleaseTag, now.UnixMilli())
	if err != nil {
		return "", fmt.Errorf("resolve issue: %w", err)
	}

	// Serialize JSONB fields
	stackJSON := jsonOrEmpty(input.StackTrace)

	// Resolve minified stack trace via source maps (if available)
	if input.ReleaseTag != "" && s.srcmapSvc != nil {
		if resolved, err := s.srcmapSvc.ResolveStackTrace(ctx, input.SiteID, input.ReleaseTag, stackJSON); err == nil {
			stackJSON = resolved
		}
	}

	breadcrumbsJSON := jsonOrEmpty(input.Breadcrumbs)
	contextsJSON := jsonOrEmpty(input.Contexts)
	extraJSON := jsonOrEmpty(input.Extra)

	errorID := genID()
	handled := "true"
	if !input.Handled {
		handled = "false"
	}

	// Resolve distinct_id: hash with the per-site salt if a privacy
	// lookup is wired and the site is known; otherwise fall back to the
	// global salt; otherwise leave raw (test/dev fallback only).
	distinctID := ""
	if input.DistinctID != "" {
		salt := s.fallbackSalt
		rawOptIn := false
		if s.privacy != nil {
			if siteSalt, raw, ok := s.privacy(ctx, input.SiteID); ok {
				salt = siteSalt
				rawOptIn = raw
			}
		}
		if salt == "" && !rawOptIn {
			// No salt anywhere — store as-is rather than hash with empty
			// key (which would give every site the same digest).
			distinctID = input.DistinctID
		} else {
			distinctID = identity.MaybeHashDistinctID(input.DistinctID, salt, rawOptIn)
		}
	}

	// Insert error event
	_, err = s.db.SQL().Exec(ctx,
		`INSERT INTO error_events (
			error_id, tenant_id, site_id, session_id, replay_id, issue_id, group_hash,
			timestamp, error_type, error_value, mechanism, handled, level,
			release_tag, environment, url, browser, os, device,
			stack_trace, breadcrumbs, contexts, extra, distinct_id
		) VALUES ($1,'default',$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23)`,
		errorID, input.SiteID, input.SessionID, input.ReplayID, issueID, groupHash,
		now.UnixMilli(), input.ErrorType, input.ErrorValue, input.Mechanism, handled, input.Level,
		input.ReleaseTag, input.Environment, input.URL, input.Browser, input.OS, input.Device,
		stackJSON, breadcrumbsJSON, contextsJSON, extraJSON, distinctID,
	)
	if err != nil {
		return "", fmt.Errorf("insert error event: %w", err)
	}

	// Index in FTS for BM25 search (non-fatal — search degrades gracefully).
	if s.searchSvc != nil {
		if err := s.searchSvc.IndexError(ctx, errorID, input.ErrorType, input.ErrorValue); err != nil {
			// Log but don't fail the ingestion. Operators can rebuild via
			// `observe reindex` if FTS files are corrupted or missing.
			_ = err
		}
	}

	return issueID, nil
}

// Handle is the legacy HTTP-shape wrapper around IngestErrorEvent. Returns
// the SDK-facing ErrorResponse envelope.
func (s *Service) Handle(ctx context.Context, input ErrorInput) (ErrorResponse, error) {
	issueID, err := s.IngestErrorEvent(ctx, input)
	if err != nil {
		return ErrorResponse{}, err
	}
	return ErrorResponse{OK: true, IssueID: issueID}, nil
}

func customFingerprint(parts []string) string {
	h := md5.New()
	for _, p := range parts {
		fmt.Fprintf(h, "%s|", p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func jsonOrEmpty(v any) string {
	if v == nil {
		return ""
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	s := string(raw)
	if s == "null" || s == "[]" || s == "{}" {
		return ""
	}
	return s
}

func genID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
