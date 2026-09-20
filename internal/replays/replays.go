package replays

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/neutron-dev/neutron-go/neutron"
	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
	"github.com/useteploy/teploy-observe/internal/heatmaps"
	"github.com/useteploy/teploy-observe/internal/identity"
	"github.com/useteploy/teploy-observe/internal/ingest"
	"github.com/useteploy/teploy-observe/internal/query"
)

// ErrCrossSiteReplay indicates the caller tried to append events to a replay
// owned by a different site (audit F08). Handlers map it to 403.
var ErrCrossSiteReplay = errors.New("replay_id belongs to a different site")

// ErrBatchIDReuse indicates a (producer_id, batch_id) pair was resubmitted
// with DIFFERENT event content (F12/F19). A legitimate retry of an
// ambiguous batch replays byte-identical events, so a digest mismatch is a
// producer bug or an intentional replay-ID squat; either way it is refused
// loudly rather than silently dropping the new content as a "duplicate".
var ErrBatchIDReuse = errors.New("batch_id reused with different event content")

// ErrV2BatchNeedsReplayID: the v2 idempotency key is (site, replay, batch);
// a batch without a client-generated replay_id could never be deduplicated
// because the server would assign a fresh replay id on every attempt.
var ErrV2BatchNeedsReplayID = errors.New("v2 batches require a client-generated replay_id")

// replaySessionCols are the non-key columns of replay_sessions, in the order
// the collapse helpers expect (see internal/query/replacing.go). The ORDER BY
// key is (tenant_id, site_id, start_time, replay_id); key columns are selected
// verbatim by LatestRows and must not appear here.
var replaySessionCols = []string{
	"session_id", "duration_ms", "page_count", "url", "browser", "os",
	"device", "has_error", "distinct_id",
}

func replaySessionsLatest(where string) string {
	return query.LatestRows("replay_sessions", replaySessionCols, where) + " AS replay_sessions"
}

// hashDistinctID is a local alias so the call site reads cleanly. The
// real impl lives in internal/identity.
func hashDistinctID(raw, salt string, rawOptIn bool) string {
	return identity.MaybeHashDistinctID(raw, salt, rawOptIn)
}

// PrivacyLookup mirrors errors.PrivacyLookup — see that doc for shape.
// Duplicated here to avoid a replays -> errors import cycle (errors
// already depends on sourcemaps; both packages need the same lookup).
type PrivacyLookup func(ctx context.Context, siteID string) (salt string, rawOptIn bool, ok bool)

type ReplayService struct {
	db       *nucleus.Client
	heatmaps *heatmaps.Service
	logger   *slog.Logger
	privacy  PrivacyLookup
	salt     string
	// replayLocks stripe serialization per replay ID (AUD-018, round 2).
	// The session upsert is a read-merge-insert with no engine-side
	// compare-and-swap: two concurrent first batches could insert different
	// start_times (forking the replacing-table key so the rows never
	// collapse), and concurrent later batches could derive the same next
	// version from the same prior row, losing navigation increments. One
	// striped lock per replay removes the interleaving within a single
	// process; a multi-replica deployment needs the stable-key + CAS design
	// deferred with F03-class schema work.
	replayLocks [64]sync.Mutex
}

// lockReplay serializes all writes for one replay ID. NOT keyed by site:
// the cross-site first-claim check must share the lock with the eventual
// winner's insert.
func (s *ReplayService) lockReplay(replayID string) func() {
	h := fnv.New32a()
	_, _ = h.Write([]byte(replayID))
	mu := &s.replayLocks[h.Sum32()%uint32(len(s.replayLocks))]
	mu.Lock()
	return mu.Unlock
}

func NewReplayService(db *nucleus.Client) *ReplayService {
	return &ReplayService{
		db:       db,
		heatmaps: heatmaps.NewService(db),
		logger:   slog.Default(),
	}
}

// WithPrivacy installs the per-site distinct_id hashing lookup and a
// fallback global salt for sites the lookup doesn't know about.
func (s *ReplayService) WithPrivacy(lookup PrivacyLookup, fallbackSalt string) *ReplayService {
	s.privacy = lookup
	s.salt = fallbackSalt
	return s
}

