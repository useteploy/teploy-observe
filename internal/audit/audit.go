// Package audit is observe's compliance/audit trail: an append-only,
// admin-only log of who did what, when, from where. It is the shared sink for
// access-audit events across the Teploy stack — observe's own admin mutations,
// the CLI, dash-initiated actions, and the Ship agent all record here (HashiCorp
// parity A4 + SOC2 evidence). Events are immutable: written synchronously,
// never updated, never deleted through the API.
package audit

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"
	"github.com/useteploy/teploy-observe/internal/dbutil"
)

// AuditEvent is one immutable audit record. Timestamp is unix-millis (stored as
// BIGINT, matching the rest of observe).
type AuditEvent struct {
	AuditID   string `json:"id" db:"audit_id"`
	TenantID  string `json:"tenant_id" db:"tenant_id"`
	SiteID    string `json:"site_id" db:"site_id"`
	Timestamp int64  `json:"timestamp" db:"timestamp"`
	Actor     string `json:"actor" db:"actor"`           // username / api-key id / agent id ("" = system)
	ActorType string `json:"actor_type" db:"actor_type"` // user | apikey | agent | system
	Action    string `json:"action" db:"action"`         // dotted verb, e.g. auth.login, user.create, sql.run
	Target    string `json:"target" db:"target"`         // resource acted on (id/name), optional
	Result    string `json:"result" db:"result"`         // success | failure | denied
	SourceIP  string `json:"source_ip" db:"source_ip"`
	UserAgent string `json:"user_agent" db:"user_agent"`
	Metadata  string `json:"metadata" db:"metadata"` // JSON object of extra context

	// Tamper-evidence chain. Seq is a per-writer monotonic counter; Hash is
	// HMAC(key, prev_hash || event fields); PrevHash links to the previous
	// record. Any edit/delete/insert breaks the chain (see Verify).
	Seq      int64  `json:"seq" db:"seq"`
	PrevHash string `json:"prev_hash" db:"prev_hash"`
	Hash     string `json:"hash" db:"hash"`
	// KeyID names the signing key (F46): '' is the pre-042 legacy encoding
	// (verified against the configured legacy candidates), any other value
	// is looked up in the verification keyring. Not part of the MAC input —
	// it selects the key, like an algorithm identifier; swapping it between
	// rows still breaks their hashes.
	KeyID string `json:"key_id,omitempty" db:"key_id"`
}

// Result constants.
const (
	ResultSuccess = "success"
	ResultFailure = "failure"
	ResultDenied  = "denied"
)

// Actor-type constants.
const (
	ActorUser   = "user"
	ActorAPIKey = "apikey"
	ActorAgent  = "agent"
	ActorSystem = "system"
)

const (
	defaultLimit = 200
	maxLimit     = 1000
)

// Service is the audit store. Construct one shared instance and hand it to
// every producer (like the other observe services).
//
// It maintains a tamper-evidence hash chain: writes are serialized behind mu
// and each record's Hash = HMAC(key, prev_hash || fields). This assumes a
// single writer (one observe instance owns the chain). A multi-writer setup
// would need a shared sequence — documented, not supported here.
type Service struct {
	db *nucleus.Client
	// keys is the resolved chain-key state (F46). A nil keyring behaves as
	// the unkeyed legacy service (empty signer, empty-key legacy
	// verification) — NewService without keys keeps old call sites working.
	keys *Keyring
	// Logger, when set, carries the periodic checkpoint digest lines an
	// operator ships out of band (nil = checkpoints are still written and
	// readable via the API, just not logged).
	Logger *slog.Logger

	mu       sync.Mutex
	lastHash string
	lastSeq  int64
	loaded   bool
}

// WithLogger sets the sink for checkpoint digest lines (chain-of-custody
// export). Builder-style; returns s.
func (s *Service) WithLogger(l *slog.Logger) *Service {
	s.Logger = l
	return s
}

// logf is the nil-safe logger.
func (s *Service) logf(format string, args ...any) {
	if s.Logger != nil {
		s.Logger.Info(fmt.Sprintf(format, args...))
	}
}

