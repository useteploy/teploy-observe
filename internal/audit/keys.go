package audit

import (
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
// legacy candidates used to verify pre-042 rows (key_id='').
type Keyring struct {
	Status  KeyStatus
	Signer  KeyMaterial
	verify  map[string][]byte
	legacy  [][]byte
	keyFile string
	fileErr error
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
// Legacy verification candidates (for key_id='' rows) are, deduplicated:
// the resolved signer, the RAW bytes of OBSERVE_AUDIT_KEY (the pre-F46
// interpretation), the JWT fallback, and the empty key.
func LoadKeyring(env KeyEnv) (*Keyring, error) {
	kr := &Keyring{verify: map[string][]byte{}}

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
		kr.verify[id] = km.Key
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
		if err == nil {
			kr.Status = KeyStatusPersistent
			kr.Signer = km
			kr.keyFile = env.File
		} else if env.Fallback != "" {
			kr.Status = KeyStatusFallback
			kr.Signer = KeyMaterial{ID: keyID([]byte(env.Fallback)), Key: []byte(env.Fallback)}
			kr.fileErr = err
		} else {
			kr.Status = KeyStatusUnkeyed
			kr.fileErr = err
		}
	case env.Fallback != "":
		kr.Status = KeyStatusFallback
		kr.Signer = KeyMaterial{ID: keyID([]byte(env.Fallback)), Key: []byte(env.Fallback)}
	default:
		kr.Status = KeyStatusUnkeyed
	}

	if kr.Keyed() {
		kr.verify[kr.Signer.ID] = kr.Signer.Key
	}
	// Legacy candidates for key_id='' rows (pre-042 rows carry no id and
	// were signed with whatever the process was handed back then).
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
	candidates = append(candidates, []byte{})
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
// persisting a fresh 32-byte key when the file does not exist. The file is
// written 0600 via temp+fsync+rename+dirsync so a crash never leaves a
// partially-written key that would silently re-key the chain.
func loadOrCreateKeyFile(path string) (KeyMaterial, error) {
	if raw, err := os.ReadFile(path); err == nil {
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
	if err := writeKeyFile0600(path, raw); err != nil {
		return KeyMaterial{}, fmt.Errorf("persisting audit key file %s: %w", path, err)
	}
	return km, nil
}

func writeKeyFile0600(path string, data []byte) error {
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
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}

// RotationSpec returns the "id:base64key" line an operator pastes into
// OBSERVE_AUDIT_KEYRING when rotating away from the persistent file key,
// or "" when no file key is in use.
func (kr *Keyring) RotationSpec() string {
	if kr.keyFile == "" || kr.Status != KeyStatusPersistent {
		return ""
	}
	return fmt.Sprintf("%s:%s", kr.Signer.ID, base64.StdEncoding.EncodeToString(kr.Signer.Key))
}

// FileErr reports why the persistent key file was unusable, if it was.
func (kr *Keyring) FileErr() error { return kr.fileErr }

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

// LegacyCandidates returns the keys a key_id='' row may have been signed
// with (pre-042 semantics: the then-configured key, which the row does not
// record, so each configured candidate is tried).
func (kr *Keyring) LegacyCandidates() [][]byte { return kr.legacy }

// Keyed reports whether the signer exists (all statuses but unkeyed).
func (kr *Keyring) Keyed() bool { return kr.Status != KeyStatusUnkeyed }