// WithLogger threads a custom logger so heatmap-rollup write failures
// surface under the same handler context as the replay ingest itself.
func (s *ReplayService) WithLogger(logger *slog.Logger) *ReplayService {
	if logger != nil {
		s.logger = logger
	}
	return s
}

// ReplaySession is the domain type with typed fields.
type ReplaySession struct {
	ReplayID  string    `json:"replay_id"`
	SiteID    string    `json:"site_id"`
	SessionID string    `json:"session_id"`
	StartTime time.Time `json:"start_time"`
	Duration  int64     `json:"duration_ms"`
	PageCount int       `json:"page_count"`
	URL       string    `json:"url"`
	Browser   string    `json:"browser"`
	OS        string    `json:"os"`
	Device    string    `json:"device"`
	HasError  bool      `json:"has_error"`
}

// ReplayEvent is the domain type for replay events.
type ReplayEvent struct {
	EventID   string    `json:"event_id"`
	ReplayID  string    `json:"replay_id"`
	Timestamp time.Time `json:"timestamp"`
	EventType string    `json:"event_type"`
	Data      string    `json:"data"`
}

// IngestInput is the JSON body from the replay SDK.
//
// ViewportWidth is optional; when set it seeds the heatmap aggregator with
// a vw_bucket for clicks that occur before any `resize` event in the
// batch. The replay SDK populates it from `window.innerWidth` at flush
// time (see cmd/observe/tracker/observe-replay.js).
//
// F12/F19 idempotency fields (protocol v2, all optional for v1 producers):
// V is the wire protocol version (2); ProducerID is a stable id for the
// tracker instance (one per page load); BatchID identifies one flushed
// batch and MUST be reused when that same batch is retried. When both
// ProducerID and BatchID are present the batch is deduplicated against the
// replay_batches ledger written in the same transaction as its children,
// and child event ids become deterministic functions of
// (site, replay, batch, index) - so a retry after a lost response or a
// rolled-back transaction can never duplicate children, session
// aggregates, or heatmap contributions.
type IngestInput struct {
	SiteID    string `json:"site_id"`
	SessionID string `json:"session_id"`
	// ReplayID is generated client-side so observe-errors.js can attach
	// errors to the same replay before the first batch reaches the server.
	// Empty -> the server assigns a fresh id (v1 semantics only; a v2
	// batch with idempotency fields and no replay_id is rejected, see
	// ErrV2BatchNeedsReplayID).
	ReplayID      string `json:"replay_id"`
	URL           string `json:"url"`
	Browser       string `json:"browser"`
	OS            string `json:"os"`
	Device        string `json:"device"`
	HasError      bool   `json:"has_error"`
	ViewportWidth int    `json:"viewport_width"`
	// DistinctID, when present, is the user identifier the SDK passed
	// via identify(userId). Hashed with the per-site session_salt
	// before storage.
	DistinctID string `json:"distinct_id,omitempty"`
	V          int    `json:"v,omitempty"`
	ProducerID string `json:"producer_id,omitempty"`
	BatchID    string `json:"batch_id,omitempty"`
	Events     []struct {
		Type      string `json:"type"`
		Timestamp int64  `json:"timestamp"`
		Data      any    `json:"data"`
	} `json:"events"`
}

// idempotent reports whether this batch carries the v2 producer identity
// that enables ledger dedupe (F12/F19).
func (in *IngestInput) idempotent() bool {
	return in.ProducerID != "" && in.BatchID != ""
}

