package audit

// O14: checkpoint digests - the path from "internally consistent" to a
// truncation-detectable claim. An append-only chain in one mutable
// database cannot prove its tail was not truncated (F47 records the full
// external-anchor design); a checkpoint digest the operator stores
// OUTSIDE the database gives Verify something to anchor against: the
// digest is keyed with the host-held signing key, so a DB-level attacker
// cannot forge one that matches an externally stored artifact.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
)

// CheckpointEvery is how many chain records elapse between automatic
// checkpoints (each one logs the exportable digest line). A var so tests
// can lower the boundary.
var CheckpointEvery = int64(1000)

// VerifyCheckpointDigest recomputes a checkpoint's digest under key - the
// offline half of the truncation-detectable claim. An operator (or tooling)
// compares it against the copy stored outside the database: a match proves
// the chain once reached at least cp.Seq with exactly that head, so a
// database that no longer reproduces it was truncated or rewritten.
func VerifyCheckpointDigest(cp Checkpoint, key []byte) bool {
	return checkpointDigest(key, cp.Seq, cp.HeadHash) == cp.Digest
}

// checkpointDigest is the operator-storable artifact: HMAC over the
// checkpoint identity under the host-held key. A nil key (unkeyed
// install) yields the empty-key HMAC - internally consistent, honestly
// reported as unkeyed by Verify, same classification as chain rows.
func checkpointDigest(key []byte, seq int64, headHash string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("observe-audit-checkpoint\n"))
	mac.Write([]byte(strconv.FormatInt(seq, 10)))
	mac.Write([]byte("\n"))
	mac.Write([]byte(headHash))
	return hex.EncodeToString(mac.Sum(nil))
}

// Checkpoint is one exported chain anchor: the current head's (seq, hash)
// plus the keyed digest. Rows land in audit_checkpoints (append-only like
// the chain itself) and the returned value is what the operator stores
// externally.
type Checkpoint struct {
	CheckpointID string `json:"checkpoint_id"`
	Seq          int64  `json:"seq"`
	HeadHash     string `json:"head_hash"`
	Digest       string `json:"digest"`
	KeyID        string `json:"key_id,omitempty"`
	CreatedAt    int64  `json:"created_at"`
}

// Checkpoint records and returns a checkpoint of the current chain head.
// Fails when the head cannot be read (fail-safe: a checkpoint of an
// unknown head proves nothing).
func (s *Service) Checkpoint(ctx context.Context) (Checkpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checkpointLocked(ctx)
}

// checkpointLocked writes the checkpoint row for the CURRENT head state.
// Call under mu. Fails when the head has not been loaded.
func (s *Service) checkpointLocked(ctx context.Context) (Checkpoint, error) {
	if !s.loaded {
		if err := s.loadStateLocked(ctx); err != nil {
			return Checkpoint{}, fmt.Errorf("audit: checkpoint: loading chain head: %w", err)
		}
	}
	if s.lastSeq == 0 {
		return Checkpoint{}, fmt.Errorf("audit: checkpoint: empty chain - nothing to anchor")
	}
	var key []byte
	var keyID string
	if s.keys != nil {
		key = s.keys.Signer.Key
		keyID = s.keys.Signer.ID
	}
	cp := Checkpoint{
		CheckpointID: genID(),
		Seq:          s.lastSeq,
		HeadHash:     s.lastHash,
		Digest:       checkpointDigest(key, s.lastSeq, s.lastHash),
		KeyID:        keyID,
		CreatedAt:    time.Now().UnixMilli(),
	}
	if _, err := s.db.SQL().Exec(ctx,
		`INSERT INTO audit_checkpoints (checkpoint_id, tenant_id, seq, head_hash, digest, key_id, created_at)
		 VALUES ($1, 'default', $2, $3, $4, $5, $6)`,
		cp.CheckpointID, dbutil.IntParam(cp.Seq), cp.HeadHash, cp.Digest, cp.KeyID, dbutil.IntParam(cp.CreatedAt),
	); err != nil {
		return Checkpoint{}, fmt.Errorf("audit: checkpoint write: %w", err)
	}
	return cp, nil
}

// LatestCheckpoint returns the newest recorded checkpoint (by seq), or nil
// when none exists. Exported for the admin read route.
func (s *Service) LatestCheckpoint(ctx context.Context) (*Checkpoint, error) {
	return s.latestCheckpoint(ctx)
}

// latestCheckpoint is the internal reader both Checkpoint-adjacent paths
// share.
func (s *Service) latestCheckpoint(ctx context.Context) (*Checkpoint, error) {
	rows, err := nucleus.Query[Checkpoint](ctx, s.db.SQL(),
		`SELECT checkpoint_id, CAST(seq AS BIGINT) AS seq, head_hash, digest, key_id, CAST(created_at AS BIGINT) AS created_at
		 FROM audit_checkpoints ORDER BY CAST(seq AS BIGINT) DESC, CAST(created_at AS BIGINT) DESC LIMIT 1`)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}