// NewService wires the audit store to the shared Nucleus client. key is the
// HMAC key for the tamper-evidence chain — without it (nil), the chain still
// links but a DB-level attacker could recompute it; with a key held outside the
// DB, they can't forge the chain. The key becomes both the signer and a legacy
// verification candidate, so rows this process's predecessor signed with it
// keep verifying.
func NewService(db *nucleus.Client, key []byte) *Service {
	kr := &Keyring{verify: map[string][]byte{}, Status: KeyStatusUnkeyed}
	if len(key) > 0 {
		kr.Status = KeyStatusDedicated
		kr.Signer = KeyMaterial{ID: keyID(key), Key: key}
		kr.verify[kr.Signer.ID] = key
	}
	kr.legacy = dedupKeys([][]byte{key})
	return &Service{db: db, keys: kr}
}

// NewServiceWithKeys wires the audit store with a fully resolved keyring
// (F46): dedicated/generated/fallback signer plus rotation keyring.
func NewServiceWithKeys(db *nucleus.Client, kr *Keyring) *Service {
	return &Service{db: db, keys: kr}
}

// computeHashWith is the keyed chain hash over prev_hash + the event's
// fields, length-prefixed so no field value can be smuggled across a
// delimiter.
func computeHashWith(key []byte, ev AuditEvent) string {
	mac := hmac.New(sha256.New, key)
	write := func(v string) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(v)))
		mac.Write(n[:])
		mac.Write([]byte(v))
	}
	write(ev.PrevHash)
	write(strconv.FormatInt(ev.Seq, 10))
	write(strconv.FormatInt(ev.Timestamp, 10))
	write(ev.AuditID)
	write(ev.TenantID)
	write(ev.SiteID)
	write(ev.Actor)
	write(ev.ActorType)
	write(ev.Action)
	write(ev.Target)
	write(ev.Result)
	write(ev.SourceIP)
	write(ev.UserAgent)
	write(ev.Metadata)
	return hex.EncodeToString(mac.Sum(nil))
}

// computeHash signs with the active signer (empty key when unkeyed — the
// documented accidental-edit-only posture).
func (s *Service) computeHash(ev AuditEvent) string {
	var key []byte
	if s.keys != nil {
		key = s.keys.Signer.Key
	}
	return computeHashWith(key, ev)
}

// rowMatchKind classifies how (whether) one row's hash verifies (TO-001):
//
//   - matchKeyed: the row names a key id that resolves and its MAC matches
//     — authenticated under that key.
//   - matchLegacy: a pre-042 key_id=” row whose MAC matches one of the
//     configured SECRET legacy candidates — authenticated under that key.
//   - matchUnkeyed: the row verifies only with the EMPTY (public) key —
//     internally consistent but NOT tamper-evident against a database
//     writer; Verify reports these separately instead of counting them as
//     authenticated history.
//   - matchNone: no candidate verifies.
func (s *Service) rowMatchKind(ev AuditEvent) rowMatch {
	if s.keys == nil {
		if computeHashWith(nil, ev) == ev.Hash {
			return matchUnkeyed
		}
		return matchNone
	}
	if ev.KeyID == "" {
		for _, k := range s.keys.LegacyCandidates() {
			if len(k) > 0 && computeHashWith(k, ev) == ev.Hash {
				return matchLegacy
			}
		}
		if computeHashWith(nil, ev) == ev.Hash {
			return matchUnkeyed
		}
		return matchNone
	}
	k, ok := s.keys.KeyFor(ev.KeyID)
	if !ok {
		return matchNone
	}
	if computeHashWith(k, ev) == ev.Hash {
		return matchKeyed
	}
	return matchNone
}

type rowMatch int

const (
	matchNone rowMatch = iota
	matchKeyed
	matchLegacy
	matchUnkeyed
)

// rowHashMatches reports whether the row verifies under ANY mode (keyed,
// legacy-secret, or unkeyed). Chain continuity; authenticity classification
// is rowMatchKind's job.
func (s *Service) rowHashMatches(ev AuditEvent) bool {
	return s.rowMatchKind(ev) != matchNone
}

// keyKnown reports whether a row's key id resolves (legacy ” always does).
func (s *Service) keyKnown(keyID string) bool {
	if s.keys == nil || keyID == "" {
		return true
	}
	_, ok := s.keys.KeyFor(keyID)
	return ok
}