// validateReplayProtocol enforces the v2 contract (TO-019): a request
// declaring v2 — or carrying any identity field — must carry ALL of
// producer_id, batch_id, and replay_id, and the declared version must be
// one this server speaks. A partial identity used to fall back silently to
// non-idempotent v1 writes, which could double-count on retry.
func validateReplayProtocol(in *IngestInput) error {
	if in.V != 0 && in.V != 1 && in.V != 2 {
		return fmt.Errorf("unsupported replay protocol version %d (this server speaks v1 and v2)", in.V)
	}
	carriesIdentity := in.ProducerID != "" || in.BatchID != ""
	if (in.V == 2 || carriesIdentity) && !in.idempotent() {
		return neutron.ErrBadRequest("a v2 replay batch requires both producer_id and batch_id")
	}
	return nil
}

// validReplayIdentities enforces the canonical bounded alphabet on a batch's
// v2 identity fields before its children are written (R27). The ledger-hit
// path above has already returned by the time this runs, so retries of
// committed work are unaffected.
func validReplayIdentities(in *IngestInput, replayID string) error {
	if !in.idempotent() {
		return nil
	}
	for _, pair := range []struct{ name, value string }{
		{"producer_id", in.ProducerID},
		{"batch_id", in.BatchID},
		{"replay_id", replayID},
	} {
		if !validProtocolID(pair.value) {
			return neutron.ErrBadRequest(fmt.Sprintf(
				"%s must be 8-64 characters of [A-Za-z0-9_-] (canonical identity alphabet)", pair.name))
		}
	}
	return nil
}

