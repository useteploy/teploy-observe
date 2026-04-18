package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
)

// apiKeyRow maps to the api_keys table.
type apiKeyRow struct {
	KeyID     string `db:"key_id"`
	TenantID  string `db:"tenant_id"`
	SiteID    string `db:"site_id"`
	KeyHash   string `db:"key_hash"`
	Label     string `db:"label"`
	CreatedAt string `db:"created_at"`
	Revoked   string `db:"revoked"`
}

// APIKeyInfo is the response returned when listing or creating API keys.
type APIKeyInfo struct {
	KeyID     string `json:"key_id"`
	SiteID    string `json:"site_id"`
	Label     string `json:"label"`
	CreatedAt string `json:"created_at"`
	Revoked   bool   `json:"revoked"`
}

// CreateAPIKey generates a new API key for a site. Returns the plaintext key
// (shown once) and metadata. The key hash is stored in the database.
func (s *AuthService) CreateAPIKey(ctx context.Context, siteID, label string) (plaintext string, info APIKeyInfo, err error) {
	sql := s.db.SQL()

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
		"INSERT INTO api_keys (key_id, tenant_id, site_id, key_hash, label, created_at) VALUES ($1, $2, $3, $4, $5, $6)",
		keyID, "default", siteID, keyHash, label, nowStr,
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
	}
	return plaintext, info, nil
}

// ValidateAPIKey hashes the provided plaintext key and looks it up in the
// api_keys table. Returns the associated site_id if the key is valid and
// not revoked.
func (s *AuthService) ValidateAPIKey(ctx context.Context, key string) (string, error) {
	sql := s.db.SQL()

	h := sha256.Sum256([]byte(key))
	keyHash := hex.EncodeToString(h[:])

	row, err := nucleus.QueryOne[apiKeyRow](ctx, sql,
		"SELECT key_id, tenant_id, site_id, key_hash, label, created_at, revoked FROM api_keys WHERE key_hash = $1",
		keyHash,
	)
	if err != nil {
		return "", fmt.Errorf("auth: invalid api key")
	}

	if row.Revoked == "true" {
		return "", fmt.Errorf("auth: api key revoked")
	}

	return row.SiteID, nil
}

// RevokeAPIKey marks an API key as revoked.
func (s *AuthService) RevokeAPIKey(ctx context.Context, keyID string) error {
	sql := s.db.SQL()
	// Nucleus plain tables don't support UPDATE — delete and re-insert
	// would be needed. For now, this is best-effort.
	_, err := sql.Exec(ctx,
		"UPDATE api_keys SET revoked = 'true' WHERE key_id = $1",
		keyID,
	)
	if err != nil {
		return fmt.Errorf("auth: revoke api key: %w", err)
	}
	return nil
}

// HasAPIKeys returns true if at least one API key exists in the system.
func (s *AuthService) HasAPIKeys(ctx context.Context) bool {
	sql := s.db.SQL()
	rows, err := nucleus.Query[countRow](ctx, sql, "SELECT COUNT(*) AS count FROM api_keys")
	if err != nil {
		return false
	}
	return len(rows) > 0 && rows[0].Count > 0
}

// ListAPIKeys returns all API keys for a site.
func (s *AuthService) ListAPIKeys(ctx context.Context, siteID string) ([]APIKeyInfo, error) {
	sql := s.db.SQL()
	rows, err := nucleus.Query[apiKeyRow](ctx, sql,
		"SELECT key_id, tenant_id, site_id, key_hash, label, created_at, revoked FROM api_keys WHERE site_id = $1",
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
		}
	}
	return result, nil
}
