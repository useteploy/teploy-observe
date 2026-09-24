package audit

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// KeyMaterial is one audit chain key with the id rows stamp for it.
type KeyMaterial struct {
	ID  string
	Key []byte
}

// KeyStatus is how the signing key was resolved, surfaced at startup and
// through the compliance report.
type KeyStatus string

const (
	// KeyStatusDedicated: OBSERVE_AUDIT_KEY resolved to a >=32-byte key.
	KeyStatusDedicated KeyStatus = "dedicated"
	// KeyStatusPersistent: no operator key configured; a random key was
	// generated once and persisted to the key file. Every install's chain
	// is keyed by default now (F46).
	KeyStatusPersistent KeyStatus = "persistent"
	// KeyStatusFallback: signing with the JWT secret — keyed, but shared
	// with a different security domain; rotate to a dedicated key.
	KeyStatusFallback KeyStatus = "jwt-fallback"
	// KeyStatusUnkeyed: no key at all (file creation failed and no
	// fallback existed). Accidental-edit detection only.
	KeyStatusUnkeyed KeyStatus = "unkeyed"
)

// minAuditKeyBytes is the floor for any signing key (F46: base64 >=32 bytes).
const minAuditKeyBytes = 32

// Keyring is the resolved audit-chain key state: exactly one signer, a map
// of historical verification keys (for rotation), and the deduplicated
// legacy candidates used to verify pre-042 rows (key_id=”).
type Keyring struct {
	Status  KeyStatus
	Signer  KeyMaterial
	verify  map[string][]byte
	legacy  [][]byte
	keyFile string
}

// putVerificationKey inserts one verification key, refusing a silent
// overwrite when the id already names DIFFERENT bytes (TO-009): a colliding
// id must never replace historical verification material.
func (kr *Keyring) putVerificationKey(id string, key []byte) error {
	if prev, ok := kr.verify[id]; ok && !bytes.Equal(prev, key) {
		return fmt.Errorf("audit: keyring id %q names two different keys — refusing the ambiguous keyring", id)
	}
	kr.verify[id] = bytes.Clone(key)
	return nil
}

// KeyEnv is the operator-facing key configuration.
type KeyEnv struct {
	// Dedicated is OBSERVE_AUDIT_KEY: base64 (>=32 decoded bytes) or a raw
	// string of >=32 bytes.
	Dedicated string
	// KeyringSpec is OBSERVE_AUDIT_KEYRING: comma-separated "id:base64key"
	// entries, each a HISTORICAL key kept for verification only.
	KeyringSpec string
	// Fallback is the JWT secret — the pre-F46 default chain key.
	Fallback string
	// File is the persistent key store path (data/audit.key). Used to sign
	// when no dedicated key is configured; created on first use.
	File string
}

// keyID derives the stable id for a key: the first 8 hex chars of its
// sha256 — enough to name a key in rows and keyring specs without leaking
// material.
func keyID(key []byte) string {
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:4])
}

// parseKeyMaterial accepts a base64-encoded key (>=32 decoded bytes) or a
// raw string of >=32 bytes. Anything shorter is rejected: HMAC-SHA256 below
// 256 bits is below the floor this chain's tamper-evidence claim rests on.
//
// Ambiguity note: a 44+-char raw passphrase that happens to be valid base64
// decodes to different bytes than its raw form. Pre-F46 rows were signed
// with the RAW form, so LoadKeyring keeps the raw interpretation among the
// legacy verification candidates either way — signing moves to the decoded
// (canonical) form, old rows keep verifying.
func parseKeyMaterial(name, raw string) (KeyMaterial, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return KeyMaterial{}, fmt.Errorf("%s is empty", name)
	}
	if dec, err := base64.StdEncoding.DecodeString(trimmed); err == nil && len(trimmed)%4 == 0 {
		// Valid canonical base64: treat as base64 intent and hold it to the
		// decoded floor — a 32-char string encoding 24 bytes carries 144
		// bits, not the 256 the floor claims.
		if len(dec) < minAuditKeyBytes {
			return KeyMaterial{}, fmt.Errorf("%s decodes from base64 to %d bytes, below the %d-byte floor (raw passphrases that are not valid base64 are also accepted)", name, len(dec), minAuditKeyBytes)
		}
		return KeyMaterial{ID: keyID(dec), Key: dec}, nil
	}
	if len(trimmed) >= minAuditKeyBytes {
		return KeyMaterial{ID: keyID([]byte(trimmed)), Key: []byte(trimmed)}, nil
	}
	return KeyMaterial{}, fmt.Errorf("%s is shorter than %d bytes (decoded and raw)", name, minAuditKeyBytes)
}