// batchDigest is the sha256 over the canonical JSON encoding of the batch's
// event list. Go marshals struct fields in declaration order and map keys
// sorted, so the same events always digest identically on producer and
// server retries alike.
func (in *IngestInput) batchDigest() string {
	raw, err := json.Marshal(in.Events)
	if err != nil {
		// Events already marshal-checked per-child during insert; a digest
		// failure here degrades to a per-attempt unique digest (no dedupe)
		// rather than blocking ingest.
		return fmt.Sprintf("undigestable:%d:%v", len(in.Events), err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// validProtocolID reports whether a v2 identity field uses the canonical
// bounded URL-safe alphabet (R27, round 4). The deterministic child id hashes
// a pipe-joined tuple; a '|' in producer_id or batch_id makes different
// tuples collide on the same child id ("producer|extra" + "batch" vs
// "producer" + "extra|batch"). First-party producers emit hex ids, so this
// rejects nothing legitimate while making the encoding collision
// unconstructible for NEW batches.
func validProtocolID(s string) bool {
	if len(s) < 8 || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' ||
			c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// DeterministicChildID derives one replay child event id from
// (site, replay, producer, batch, index) - the F12/F19 stable child
// identity, producer-scoped since TO-019. A retried batch re-derives the
// same ids, so children are natural deduplicates of themselves under any
// future unique-key or CAS scheme.
func DeterministicChildID(siteID, replayID, producerID, batchID string, index int) string {
	sum := sha256.Sum256([]byte("observe-replay-v2|" + siteID + "|" + replayID + "|" + producerID + "|" + batchID + "|" + strconv.Itoa(index)))
	return hex.EncodeToString(sum[:16])
}

// Result is the outcome of one Ingest call.
type Result struct {
	ReplayID string
	// Deduped is true when the batch was recognized as a retry of an
	// already-committed batch (v2 ledger hit with a matching digest) and
	// nothing was written.
	Deduped bool
}

// ledgerRow is the collapsed state of one replay_batches key.
type ledgerRow struct {
	EventCount int64  `db:"event_count"`
	PayloadSHA string `db:"payload_sha"`
}

var replayBatchCols = []string{"event_count", "payload_sha"}

// replayBatchLatest renders the collapsed replay_batches derived table for
// one (site, replay, producer, batch) key.
func replayBatchLatest(where string) string {
	return query.LatestRows("replay_batches", replayBatchCols, where) + " AS replay_batches"
}

// ledgerLookup returns the committed ledger row for a batch key, or nil.
// TO-019: the key is producer-scoped; when the producer-scoped lookup
// misses, a LEGACY row (producer_id=”, written pre-043) for the same
// batch is honored — an in-flight retry of a pre-upgrade batch keeps
// deduplicating across the migration. Legacy rows are never written.
func (s *ReplayService) ledgerLookup(ctx context.Context, sqlc *nucleus.SQLModel, siteID, replayID, producerID, batchID string) (*ledgerRow, error) {
	read := func(producer string) ([]ledgerRow, error) {
		return nucleus.Query[ledgerRow](ctx, sqlc,
			`SELECT event_count, payload_sha FROM `+
				replayBatchLatest("site_id = $1 AND replay_id = $2 AND producer_id = $3 AND batch_id = $4"),
			siteID, replayID, producer, batchID)
	}
	rows, err := read(producerID)
	if err != nil {
		return nil, fmt.Errorf("replays: batch ledger lookup: %w", err)
	}
	if len(rows) == 0 && producerID != "" {
		// Legacy pre-043 row (no producer recorded).
		rows, err = read("")
		if err != nil {
			return nil, fmt.Errorf("replays: batch ledger legacy lookup: %w", err)
		}
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

// existingSession is the collapsed current state of one replay's session row,
// read before writing a new version (see upsertSession).
type existingSession struct {
	SiteID     string `db:"site_id"`
	StartTime  int64  `db:"start_time"`
	DurationMS int64  `db:"duration_ms"`
	PageCount  int64  `db:"page_count"`
	HasError   string `db:"has_error"`
	Version    int64  `db:"version"`
}

func (e existingSession) exists() bool   { return e.SiteID != "" }
func (e existingSession) hasError() bool { return e.HasError == "true" }

// batchAggregate is the metadata one batch contributes to its session row.
// Duration is min..max over the batch's timestamped events (not first/last
// positional — batches are not guaranteed timestamp-ordered); pages count
// navigation events plus the initial page, not raw event count (audit F20:
// 100 mouse events must not become 100 pages).
type batchAggregate struct {
	StartMS     int64
	EndMS       int64
	Navigations int64
	Initialized bool
}

func aggregateBatch(input *IngestInput) batchAggregate {
	var agg batchAggregate
	for _, ev := range input.Events {
		if ev.Timestamp <= 0 {
			continue
		}
		if !agg.Initialized {
			agg.StartMS, agg.EndMS, agg.Initialized = ev.Timestamp, ev.Timestamp, true
		}
		if ev.Timestamp < agg.StartMS {
			agg.StartMS = ev.Timestamp
		}
		if ev.Timestamp > agg.EndMS {
			agg.EndMS = ev.Timestamp
		}
		if ev.Type == "navigation" {
			agg.Navigations++
		}
	}
	return agg
}

// replayOwner resolves the site that owns replayID through the collapsed
// session table. Empty siteID means no session exists yet. A transport error
// is returned (fail closed) — an unavailable store must not read as "no
// owner" and let a cross-site append through.
func (s *ReplayService) replayOwner(ctx context.Context, replayID string) (string, error) {
	rows, err := nucleus.Query[struct {
		SiteID string `db:"site_id"`
	}](ctx, s.db.SQL(),
		`SELECT site_id FROM `+replaySessionsLatest("replay_id = $1"), replayID)
	if err != nil {
		return "", fmt.Errorf("replays: ownership lookup: %w", err)
	}
	for _, r := range rows {
		if r.SiteID != "" {
			return r.SiteID, nil
		}
	}
	return "", nil
}

// upsertSession writes the session row as a new version of the replacing
// table, merging this batch's aggregates into whatever is already recorded
// (audit F20: duration grows to the max seen, has_error is sticky, page_count
// accumulates navigations). Collapsing by (tenant, site, start_time,
// replay_id) keeps one visible row per replay, so there is no claim-then-
// insert window to orphan (audit F19 — the old KV SetNX guard is gone).
//
// AUD-019 (round 2): sqlc is the caller's transaction — the session row and
// the batch's child events commit (or roll back) as ONE unit, so a child
// insert failure can no longer leave updated session counters behind.
func (s *ReplayService) upsertSession(ctx context.Context, sqlc *nucleus.SQLModel, input *IngestInput, replayID string, agg batchAggregate, distinctID string) error {
	// Non-key columns collapse newest-wins via argMax on version — the same
	// collapse LatestRows applies elsewhere; they cannot be selected bare
	// next to this GROUP BY (strict-mode engines reject that, 0A000).
	rows, err := nucleus.Query[existingSession](ctx, sqlc,
		`SELECT site_id, start_time,
		        CAST(argMax(duration_ms, version) AS BIGINT) AS duration_ms,
		        CAST(argMax(page_count, version) AS BIGINT) AS page_count,
		        argMax(has_error, version) AS has_error,
		        MAX(version) AS version
		 FROM replay_sessions
		 WHERE replay_id = $1 AND site_id = $2
		 GROUP BY tenant_id, site_id, start_time, replay_id`, replayID, input.SiteID)
	if err != nil {
		// Fail the batch rather than guess at stored aggregates: a fresh
		// insert under a read error could fork a second visible session
		// (different start_time -> different ORDER BY key -> no collapse).
		return fmt.Errorf("replays: read session state: %w", err)
	}
	var existing existingSession
	if len(rows) > 0 {
		existing = rows[0]
	}

	startTime := agg.StartMS
	if !agg.Initialized {
		startTime = time.Now().UTC().UnixMilli()
	}
	if existing.exists() && existing.StartTime > 0 {
		// Preserve the session's original start_time — it is part of the
		// ORDER BY key, so a version that moved it would not collapse with
		// its predecessors.
		startTime = existing.StartTime
	}
	// Duration spans from the session's start (startTime, preserved from the
	// first batch when merging) to this batch's latest event, so a later
	// batch extends the session rather than reporting only its own span.
	// The max against the stored value keeps it monotonic: a late batch of
	// older events must not shrink the recorded duration.
	duration := agg.EndMS - startTime
	if !agg.Initialized || duration < 0 {
		duration = 0
	}
	if existing.exists() && existing.DurationMS > duration {
		duration = existing.DurationMS
	}
	pages := int64(1) + agg.Navigations
	if existing.exists() {
		pages = existing.PageCount + agg.Navigations
	}
	hasError := input.HasError || (existing.exists() && existing.hasError())

	version := time.Now().UTC().UnixMilli()
	if existing.exists() && existing.Version >= version {
		version = existing.Version + 1
	}

	hasErrStr := "false"
	if hasError {
		hasErrStr = "true"
	}

	// R28 (round 4): the session's captured URL gets the same privacy
	// boundary as analytics (F41) and error ingestion — userinfo, query, and
	// fragment never persist; unparseable/non-http(s) drops. Legacy and
	// third-party replay producers can no longer park password-reset tokens
	// in the session row.
	storedURL := ingest.CapturedURL(input.URL)

	_, err = sqlc.Exec(ctx,
		`INSERT INTO replay_sessions (replay_id, tenant_id, site_id, session_id, start_time,
			duration_ms, page_count, url, browser, os, device, has_error, distinct_id, version)
		 VALUES ($1, 'default', $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		replayID, input.SiteID, input.SessionID, dbutil.IntParam(startTime),
		strconv.FormatInt(duration, 10), strconv.FormatInt(pages, 10),
		storedURL, input.Browser, input.OS, input.Device, hasErrStr, distinctID,
		dbutil.IntParam(version),
	)
	if err != nil {
		return fmt.Errorf("upsert replay session: %w", err)
	}
	return nil
}

// Ingest stores a batch of replay events.
//
// AUD-018 (round 2): the owner check, session upsert, and child inserts
// run under the per-replay striped lock, so concurrent batches for the
// same replay serialize. AUD-019 (round 2): the session row and every
// child event commit in ONE transaction — a mid-batch child failure
// rolls the session metadata back with the children instead of leaving
// updated counters behind a failed batch.
//
// F12/F19 (batch idempotency): a v2 batch (producer_id + batch_id
// present, replay_id client-generated) is deduplicated through the
// replay_batches ledger. The ledger row commits in the SAME transaction
// as the session upsert and the children, so the dedupe boundary is one
// atomic SQL unit - not a KV SetNX followed by an insert, the
// non-atomic pair the audit rejected. A retry of a committed batch hits
// the ledger and returns Deduped without writing; a retry of a batch
// whose transaction rolled back finds no ledger row (nor children, nor
// session - they rolled back together) and re-inserts children with the
// SAME deterministic ids. Session aggregates and heatmap rollups run
// only on the non-duplicate path, so a retry cannot double-count them.
func (s *ReplayService) Ingest(ctx context.Context, input IngestInput) (Result, error) {
	if len(input.Events) == 0 {
		return Result{}, nil
	}
	// TO-019: partial v2 identities and unknown versions are rejected
	// instead of silently degrading to non-idempotent v1 writes.
	if err := validateReplayProtocol(&input); err != nil {
		return Result{}, err
	}

	replayID := input.ReplayID
	if replayID == "" {
		if input.idempotent() {
			return Result{}, ErrV2BatchNeedsReplayID
		}
		replayID = genID()
	}

	unlock := s.lockReplay(replayID)
	defer unlock()

	// Audit F08: the replay's owning site (from its session row) is
	// authoritative. A key valid for site A must not append child events to
	// a replay recorded under site B just by naming its (client-generated)
	// replay ID.
	if input.ReplayID != "" {
		owner, err := s.replayOwner(ctx, replayID)
		if err != nil {
			return Result{}, err
		}
		if owner != "" && owner != input.SiteID {
			return Result{}, fmt.Errorf("%w: replay %s is owned by another site", ErrCrossSiteReplay, replayID)
		}
	}

	agg := aggregateBatch(&input)

	// Resolve and hash the user-supplied distinct_id (if any).
	distinctID := ""
	if input.DistinctID != "" {
		salt := s.salt
		rawOptIn := false
		if s.privacy != nil {
			if siteSalt, raw, ok := s.privacy(ctx, input.SiteID); ok {
				salt = siteSalt
				rawOptIn = raw
			}
		}
		if salt == "" && !rawOptIn {
			// Fail closed (OBS-030): this is a privacy control, not a
			// best-effort one — never fall through to raw storage just
			// because no salt was available. main.go always seeds a random
			// fallback salt at startup so this path isn't reachable through
			// normal wiring today, but the package must not depend on the
			// caller continuing to do that correctly.
			//
			// The identifier is dropped, not the whole batch: the session
			// and its events are still real, valuable data, and rejecting
			// the entire ingest over a hashing-config gap would lose them
			// too. Logged loudly (not silently swallowed) so a persistent
			// salt-configuration gap is actually visible operationally.
			s.logger.Warn("replays: dropping distinct_id — no salt available and site has not opted into raw storage",
				"site", input.SiteID)
		} else {
			distinctID = hashDistinctID(input.DistinctID, salt, rawOptIn)
		}
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("replays: begin ingest tx: %w", err)
	}
	defer tx.Rollback(ctx) // no-op once Commit has succeeded
	sqlc := tx.SQL()

	// F12/F19: the ledger read runs INSIDE the transaction (under the
	// striped replay lock), so for any single process the check and the
	// writes it guards are one serialized unit. A duplicate whose digest
	// matches returns without writing anything.
	if input.idempotent() {
		prior, err := s.ledgerLookup(ctx, sqlc, input.SiteID, replayID, input.ProducerID, input.BatchID)
		if err != nil {
			return Result{}, err
		}
		if prior != nil {
			if prior.PayloadSHA != input.batchDigest() {
				return Result{}, fmt.Errorf("%w: producer %s batch %s", ErrBatchIDReuse, input.ProducerID, input.BatchID)
			}
			return Result{ReplayID: replayID, Deduped: true}, nil
		}
		// R27 (round 4): charset validation runs AFTER the ledger check, so
		// a retry of an already-committed batch (whatever its identity looked
		// like when it was accepted) still dedupes cleanly; only batches
		// about to be WRITTEN must carry canonical, collision-free identities.
		if err := validReplayIdentities(&input, replayID); err != nil {
			return Result{}, err
		}
	}

	if err := s.upsertSession(ctx, sqlc, &input, replayID, agg, distinctID); err != nil {
		return Result{}, err
	}

	// Track the most recent viewport width seen in this batch so click
	// events can carry a vw_bucket without requiring the tracker to
	// re-emit window size on every click. ViewportWidth defaults to the
	// session's `viewport_width` field if the SDK supplied it, else 0.
	currentVW := input.ViewportWidth
	type attributedClick struct {
		page string
		raw  heatmaps.RawEvent
	}
	clickEvents := make([]attributedClick, 0)

	idempotent := input.idempotent()
	for i, ev := range input.Events {
		eventID := genID()
		if idempotent {
			// F12/F19: children of an idempotent batch are named by
			// (site, replay, producer, batch, index) so any re-insert
			// path lands on the same rows.
			eventID = DeterministicChildID(input.SiteID, replayID, input.ProducerID, input.BatchID, i)
		}
		dataJSON := "null"
		if ev.Data != nil {
			if raw, err := json.Marshal(ev.Data); err == nil {
				dataJSON = string(raw)
			}
		}
		// Audit F08: child events carry the authenticated site so two sites
		// reusing the same client-generated replay ID stay disjoint.
		_, err := sqlc.Exec(ctx,
			`INSERT INTO replay_events (event_id, tenant_id, site_id, replay_id, timestamp, event_type, data)
			 VALUES ($1, 'default', $2, $3, $4, $5, $6)`,
			eventID, input.SiteID, replayID, ev.Timestamp, ev.Type, dataJSON,
		)
		if err != nil {
			return Result{}, fmt.Errorf("insert replay event: %w", err)
		}

		switch ev.Type {
		case "resize":
			if w, ok := readIntField(ev.Data, "w"); ok {
				currentVW = w
			}
		case "click":
			// AUD-033 (round 2): a click is attributed to the page it
			// happened on (captured per-click by current trackers), not
			// the URL observed at flush time — a click followed by SPA
			// navigation used to be credited to the post-navigation page.
			// Legacy clicks without page context fall back to the (R28:
			// sanitized) batch URL.
			page := ingest.CapturedURL(input.URL)
			if raw, ok := ev.Data.(map[string]any); ok {
				if pu, ok := raw["page_url"].(string); ok {
					if cleaned := telemetryPageURL(pu); cleaned != "" {
						page = cleaned
					}
				}
				if w, ok := readIntField(ev.Data, "viewport_width"); ok {
					currentVW = w
				}
			}
			clickEvents = append(clickEvents, attributedClick{
				page: page,
				raw: heatmaps.RawEvent{
					Type:          ev.Type,
					Data:          ev.Data,
					ViewportWidth: currentVW,
				},
			})
		}
	}

	// F12/F19: the ledger row commits with the children and the session
	// version it describes. Crash before commit -> no ledger, no children
	// (the tx rolled back) -> a retry reprocesses fully with the same
	// deterministic child ids. Crash or response-loss after commit -> the
	// ledger hit returns Deduped above with zero writes.
	if idempotent {
		now := time.Now().UTC().UnixMilli()
		if _, err := sqlc.Exec(ctx,
			`INSERT INTO replay_batches (tenant_id, site_id, replay_id, producer_id, batch_id, event_count, payload_sha, first_seen, version)
			 VALUES ('default', $1, $2, $3, $4, $5, $6, $7, $7)`,
			input.SiteID, replayID, input.ProducerID, input.BatchID,
			strconv.FormatInt(int64(len(input.Events)), 10),
			input.batchDigest(), dbutil.IntParam(now),
		); err != nil {
			return Result{}, fmt.Errorf("replays: write batch ledger: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return Result{ReplayID: replayID}, fmt.Errorf("replays: commit ingest tx: %w", err)
	}

	// Write the per-bucket heatmap rollups. Best-effort: a heatmap write
	// failure must not fail the underlying replay ingest because the raw
	// event rows are already durable. Pattern matches tracing rollups
	// (see internal/tracing/ingest.go). F12/F19: this path is reached only
	// for batches that were NOT ledger deduped, so a retried batch cannot
	// double-count click heat. A durable derived-work outbox remains
	// deferred (a heatmap write failure here still skips the rollup for
	// that batch - the retry that would redo it is deduped away).
	byPage := make(map[string][]heatmaps.RawEvent, 1)
	for _, c := range clickEvents {
		if c.page == "" {
			continue
		}
		byPage[c.page] = append(byPage[c.page], c.raw)
	}
	for page, raws := range byPage {
		clicks := heatmaps.ExtractClicks(raws)
		if len(clicks) == 0 {
			continue
		}
		if err := s.heatmaps.Aggregate(ctx, input.SiteID, page, clicks); err != nil {
			s.logger.Warn("heatmaps: aggregate failed",
				"site", input.SiteID, "url", page, "err", err)
		}
	}

	return Result{ReplayID: replayID}, nil
}

// telemetryPageURL sanitizes a tracker-captured page URL for heatmap
// bucketing: origin + path only — no userinfo, query, or fragment
// (AUD-030/F41 containment for replay-side URLs).
func telemetryPageURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	u.RawFragment = ""
	u.ForceQuery = false
	return u.String()
}

// readIntField is the same defensive numeric extractor used by the
// heatmaps package, kept here so the resize-tracking shortcut doesn't
// need to import a parser. Returns false on missing or non-numeric
// values.
func readIntField(data any, key string) (int, bool) {
	m, ok := data.(map[string]any)
	if !ok {
		return 0, false
	}
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	}
	return 0, false
}

// maxListReplaysLimit bounds the listing page size (audit F28: limit had a
// default but no cap, so an extreme value produced an extreme query).
const maxListReplaysLimit = 200

// ListReplays returns recent replay sessions for a site, read through the
// version collapse so multi-batch upserts surface as one row per replay.
func (s *ReplayService) ListReplays(ctx context.Context, siteID string, from, to time.Time, limit, offset int) ([]ReplaySession, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > maxListReplaysLimit {
		limit = maxListReplaysLimit
	}
	if offset < 0 {
		offset = 0
	}
	fromMs := dbutil.IntParam(from.UnixMilli())
	toMs := dbutil.IntParam(to.UnixMilli())

	return nucleus.Query[ReplaySession](ctx, s.db.SQL(),
		fmt.Sprintf(`SELECT replay_id, tenant_id, site_id, session_id,
			CAST(start_time AS TEXT) AS start_time,
			duration_ms, page_count, url, browser, os, device, has_error
		 FROM `+replaySessionsLatest("site_id = $1 AND start_time >= $2 AND start_time < $3")+`
		 ORDER BY start_time DESC
		 LIMIT %d OFFSET %d`, limit, offset),
		siteID, fromMs, toMs,
	)
}

// GetReplayEvents returns the events of one replay, scoped to the site that
// owns it (audit F08). Legacy rows written before the site column existed
// carry site_id=” and still belong to the owning session's site.
//
// AUD-020 (round 2): event_id is the deterministic tiebreak — timestamp
// alone left equal-timestamp events in storage order, which shifts between
// reads. Full keyset pagination (+ UI support for windows) stays deferred
// with the player protocol work (F39).
func (s *ReplayService) GetReplayEvents(ctx context.Context, replayID string) ([]ReplayEvent, error) {
	if replayID == "" {
		return nil, fmt.Errorf("replays: replay_id is required")
	}
	owner, err := s.replayOwner(ctx, replayID)
	if err != nil {
		return nil, err
	}
	return nucleus.Query[ReplayEvent](ctx, s.db.SQL(),
		`SELECT event_id, tenant_id, replay_id,
			CAST(timestamp AS TEXT) AS timestamp,
			event_type,
			COALESCE(data, '') AS data
		 FROM replay_events
		 WHERE replay_id = $1 AND (site_id = $2 OR site_id = '')
		 ORDER BY timestamp ASC, event_id ASC`,
		replayID, owner,
	)
}

func genID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
