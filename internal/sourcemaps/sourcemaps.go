package sourcemaps

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"
)

// SourceMapService manages source map uploads and stack trace resolution.
// Source maps are stored in Nucleus KV with key format: srcmap:{site_id}:{release}:{filename}
type SourceMapService struct {
	db *nucleus.Client
}

func NewSourceMapService(db *nucleus.Client) *SourceMapService {
	return &SourceMapService{db: db}
}

// SourceMapping represents a single mapping from generated to original position.
type SourceMapping struct {
	GeneratedLine   int    `json:"generated_line"`
	GeneratedColumn int    `json:"generated_column"`
	OriginalFile    string `json:"original_file"`
	OriginalLine    int    `json:"original_line"`
	OriginalColumn  int    `json:"original_column"`
	OriginalName    string `json:"original_name"`
}

// SourceMapMeta is the parsed header of a source map.
type SourceMapMeta struct {
	Version  int      `json:"version"`
	File     string   `json:"file"`
	Sources  []string `json:"sources"`
	Names    []string `json:"names"`
	Mappings string   `json:"mappings"`
}

// DefaultKeepReleases is how many releases of source maps a site retains.
//
// Overridable with OBSERVE_SOURCEMAP_KEEP_RELEASES.
const DefaultKeepReleases = 10

// Upload stores a source map in KV, associated with a release and filename.
//
// Deliberately stored without a TTL. Nucleus keeps entries that carry an expiry
// resident in memory — its cold tier has no way to represent a deadline — so a
// TTL here would pin every source map in RAM until it expired, which is the
// opposite of what is wanted for multi-megabyte blobs. Without one they are
// free to spill to disk under pressure and are bounded instead by the release
// retention below.
func (s *SourceMapService) Upload(ctx context.Context, siteID, release, filename string, mapData []byte) error {
	kv := s.db.KV()
	key := kvKey(siteID, release, filename)
	if err := kv.Set(ctx, key, mapData); err != nil {
		return err
	}
	// Record when this release was last written so retention can order them.
	// Score is refreshed on every upload, so a release stays "recent" while it
	// is still being published to.
	if _, err := kv.ZAdd(ctx, releaseAgeKey(siteID), float64(time.Now().Unix()), release); err != nil {
		return fmt.Errorf("recording release age: %w", err)
	}
	return nil
}

// KeepReleases resolves the retention count from the environment.
func KeepReleases() int {
	if raw := strings.TrimSpace(os.Getenv("OBSERVE_SOURCEMAP_KEEP_RELEASES")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return n
		}
	}
	return DefaultKeepReleases
}

// releaseAgeKey is the sorted set ordering a site's releases by last upload.
func releaseAgeKey(siteID string) string {
	return "srcmap:relage:" + siteID
}

// PruneReleases deletes source maps for all but the `keep` most recently
// uploaded releases of a site. Returns the number of releases removed.
//
// Source maps were previously kept forever: one entry per site x release x
// file, several megabytes each, with no expiry and no delete path anywhere in
// the service. A production instance accumulated 4.8 GB this way, which was
// enough to push the database over its memory limit and make it refuse writes.
//
// Releases predating the age index have no score. They are treated as oldest
// and pruned first, which is both correct — they are by definition older than
// anything recorded since — and how existing installations reclaim their space.
func (s *SourceMapService) PruneReleases(ctx context.Context, siteID string, keep int) (int, error) {
	if keep < 1 {
		keep = 1
	}
	kv := s.db.KV()

	// Seed any release that predates the age index, so it can be ordered.
	tracked, err := kv.SMembers(ctx, releasesSetKey(siteID))
	if err != nil {
		return 0, fmt.Errorf("listing releases: %w", err)
	}
	scored, err := kv.ZRange(ctx, releaseAgeKey(siteID), 0, -1)
	if err != nil {
		return 0, fmt.Errorf("reading release ages: %w", err)
	}
	known := make(map[string]struct{}, len(scored))
	for _, r := range scored {
		known[r] = struct{}{}
	}
	for _, r := range tracked {
		if _, ok := known[r]; ok {
			continue
		}
		if _, err := kv.ZAdd(ctx, releaseAgeKey(siteID), 0, r); err != nil {
			return 0, fmt.Errorf("seeding release age: %w", err)
		}
	}

	ordered, err := kv.ZRange(ctx, releaseAgeKey(siteID), 0, -1)
	if err != nil {
		return 0, fmt.Errorf("ordering releases: %w", err)
	}
	if len(ordered) <= keep {
		return 0, nil
	}

	// Retained releases are needed by the delete step so it never removes their
	// keys — see deleteRelease.
	retained := make([]string, 0, keep)
	retained = append(retained, ordered[len(ordered)-keep:]...)

	removed := 0
	for _, release := range ordered[:len(ordered)-keep] {
		if err := s.deleteRelease(ctx, siteID, release, retained); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// deleteRelease removes every source map belonging to one release, plus its
// index entries.
//
// Files are found by key prefix rather than from a stored file list, so this
// also cleans up releases uploaded before any index existed — which is the case
// for every release currently on disk.
func (s *SourceMapService) deleteRelease(ctx context.Context, siteID, release string, retained []string) error {
	kv := s.db.KV()
	// KV_KEYS is issued directly rather than through the SDK: this repo vendors
	// its dependencies, and the vendored SDK predates a KV.Keys helper. Adding
	// one means re-vendoring, which currently pulls unrelated drift in the local
	// SDK and breaks the build (neutronauth.WithClaims has since been removed
	// upstream). Reconciling that is worth doing, and is not this change.
	var raw string
	if err := s.db.Pool().QueryRow(ctx, "SELECT KV_KEYS($1)",
		kvKey(siteID, release, "")+"*").Scan(&raw); err != nil {
		return fmt.Errorf("listing source maps for %s: %w", release, err)
	}
	var keys []string
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &keys); err != nil {
			return fmt.Errorf("decoding source map keys for %s: %w", release, err)
		}
	}
	// Release names come from the upload request, so one can be a prefix of
	// another: deleting "v1" by prefix would also take "v1:beta" with it. Skip
	// any key that belongs to a release being kept.
	for _, k := range keys {
		if belongsToOther(k, siteID, release, retained) {
			continue
		}
		if _, err := kv.Delete(ctx, k); err != nil {
			return fmt.Errorf("deleting %s: %w", k, err)
		}
	}
	if _, err := kv.ZRem(ctx, releaseAgeKey(siteID), release); err != nil {
		return fmt.Errorf("untracking release age: %w", err)
	}
	if _, err := kv.SRem(ctx, releasesSetKey(siteID), release); err != nil {
		return fmt.Errorf("untracking release: %w", err)
	}
	return nil
}

