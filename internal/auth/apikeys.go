package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
)

// Key capabilities (R07, round 4). A site key used to authorize anything its
// site could do; browser-published telemetry keys must not carry artifact
// publication power.
const (
	// ScopeTelemetry covers every keyed ingest route (events, errors, logs,
	// traces, metrics, OTLP).
	ScopeTelemetry = "telemetry"
	// ScopePublish covers source-map upload and its retention trigger, the
	// CI-only capability. Never implied by telemetry.
	ScopePublish = "publish"
)

// validScopes is the closed capability set.
var validScopes = map[string]bool{
	ScopeTelemetry: true,
	ScopePublish:   true,
}

// NormalizeScopes validates and canonicalizes a requested capability list.
// An empty list is the telemetry-only default; unknown capabilities are
// rejected (a typo must not mint a key that does nothing it claims).
func NormalizeScopes(requested []string) ([]string, error) {
	if len(requested) == 0 {
		return []string{ScopeTelemetry}, nil
	}
	seen := make(map[string]bool, len(requested))
	out := make([]string, 0, len(requested))
	for _, s := range requested {
		if !validScopes[s] {
			return nil, fmt.Errorf("auth: unknown key scope %q (valid: telemetry, publish)", s)
		}
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out, nil
}

// HasScope reports whether a stored scopes value (comma-separated) carries
// the capability.
func HasScope(stored, scope string) bool {
	for _, s := range strings.Split(stored, ",") {
		if strings.TrimSpace(s) == scope {
			return true
		}
	}
	return false
}

// apiKeyRow maps to the api_keys table.
type apiKeyRow struct {
	KeyID     string `db:"key_id"`
	TenantID  string `db:"tenant_id"`
	SiteID    string `db:"site_id"`
	KeyHash   string `db:"key_hash"`
	Label     string `db:"label"`
	CreatedAt string `db:"created_at"`
	Revoked   string `db:"revoked"`
	Scopes    string `db:"scopes"`
}

// APIKeyInfo is the response returned when listing or creating API keys.
type APIKeyInfo struct {
	KeyID     string   `json:"key_id"`
	SiteID    string   `json:"site_id"`
	Label     string   `json:"label"`
	CreatedAt string   `json:"created_at"`
	Revoked   bool     `json:"revoked"`
	Scopes    []string `json:"scopes"`
}

// ValidatedKey is the outcome of a successful key validation.
type ValidatedKey struct {
	SiteID string
	Scopes string
}

// CreateAPIKey generates a new API key for a site. Returns the plaintext key
// (shown once) and metadata. The key hash is stored in the database.
// scopes selects the key's capabilities (R07, round 4); empty means
// telemetry-only — a browser-exposed ingest key must never silently carry
// artifact-publication power.
func (s *AuthService) CreateAPIKey(ctx context.Context, siteID, label string, scopes []string) (plaintext string, info APIKeyInfo, err error) {
	sql := s.db.SQL()

	normalized, err := NormalizeScopes(scopes)
	if err != nil {
		return "", APIKeyInfo{}, err
	}
	scopesStr := strings.Join(normalized, ",")

	keyBytes := make([]byte, 32)
	if _, err := rand.Read(keyBytes); err != nil {
		return "", APIKeyInfo{}, fmt.Errorf("auth: generate key: %w", err)
	}
	plaintext = "obs_" + hex.EncodeToString(keyBytes)

	h := sha256.Sum256([]byte(plaintext))
	keyHash := hex.EncodeToString(h[:])

	keyID := generateID()
	now := time.Now().UnixMilli()
	nowStr := dbutil.IntParam(now)

	_, err = sql.Exec(ctx,
		"INSERT INTO api_keys (key_id, tenant_id, site_id, key_hash, label, created_at, revoked, scopes) VALUES ($1, $2, $3, $4, $5, $6, 'false', $7)",
		keyID, "default", siteID, keyHash, label, nowStr, scopesStr,
	)
	if err != nil {
		return "", APIKeyInfo{}, fmt.Errorf("auth: store api key: %w", err)
	}

	info = APIKeyInfo{
		KeyID:     keyID,
		SiteID:    siteID,
		Label:     label,
		CreatedAt: nowStr,
		Revoked:   false,
		Scopes:    normalized,
	}
	return plaintext, info, nil
}

// ErrAuthUnavailable marks a validation failure caused by the AUTH STORE (or
// the site-existence dependency) being unreachable — not by a bad credential.
// R12 (round 4): the middleware maps it to 503 + Retry-After so clients can
// distinguish "retry later" from "your key is wrong" instead of retiring
// credentials during a backend outage.
var ErrAuthUnavailable = errors.New("authentication store unavailable")

// ValidateAPIKey hashes the provided plaintext key and looks it up in the
// api_keys table. Returns the key's site and capability set if the key is
// valid, not revoked, and its site still exists.
//
// The site-existence check (audit F09) closes the window where a deletion
// failure — or a pre-revoke-first deletion — left a live key pointing at a
// removed site. A store error here fails CLOSED (the key is rejected): an
// unavailable sites table must not silently authorize ingest — but since R12
// it is distinguishable from an invalid key through ErrAuthUnavailable.
func (s *AuthService) ValidateAPIKey(ctx context.Context, key string) (ValidatedKey, error) {
	sql := s.db.SQL()

	h := sha256.Sum256([]byte(key))
	keyHash := hex.EncodeToString(h[:])

	rows, err := nucleus.Query[apiKeyRow](ctx, sql,
		"SELECT key_id, tenant_id, site_id, key_hash, label, created_at, revoked, scopes FROM api_keys WHERE key_hash = $1",
		keyHash,
	)
	if err != nil {
		return ValidatedKey{}, fmt.Errorf("%w: key lookup: %w", ErrAuthUnavailable, err)
	}
	if len(rows) == 0 {
		return ValidatedKey{}, fmt.Errorf("auth: invalid api key")
	}
	row := rows[0]

	if row.Revoked == "true" {
		return ValidatedKey{}, fmt.Errorf("auth: api key revoked")
	}

	siteRows, err := nucleus.Query[struct {
		SiteID string `db:"site_id"`
	}](ctx, sql, "SELECT site_id FROM sites WHERE site_id = $1", row.SiteID)
	if err != nil {
		return ValidatedKey{}, fmt.Errorf("%w: site validation: %w", ErrAuthUnavailable, err)
	}
	if len(siteRows) == 0 {
		return ValidatedKey{}, fmt.Errorf("auth: api key site is unavailable")
	}

	return ValidatedKey{SiteID: row.SiteID, Scopes: row.Scopes}, nil
}

// RevokeAPIKey marks an API key as revoked.
//
// api_keys is ORDER BY (tenant_id, key_hash). A Nucleus UPDATE that filters on a
// column NOT in the ORDER BY key (here key_id) silently no-ops — the row keeps
// revoked='false' and the key KEEPS AUTHENTICATING, a real security hole. So we
// look the key up and UPDATE by its ORDER-BY columns (tenant_id, key_hash),
// which actually lands the write. (UPDATEs on admin_users/sites work as-is
// because they already filter on an ORDER-BY column.)
func (s *AuthService) RevokeAPIKey(ctx context.Context, keyID string) error {
	sql := s.db.SQL()

	row, err := nucleus.QueryOne[apiKeyRow](ctx, sql,
		"SELECT key_id, tenant_id, site_id, key_hash, label, created_at, revoked, scopes FROM api_keys WHERE key_id = $1",
		keyID,
	)
	if err != nil {
		return fmt.Errorf("auth: revoke api key: key %q not found: %w", keyID, err)
	}

	if _, err := sql.Exec(ctx,
		"UPDATE api_keys SET revoked = 'true' WHERE tenant_id = $1 AND key_hash = $2",
		row.TenantID, row.KeyHash,
	); err != nil {
		return fmt.Errorf("auth: revoke api key: %w", err)
	}
	return nil
}

// RevokeKeysForSite revokes every API key bound to a site. A Nucleus UPDATE
// filtered on site_id (not in the ORDER BY) silently no-ops, so we list the
// site's keys and revoke each by its (tenant_id, key_hash) ORDER-BY columns.
// Called on site deletion so a deleted site's keys can't keep ingesting.
func (s *AuthService) RevokeKeysForSite(ctx context.Context, siteID string) error {
	sql := s.db.SQL()
	rows, err := nucleus.Query[apiKeyRow](ctx, sql,
		"SELECT key_id, tenant_id, site_id, key_hash, label, created_at, revoked, scopes FROM api_keys WHERE site_id = $1",
		siteID,
	)
	if err != nil {
		return fmt.Errorf("auth: revoke keys for site: %w", err)
	}
	for _, r := range rows {
		if r.Revoked == "true" {
			continue
		}
		if _, err := sql.Exec(ctx,
			"UPDATE api_keys SET revoked = 'true' WHERE tenant_id = $1 AND key_hash = $2",
			r.TenantID, r.KeyHash,
		); err != nil {
			return fmt.Errorf("auth: revoke key %q: %w", r.KeyID, err)
		}
	}
	return nil
}

// HasAPIKeys reports whether at least one API key exists. The error is returned
// (not swallowed as false) so the ingest middleware can fail CLOSED on a DB
// outage instead of falling into the no-keys grace path and accepting writes.
func (s *AuthService) HasAPIKeys(ctx context.Context) (bool, error) {
	sql := s.db.SQL()
	rows, err := nucleus.Query[countRow](ctx, sql, "SELECT COUNT(*) AS count FROM api_keys")
	if err != nil {
		return false, err
	}
	return len(rows) > 0 && rows[0].Count > 0, nil
}

// ListAPIKeys returns all API keys for a site.
func (s *AuthService) ListAPIKeys(ctx context.Context, siteID string) ([]APIKeyInfo, error) {
	sql := s.db.SQL()
	rows, err := nucleus.Query[apiKeyRow](ctx, sql,
		"SELECT key_id, tenant_id, site_id, key_hash, label, created_at, revoked, scopes FROM api_keys WHERE site_id = $1",
		siteID,
	)
	if err != nil {
		return nil, fmt.Errorf("auth: list api keys: %w", err)
	}

	result := make([]APIKeyInfo, len(rows))
	for i, r := range rows {
		result[i] = APIKeyInfo{
			KeyID:     r.KeyID,
			SiteID:    r.SiteID,
			Label:     r.Label,
			CreatedAt: r.CreatedAt,
			Revoked:   r.Revoked == "true",
			Scopes:    strings.Split(r.Scopes, ","),
		}
	}
	return result, nil
}
