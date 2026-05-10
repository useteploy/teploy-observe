package tracking

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"
)

// 1x1 transparent GIF (43 bytes).
var transparentGIF = []byte{
	0x47, 0x49, 0x46, 0x38, 0x39, 0x61, 0x01, 0x00,
	0x01, 0x00, 0x80, 0x00, 0x00, 0xff, 0xff, 0xff,
	0x00, 0x00, 0x00, 0x21, 0xf9, 0x04, 0x01, 0x00,
	0x00, 0x00, 0x00, 0x2c, 0x00, 0x00, 0x00, 0x00,
	0x01, 0x00, 0x01, 0x00, 0x00, 0x02, 0x02, 0x44,
	0x01, 0x00, 0x3b,
}

// LinkService handles tracked link operations backed by Nucleus.
type LinkService struct {
	DB *nucleus.Client
}

func NewLinkService(db *nucleus.Client) *LinkService {
	return &LinkService{DB: db}
}

// TrackedLink represents a shortened/tracked link stored in the links table.
type TrackedLink struct {
	LinkID     string `db:"link_id"`
	TenantID   string `db:"tenant_id"`
	SiteID     string `db:"site_id"`
	Name       string `db:"name"`
	Destination string `db:"destination"`
	Slug       string `db:"slug"`
	ClickCount string `db:"click_count"`
	CreatedAt  string `db:"created_at"`
	Version    string `db:"version"`
}

// LinkClick represents a single click event on a tracked link.
type LinkClick struct {
	ClickID   string `db:"click_id"`
	TenantID  string `db:"tenant_id"`
	LinkID    string `db:"link_id"`
	Timestamp int64  `db:"timestamp"`
	Referrer  string `db:"referrer"`
	Country   string `db:"country"`
	Browser   string `db:"browser"`
	Device    string `db:"device"`
}

// generateID returns a random hex string of the given byte length (output is 2x chars).
func generateID(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("tracking: generate id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// generateSlug returns a random 8-character alphanumeric slug.
func generateSlug() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("tracking: generate slug: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// CreateLink creates a new tracked link with a random 8-char slug.
func (s *LinkService) CreateLink(ctx context.Context, tenantID, siteID, name, destination string) (TrackedLink, error) {
	linkID, err := generateID(16)
	if err != nil {
		return TrackedLink{}, err
	}

	slug, err := generateSlug()
	if err != nil {
		return TrackedLink{}, err
	}

	now := time.Now().UTC().Format(time.RFC3339)

	link := TrackedLink{
		LinkID:      linkID,
		TenantID:    tenantID,
		SiteID:      siteID,
		Name:        name,
		Destination: destination,
		Slug:        slug,
		ClickCount:  "0",
		CreatedAt:   now,
		Version:     "1",
	}

	_, err = s.DB.SQL().Exec(ctx,
		`INSERT INTO tracked_links (link_id, tenant_id, site_id, name, destination, slug, click_count, created_at, version)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		link.LinkID, link.TenantID, link.SiteID, link.Name, link.Destination,
		link.Slug, link.ClickCount, link.CreatedAt, link.Version,
	)
	if err != nil {
		return TrackedLink{}, fmt.Errorf("tracking: create link: %w", err)
	}

	return link, nil
}

// ListLinks returns all tracked links for a given site.
func (s *LinkService) ListLinks(ctx context.Context, tenantID, siteID string) ([]TrackedLink, error) {
	links, err := nucleus.Query[TrackedLink](ctx, s.DB.SQL(),
		`SELECT link_id, tenant_id, site_id, name, destination, slug, click_count, created_at, version
		 FROM tracked_links WHERE tenant_id = $1 AND site_id = $2 ORDER BY created_at DESC`,
		tenantID, siteID,
	)
	if err != nil {
		return nil, fmt.Errorf("tracking: list links: %w", err)
	}
	return links, nil
}

// GetLinkBySlug finds a tracked link by its slug.
func (s *LinkService) GetLinkBySlug(ctx context.Context, slug string) (TrackedLink, error) {
	link, err := nucleus.QueryOne[TrackedLink](ctx, s.DB.SQL(),
		`SELECT link_id, tenant_id, site_id, name, destination, slug, click_count, created_at, version
		 FROM tracked_links WHERE slug = $1`,
		slug,
	)
	if err != nil {
		return TrackedLink{}, fmt.Errorf("tracking: get link by slug: %w", err)
	}
	return link, nil
}

// RecordClick records a click event for a link identified by slug.
// It inserts a click row and re-inserts the link with an incremented click_count
// and higher version so the ReplacingMergeTree collapses the old row.
func (s *LinkService) RecordClick(ctx context.Context, slug, referrer, country, browser, device string) error {
	link, err := s.GetLinkBySlug(ctx, slug)
	if err != nil {
		return fmt.Errorf("tracking: record click: %w", err)
	}

	clickID, err := generateID(16)
	if err != nil {
		return err
	}

	now := time.Now().UTC().UnixMilli()

	// Insert click event.
	_, err = s.DB.SQL().Exec(ctx,
		`INSERT INTO link_clicks (click_id, tenant_id, link_id, timestamp, referrer, country, browser, device)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		clickID, link.TenantID, link.LinkID, now, referrer, country, browser, device,
	)
	if err != nil {
		return fmt.Errorf("tracking: insert click: %w", err)
	}

	// Bump click_count by re-inserting with a higher version (ReplacingMergeTree).
	newCount := fmt.Sprintf("%d", parseInt(link.ClickCount)+1)
	newVersion := fmt.Sprintf("%d", parseInt(link.Version)+1)

	_, err = s.DB.SQL().Exec(ctx,
		`INSERT INTO tracked_links (link_id, tenant_id, site_id, name, destination, slug, click_count, created_at, version)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		link.LinkID, link.TenantID, link.SiteID, link.Name, link.Destination,
		link.Slug, newCount, link.CreatedAt, newVersion,
	)
	if err != nil {
		return fmt.Errorf("tracking: bump click count: %w", err)
	}

	return nil
}

// parseInt parses a string to int64, returning 0 on failure.
func parseInt(s string) int64 {
	var n int64
	fmt.Sscanf(s, "%d", &n)
	return n
}

// ClickHandler returns an http.HandlerFunc that looks up a link by slug from the
// URL path, records a click, and issues a 302 redirect to the destination URL.
// The slug is expected as the last path segment.
func (s *LinkService) ClickHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slug := r.PathValue("slug")
		if slug == "" {
			http.Error(w, "missing slug", http.StatusBadRequest)
			return
		}

		link, err := s.GetLinkBySlug(r.Context(), slug)
		if err != nil {
			http.Error(w, "link not found", http.StatusNotFound)
			return
		}

		referrer := r.Referer()
		country := r.Header.Get("CF-IPCountry")
		browser := r.UserAgent()
		device := ""

		_ = s.RecordClick(r.Context(), slug, referrer, country, browser, device)

		http.Redirect(w, r, link.Destination, http.StatusFound)
	}
}

// PixelHandler returns an http.HandlerFunc that serves a 1x1 transparent GIF
// and records the request as a tracking event in the link_clicks table.
// The link slug is expected as a path parameter.
func (s *LinkService) PixelHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slug := r.PathValue("slug")

		if slug != "" {
			referrer := r.Referer()
			country := r.Header.Get("CF-IPCountry")
			browser := r.UserAgent()
			device := ""

			_ = s.RecordClick(r.Context(), slug, referrer, country, browser, device)
		}

		w.Header().Set("Content-Type", "image/gif")
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		w.WriteHeader(http.StatusOK)
		w.Write(transparentGIF)
	}
}