// ListReleases returns all releases that have source maps for a site.
//
// Backed by a native Nucleus KV Set (KV_SADD/KV_SMEMBERS) rather than a
// single JSON-blob value. The blob approach did a client-side
// read-modify-write with no compare-and-swap: two concurrent uploads for
// different releases could both read the same old list, append their own,
// and overwrite each other, silently dropping a release from this list even
// though its source-map blob still exists in KV (OBS-025). A Set add is
// atomic on the engine side (verified: 30 concurrent SADDs for distinct
// members all survive), so concurrent uploads for different releases can no
// longer collide. This also fixes OBS-026: a genuine KV read failure is now
// returned as an error instead of silently reported as "no releases", and
// there is no longer a read-modify-write step that could overwrite an index
// after a failed read.
func (s *SourceMapService) ListReleases(ctx context.Context, siteID string) ([]string, error) {
	kv := s.db.KV()
	releases, err := kv.SMembers(ctx, releasesSetKey(siteID))
	if err != nil {
		return nil, fmt.Errorf("listing releases: %w", err)
	}
	return releases, nil
}

// TrackRelease adds a release to the known releases set. Idempotent: adding
// an already-tracked release is a no-op (SAdd reports false, not an error).
func (s *SourceMapService) TrackRelease(ctx context.Context, siteID, release string) error {
	kv := s.db.KV()
	if _, err := kv.SAdd(ctx, releasesSetKey(siteID), release); err != nil {
		return fmt.Errorf("tracking release: %w", err)
	}
	return nil
}

// releasesSetKey is the KV Set key holding a site's known releases. siteID
// can't itself contain ':' safely without risking collision with a future
// key scheme, but site IDs are generated by this system (not user-chosen
// free text), so no additional encoding is applied here.
func releasesSetKey(siteID string) string {
	return "srcmap:releases:" + siteID
}

// ResolveFrame attempts to map a minified stack frame to its original source.
// Returns the original frame info or nil if no source map is available.
func (s *SourceMapService) ResolveFrame(ctx context.Context, siteID, release, filename string, line, col int) (*SourceMapping, error) {
	kv := s.db.KV()
	key := kvKey(siteID, release, filename)
	data, err := kv.Get(ctx, key)
	if err != nil || data == nil {
		return nil, nil // no source map available
	}

	var meta SourceMapMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("parse source map: %w", err)
	}

	// Decode VLQ mappings and find the closest match
	mapping := decodeMappings(meta.Mappings, meta.Sources, meta.Names, line, col)
	return mapping, nil
}

