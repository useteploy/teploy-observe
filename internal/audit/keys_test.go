package audit

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
)

func b64key(byteVal byte) string {
	b := make([]byte, 32)
	for i := range b {
		b[i] = byteVal
	}
	return base64.StdEncoding.EncodeToString(b)
}

// F46: dedicated keys resolve from base64 (>=32 decoded bytes) and raw
// strings; short keys are rejected outright.
func TestLoadKeyring_DedicatedKeyForms(t *testing.T) {
	kr, err := LoadKeyring(KeyEnv{Dedicated: b64key(1)})
	if err != nil {
		t.Fatalf("base64 key: %v", err)
	}
	if kr.Status != KeyStatusDedicated || len(kr.Signer.Key) != 32 {
		t.Fatalf("base64 dedicated key must decode to 32 bytes: %+v", kr)
	}
	if len(kr.Signer.ID) != 8 {
		t.Fatalf("key id must be the 8-hex derived id, got %q", kr.Signer.ID)
	}

	raw := strings.Repeat("r!", 20) // 40 chars, not valid base64 -> raw interpretation
	kr, err = LoadKeyring(KeyEnv{Dedicated: raw})
	if err != nil {
		t.Fatalf("raw key: %v", err)
	}
	if string(kr.Signer.Key) != raw {
		t.Fatal("non-base64 raw key must be used as raw bytes")
	}

	if _, err := LoadKeyring(KeyEnv{Dedicated: "too-short"}); err == nil {
		t.Fatal("short key must be rejected")
	}
	// 24 bytes base64-encoded decodes below the floor even though the
	// encoded string is 32 chars (the config-level raw length check alone
	// would pass it).
	if _, err := LoadKeyring(KeyEnv{Dedicated: base64.StdEncoding.EncodeToString(make([]byte, 24))}); err == nil {
		t.Fatal("base64 decoding below 32 bytes must be rejected")
	}
}

// F46: an ambiguous 44-char value that is valid base64 decodes for SIGNING,
// but the raw interpretation stays among the legacy candidates so pre-F46
// rows (signed with raw bytes) keep verifying.
func TestLoadKeyring_AmbiguousFormKeepsRawLegacyCandidate(t *testing.T) {
	raw := strings.Repeat("a", 44) // valid base64 alphabet, decodes to 33 bytes
	kr, err := LoadKeyring(KeyEnv{Dedicated: raw})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if string(kr.Signer.Key) == raw {
		t.Fatal("canonical form is the decoded bytes")
	}
	found := false
	for _, k := range kr.LegacyCandidates() {
		if string(k) == raw {
			found = true
		}
	}
	if !found {
		t.Fatal("the raw interpretation must remain a legacy verification candidate")
	}
}

// F46: the persistent file key is generated once, survives reloads, and its
// rotation spec round-trips into OBSERVE_AUDIT_KEYRING.
func TestLoadKeyring_PersistentFileKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.key")
	kr1, err := LoadKeyring(KeyEnv{File: path})
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	if kr1.Status != KeyStatusPersistent || !kr1.Keyed() {
		t.Fatalf("expected a persistent generated key, got %q", kr1.Status)
	}
	kr2, err := LoadKeyring(KeyEnv{File: path})
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if kr1.Signer.ID != kr2.Signer.ID || string(kr1.Signer.Key) != string(kr2.Signer.Key) {
		t.Fatal("the persistent key must be stable across reloads")
	}
	spec := kr1.RotationSpec()
	if spec == "" {
		t.Fatal("file key must expose a rotation spec")
	}
	// Rotating: new dedicated key + old file key in the keyring.
	kr3, err := LoadKeyring(KeyEnv{Dedicated: b64key(9), KeyringSpec: spec})
	if err != nil {
		t.Fatalf("rotated keyring: %v", err)
	}
	if kr3.Status != KeyStatusDedicated || kr3.Signer.ID == kr1.Signer.ID {
		t.Fatalf("rotation must switch the signer: %+v", kr3)
	}
	if _, ok := kr3.KeyFor(kr1.Signer.ID); !ok {
		t.Fatal("the old key must verify via the keyring")
	}
}

