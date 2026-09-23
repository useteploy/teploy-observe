package errors

// O01 implementation slice 2 (docs/O01_DURABLE_INGEST_ADR.md §5.6, §5.4):
// the durable, idempotent error inbox. Identity contract:
//
//   - Producers that can mint a stable id send `event_id` (validated
//     alphabet below; the sentry-shim reuses the Sentry-shaped id it
//     already mints, the browser SDK and tracker mint one per capture).
//     `producer_id` is optional namespace; the scoped key is
//     (site_id, producer_id, event_id).
//   - The payload digest is sha256 over the frozen request body computed
//     at admission — the exact bytes the WAL frame carries — so replayed
//     frames and retried requests derive the same digest by construction.
//   - Producers without an event_id keep today's semantics: they get the
//     durable WAL ack and exactly-once SERVER-side replay, but a client
//     retry re-applies (documented v1 posture, same as identity-less
//     analytics events). The server never fabricates cross-request
//     identity it cannot honestly promise.
//
// Dedupe layers (mirroring the events design):
//  1. In-process admission cache (bounded, TTL) — the fast path that
//     turns a response-lost retry into {ok, deduped:true} and a
//     conflicting reuse into 409 while the process still knows the id.
//  2. Durable error_inbox ledger checked and claimed INSIDE the same
//     transaction as the error_events insert — the arbiter across
//     restarts, WAL replay, and admission-cache misses.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sync"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"
)

// eventIDAlphabet is the same bounded identity alphabet the replay v2
// fields use (R27): [A-Za-z0-9_-]{8,64}. Sentry event_ids (32 hex) and
// the SDKs' makeId() output fit.
var eventIDAlphabet = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

// ErrInvalidEventID is a boundary rejection (400): a producer-supplied
// identity field that cannot be a stable key.
var ErrInvalidEventID = errors.New("event_id/producer_id must match [A-Za-z0-9_-]{8,64}")

// ErrEventIDConflict is the 409-class rejection (ADR §5.6): the same
// (site, producer, event_id) was admitted with a DIFFERENT payload
// digest. Conflicting reuse is a producer bug; it is counted, never
// silently merged.
var ErrEventIDConflict = errors.New("event_id reused with different payload")

// ErrAdmittedDuplicate tells the handler to answer 200 {ok:true,
// deduped:true} — the request is a retry this process already admitted
// (same identity, same digest); zero new writes.
var ErrAdmittedDuplicate = errors.New("event already admitted (deduplicated)")

// ValidateEventIdentity checks producer-supplied identity fields. Empty
// event_id means "identity-less producer" and is legal.
func ValidateEventIdentity(eventID, producerID string) error {
	if eventID != "" && !eventIDAlphabet.MatchString(eventID) {
		return fmt.Errorf("%w: got event_id %q", ErrInvalidEventID, eventID)
	}
	if producerID != "" && !eventIDAlphabet.MatchString(producerID) {
		return fmt.Errorf("%w: got producer_id %q", ErrInvalidEventID, producerID)
	}
	return nil
}

func digestBytes(body []byte) string {
	h := sha256.Sum256(body)
	return hex.EncodeToString(h[:])
}

// errorRecord is the WAL/inbox envelope: the frozen body plus the
// identity the server derived at admission. One per WAL record frame.
type errorRecord struct {
	SiteID     string          `json:"site_id"`
	ProducerID string          `json:"producer_id,omitempty"`
	EventID    string          `json:"event_id,omitempty"`
	Digest     string          `json:"digest,omitempty"`
	Body       json.RawMessage `json:"body"`
}

// InboxOutcome is the flush-time disposition of an identified record.
type InboxOutcome string

const (
	InboxApplied  InboxOutcome = "applied"
	InboxDeduped  InboxOutcome = "deduped"
	InboxConflict InboxOutcome = "conflict"
)

