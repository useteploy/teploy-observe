package sites

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
)

// Site represents a tracked website.
type Site struct {
	SiteID      string `json:"site_id" db:"site_id"`
	TenantID    string `json:"tenant_id" db:"tenant_id"`
	Domain      string `json:"domain" db:"domain"`
	Name        string `json:"name" db:"name"`
	CreatedAt   int64  `json:"created_at" db:"created_at"`
	SessionSalt string `json:"-" db:"session_salt"`
	// RawDistinctID, when true, opts the site OUT of the default
	// HMAC hashing of distinct_id values. Useful for operators that
	// need server-side identity resolution (e.g. CRM joins by email).
	RawDistinctID bool `json:"raw_distinct_id" db:"raw_distinct_id"`
	// ExternalRef and ManagedBy identify a site provisioned by a service token
	// (site_management table). Both empty means an operator created it. They
	// are never selected from sites, so they stay zero on the ingest path.
	ExternalRef string `json:"external_ref,omitempty" db:"-"`
	ManagedBy   string `json:"managed_by,omitempty" db:"-"`
}

// SiteService provides CRUD operations for the sites table.
type SiteService struct {
	db *nucleus.Client
	// Small in-memory cache so the ingest hot path can call PrivacyConfig
	// without a DB round-trip per event. Populated on first miss; the
	// Cache size is naturally bounded by the number of sites.
	mu    sync.RWMutex
	cache map[string]Site
	// managedMu serializes EnsureManaged's read-then-insert. The store has no
	// unique constraint, and Observe runs one process per store.
	managedMu sync.Mutex
}

// NewSiteService creates a new SiteService.
func NewSiteService(db *nucleus.Client) *SiteService {
	return &SiteService{db: db, cache: map[string]Site{}}
}

// PrivacyConfig returns the (session_salt, raw_distinct_id_optin) pair
// for a site. Used by ingest handlers to hash distinct_id consistently
// per site. Cached in-process; hot reads take the RLock fast path.
// Returns ("", false) if the site is unknown — callers should treat that
// as "do not hash" or apply the global salt as a fallback.
//
// Audit F21: the DB fetch on a cache miss runs under the WRITE lock. The
// old read-lock/fetch/install sequence let an in-flight miss load the old
// privacy row, overlap a SetRawDistinctID flip (which invalidated the
// cache), and then install the stale opt-out afterwards — silently
// reinstating raw-identity storage the operator had just turned off.
// Serializing misses against invalidations removes that window.
func (s *SiteService) PrivacyConfig(ctx context.Context, siteID string) (salt string, rawOptIn bool, ok bool) {
	salt, rawOptIn, ok, _ = s.PrivacyConfigChecked(ctx, siteID)
	return
}

// PrivacyConfigChecked distinguishes an unavailable policy from an unknown site.
// Ingestion must refuse a lookup failure instead of changing identity salts.
func (s *SiteService) PrivacyConfigChecked(ctx context.Context, siteID string) (salt string, rawOptIn bool, ok bool, err error) {
	if siteID == "" {
		return "", false, false, nil
	}
	s.mu.RLock()
	if site, hit := s.cache[siteID]; hit {
		s.mu.RUnlock()
		return site.SessionSalt, site.RawDistinctID, true, nil
	}
	s.mu.RUnlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	// Re-check under the write lock: another goroutine may have filled the
	// entry (or an invalidation may have raced) since the RLock read.
	if site, hit := s.cache[siteID]; hit {
		return site.SessionSalt, site.RawDistinctID, true, nil
	}
	site, err := s.Get(ctx, siteID)
	if err != nil {
		return "", false, false, err
	}
	if site.SiteID == "" {
		return "", false, false, nil
	}
	s.cache[siteID] = site
	return site.SessionSalt, site.RawDistinctID, true, nil
}

// InvalidatePrivacyConfig drops the cached entry for a site. Call after
// flipping raw_distinct_id or rotating session_salt so the next ingest
// picks up the new value.
func (s *SiteService) InvalidatePrivacyConfig(siteID string) {
	s.mu.Lock()
	delete(s.cache, siteID)
	s.mu.Unlock()
}