// F46: keyring entries with a mistyped id are rejected at startup — a wrong
// id would silently fail to verify the history it was configured to cover.
func TestLoadKeyring_KeyringEntryValidation(t *testing.T) {
	other, err := LoadKeyring(KeyEnv{Dedicated: b64key(2)})
	if err != nil {
		t.Fatal(err)
	}
	good := other.Signer.ID + ":" + b64key(2)
	if _, err := LoadKeyring(KeyEnv{Dedicated: b64key(3), KeyringSpec: good}); err != nil {
		t.Fatalf("well-formed entry must load: %v", err)
	}
	// Entry whose stated id does not match its material.
	if _, err := LoadKeyring(KeyEnv{Dedicated: b64key(3), KeyringSpec: "deadbeef:" + b64key(2)}); err == nil {
		t.Fatal("mismatched keyring id must be rejected")
	}
	if _, err := LoadKeyring(KeyEnv{Dedicated: b64key(3), KeyringSpec: "no-colon-here"}); err == nil {
		t.Fatal("malformed keyring entry must be rejected")
	}
}

// F46: rows verify across a rotation — the pre-rotation rows carry the old
// key's id (or '' for pre-042 rows), the post-rotation rows the new key's,
// and one Verify walk accepts both.
func TestKeyring_VerificationAcrossRotation(t *testing.T) {
	oldKey := []byte(strings.Repeat("o", 32))
	newKey := []byte(strings.Repeat("n", 32))

	// Pre-042-style rows: no key id, signed with the raw old key.
	var rows []AuditEvent
	prev := ""
	for i := 1; i <= 3; i++ {
		ev := AuditEvent{Seq: int64(i), PrevHash: prev, Timestamp: int64(i), AuditID: genID(),
			Actor: "a", Action: "x.do", Result: "success"}
		ev.Hash = computeHashWith(oldKey, ev)
		rows = append(rows, ev)
		prev = ev.Hash
	}

	// Post-042 rows under the new signer, stamped with its id.
	kr := &Keyring{
		Status: KeyStatusDedicated,
		Signer: KeyMaterial{ID: keyID(newKey), Key: newKey},
		verify: map[string][]byte{keyID(newKey): newKey},
		legacy: dedupKeys([][]byte{oldKey, {}}),
	}
	newSvc := NewServiceWithKeys(nil, kr)
	for i := 4; i <= 6; i++ {
		ev := AuditEvent{Seq: int64(i), PrevHash: prev, Timestamp: int64(i), AuditID: genID(),
			Actor: "a", Action: "x.do", Result: "success", KeyID: keyID(newKey)}
		ev.Hash = computeHashWith(newKey, ev)
		rows = append(rows, ev)
		prev = ev.Hash
	}
	for _, ev := range rows {
		if !newSvc.rowHashMatches(ev) {
			t.Fatalf("row seq %d (key_id %q) must verify after rotation", ev.Seq, ev.KeyID)
		}
	}

	// A row naming an unknown key id fails loudly (removed rotation key or
	// forged id) — never a silent fallback.
	orphan := AuditEvent{Seq: 7, PrevHash: prev, Timestamp: 7, AuditID: genID(),
		Actor: "a", Action: "x.do", Result: "success", KeyID: "00112233"}
	orphan.Hash = computeHashWith(newKey, orphan)
	if newSvc.rowHashMatches(orphan) || newSvc.keyKnown(orphan.KeyID) {
		t.Fatal("unknown key id must not verify")
	}

	// A tampered pre-042 row still fails under the legacy candidates.
	rows[1].Actor = "attacker"
	for _, ev := range rows {
		if newSvc.rowHashMatches(ev) && ev.Seq == 2 {
			t.Fatal("tampered legacy row must not verify")
		}
	}
}