// loadStateLocked reads the chain head (highest seq + its hash) so a restarted
// process continues the same chain. Call under mu.
func (s *Service) loadStateLocked(ctx context.Context) error {
	rows, err := nucleus.Query[AuditEvent](ctx, s.db.SQL(),
		"SELECT "+auditColumns+" FROM audit_events ORDER BY CAST(seq AS BIGINT) DESC LIMIT 1")
	if err != nil {
		return err
	}
	if len(rows) > 0 {
		s.lastSeq = rows[0].Seq
		s.lastHash = rows[0].Hash
	}
	s.loaded = true
	return nil
}

// VerifyResult reports whether the audit chain is intact.
type VerifyResult struct {
	Intact      bool   `json:"intact"`
	Count       int    `json:"count"`
	BrokenAtSeq int64  `json:"broken_at_seq,omitempty"`
	Detail      string `json:"detail,omitempty"`
	// Authenticated (TO-001): false when any verified row matches only the
	// EMPTY key — internally consistent history a database writer could
	// recompute, which must never be presented as keyed/tamper-evident.
	// True when every row verifies under a secret key (keyed or legacy).
	// False (with Detail) also on a broken chain.
	Authenticated bool `json:"authenticated"`
	// UnkeyedCount is how many rows verified only under the empty key
	// (pre-F46 unkeyed history, or a downgrade attack on it).
	UnkeyedCount int `json:"unkeyed_count,omitempty"`
	// VerifiedThroughSeq is the chain head this verification walked to
	// (O14): "intact" means internally consistent 1..VerifiedThroughSeq,
	// NOT that nothing was removed past an external anchor.
	VerifiedThroughSeq int64 `json:"verified_through_seq"`
	// LatestCheckpoint (O14): the newest recorded checkpoint, when one
	// exists. CheckpointMatch says whether the chain hash AT that
	// checkpoint's seq equals its recorded head_hash - the in-database
	// half of the truncation-detectable claim. The OTHER half is external:
	// the digest, recomputed from this database and compared against a
	// copy stored outside it. Verify cannot do that comparison for you;
	// the wording in Detail says exactly what was and was not proven.
	LatestCheckpoint *Checkpoint `json:"latest_checkpoint,omitempty"`
	CheckpointMatch  bool        `json:"checkpoint_match"`
	HasCheckpoint    bool        `json:"has_checkpoint"`
}

// narrowIntactDetail is the O14 wording: what an in-database chain walk
// actually proves, and what it cannot.
const narrowIntactDetail = "chain internally consistent from seq 1 through %d; an append-only hash chain in one mutable database does not prove that its tail was not truncated - compare the latest checkpoint digest against an externally stored copy to extend the guarantee"

