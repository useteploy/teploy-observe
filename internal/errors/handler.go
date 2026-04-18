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

	"github.com/useteploy/teploy-observe/internal/sourcemaps"
)

// ErrorInput is the JSON body for POST /api/v1/errors (SDK envelope format).
type ErrorInput struct {
	SiteID      string       `json:"site_id"`
	SessionID   string       `json:"session_id"`
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

// ErrorHandler processes incoming error events.
type ErrorHandler struct {
	db        *nucleus.Client
	issueSvc  *IssueService
	searchSvc *SearchService
	srcmapSvc *sourcemaps.SourceMapService
}

func NewErrorHandler(db *nucleus.Client, issueSvc *IssueService, searchSvc *SearchService, srcmapSvc *sourcemaps.SourceMapService) *ErrorHandler {
	return &ErrorHandler{db: db, issueSvc: issueSvc, searchSvc: searchSvc, srcmapSvc: srcmapSvc}
}

// Handle processes a single error event: compute grouphash, resolve issue, store event.
func (h *ErrorHandler) Handle(ctx context.Context, input ErrorInput) (ErrorResponse, error) {
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
	} else {
		groupHash = GroupHash(input.ErrorType, input.ErrorValue, input.StackTrace)
	}

	title := IssueTitle(input.ErrorType, input.ErrorValue)
	culprit := IssueCulprit(input.StackTrace)

	// Resolve or create issue
	issueID, err := h.issueSvc.ResolveIssue(ctx, input.SiteID, groupHash, title, culprit, input.Level, input.ReleaseTag, now.UnixMilli())
	if err != nil {
		return ErrorResponse{}, fmt.Errorf("resolve issue: %w", err)
	}

	// Serialize JSONB fields
	stackJSON := jsonOrEmpty(input.StackTrace)

	// Resolve minified stack trace via source maps (if available)
	if input.ReleaseTag != "" && h.srcmapSvc != nil {
		if resolved, err := h.srcmapSvc.ResolveStackTrace(ctx, input.SiteID, input.ReleaseTag, stackJSON); err == nil {
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

	// Insert error event
	_, err = h.db.SQL().Exec(ctx,
		`INSERT INTO error_events (
			error_id, tenant_id, site_id, session_id, issue_id, group_hash,
			timestamp, error_type, error_value, mechanism, handled, level,
			release_tag, environment, url, browser, os, device,
			stack_trace, breadcrumbs, contexts, extra
		) VALUES ($1,'default',$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)`,
		errorID, input.SiteID, input.SessionID, issueID, groupHash,
		now.UnixMilli(), input.ErrorType, input.ErrorValue, input.Mechanism, handled, input.Level,
		input.ReleaseTag, input.Environment, input.URL, input.Browser, input.OS, input.Device,
		stackJSON, breadcrumbsJSON, contextsJSON, extraJSON,
	)
	if err != nil {
		return ErrorResponse{}, fmt.Errorf("insert error event: %w", err)
	}

	// Index in FTS for BM25 search (non-fatal)
	if h.searchSvc != nil {
		if err := h.searchSvc.IndexError(ctx, errorID, input.ErrorType, input.ErrorValue); err != nil {
			// Log but don't fail the ingestion
			_ = err
		}
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