// persistentKeyFile is the on-disk form of the generated key.
type persistentKeyFile struct {
	KeyID string `json:"key_id"`
	Key   string `json:"key_b64"`
}

// LoadKeyring resolves the signing key and verification keyring from the
// environment (F46). Resolution order for the signer:
//
//  1. OBSERVE_AUDIT_KEY (dedicated).
//  2. The persistent key file — generated on first boot, 0600, so every
//     install's chain is keyed without operator action. The file lives on
//     the observe host, not in the database: a DB-level attacker who can
//     rewrite audit rows cannot recompute the chain without also owning
//     the host.
//  3. The JWT-secret fallback (chain still keyed, but shared with the
//     session domain — the compliance surface says so).
//  4. Unkeyed (file unusable and no fallback): the pre-F46 empty-key
//     semantics, loudly.
//
// Legacy verification candidates (for key_id=” rows) are, deduplicated:
// the resolved signer, the RAW bytes of OBSERVE_AUDIT_KEY (the pre-F46
// interpretation), the JWT fallback, and the empty key.
func LoadKeyring(env KeyEnv) (*Keyring, error) {
	kr := &Keyring{verify: map[string][]byte{}}
	// historical collects the OBSERVE_AUDIT_KEYRING secrets: they verify
	// rows stamped with their id AND, per TO-008, pre-042 key_id='' rows
	// the then-configured process signed with them (the documented
	// rotation procedure moves exactly such keys here).
	var historical [][]byte

	for _, entry := range strings.Split(env.KeyringSpec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		id, b64, ok := strings.Cut(entry, ":")
		if !ok {
			return nil, fmt.Errorf("OBSERVE_AUDIT_KEYRING entry %q is not id:base64key", entry)
		}
		id = strings.TrimSpace(id)
		if len(id) < 1 || len(id) > 32 || !isKeyIDShape(id) {
			return nil, fmt.Errorf("OBSERVE_AUDIT_KEYRING id %q must be 1-32 chars of a-z0-9", id)
		}
		km, err := parseKeyMaterial(fmt.Sprintf("OBSERVE_AUDIT_KEYRING entry %q", id), b64)
		if err != nil {
			return nil, err
		}
		if km.ID != id {
			// A mistyped rotation entry would silently fail to verify
			// every row it should cover; the id must BE the derived id.
			return nil, fmt.Errorf("OBSERVE_AUDIT_KEYRING entry %q: id does not match the key material (derived id is %s)", id, km.ID)
		}
		if err := kr.putVerificationKey(id, km.Key); err != nil {
			return nil, err
		}
		historical = append(historical, km.Key)
	}

	switch {
	case strings.TrimSpace(env.Dedicated) != "":
		km, err := parseKeyMaterial("OBSERVE_AUDIT_KEY", env.Dedicated)
		if err != nil {
			return nil, err
		}
		kr.Status = KeyStatusDedicated
		kr.Signer = km
	case env.File != "":
		km, err := loadOrCreateKeyFile(env.File)
		if err != nil {
			// TO-007: an existing-but-unusable persistent key is NOT
			// absence — silently downgrading the signer (to the JWT
			// fallback or unkeyed) would decouple new rows from the
			// key the install's history is keyed with. Refuse to
			// start; recovery is restoring the file (or moving its
			// key into OBSERVE_AUDIT_KEYRING and configuring
			// OBSERVE_AUDIT_KEY).
			return nil, err
		}
		kr.Status = KeyStatusPersistent
		kr.Signer = km
		kr.keyFile = env.File
	case env.Fallback != "":
		kr.Status = KeyStatusFallback
		kr.Signer = KeyMaterial{ID: keyID([]byte(env.Fallback)), Key: []byte(env.Fallback)}
	default:
		kr.Status = KeyStatusUnkeyed
	}

	if kr.Keyed() {
		if err := kr.putVerificationKey(kr.Signer.ID, kr.Signer.Key); err != nil {
			return nil, err
		}
	}
	// Legacy candidates for key_id='' rows (pre-042 rows carry no id and
	// were signed with whatever the process was handed back then): the
	// signer, the RAW bytes of OBSERVE_AUDIT_KEY (the pre-F46
	// interpretation), the JWT fallback, and every historical keyring
	// secret (TO-008 — a rotated-out key still verifies the unkeyed rows
	// it signed). The EMPTY key is deliberately absent: an empty-key
	// match is not authentication, and the verifier reports it as the
	// unkeyed classification instead (TO-001).
	candidates := [][]byte{}
	if kr.Keyed() {
		candidates = append(candidates, kr.Signer.Key)
	}
	if raw := strings.TrimSpace(env.Dedicated); raw != "" {
		candidates = append(candidates, []byte(raw))
	}
	if env.Fallback != "" {
		candidates = append(candidates, []byte(env.Fallback))
	}
	candidates = append(candidates, historical...)
	kr.legacy = dedupKeys(candidates)
	return kr, nil
}