// Verify walks the whole chain in order and recomputes each hash. It detects a
// modified row (hash mismatch), a deleted row (sequence gap), and a relinked or
// inserted row (prev_hash mismatch). Returns the first break, if any.
//
// WHAT THIS PROVES (O14, narrowed deliberately): the chain is internally
// consistent from seq 1 through VerifiedThroughSeq as of this call. It does
// NOT prove the tail was not truncated after the last externally anchored
// checkpoint - an attacker with database write access can delete the tail
// (checkpoints included) and the shorter chain still verifies. The
// result carries the latest checkpoint plus whether the chain at its seq
// still hashes to the recorded head; the truncation-detectable claim
// additionally requires the checkpoint DIGEST to match a copy stored
// outside this database. That comparison is the operator's; F47 tracks
// the automated external anchor.
//
// AUD-051 (round 2): verification pages through the chain in keyset batches
// instead of materializing the entire unbounded history in memory — a
// long-lived installation's Verify used to load every row at once, and
// concurrent verifications compounded the spike. The watermark is captured
// once so concurrent appends cannot extend the scan. This assumes the
// engine enforces one row per sequence (Record is the only writer and is
// serialized behind mu).
func (s *Service) Verify(ctx context.Context) (VerifyResult, error) {
	const pageSize = 500

	// Capture the watermark before scanning: appends past this point belong
	// to the next verification, not this one.
	head, err := nucleus.Query[struct {
		Seq int64 `db:"seq"`
	}](ctx, s.db.SQL(), "SELECT CAST(seq AS BIGINT) AS seq FROM audit_events ORDER BY CAST(seq AS BIGINT) DESC LIMIT 1")
	if err != nil {
		return VerifyResult{}, err
	}
	var through int64
	if len(head) > 0 {
		through = head[0].Seq
	}

	checkpoint, err := s.latestCheckpoint(ctx)
	if err != nil {
		// A checkpoint-table read failure must not weaken the chain walk's
		// report - it degrades to no-checkpoint, and the error says so.
		return VerifyResult{}, fmt.Errorf("audit: latest checkpoint read: %w", err)
	}
	var hashAtCheckpoint string
	var checkpointReached bool

	prev := ""
	var expectSeq int64 = 1
	checked := 0
	unkeyed := 0
	var last int64
	for last < through {
		// TO-011: fetch one row PAST the page so a duplicate sequence
		// straddling the page boundary is caught here instead of being
		// skipped by the next page's `> last` cursor.
		rows, err := nucleus.Query[AuditEvent](ctx, s.db.SQL(),
			"SELECT "+auditColumns+" FROM audit_events "+
				"WHERE CAST(seq AS BIGINT) > $1 AND CAST(seq AS BIGINT) <= $2 "+
				"ORDER BY CAST(seq AS BIGINT) ASC LIMIT "+strconv.Itoa(pageSize+1),
			dbutil.IntParam(last), dbutil.IntParam(through))
		if err != nil {
			return VerifyResult{}, err
		}
		if len(rows) == 0 {
			return VerifyResult{Count: checked, BrokenAtSeq: last + 1, VerifiedThroughSeq: through,
				Detail: fmt.Sprintf("missing records before verification watermark %d", through)}, nil
		}
		if len(rows) > pageSize && rows[pageSize-1].Seq == rows[pageSize].Seq {
			return VerifyResult{Count: checked, BrokenAtSeq: rows[pageSize].Seq, VerifiedThroughSeq: through,
				Detail: fmt.Sprintf("duplicate audit sequence %d (record duplicated or chain forked)", rows[pageSize].Seq)}, nil
		}
		if len(rows) > pageSize {
			rows = rows[:pageSize]
		}
		for _, ev := range rows {
			kind := s.rowMatchKind(ev)
			if ev.Seq != expectSeq || ev.PrevHash != prev || !s.keyKnown(ev.KeyID) || kind == matchNone {
				detail := "sequence gap: expected %d, got %d (record deleted or reordered)"
				switch {
				case ev.Seq != expectSeq:
				case ev.PrevHash != prev:
					detail = "prev_hash mismatch (record inserted or chain relinked)"
				case !s.keyKnown(ev.KeyID):
					detail = fmt.Sprintf("record signed by unknown key id %q (rotation key removed from the keyring?)", ev.KeyID)
				default:
					detail = "hash mismatch (record contents modified)"
				}
				return VerifyResult{Count: checked, BrokenAtSeq: ev.Seq, VerifiedThroughSeq: through,
					Detail: fmt.Sprintf(detail, expectSeq, ev.Seq)}, nil
			}
			if kind == matchUnkeyed {
				unkeyed++
			}
			if checkpoint != nil && ev.Seq == checkpoint.Seq {
				hashAtCheckpoint = ev.Hash
				checkpointReached = true
			}
			prev = ev.Hash
			expectSeq++
			checked++
			last = ev.Seq
		}
	}

	res := VerifyResult{Intact: true, Count: checked, VerifiedThroughSeq: through}
	if checkpoint != nil {
		res.HasCheckpoint = true
		res.LatestCheckpoint = checkpoint
		// The in-database half: the chain at the checkpoint's seq still
		// hashes to the recorded head. A truncation that removed rows at
		// or before the checkpoint makes this false (or the checkpoint
		// disappears entirely - HasCheckpoint says which).
		res.CheckpointMatch = checkpointReached && hashAtCheckpoint == checkpoint.HeadHash
		// A SURVIVING checkpoint above the current head is in-database
		// proof of truncation: the chain once reached cp.Seq and now ends
		// below it. (An attacker who deletes the checkpoint rows too
		// leaves no in-database trace - that case is exactly what the
		// externally stored digest exists for.)
		if checkpoint.Seq > through {
			res.Intact = false
			res.BrokenAtSeq = through + 1
			res.Detail = fmt.Sprintf("chain head (%d) is below the last recorded checkpoint (seq %d) - records after the checkpoint were truncated", through, checkpoint.Seq)
			return res, nil
		}
		if !res.CheckpointMatch {
			// The checkpointed row no longer hashes to the recorded head -
			// history before the anchor was rewritten.
			res.Intact = false
			res.BrokenAtSeq = checkpoint.Seq
			res.Detail = fmt.Sprintf("chain hash at checkpoint seq %d does not match the recorded checkpoint head - history before the anchor was rewritten", checkpoint.Seq)
			return res, nil
		}
	}
	if unkeyed > 0 {
		res.Authenticated = false
		res.UnkeyedCount = unkeyed
		res.Detail = fmt.Sprintf("chain is internally consistent through seq %d, but %d record(s) verify only under the EMPTY key (pre-F46 unkeyed history, or a downgrade of it) — not tamper-evident against a database writer; ", through, unkeyed) +
			fmt.Sprintf(narrowIntactDetail, through)
		return res, nil
	}
	res.Authenticated = true
	res.Detail = fmt.Sprintf(narrowIntactDetail, through)
	return res, nil
}