// List returns all sites.
func (s *SiteService) List(ctx context.Context) ([]Site, error) {
	sql := s.db.SQL()
	rows, err := nucleus.Query[Site](ctx, sql,
		"SELECT site_id, tenant_id, domain, name, created_at, session_salt, raw_distinct_id FROM sites",
	)
	if err != nil {
		return nil, fmt.Errorf("sites: list: %w", err)
	}
	if rows == nil {
		rows = []Site{}
	}
	mgmt, err := nucleus.Query[managementRow](ctx, sql,
		"SELECT site_id, managed_by, external_ref, created_at FROM site_management",
	)
	if err != nil {
		return nil, fmt.Errorf("sites: list management: %w", err)
	}
	byID := make(map[string]managementRow, len(mgmt))
	for _, m := range mgmt {
		byID[m.SiteID] = m
	}
	for i := range rows {
		if m, ok := byID[rows[i].SiteID]; ok {
			rows[i].ExternalRef, rows[i].ManagedBy = m.ExternalRef, m.ManagedBy
		}
	}
	return rows, nil
}

// Create adds a new site with a random site_id and session_salt.
func (s *SiteService) Create(ctx context.Context, domain, name string) (Site, error) {
	sql := s.db.SQL()

	siteID := generateID()
	sessionSalt := generateID()
	now := time.Now().UnixMilli()
	nowStr := dbutil.IntParam(now)

	_, err := sql.Exec(ctx,
		"INSERT INTO sites (site_id, tenant_id, domain, name, created_at, session_salt) VALUES ($1, $2, $3, $4, $5, $6)",
		siteID, "default", domain, name, nowStr, sessionSalt,
	)
	if err != nil {
		return Site{}, fmt.Errorf("sites: create: %w", err)
	}

	return Site{
		SiteID:      siteID,
		TenantID:    "default",
		Domain:      domain,
		Name:        name,
		CreatedAt:   now,
		SessionSalt: sessionSalt,
	}, nil
}

// Get returns a single site by ID.
func (s *SiteService) Get(ctx context.Context, siteID string) (Site, error) {
	sql := s.db.SQL()
	site, err := nucleus.QueryOne[Site](ctx, sql,
		// raw_distinct_id MUST be selected — it drives the per-site privacy
		// opt-out on the ingest hot path (PrivacyConfig reads it). Omitting it
		// silently zero-valued RawDistinctID, defeating the opt-out.
		"SELECT site_id, tenant_id, domain, name, created_at, session_salt, raw_distinct_id FROM sites WHERE site_id = $1",
		siteID,
	)
	if err != nil {
		return Site{}, fmt.Errorf("sites: get: %w", err)
	}
	return site, nil
}

// Exists reports whether a site row exists. Distinguishes absence (false,
// nil) from a store failure (error) so fail-closed callers — e.g. the public
// feedback route's abuse control (R41, round 4) — never treat an outage as
// permission to write.
func (s *SiteService) Exists(ctx context.Context, siteID string) (bool, error) {
	rows, err := nucleus.Query[struct {
		SiteID string `db:"site_id"`
	}](ctx, s.db.SQL(), "SELECT site_id FROM sites WHERE site_id = $1", siteID)
	if err != nil {
		return false, fmt.Errorf("sites: exists: %w", err)
	}
	return len(rows) > 0, nil
}

// EnsureDefault creates a site with site_id="default" if none exists.
// Used on first boot so UI defaults that reference "default" work out of the box.
func (s *SiteService) EnsureDefault(ctx context.Context) error {
	sql := s.db.SQL()

	type countRow struct {
		Count int64 `db:"count"`
	}
	rows, err := nucleus.Query[countRow](ctx, sql,
		"SELECT COUNT(*) AS count FROM sites WHERE site_id = $1",
		"default",
	)
	if err != nil {
		return fmt.Errorf("sites: ensure default: %w", err)
	}
	if len(rows) > 0 && rows[0].Count > 0 {
		return nil
	}

	now := dbutil.IntParam(time.Now().UnixMilli())
	_, err = sql.Exec(ctx,
		"INSERT INTO sites (site_id, tenant_id, domain, name, created_at, session_salt) VALUES ($1, $2, $3, $4, $5, $6)",
		"default", "default", "localhost", "Default Site", now, generateID(),
	)
	if err != nil {
		return fmt.Errorf("sites: insert default: %w", err)
	}
	return nil
}

// ListRatelimits returns {site_id: ratelimit_per_second} for every site.
// Used at boot to hydrate the in-memory rate limiter cache.
func (s *SiteService) ListRatelimits(ctx context.Context) (map[string]int, error) {
	type row struct {
		SiteID string `db:"site_id"`
		Rate   int    `db:"ratelimit_per_second"`
	}
	sql := s.db.SQL()
	rows, err := nucleus.Query[row](ctx, sql,
		"SELECT site_id, ratelimit_per_second FROM sites",
	)
	if err != nil {
		return nil, fmt.Errorf("sites: list ratelimits: %w", err)
	}
	out := make(map[string]int, len(rows))
	for _, r := range rows {
		out[r.SiteID] = r.Rate
	}
	return out, nil
}