// isKeyIDShape accepts the ids keyID() derives (8 hex chars) plus the same
// shape operators may hand-write in keyring specs.
func isKeyIDShape(s string) bool {
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		default:
			return false
		}
	}
	return true
}

func dedupKeys(keys [][]byte) [][]byte {
	var out [][]byte
	for _, k := range keys {
		dup := false
		for _, seen := range out {
			if string(seen) == string(k) {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, k)
		}
	}
	return out
}

// loadOrCreateKeyFile reads the persistent audit key, generating and
// persisting a fresh 32-byte key when the file does not exist. Publication
// is NO-REPLACE (TO-007): the fully-written temp file is linked (or
// exclusively created) at the destination, so two processes racing the
// first boot cannot overwrite each other's winner — the loser re-reads and
// returns the canonical on-disk key. Any error involving an EXISTING file
// (corrupt JSON, wrong id, short key, unreadable) is returned as an error:
// the caller refuses to start rather than silently re-keying the chain.
// The file is written 0600 via temp+fsync+link+dirsync so a crash never
// leaves a partially-written key.
func loadOrCreateKeyFile(path string) (KeyMaterial, error) {
	if raw, err := os.ReadFile(path); err == nil {
		return parseKeyFile(path, raw)
	} else if !errors.Is(err, os.ErrNotExist) {
		return KeyMaterial{}, fmt.Errorf("reading audit key file %s: %w", path, err)
	}

	material := make([]byte, 32)
	if _, err := rand.Read(material); err != nil {
		return KeyMaterial{}, fmt.Errorf("generating audit key: %w", err)
	}
	km := KeyMaterial{ID: keyID(material), Key: material}
	raw, err := json.Marshal(persistentKeyFile{KeyID: km.ID, Key: base64.StdEncoding.EncodeToString(material)})
	if err != nil {
		return KeyMaterial{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return KeyMaterial{}, fmt.Errorf("creating audit key directory: %w", err)
	}
	if err := publishKeyNoReplace(path, raw); err != nil {
		return KeyMaterial{}, fmt.Errorf("persisting audit key file %s: %w", path, err)
	}
	// Always re-read the canonical winner: a concurrent start may have
	// published first, and both processes must sign with the same key.
	finalRaw, err := os.ReadFile(path)
	if err != nil {
		return KeyMaterial{}, fmt.Errorf("re-reading audit key file %s: %w", path, err)
	}
	return parseKeyFile(path, finalRaw)
}

// parseKeyFile validates and decodes the on-disk key form.
func parseKeyFile(path string, raw []byte) (KeyMaterial, error) {
	var pf persistentKeyFile
	if err := json.Unmarshal(raw, &pf); err != nil {
		return KeyMaterial{}, fmt.Errorf("audit key file %s is corrupt: %w", path, err)
	}
	dec, err := base64.StdEncoding.DecodeString(pf.Key)
	if err != nil || len(dec) < minAuditKeyBytes {
		return KeyMaterial{}, fmt.Errorf("audit key file %s does not hold a >=%d-byte key", path, minAuditKeyBytes)
	}
	if pf.KeyID != keyID(dec) {
		return KeyMaterial{}, fmt.Errorf("audit key file %s key_id %q does not match its key material", path, pf.KeyID)
	}
	return KeyMaterial{ID: pf.KeyID, Key: dec}, nil
}

// publishKeyNoReplace writes data to a fully-flushed temp file and
// publishes it at path WITHOUT replacing an existing file. os.Link is the
// atomic no-replace primitive on the Linux/macOS deployment targets; when
// the filesystem refuses hard links it falls back to O_EXCL creation.
// Returns nil regardless of who won the race — the caller re-reads the
// canonical file either way.
func publishKeyNoReplace(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".audit-key-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := f.Chmod(0o600); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := f.Sync(); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Link(tmp, path); err == nil {
		return syncDirKey(filepath.Dir(path))
	} else if !errors.Is(err, os.ErrExist) {
		// Filesystems without hard-link support (some network mounts):
		// exclusive creation is the same no-replace guarantee, at the
		// cost of a crash window that leaves a partial file the next
		// start refuses loudly (never a silent overwrite).
		dst, oerr := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if oerr != nil {
			if errors.Is(oerr, os.ErrExist) {
				return nil
			}
			return oerr
		}
		_, werr := dst.Write(data)
		if werr == nil {
			werr = dst.Sync()
		}
		return errors.Join(werr, dst.Close())
	}
	return nil // another process won the race; caller re-reads the winner
}