// verifyChain is the pure chain-verification core (DB-less, unit-tested). Rows
// must be in ascending seq order.
func verifyChain(rows []AuditEvent, hashFn func(AuditEvent) string) VerifyResult {
	prev := ""
	var expectSeq int64 = 1
	for _, ev := range rows {
		if ev.Seq != expectSeq {
			return VerifyResult{Count: len(rows), BrokenAtSeq: ev.Seq,
				Detail: fmt.Sprintf("sequence gap: expected %d, got %d (record deleted or reordered)", expectSeq, ev.Seq)}
		}
		if ev.PrevHash != prev {
			return VerifyResult{Count: len(rows), BrokenAtSeq: ev.Seq,
				Detail: "prev_hash mismatch (record inserted or chain relinked)"}
		}
		if hashFn(ev) != ev.Hash {
			return VerifyResult{Count: len(rows), BrokenAtSeq: ev.Seq,
				Detail: "hash mismatch (record contents modified)"}
		}
		prev = ev.Hash
		expectSeq++
	}
	return VerifyResult{Intact: true, Count: len(rows)}
}

// Filter narrows an audit query. All fields are optional; zero-value = no
// constraint. From/To are unix-millis bounds (inclusive).
type Filter struct {
	SiteID string
	Actor  string
	Action string
	Result string
	From   int64
	To     int64
	Limit  int
}

var auditColumns = "audit_id, tenant_id, site_id, timestamp, actor, actor_type, action, target, result, source_ip, user_agent, metadata, key_id, seq, prev_hash, hash"