// SetRatelimit updates the per-site events-per-second cap. 0 means "default".
func (s *SiteService) SetRatelimit(ctx context.Context, siteID string, ratePerSecond int) error {
	_, err := s.db.SQL().Exec(ctx,
		"UPDATE sites SET ratelimit_per_second = $1 WHERE site_id = $2",
		ratePerSecond, siteID,
	)
	if err != nil {
		return fmt.Errorf("sites: set ratelimit: %w", err)
	}
	return nil
}

// SetRawDistinctID flips the per-site privacy opt-out (store raw distinct_id vs
// hash it) and invalidates the privacy cache so the next ingest sees the change.
func (s *SiteService) SetRawDistinctID(ctx context.Context, siteID string, optIn bool) error {
	_, err := s.db.SQL().Exec(ctx,
		"UPDATE sites SET raw_distinct_id = $1 WHERE site_id = $2",
		optIn, siteID,
	)
	if err != nil {
		return fmt.Errorf("sites: set raw_distinct_id: %w", err)
	}
	s.InvalidatePrivacyConfig(siteID)
	return nil
}

// managementRow maps to the site_management table.
type managementRow struct {
	SiteID      string `db:"site_id"`
	ManagedBy   string `db:"managed_by"`
	ExternalRef string `db:"external_ref"`
	CreatedAt   int64  `db:"created_at"`
}

// EnsureManaged is the idempotent create behind POST /api/v1/service/sites: the
// same (managedBy, externalRef) always answers with the same site. The ref is
// namespaced by manager, so one manager can neither collide with another's ref
// nor learn that it exists.
//
// The mapping row is written before the site. A crash between the two leaves a
// mapping to nothing, which every read ignores and a retry replaces; the other
// order would leave an orphan site visible in the dashboard forever.
func (s *SiteService) EnsureManaged(ctx context.Context, managedBy, externalRef, domain, name string) (Site, bool, error) {
	if managedBy == "" || externalRef == "" {
		return Site{}, false, fmt.Errorf("sites: managed site needs a manager and an external ref")
	}
	s.managedMu.Lock()
	defer s.managedMu.Unlock()

	sql := s.db.SQL()
	rows, err := nucleus.Query[managementRow](ctx, sql,
		"SELECT site_id, managed_by, external_ref, created_at FROM site_management WHERE managed_by = $1 AND external_ref = $2",
		managedBy, externalRef,
	)
	if err != nil {
		return Site{}, false, fmt.Errorf("sites: find managed site: %w", err)
	}
	// Oldest first, so that if a second process ever raced a duplicate in,
	// every caller still converges on one site.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].CreatedAt != rows[j].CreatedAt {
			return rows[i].CreatedAt < rows[j].CreatedAt
		}
		return rows[i].SiteID < rows[j].SiteID
	})
	for _, m := range rows {
		exists, err := s.Exists(ctx, m.SiteID)
		if err != nil {
			return Site{}, false, err
		}
		if !exists {
			continue // the site was deleted; its ref is free again
		}
		site, err := s.Get(ctx, m.SiteID)
		if err != nil {
			return Site{}, false, err
		}
		site.ExternalRef, site.ManagedBy = externalRef, managedBy
		return site, false, nil
	}

	siteID := generateID()
	sessionSalt := generateID()
	now := time.Now().UnixMilli()
	nowStr := dbutil.IntParam(now)
	if _, err := sql.Exec(ctx,
		"INSERT INTO site_management (tenant_id, site_id, managed_by, external_ref, created_at) VALUES ('default', $1, $2, $3, $4)",
		siteID, managedBy, externalRef, nowStr,
	); err != nil {
		return Site{}, false, fmt.Errorf("sites: record managed site: %w", err)
	}
	if _, err := sql.Exec(ctx,
		"INSERT INTO sites (site_id, tenant_id, domain, name, created_at, session_salt) VALUES ($1, $2, $3, $4, $5, $6)",
		siteID, "default", domain, name, nowStr, sessionSalt,
	); err != nil {
		return Site{}, false, fmt.Errorf("sites: create managed site: %w", err)
	}
	return Site{
		SiteID: siteID, TenantID: "default", Domain: domain, Name: name,
		CreatedAt: now, SessionSalt: sessionSalt,
		ExternalRef: externalRef, ManagedBy: managedBy,
	}, true, nil
}