func syncDirKey(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}

// RotationSpec returns the "id:base64key" line an operator pastes into
// OBSERVE_AUDIT_KEYRING when rotating away from the persistent file key,
// or "" when no file key is in use. Never logged by the application
// (TO-002): startup logs carry the key id only, and the secret leaves the
// host exclusively through operator-driven file access.
func (kr *Keyring) RotationSpec() string {
	if kr.keyFile == "" || kr.Status != KeyStatusPersistent {
		return ""
	}
	return fmt.Sprintf("%s:%s", kr.Signer.ID, base64.StdEncoding.EncodeToString(kr.Signer.Key))
}

// KeyFor returns the verification key for a row's key_id. The second
// return is false when the id names no configured key — verification must
// fail loudly rather than fall back: a row naming an unknown key is either
// a removed rotation key or a forged id.
func (kr *Keyring) KeyFor(id string) ([]byte, bool) {
	if id == "" {
		return nil, false // handled by the legacy candidate loop
	}
	k, ok := kr.verify[id]
	return k, ok
}

// LegacyCandidates returns the SECRET keys a key_id=” row may have been
// signed with (pre-042 semantics: the then-configured key, which the row
// does not record, so each configured candidate is tried). The empty key
// is not among them: a row that matches only the empty key is reported as
// UNKEYED by the verifier, never as authenticated (TO-001).
func (kr *Keyring) LegacyCandidates() [][]byte { return kr.legacy }

// Keyed reports whether the signer exists (all statuses but unkeyed).
func (kr *Keyring) Keyed() bool { return kr.Status != KeyStatusUnkeyed }
