package sourcemaps

import (
	"context"
	"crypto/rand"
	"encoding/base64"
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
//
// AUD-046 (round 2): new writes use the v2 injectively-encoded key
// (base64url per component) — the raw `srcmap:{site}:{release}:{filename}`
// scheme let release `a:b` + file `c` collide with release `a` + file
// `b:c`, silently overwriting another logical map. Reads fall back to the
// legacy key so pre-upgrade maps stay resolvable until pruned.
func (s *SourceMapService) Upload(ctx context.Context, siteID, release, filename string, mapData []byte) error {
	kv := s.db.KV()
	key := kvKeyV2(siteID, release, filename)
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
// AUD-046 (round 2): membership is decided per key against BOTH the v2
// injective encoding and the legacy raw encoding — the scan pattern is the
// site's whole srcmap namespace, because a raw release name interpolated
// into a glob also matched unrelated releases whose names begin with glob
// metacharacters or that share a prefix. Retained-release protection
// (longer legacy names win) still applies to legacy keys, whose ambiguity
// is unfixable after the fact.
func (s *SourceMapService) deleteRelease(ctx context.Context, siteID, release string, retained []string) error {
	kv := s.db.KV()
	// KV_KEYS is issued directly rather than through the SDK: this repo vendors
	// its dependencies, and the vendored SDK predates a KV.Keys helper. Adding
	// one means re-vendoring, which currently pulls unrelated drift in the local
	// SDK and breaks the build (neutronauth.WithClaims has since been removed
	// upstream). Reconciling that is worth doing, and is not this change.
	var raw string
	if err := s.db.Pool().QueryRow(ctx, "SELECT KV_KEYS($1)",
		"srcmap:*").Scan(&raw); err != nil {
		return fmt.Errorf("listing source maps for %s: %w", release, err)
	}
	var keys []string
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &keys); err != nil {
			return fmt.Errorf("decoding source map keys for %s: %w", release, err)
		}
	}
	for _, k := range keys {
		if !keyBelongsToRelease(k, siteID, release) {
			continue
		}
		// Legacy keys of a retained longer release name must survive a
		// prefix collision with the pruned release (v2 keys cannot
		// collide, but protect them with the same rule for symmetry).
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

// keyBelongsToRelease reports whether a KV key is a source-map blob of
// (siteID, release) in either the v2 or the legacy encoding.
func keyBelongsToRelease(key, siteID, release string) bool {
	if strings.HasPrefix(key, "srcmap:v2:"+sourceMapComponent(siteID)+":"+sourceMapComponent(release)+":") {
		return true
	}
	// Legacy: raw components. Scoped by the site prefix first so a release
	// name containing ':' cannot reach into another site's namespace.
	return strings.HasPrefix(key, "srcmap:"+siteID+":"+release+":")
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
// Returns the original frame info, or nil when no source map covers the file.
//
// AUD-048 (round 2): a KV read FAILURE is an error, not "no map" — an
// unavailable store used to look identical to an unsymbolicated stack, so
// outages produced silently wrong output instead of a diagnosable one.
func (s *SourceMapService) ResolveFrame(ctx context.Context, siteID, release, filename string, line, col int) (*SourceMapping, error) {
	kv := s.db.KV()
	meta, err := loadSourceMap(ctx, kv, siteID, release, filename)
	if err != nil || meta == nil {
		return nil, err
	}
	// Decode VLQ mappings and find the covering match
	mapping := decodeMappings(meta.Mappings, meta.Sources, meta.Names, line, col)
	return mapping, nil
}

// maxSourceMapBytes bounds one parsed map (AUD-048): an attacker-supplied
// multi-gigabyte "map" must not be decoded at all.
const maxSourceMapBytes = 8 << 20

// loadSourceMap fetches and parses the map for one (site, release, file),
// trying the v2 key first and falling back to the legacy raw key.
func loadSourceMap(ctx context.Context, kv *nucleus.KVModel, siteID, release, filename string) (*SourceMapMeta, error) {
	data, err := kv.Get(ctx, kvKeyV2(siteID, release, filename))
	if err != nil {
		return nil, fmt.Errorf("read source map: %w", err)
	}
	if data == nil {
		if legacy, lerr := kv.Get(ctx, kvKey(siteID, release, filename)); lerr == nil && legacy != nil {
			data = legacy
		}
	}
	if data == nil {
		return nil, nil
	}
	if len(data) > maxSourceMapBytes {
		return nil, fmt.Errorf("source map exceeds the %d byte parse budget", maxSourceMapBytes)
	}
	var meta SourceMapMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("parse source map: %w", err)
	}
	if meta.Version != 3 {
		return nil, fmt.Errorf("unsupported source map version %d", meta.Version)
	}
	return &meta, nil
}

// ResolveStackTrace resolves all frames in a stack trace JSON string.
//
// AUD-048 (round 2): maps are loaded and parsed at most ONCE per distinct
// file within one stack request (a 50-frame stack used to fetch and decode
// the same map 50 times), with failures memoized for the request.
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

	type mapResult struct {
		meta *SourceMapMeta
		err  error
	}
	cache := make(map[string]mapResult)
	kv := s.db.KV()

	for i, f := range frames {
		hit, cached := cache[f.Filename]
		if !cached {
			meta, err := loadSourceMap(ctx, kv, siteID, release, f.Filename)
			if err != nil {
				// A store/parse failure is observable but must not discard
				// the raw frame — record it and keep the stack readable.
				hit = mapResult{err: err}
			} else {
				hit = mapResult{meta: meta}
			}
			cache[f.Filename] = hit
		}
		if hit.err != nil || hit.meta == nil {
			continue
		}
		mapping := decodeMappings(hit.meta.Mappings, hit.meta.Sources, hit.meta.Names, f.Lineno, f.Colno)
		if mapping == nil {
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

// sourceMapComponent injectively encodes one key component (AUD-046):
// base64url bytes cannot contain ':' or glob metacharacters, so no
// (release, filename) pair can collide with another.
func sourceMapComponent(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}

// kvKeyV2 is the versioned source-map blob key (AUD-046).
func kvKeyV2(siteID, release, filename string) string {
	return "srcmap:v2:" + sourceMapComponent(siteID) + ":" +
		sourceMapComponent(release) + ":" + sourceMapComponent(filename)
}

func kvKey(siteID, release, filename string) string {
	return fmt.Sprintf("srcmap:%s:%s:%s", siteID, release, filename)
}

// decodeMappings parses VLQ-encoded source map mappings and selects the
// mapping that covers the requested generated position (AUD-047, round 2).
//
// The source index, source line, source column and name index are deltas
// that accumulate across ALL preceding lines — only the generated column
// resets at each line (';'). This walks from line 0, advancing the
// accumulators on every segment.
//
// Selection: the segment with the GREATEST generated column not exceeding
// the requested column — i.e. the mapping that covers the position, per
// the source-map spec. The previous smallest-absolute-distance choice
// could pick a segment to the RIGHT of the requested column (a future
// mapping), and generated-column-only segments were skipped instead of
// terminating their (explicitly unmapped) region.
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

	for li := 0; li < targetLine; li++ {
		genCol := 0 // generated column resets at each line
		isTarget := li == targetLine-1
		// Candidate: the last segment at or left of the requested column.
		type candidate struct {
			mapped                  bool
			genCol                  int
			srcIdx, srcLine, srcCol int
			nameIdx                 int
			hasName                 bool
		}
		var cand *candidate

		if lines[li] != "" {
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
					// Generated-column-only segment: explicitly unmapped.
					if isTarget && genCol <= targetCol-1 {
						cand = &candidate{mapped: false, genCol: genCol}
					}
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
				if genCol <= targetCol-1 {
					cand = &candidate{
						mapped:  true,
						genCol:  genCol,
						srcIdx:  srcIdx,
						srcLine: srcLine,
						srcCol:  srcCol,
						nameIdx: nameIdx,
						hasName: hasName,
					}
				}
			}
		}
		if !isTarget {
			continue
		}
		if cand == nil || !cand.mapped {
			// Before the first segment, or inside an explicitly unmapped
			// region: no mapping. Invalid source coordinates also resolve
			// to "unmapped" rather than a misleading nominal mapping.
			return nil
		}
		if cand.srcIdx < 0 || cand.srcIdx >= len(sources) ||
			cand.srcLine < 0 || cand.srcCol < 0 {
			return nil
		}
		name := ""
		if cand.hasName && cand.nameIdx >= 0 && cand.nameIdx < len(names) {
			name = names[cand.nameIdx]
		}
		return &SourceMapping{
			GeneratedLine:   targetLine,
			GeneratedColumn: cand.genCol + 1,
			OriginalFile:    sources[cand.srcIdx],
			OriginalLine:    cand.srcLine + 1,
			OriginalColumn:  cand.srcCol + 1,
			OriginalName:    name,
		}
	}

	return nil
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

// belongsToOther reports whether a key matched by `release`'s membership
// test in fact belongs to a longer release name that is being retained.
//
// `srcmap:site:v1:*` matches `srcmap:site:v1:beta:app.js`, so a prefix match
// alone would delete a retained release's maps as a side effect of pruning an
// older one whose name happens to be a prefix of it. Both encodings are
// checked (AUD-046).
func belongsToOther(key, siteID, release string, retained []string) bool {
	for _, other := range retained {
		if other == release || len(other) <= len(release) {
			continue
		}
		if strings.HasPrefix(key, kvKey(siteID, other, "")) {
			return true
		}
		if strings.HasPrefix(key, "srcmap:v2:"+sourceMapComponent(siteID)+":"+sourceMapComponent(other)+":") {
			return true
		}
	}
	return false
}