// ErrSiteNotFound means the site to adopt does not exist.
var ErrSiteNotFound = errors.New("site not found")

// ErrManagementConflict means adoption would give a site a second owner, or give a
// manager two sites for one ref, which would break EnsureManaged's idempotence.
var ErrManagementConflict = errors.New("site management conflict")

// Adopt hands an existing operator site to a manager under an external ref, so a
// service token can then reach it. It returns false when the site is already
// exactly that, so a retry is harmless. It never moves a site between managers:
// that would silently take it away from the first one, so the caller has to refuse
// and an operator has to decide.
func (s *SiteService) Adopt(ctx context.Context, siteID, managedBy, externalRef string) (Site, bool, error) {
	if managedBy == "" || externalRef == "" {
		return Site{}, false, fmt.Errorf("sites: adoption needs a manager and an external ref")
	}
	s.managedMu.Lock()
	defer s.managedMu.Unlock()

	exists, err := s.Exists(ctx, siteID)
	if err != nil {
		return Site{}, false, err
	}
	if !exists {
		return Site{}, false, ErrSiteNotFound
	}
	sql := s.db.SQL()
	current, err := nucleus.Query[managementRow](ctx, sql,
		"SELECT site_id, managed_by, external_ref, created_at FROM site_management WHERE site_id = $1", siteID)
	if err != nil {
		return Site{}, false, fmt.Errorf("sites: read site manager: %w", err)
	}
	for _, m := range current {
		if m.ManagedBy != managedBy || m.ExternalRef != externalRef {
			return Site{}, false, fmt.Errorf("%w: site is already managed by %q as %q", ErrManagementConflict, m.ManagedBy, m.ExternalRef)
		}
	}
	adopted := len(current) == 0
	if adopted {
		taken, err := nucleus.Query[managementRow](ctx, sql,
			"SELECT site_id, managed_by, external_ref, created_at FROM site_management WHERE managed_by = $1 AND external_ref = $2",
			managedBy, externalRef)
		if err != nil {
			return Site{}, false, fmt.Errorf("sites: read managed refs: %w", err)
		}
		for _, m := range taken {
			live, err := s.Exists(ctx, m.SiteID)
			if err != nil {
				return Site{}, false, err
			}
			if live {
				return Site{}, false, fmt.Errorf("%w: %q already names site %s for this manager", ErrManagementConflict, externalRef, m.SiteID)
			}
		}
		if _, err := sql.Exec(ctx,
			"INSERT INTO site_management (tenant_id, site_id, managed_by, external_ref, created_at) VALUES ('default', $1, $2, $3, $4)",
			siteID, managedBy, externalRef, dbutil.IntParam(time.Now().UnixMilli()),
		); err != nil {
			return Site{}, false, fmt.Errorf("sites: record adoption: %w", err)
		}
	}
	site, err := s.Get(ctx, siteID)
	if err != nil {
		return Site{}, false, err
	}
	site.ExternalRef, site.ManagedBy = externalRef, managedBy
	return site, adopted, nil
}

// IsManagedBy reports whether a live site belongs to managedBy. An empty
// manager owns nothing, so an operator-created site can never match. A store
// failure is an error, never a false "not yours" and never a true.
func (s *SiteService) IsManagedBy(ctx context.Context, siteID, managedBy string) (bool, error) {
	if siteID == "" || managedBy == "" {
		return false, nil
	}
	rows, err := nucleus.Query[managementRow](ctx, s.db.SQL(),
		"SELECT site_id, managed_by, external_ref, created_at FROM site_management WHERE site_id = $1 AND managed_by = $2",
		siteID, managedBy,
	)
	if err != nil {
		return false, fmt.Errorf("sites: check manager: %w", err)
	}
	if len(rows) == 0 {
		return false, nil
	}
	return s.Exists(ctx, siteID)
}

// Delete removes a site by ID and drops its cached privacy config so a deleted
// (or recreated) site can't keep serving a stale salt/opt-out.
func (s *SiteService) Delete(ctx context.Context, siteID string) error {
	sql := s.db.SQL()
	_, err := sql.Exec(ctx,
		"DELETE FROM sites WHERE site_id = $1",
		siteID,
	)
	if err != nil {
		return fmt.Errorf("sites: delete: %w", err)
	}
	s.InvalidatePrivacyConfig(siteID)
	// Best effort: every read of site_management confirms the site exists, so
	// a leftover row is inert and must not fail a deletion that succeeded.
	_, _ = sql.Exec(ctx, "DELETE FROM site_management WHERE site_id = $1", siteID)
	return nil
}

func generateID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