// ApplyInbox applies one error record under the inbox contract (ADR
// §5.6). For identity-less records it is exactly IngestErrorEvent. For
// identified records the error_inbox lookup, the inbox claim, and the
// error_events insert commit in ONE transaction:
//
//   - ledger hit + same digest  -> InboxDeduped, zero writes
//   - ledger hit + other digest -> InboxConflict, zero writes (counted
//     upstream; a post-restart conflicting reuse lands here)
//   - miss -> claim + issue resolution + error_events insert -> COMMIT
//
// A transient failure returns a non-nil error and NO ledger row — the
// caller keeps the record PENDING and retries the identical apply.
//
// ResolveIssue's counter/cache writes ride the pool (autocommit) beside
// the transaction, as they do on the legacy path: issue event_count is
// recomputed from error_events on read, so a rolled-back attempt
// self-heals — the same exposure class the drop-on-failure path always
// had, now bounded by retry instead of finalized by loss.
func (s *Service) ApplyInbox(ctx context.Context, input ErrorInput, producerID, eventID, digest string) (InboxOutcome, string, error) {
	if eventID == "" {
		issueID, err := s.IngestErrorEvent(ctx, input)
		if err != nil {
			return "", "", err
		}
		return InboxApplied, issueID, nil
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return "", "", fmt.Errorf("error inbox begin: %w", err)
	}
	type inboxRow struct {
		PayloadSHA string `db:"payload_sha"`
	}
	rows, err := nucleus.Query[inboxRow](ctx, tx.SQL(),
		`SELECT payload_sha FROM error_inbox
		 WHERE tenant_id = 'default' AND site_id = $1 AND producer_id = $2 AND event_id = $3`,
		input.SiteID, producerID, eventID)
	if err != nil {
		_ = tx.Rollback(ctx)
		return "", "", fmt.Errorf("error inbox lookup: %w", err)
	}
	if len(rows) > 0 {
		_ = tx.Rollback(ctx)
		if rows[0].PayloadSHA == digest {
			return InboxDeduped, "", nil
		}
		if rows[0].PayloadSHA == "" {
			// Nucleus resolves an unknown column to NULL rather than
			// erroring; an unreadable digest must never masquerade as a
			// conflict. Surface it as a retryable failure instead.
			return "", "", fmt.Errorf("error inbox row for event %s has no readable digest — schema/migration mismatch", eventID)
		}
		return InboxConflict, "", nil
	}

	errorID, issueID, err := s.insertErrorEvent(ctx, tx.SQL(), input)
	if err != nil {
		_ = tx.Rollback(ctx)
		return "", "", err
	}
	_, err = tx.SQL().Exec(ctx,
		`INSERT INTO error_inbox (tenant_id, site_id, producer_id, event_id, payload_sha, issue_id, applied_at, version)
		 VALUES ('default', $1, $2, $3, $4, $5, $6, 0)`,
		input.SiteID, producerID, eventID, digest, issueID, time.Now().UTC().UnixMilli())
	if err != nil {
		_ = tx.Rollback(ctx)
		return "", "", fmt.Errorf("error inbox claim: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", "", fmt.Errorf("error inbox commit: %w", err)
	}

	// FTS indexing stays non-fatal and post-commit (the legacy posture:
	// search degrades gracefully, `observe reindex` rebuilds).
	if s.searchSvc != nil {
		if err := s.searchSvc.IndexError(ctx, input.SiteID, errorID, input.ErrorType, input.ErrorValue); err != nil {
			slog.Warn("errors: FTS indexing failed (search will lag until reindex)",
				"site", input.SiteID, "error_id", errorID, "err", err)
		}
	}
	return InboxApplied, issueID, nil
}

// admissionCache is the in-process request-time dedupe layer (the errors
// twin of ingest.BatchDeduper): key (site, producer, event_id) -> the
// digest it was admitted with. One digest per key — an errors event_id
// names exactly one payload; a different digest is a conflict, not a
// repack. Bounded and TTL'd; its loss (restart, expiry, eviction) never
// produces duplicates, only a redundant admission the inbox ledger then
// dedupes at flush.
type admissionCache struct {
	mu    sync.Mutex
	seen  map[string]admissionEntry
	order []string
	head  int
	ttl   time.Duration
	max   int
	now   func() time.Time
}

type admissionEntry struct {
	digest   string
	admitted time.Time
}

func newAdmissionCache(ttl time.Duration, max int) *admissionCache {
	return &admissionCache{
		seen: make(map[string]admissionEntry),
		ttl:  ttl,
		max:  max,
		now:  time.Now,
	}
}

func admissionKey(site, producer, event string) string {
	return site + "\x00" + producer + "\x00" + event
}

// lookup returns miss / duplicate / conflict for the key. A duplicate
// refreshes the entry's TTL (a busy retrying producer cannot slide its
// own id out from under itself).
func (c *admissionCache) lookup(site, producer, event, digest string) (duplicate, conflict bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.seen[admissionKey(site, producer, event)]
	if !ok || c.now().Sub(e.admitted) >= c.ttl {
		return false, false
	}
	if e.digest == digest {
		e.admitted = c.now()
		c.seen[admissionKey(site, producer, event)] = e
		return true, false
	}
	return false, true
}

// record remembers an admission — ONLY after it succeeded, so a refused
// first attempt stays retryable. Bounded ring over keys.
func (c *admissionCache) record(site, producer, event, digest string) {
	key := admissionKey(site, producer, event)
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.seen[key]; ok {
		c.seen[key] = admissionEntry{digest: digest, admitted: c.now()}
		return
	}
	c.seen[key] = admissionEntry{digest: digest, admitted: c.now()}
	if len(c.order) < c.max {
		c.order = append(c.order, key)
		return
	}
	delete(c.seen, c.order[c.head])
	c.order[c.head] = key
	c.head = (c.head + 1) % c.max
}
