package flags

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// ConfigBundle is the body of GET /api/v1/flags/config: every flag of one
// site in evaluable form plus the bucketing spec, so an SDK can evaluate
// locally with EvaluateDefinition-equivalent logic and no per-call round trip.
type ConfigBundle struct {
	SiteID      string           `json:"site_id"`
	GeneratedAt int64            `json:"generated_at"` // unix ms
	Bucketing   map[string]any   `json:"bucketing"`
	Flags       []FlagDefinition `json:"flags"`
}

// BuildConfig converts stored rows to a bundle. Rows whose stored config
// fails validation are included as quarantined definitions (Invalid set,
// rules omitted) so the SDK answers disabled exactly like the server does,
// rather than silently losing the flag or evaluating it unrestricted.
func BuildConfig(siteID string, rows []FeatureFlag, now time.Time) ConfigBundle {
	defs := make([]FlagDefinition, 0, len(rows))
	for _, f := range rows {
		def, cerr := ParseDefinition(f)
		if cerr != nil {
			def = FlagDefinition{
				Key: f.FlagKey, Type: f.FlagType, Enabled: f.Enabled, RolloutPct: f.RolloutPct,
				Invalid: clampDetail(fmt.Sprintf("stored %s failed validation: %v", cerr.Part, cerr.Err)),
			}
		}
		defs = append(defs, def)
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].Key < defs[j].Key })
	return ConfigBundle{SiteID: siteID, GeneratedAt: now.UnixMilli(), Bucketing: BucketingSpec, Flags: defs}
}

// ETag identifies the flag content (not GeneratedAt), so unchanged config
// revalidates with 304.
func (b ConfigBundle) ETag() string {
	raw, _ := json.Marshal(b.Flags)
	sum := sha256.Sum256(raw)
	return `"` + hex.EncodeToString(sum[:12]) + `"`
}

// Config reads every flag of a site and builds the bundle.
func (s *FlagService) Config(ctx context.Context, siteID string) (*ConfigBundle, error) {
	rows, err := s.List(ctx, siteID)
	if err != nil {
		return nil, err
	}
	b := BuildConfig(siteID, rows, time.Now().UTC())
	return &b, nil
}