// F46 integration (skips cleanly when Nucleus is unreachable): a chain
// written under one signer verifies after a rotation when the old key moves
// into the keyring, and rows keep their key ids in storage. The "old" signer
// is the shared integration key — this table is shared with the other
// integration tests and Verify walks every row, so every key that ever
// signed it must resolve in the rotated keyring.
func TestKeyring_RotationVerifiesLive_Integration(t *testing.T) {
	dsn := nucleustest.DSN(t)
	ctx := context.Background()
	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("nucleus not reachable at %s — skipping integration test", dsn)
	}
	defer db.Close()

	if _, err := db.SQL().Exec(ctx, `CREATE TABLE IF NOT EXISTS audit_events (
		audit_id TEXT NOT NULL, tenant_id TEXT NOT NULL DEFAULT 'default',
		site_id TEXT NOT NULL DEFAULT 'default', timestamp BIGINT NOT NULL,
		actor TEXT NOT NULL DEFAULT '', actor_type TEXT NOT NULL DEFAULT 'user',
		action TEXT NOT NULL, target TEXT NOT NULL DEFAULT '',
		result TEXT NOT NULL DEFAULT 'success', source_ip TEXT NOT NULL DEFAULT '',
		user_agent TEXT NOT NULL DEFAULT '', metadata TEXT NOT NULL DEFAULT '{}',
		key_id TEXT NOT NULL DEFAULT '', seq BIGINT NOT NULL DEFAULT 0,
		prev_hash TEXT NOT NULL DEFAULT '', hash TEXT NOT NULL DEFAULT ''
	) WITH (engine = 'mergetree') ORDER BY (tenant_id, site_id, timestamp)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	// Own the whole walked chain (see TestChain_Integration's sweep note).
	if _, err := db.SQL().Exec(ctx, `DELETE FROM audit_events`); err != nil {
		t.Fatalf("sweep audit_events: %v", err)
	}

	oldSvc := NewService(db, []byte(integrationAuditKey))
	oldID := keyID([]byte(integrationAuditKey))
	actor := "f46-" + genID()
	for i := 0; i < 3; i++ {
		if err := oldSvc.Record(ctx, AuditEvent{Actor: actor, Action: "f46.before"}); err != nil {
			t.Fatalf("record under old key: %v", err)
		}
	}
	rows, err := oldSvc.List(ctx, Filter{Actor: actor})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.KeyID != oldID {
			t.Fatalf("stored rows must carry the signer's key id: got %q want %q", r.KeyID, oldID)
		}
	}

	// Rotation: a new 32-byte signer; the old key resolves via the verify
	// map (rows it stamped) and the legacy candidates (pre-042 rows).
	newKey := []byte(strings.Repeat("n!", 16))
	kr := &Keyring{
		Status: KeyStatusDedicated,
		Signer: KeyMaterial{ID: keyID(newKey), Key: newKey},
		verify: map[string][]byte{
			keyID(newKey): newKey,
			oldID:         []byte(integrationAuditKey),
		},
		legacy: dedupKeys([][]byte{[]byte(integrationAuditKey), {}}),
	}
	svc2 := NewServiceWithKeys(db, kr)
	if err := svc2.Record(ctx, AuditEvent{Actor: actor, Action: "f46.after"}); err != nil {
		t.Fatalf("record under new key: %v", err)
	}

	res, err := svc2.Verify(ctx)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !res.Intact {
		t.Fatalf("chain must verify across rotation: %+v", res)
	}
}

// TO-001: a database writer who rewrites the chain, blanks every key id,
// and recomputes the MACs with the public empty key must NOT come out as
// authenticated history — the row classifies as unkeyed, never as a
// legacy-secret match.
func TestRowMatch_EmptyKeyForgeryIsNotAuthenticated(t *testing.T) {
	realKey := []byte(strings.Repeat("s", 32))
	kr := &Keyring{
		Status: KeyStatusDedicated,
		Signer: KeyMaterial{ID: keyID(realKey), Key: realKey},
		verify: map[string][]byte{keyID(realKey): realKey},
		legacy: dedupKeys([][]byte{realKey}),
	}
	svc := NewServiceWithKeys(nil, kr)

	// The attacker's product: blank id, empty-key MAC.
	forged := AuditEvent{Seq: 1, PrevHash: "", Timestamp: 1, AuditID: genID(),
		Actor: "attacker", Action: "admin.wipe", Result: "success"}
	forged.Hash = computeHashWith(nil, forged)
	if got := svc.rowMatchKind(forged); got != matchUnkeyed {
		t.Fatalf("blank-id empty-key row must classify as matchUnkeyed, got %v", got)
	}
	// Chain continuity is still reported (rowHashMatches) so an entirely
	// pre-F46 unkeyed install keeps verifying — the classification, not
	// the boolean, carries the authenticity verdict.
	if !svc.rowHashMatches(forged) {
		t.Fatal("empty-key row still counts for chain continuity")
	}

	// A row signed with the real secret classifies as a legacy match.
	legit := forged
	legit.Hash = computeHashWith(realKey, legit)
	if got := svc.rowMatchKind(legit); got != matchLegacy {
		t.Fatalf("secret-signed blank-id row must classify as matchLegacy, got %v", got)
	}

	// LoadKeyring must not offer the empty key as a candidate at all.
	env, err := LoadKeyring(KeyEnv{Dedicated: b64key(5)})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range env.LegacyCandidates() {
		if len(k) == 0 {
			t.Fatal("LoadKeyring must never include the empty key among legacy candidates")
		}
	}
}

// TO-007: a corrupt persistent key file is fatal — the signer must not
// silently downgrade to the JWT fallback (or unkeyed) while the install's
// history stays keyed by the unreadable key.
func TestLoadKeyring_CorruptKeyFileIsFatal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.key")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKeyring(KeyEnv{File: path, Fallback: "fallback-secret-value-32-bytes!!"}); err == nil {
		t.Fatal("a corrupt key file must refuse startup, not fall back")
	}
	// A key file whose id does not match its material is equally fatal.
	bad := persistentKeyFile{KeyID: "00112233", Key: b64key(7)}
	raw, _ := json.Marshal(bad)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKeyring(KeyEnv{File: path}); err == nil {
		t.Fatal("mismatched key file id must refuse startup")
	}
}

// TO-007: concurrent creators cannot overwrite each other's winner — every
// loader after the race returns the key that is actually on disk.
func TestLoadKeyring_ConcurrentCreateSingleWinner(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.key")
	const racers = 8
	errs := make(chan error, racers)
	ids := make(chan string, racers)
	for i := 0; i < racers; i++ {
		go func() {
			kr, err := LoadKeyring(KeyEnv{File: path})
			if err != nil {
				errs <- err
				return
			}
			// The returned key must equal what a fresh read sees on disk.
			finalRaw, rerr := os.ReadFile(path)
			if rerr != nil {
				errs <- rerr
				return
			}
			final, perr := parseKeyFile(path, finalRaw)
			if perr != nil {
				errs <- perr
				return
			}
			if final.ID != kr.Signer.ID || string(final.Key) != string(kr.Signer.Key) {
				errs <- fmt.Errorf("returned key %s does not match the on-disk winner %s", kr.Signer.ID, final.ID)
				return
			}
			ids <- kr.Signer.ID
		}()
	}
	first := <-ids
	for i := 1; i < racers; i++ {
		select {
		case err := <-errs:
			t.Fatal(err)
		case id := <-ids:
			if id != first {
				t.Fatalf("racers disagree on the key: %s vs %s", id, first)
			}
		}
	}
}

// TO-008: a historical keyring secret verifies the pre-042 rows it signed
// even after the active signer moved on — the rotation procedure works.
func TestLoadKeyring_HistoricalKeyVerifiesLegacyRows(t *testing.T) {
	oldKey := []byte(strings.Repeat("h", 32))
	spec := keyID(oldKey) + ":" + base64.StdEncoding.EncodeToString(oldKey)
	kr, err := LoadKeyring(KeyEnv{Dedicated: b64key(3), KeyringSpec: spec})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, k := range kr.LegacyCandidates() {
		if string(k) == string(oldKey) {
			found = true
		}
	}
	if !found {
		t.Fatal("a historical keyring secret must be a legacy verification candidate")
	}
}

// TO-009: two different keys deriving the same id must be refused, not
// silently overwrite each other in the verification map.
func TestLoadKeyring_CollidingKeyIDsRefused(t *testing.T) {
	a := []byte(strings.Repeat("a", 32))
	b := []byte(strings.Repeat("b", 32))
	kr := &Keyring{verify: map[string][]byte{}}
	if err := kr.putVerificationKey("cccccccc", a); err != nil {
		t.Fatal(err)
	}
	if err := kr.putVerificationKey("cccccccc", b); err == nil {
		t.Fatal("a colliding id naming different key bytes must be refused")
	}
	if err := kr.putVerificationKey("cccccccc", a); err != nil {
		t.Fatal("re-inserting the same bytes must be harmless")
	}
}