// Record writes one audit event synchronously (never via the lossy ingest
// buffer — an audit trail must be durable and immediate). Defaults are filled
// for id/timestamp/tenant/actor_type/result; Action is required.
func (s *Service) Record(ctx context.Context, ev AuditEvent) error {
	if strings.TrimSpace(ev.Action) == "" {
		return fmt.Errorf("audit: action is required")
	}
	if ev.AuditID == "" {
		ev.AuditID = genID()
	}
	if ev.Timestamp == 0 {
		ev.Timestamp = time.Now().UnixMilli()
	}
	if ev.TenantID == "" {
		ev.TenantID = "default"
	}
	if ev.SiteID == "" {
		ev.SiteID = "default"
	}
	if ev.ActorType == "" {
		ev.ActorType = ActorUser
	}
	if ev.Result == "" {
		ev.Result = ResultSuccess
	}
	if ev.Metadata == "" {
		ev.Metadata = "{}"
	}

	// Serialize writes to keep the hash chain linear and gap-free.
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.loaded {
		if err := s.loadStateLocked(ctx); err != nil {
			return fmt.Errorf("audit: loading chain head: %w", err)
		}
	}
	ev.Seq = s.lastSeq + 1
	ev.PrevHash = s.lastHash
	// F46: stamp the signing key's id so verification selects it (and so a
	// later rotation can keep verifying this row via the keyring).
	if s.keys != nil && s.keys.Keyed() {
		ev.KeyID = s.keys.Signer.ID
	}
	ev.Hash = s.computeHash(ev)

	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO audit_events (`+auditColumns+`)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`,
		ev.AuditID, ev.TenantID, ev.SiteID, dbutil.IntParam(ev.Timestamp),
		ev.Actor, ev.ActorType, ev.Action, ev.Target, ev.Result,
		ev.SourceIP, ev.UserAgent, ev.Metadata, ev.KeyID,
		dbutil.IntParam(ev.Seq), ev.PrevHash, ev.Hash)
	if err != nil {
		// AUD-052 (round 2): the commit outcome is ambiguous — a transport
		// error can follow an accepted insert. Drop the cached head so the
		// next append re-derives lastSeq/lastHash from storage instead of
		// reusing this sequence number for a different event (which forks
		// the chain or duplicates the sequence). A durable intent/reconcile
		// protocol remains deferred with the F47-class anchor work.
		s.loaded = false
		return fmt.Errorf("audit: append failed for %s (outcome may be unknown; head will be re-derived): %w", ev.AuditID, err)
	}
	s.lastSeq = ev.Seq
	s.lastHash = ev.Hash

	// Periodic checkpoint (O14): every CheckpointEvery records, anchor the
	// head with an exportable digest. Never fatal to the append - a
	// checkpoint failure logs and the next boundary retries.
	if s.lastSeq%CheckpointEvery == 0 {
		if cp, err := s.checkpointLocked(ctx); err != nil {
			s.logf("audit: periodic checkpoint write FAILED (chain unaffected): %v", err)
		} else {
			s.logf("audit: checkpoint seq=%d digest=%s - store this line outside the database to make tail truncation detectable",
				cp.Seq, cp.Digest)
		}
	}
	return nil
}

// List returns audit events matching the filter, newest first.
func (s *Service) List(ctx context.Context, f Filter) ([]AuditEvent, error) {
	query, args := buildListQuery(f)
	rows, err := nucleus.Query[AuditEvent](ctx, s.db.SQL(), query, args...)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// buildListQuery is the pure SQL builder (unit-tested without a DB). Time
// bounds use CAST(timestamp AS BIGINT) because Nucleus returns BIGINT as text
// over the wire, which would otherwise defeat range comparisons — and an audit
// log is unbounded, so filtering must happen in SQL, not in Go.
func buildListQuery(f Filter) (string, []any) {
	var conds []string
	var args []any
	add := func(frag string, val any) {
		args = append(args, val)
		conds = append(conds, fmt.Sprintf(frag, len(args)))
	}

	if f.SiteID != "" {
		add("site_id = $%d", f.SiteID)
	}
	if f.Actor != "" {
		add("actor = $%d", f.Actor)
	}
	if f.Action != "" {
		add("action = $%d", f.Action)
	}
	if f.Result != "" {
		add("result = $%d", f.Result)
	}
	if f.From > 0 {
		add("CAST(timestamp AS BIGINT) >= $%d", dbutil.IntParam(f.From))
	}
	if f.To > 0 {
		add("CAST(timestamp AS BIGINT) <= $%d", dbutil.IntParam(f.To))
	}

	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}

	limit := f.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}

	// limit is a validated int (clamped), inlined rather than parameterized —
	// some engines reject a bound param in LIMIT.
	query := "SELECT " + auditColumns + " FROM audit_events" + where +
		" ORDER BY CAST(timestamp AS BIGINT) DESC LIMIT " + fmt.Sprint(limit)
	return query, args
}

// MarshalMetadata is a convenience for producers: turn a map into the JSON
// string stored in Metadata. Returns "{}" on nil/empty or marshal failure so a
// metadata problem never blocks the audit write.
func MarshalMetadata(m map[string]any) string {
	if len(m) == 0 {
		return "{}"
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func genID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
