package sourcemaps

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
	Mappings string   `json:"mappings"`
}

// Upload stores a source map in KV, associated with a release and filename.
func (s *SourceMapService) Upload(ctx context.Context, siteID, release, filename string, mapData []byte) error {
	kv := s.db.KV()
	key := kvKey(siteID, release, filename)
	return kv.Set(ctx, key, mapData)
}

// ListReleases returns all releases that have source maps for a site.
func (s *SourceMapService) ListReleases(ctx context.Context, siteID string) ([]string, error) {
	// Since KV doesn't support prefix scan easily, we maintain a set
	kv := s.db.KV()
	data, err := kv.Get(ctx, "srcmap:releases:"+siteID)
	if err != nil || data == nil {
		return nil, nil
	}
	var releases []string
	json.Unmarshal(data, &releases)
	return releases, nil
}

// TrackRelease adds a release to the known releases list.
func (s *SourceMapService) TrackRelease(ctx context.Context, siteID, release string) error {
	kv := s.db.KV()
	key := "srcmap:releases:" + siteID
	data, _ := kv.Get(ctx, key)
	var releases []string
	if data != nil {
		json.Unmarshal(data, &releases)
	}
	for _, r := range releases {
		if r == release {
			return nil // already tracked
		}
	}
	releases = append(releases, release)
	raw, _ := json.Marshal(releases)
	return kv.Set(ctx, key, raw)
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
	mapping := decodeMappings(meta.Mappings, meta.Sources, line, col)
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
// mapping to the given line and column. This is a simplified decoder that
// handles the most common cases.
func decodeMappings(mappings string, sources []string, targetLine, targetCol int) *SourceMapping {
	if mappings == "" {
		return nil
	}

	lines := strings.Split(mappings, ";")
	if targetLine <= 0 || targetLine > len(lines) {
		return nil
	}

	// Decode segments on the target line
	line := lines[targetLine-1]
	if line == "" {
		return nil
	}

	segments := strings.Split(line, ",")
	var genCol, srcIdx, srcLine, srcCol, nameIdx int

	var bestMapping *SourceMapping
	bestDist := int(^uint(0) >> 1) // max int

	for _, seg := range segments {
		values := decodeVLQ(seg)
		if len(values) < 4 {
			if len(values) >= 1 {
				genCol += values[0]
			}
			continue
		}
		genCol += values[0]
		srcIdx += values[1]
		srcLine += values[2]
		srcCol += values[3]
		if len(values) >= 5 {
			nameIdx += values[4]
		}

		dist := abs(genCol - (targetCol - 1))
		if dist < bestDist {
			bestDist = dist
			src := ""
			if srcIdx >= 0 && srcIdx < len(sources) {
				src = sources[srcIdx]
			}
			bestMapping = &SourceMapping{
				GeneratedLine:   targetLine,
				GeneratedColumn: genCol + 1,
				OriginalFile:    src,
				OriginalLine:    srcLine + 1,
				OriginalColumn:  srcCol + 1,
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
				result = append(result, -(value>>1))
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