// ResolveStackTrace resolves all frames in a stack trace JSON string.
func (s *SourceMapService) ResolveStackTrace(ctx context.Context, siteID, release, stackJSON string) (string, error) {
	if stackJSON == "" || release == "" {
		return stackJSON, nil
	}

	type frame struct {
		Filename string `json:"filename"`
		Function string `json:"function"`
		Lineno   int    `json:"lineno"`
		Colno    int    `json:"colno"`
		InApp    bool   `json:"in_app"`
	}

	var frames []frame
	if err := json.Unmarshal([]byte(stackJSON), &frames); err != nil {
		return stackJSON, nil
	}

	for i, f := range frames {
		mapping, err := s.ResolveFrame(ctx, siteID, release, f.Filename, f.Lineno, f.Colno)
		if err != nil || mapping == nil {
			continue
		}
		frames[i].Filename = mapping.OriginalFile
		frames[i].Lineno = mapping.OriginalLine
		frames[i].Colno = mapping.OriginalColumn
		if mapping.OriginalName != "" {
			frames[i].Function = mapping.OriginalName
		}
	}

	result, _ := json.Marshal(frames)
	return string(result), nil
}

func kvKey(siteID, release, filename string) string {
	return fmt.Sprintf("srcmap:%s:%s:%s", siteID, release, filename)
}

// decodeMappings parses VLQ-encoded source map mappings and finds the closest
// mapping on the target line. The source index, source line, source column and
// name index are deltas that accumulate across ALL preceding lines — only the
// generated column resets at each line (';'). The previous version decoded the
// target line in isolation with all accumulators starting at 0, producing wrong
// original positions for every frame past the first mappings line. This walks
// from line 0, advancing the accumulators on every segment, and only does the
// closest-column match once it reaches the target line.
func decodeMappings(mappings string, sources, names []string, targetLine, targetCol int) *SourceMapping {
	if mappings == "" {
		return nil
	}

	lines := strings.Split(mappings, ";")
	if targetLine <= 0 || targetLine > len(lines) {
		return nil
	}

	// Accumulators carried across lines.
	var srcIdx, srcLine, srcCol, nameIdx int
	var bestMapping *SourceMapping

	for li := 0; li < targetLine; li++ {
		genCol := 0 // generated column resets at each line
		isTarget := li == targetLine-1
		bestDist := int(^uint(0) >> 1) // max int

		if lines[li] == "" {
			continue
		}
		for _, seg := range strings.Split(lines[li], ",") {
			if seg == "" {
				continue
			}
			values := decodeVLQ(seg)
			if len(values) == 0 {
				continue
			}
			genCol += values[0]
			if len(values) < 4 {
				// Generated-column-only segment: no source mapping.
				continue
			}
			srcIdx += values[1]
			srcLine += values[2]
			srcCol += values[3]
			hasName := len(values) >= 5
			if hasName {
				nameIdx += values[4]
			}

			if !isTarget {
				continue
			}
			dist := abs(genCol - (targetCol - 1))
			if dist < bestDist {
				bestDist = dist
				src := ""
				if srcIdx >= 0 && srcIdx < len(sources) {
					src = sources[srcIdx]
				}
				name := ""
				if hasName && nameIdx >= 0 && nameIdx < len(names) {
					name = names[nameIdx]
				}
				bestMapping = &SourceMapping{
					GeneratedLine:   targetLine,
					GeneratedColumn: genCol + 1,
					OriginalFile:    src,
					OriginalLine:    srcLine + 1,
					OriginalColumn:  srcCol + 1,
					OriginalName:    name,
				}
			}
		}
	}

	return bestMapping
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// decodeVLQ decodes a base64-VLQ encoded string into a slice of integers.
func decodeVLQ(s string) []int {
	var result []int
	shift := 0
	value := 0

	for _, c := range s {
		digit := vlqCharToInt(byte(c))
		if digit < 0 {
			continue
		}
		value += (digit & 0x1f) << shift
		if digit&0x20 == 0 {
			if value&1 != 0 {
				result = append(result, -(value >> 1))
			} else {
				result = append(result, value>>1)
			}
			value = 0
			shift = 0
		} else {
			shift += 5
		}
	}

	return result
}

func vlqCharToInt(c byte) int {
	if c >= 'A' && c <= 'Z' {
		return int(c - 'A')
	}
	if c >= 'a' && c <= 'z' {
		return int(c-'a') + 26
	}
	if c >= '0' && c <= '9' {
		return int(c-'0') + 52
	}
	if c == '+' {
		return 62
	}
	if c == '/' {
		return 63
	}
	return -1
}

func genID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// unused but needed for interface compatibility
var _ = strconv.Itoa
var _ = time.Now

// belongsToOther reports whether a key matched by `release`'s prefix in fact
// belongs to a longer release name that is being retained.
//
// `srcmap:site:v1:*` matches `srcmap:site:v1:beta:app.js`, so a prefix match
// alone would delete a retained release's maps as a side effect of pruning an
// older one whose name happens to be a prefix of it.
func belongsToOther(key, siteID, release string, retained []string) bool {
	for _, other := range retained {
		if other == release || len(other) <= len(release) {
			continue
		}
		if strings.HasPrefix(key, kvKey(siteID, other, "")) {
			return true
		}
	}
	return false
}
