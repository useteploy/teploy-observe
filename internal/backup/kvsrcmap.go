// KV srcmap backup section (F45's app half).
//
// Source-map blobs and their release indexes live in Nucleus KV, not SQL:
//
//	srcmap:v2:<b64 site>:<b64 release>:<b64 file>  string blob (AUD-046 v2 keys)
//	srcmap:<site>:<release>:<file>                 string blob (legacy raw keys)
//	srcmap:releases:<site>                         SET of known releases
//	srcmap:relage:<site>                           ZSET release -> last-upload unix time
//
// None of that was in a backup: an instance restored from its own archive
// lost every source map and release index, so stacks came back
// unsymbolicated and release retention re-seeded from scratch. This file
// dumps the whole srcmap namespace into one JSONL archive entry and restores
// it with a post-apply completeness check keyed on the namespace (relist
// srcmap:* and compare against the archive's key set).
//
// Consistency, stated honestly: the snapshot lease gates SQL DML but NOT the
// KV scalar functions — `SELECT KV_SET/SADD/ZADD/DEL(...)` from another
// session commit straight through a held lease (verified live against the
// 2026-09-18 engine build; upstream report in Teploy/_internal/
// UPSTREAM_BUGS.md), and the holder's own KV reads are not snapshot-pinned
// either. The KV section therefore cannot inherit the lease's moment the
// way the tables do. What it gets instead is a convergence proof: the whole
// namespace is read twice end-to-end and the dump only accepts two
// DEEP-EQUAL consecutive snapshots (same key set, same types, same values
// byte-for-byte), plus a third key relist that must still agree. Under a
// concurrent srcmap upload or retention delete the two reads disagree and
// the dump fails loudly with a quiesce instruction rather than shipping an
// archive whose KV moment is undefined. Two dumps of a quiesced instance
// remain byte-identical (sorted keys).
//
// Key types are inferred from the namespace's own shape (it is owned by
// internal/sourcemaps; site ids are system-generated, so the relage/releases
// prefixes cannot collide with a blob key). Anything under srcmap: that
// matches neither index shape is dumped as a string blob, which is correct
// for both v2 and legacy blob keys without parsing their components — the
// ambiguity that forced AUD-046's injective encoding does not matter to a
// dump that never interprets components.
package backup

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"
)

// kvSrcmapSection names the one KV section a backup carries today.
const kvSrcmapSection = "srcmap"

// kvSrcmapEntryName is the archive entry holding the section's JSONL.
const kvSrcmapEntryName = "kv-srcmap.jsonl"

// kvSrcmapResultTable is the completion-record name for the section; it
// flows through the same F44 results machinery as SQL tables.
const kvSrcmapResultTable = "kv:srcmap"

// kvSrcmapPattern is the KV glob covering every key above.
const kvSrcmapPattern = "srcmap:*"

// kvSections is the allowlist of dumpable KV sections. A manifest declaring
// anything else is rejected on restore.
var kvSections = map[string]string{
	kvSrcmapSection: kvSrcmapEntryName,
}

// kvConvergenceAttempts bounds how many read-pairs the KV section spends
// hunting for a stable moment under concurrent srcmap writes. Five pairs
// with a short pause between them spans a few seconds — enough to cross an
// in-flight upload, short enough that a genuinely churning namespace fails
// the dump quickly instead of hanging it.
const kvConvergenceAttempts = 5

// kvConvergencePause is the sleep between unsuccessful read-pairs, giving a
// transient upload time to finish and settle.
const kvConvergencePause = 250 * time.Millisecond

// kvLine is one archived KV key. String values are base64-encoded so the
// entry is line-oriented JSONL regardless of blob size or content (source
// maps are dense JSON; escaping them would inflate the line up to 2x).
type kvLine struct {
	Key     string     `json:"key"`
	Type    string     `json:"type"`              // string | set | zset
	Value   string     `json:"value,omitempty"`   // base64 payload (type=string)
	Members []string   `json:"members,omitempty"` // type=set
	Entries []kvZEntry `json:"entries,omitempty"` // type=zset
}

type kvZEntry struct {
	Member string  `json:"member"`
	Score  float64 `json:"score"`
}

// kvSnapshot is one full read of the namespace, key-sorted for
// byte-identical archives.
type kvSnapshot struct {
	keys  []string
	lines map[string]kvLine
}

// maxKVValueBytes bounds one restored string blob: the dump side carries
// whatever the instance holds (uploads are not size-capped), but restore
// must not allocate unbounded memory from an attacker-controlled archive.
const maxKVValueBytes = 256 << 20 // 256 MiB

// maxKVMembers bounds a restored set/zset's element count for the same
// reason.
const maxKVMembers = 100_000

// dumpKVSrcmap writes one JSONL entry covering every key under srcmap:*,
// reading through the dump's runner so the section shares the dump's
// connection. The entry is only written once the namespace has been proven
// stable (two deep-equal consecutive reads, then a matching relist) — the
// lease cannot pin the KV domain (see the package comment above).
func dumpKVSrcmap(ctx context.Context, r pgxRunner, tw *tar.Writer) (int64, error) {
	snap, err := stableKVSnapshot(ctx, r)
	if err != nil {
		return 0, err
	}

	tmp, err := os.CreateTemp("", "observe-backup-kv-srcmap-*.jsonl")
	if err != nil {
		return 0, fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	defer tmp.Close()

	bw := bufio.NewWriter(tmp)
	enc := json.NewEncoder(bw)
	var n int64
	for _, key := range snap.keys {
		if err := enc.Encode(snap.lines[key]); err != nil {
			return 0, fmt.Errorf("marshal kv row %s: %w", key, err)
		}
		n++
	}
	if err := bw.Flush(); err != nil {
		return 0, fmt.Errorf("flush temp file: %w", err)
	}

	info, err := tmp.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat temp file: %w", err)
	}
	hdr := &tar.Header{
		Name:    kvSrcmapEntryName,
		Mode:    0o600, // AUD-045 posture: blobs are user data, metadata included
		Size:    info.Size(),
		ModTime: time.Now(),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return 0, fmt.Errorf("tar header %s: %w", kvSrcmapEntryName, err)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return 0, fmt.Errorf("seek temp file: %w", err)
	}
	if _, err := io.Copy(tw, tmp); err != nil {
		return 0, fmt.Errorf("tar write %s: %w", kvSrcmapEntryName, err)
	}
	return n, nil
}

// errKVChurning is the loud failure for a namespace that would not hold
// still: the archive's KV moment would be undefined, so none is shipped.
var errKVChurning = fmt.Errorf("kv srcmap namespace kept changing during the dump (source-map uploads or retention deletes are live) — quiesce them for the backup and retry")

// stableKVSnapshot reads the whole namespace repeatedly until two
// consecutive full reads agree byte-for-byte AND a closing relist still
// shows the same key set. A key that vanishes or mutates between the reads
// restarts the pair; after kvConvergenceAttempts pairs the dump fails with
// errKVChurning instead of guessing a moment.
func stableKVSnapshot(ctx context.Context, r pgxRunner) (*kvSnapshot, error) {
	var prev *kvSnapshot
	for attempt := 0; attempt < kvConvergenceAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(kvConvergencePause):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		snap, err := readKVSnapshot(ctx, r)
		if err != nil {
			if isKVReadRetryable(err) {
				// A writer deleted a key between list and read; the pair
				// cannot agree, so restart it.
				prev = nil
				continue
			}
			return nil, err
		}
		if prev != nil && reflect.DeepEqual(prev.keys, snap.keys) && kvLinesEqual(prev.lines, snap.lines) {
			// Closing relist: the STRING key set must STILL match, or a
			// writer landed after the agreeing reads and before
			// acceptance. (KV_KEYS cannot see collection keys — upstream —
			// so the derived set/zset keys are reconciled by the two full
			// agreeing reads above, and only the blob keys by this relist.)
			now, err := kvListKeys(ctx, r, kvSrcmapPattern)
			if err != nil {
				return nil, err
			}
			sort.Strings(now)
			if !reflect.DeepEqual(now, stringKeysOf(snap)) {
				prev = snap
				continue
			}
			return snap, nil
		}
		prev = snap
	}
	return nil, errKVChurning
}

// stringKeysOf returns the snapshot's string-typed (KV_KEYS-visible) keys,
// sorted — the comparable subset for relist reconciliation.
func stringKeysOf(snap *kvSnapshot) []string {
	out := make([]string, 0, len(snap.keys))
	for _, k := range snap.keys {
		if kvSrcmapType(k) == "string" {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// isKVReadRetryable recognizes the mid-read interference shapes: a key the
// listing reported but the read can no longer find (deleted between the
// two), reported by readKVKey as the hard error it would be on a quiesced
// instance.
func isKVReadRetryable(err error) bool {
	return strings.Contains(err.Error(), "vanished between list and read")
}

// readKVSnapshot performs one full read of the namespace. KV_KEYS lists
// only string keys — upstream, KvStore::keys iterates the string shards and
// the sets/zsets live in the collections store (reported in
// Teploy/_internal/UPSTREAM_BUGS.md) — so the section's SET and ZSET keys
// are reconstructed from the namespace's own shape instead: their key names
// are deterministic per site id (`srcmap:releases:<site>`,
// `srcmap:relage:<site>`), and every site id appears in its blob keys.
// Residual, stated honestly: a site whose blobs were all deleted while its
// release indexes survived has no string key left to derive the site from,
// so such an orphaned index is not archived; the upstream KV_KEYS fix
// closes this for good.
func readKVSnapshot(ctx context.Context, r pgxRunner) (*kvSnapshot, error) {
	blobKeys, err := kvListKeys(ctx, r, kvSrcmapPattern)
	if err != nil {
		return nil, err
	}
	snap := &kvSnapshot{lines: make(map[string]kvLine, len(blobKeys))}
	for _, key := range blobKeys {
		line, err := dumpKVKey(ctx, r, key)
		if err != nil {
			return nil, err
		}
		snap.lines[key] = line
	}
	for _, site := range srcmapSites(blobKeys) {
		members, err := kvSMembers(ctx, r, "srcmap:releases:"+site)
		if err != nil {
			return nil, err
		}
		if len(members) > 0 {
			key := "srcmap:releases:" + site
			sort.Strings(members)
			snap.lines[key] = kvLine{Key: key, Type: "set", Members: members}
		}
		entries, err := kvZRangeAll(ctx, r, "srcmap:relage:"+site)
		if err != nil {
			return nil, err
		}
		if len(entries) > 0 {
			key := "srcmap:relage:" + site
			snap.lines[key] = kvLine{Key: key, Type: "zset", Entries: entries}
		}
	}
	snap.keys = make([]string, 0, len(snap.lines))
	for key := range snap.lines {
		snap.keys = append(snap.keys, key)
	}
	sort.Strings(snap.keys)
	return snap, nil
}

// srcmapSites derives the distinct site ids represented by a set of srcmap
// blob keys. v2 keys carry the site base64url-encoded as their second
// component; legacy keys carry it raw as their first. Reserved prefixes
// (releases/relage) can never be a blob key's site component.
func srcmapSites(blobKeys []string) []string {
	seen := make(map[string]bool)
	for _, key := range blobKeys {
		rest := strings.TrimPrefix(key, "srcmap:")
		if site, ok := strings.CutPrefix(rest, "v2:"); ok {
			enc := site
			if i := strings.IndexByte(site, ':'); i >= 0 {
				enc = site[:i]
			}
			if raw, err := base64.RawURLEncoding.DecodeString(enc); err == nil {
				s := string(raw)
				if s != "" && !isSrcmapReserved(s) {
					seen[s] = true
				}
			}
			continue
		}
		site := rest
		if i := strings.IndexByte(rest, ':'); i >= 0 {
			site = rest[:i]
		}
		if site != "" && !isSrcmapReserved(site) {
			seen[site] = true
		}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func isSrcmapReserved(component string) bool {
	switch component {
	case "v2", "releases", "relage":
		return true
	}
	return false
}

func kvLinesEqual(a, b map[string]kvLine) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok || av.Key != bv.Key || av.Type != bv.Type || av.Value != bv.Value {
			return false
		}
		if !reflect.DeepEqual(av.Members, bv.Members) || !reflect.DeepEqual(av.Entries, bv.Entries) {
			return false
		}
	}
	return true
}

// dumpKVKey reads one key's full content in its native type.
func dumpKVKey(ctx context.Context, r pgxRunner, key string) (kvLine, error) {
	switch kvSrcmapType(key) {
	case "set":
		members, err := kvSMembers(ctx, r, key)
		if err != nil {
			return kvLine{}, err
		}
		sort.Strings(members)
		return kvLine{Key: key, Type: "set", Members: members}, nil
	case "zset":
		entries, err := kvZRangeAll(ctx, r, key)
		if err != nil {
			return kvLine{}, err
		}
		return kvLine{Key: key, Type: "zset", Entries: entries}, nil
	default:
		var val *string
		if err := r.QueryRow(ctx, "SELECT KV_GET($1)", key).Scan(&val); err != nil {
			return kvLine{}, fmt.Errorf("kv get %s: %w", key, err)
		}
		if val == nil {
			// On a quiesced namespace this is impossible; under a live
			// writer a retention delete landed between listing and read.
			// stableKVSnapshot decides which reading applies.
			return kvLine{}, fmt.Errorf("kv key %s vanished between list and read", key)
		}
		return kvLine{Key: key, Type: "string", Value: base64.StdEncoding.EncodeToString([]byte(*val))}, nil
	}
}

// kvSrcmapType classifies a srcmap-namespace key from its shape.
func kvSrcmapType(key string) string {
	rest := strings.TrimPrefix(key, "srcmap:")
	switch {
	case strings.HasPrefix(rest, "relage:"):
		return "zset"
	case strings.HasPrefix(rest, "releases:"):
		return "set"
	default:
		return "string"
	}
}

// kvListKeys enumerates keys matching a glob. KV_KEYS is issued as raw SQL
// because the vendored SDK predates a Keys helper (see deleteRelease's note
// in internal/sourcemaps for why it is not being re-vendored).
func kvListKeys(ctx context.Context, r pgxRunner, pattern string) ([]string, error) {
	var raw *string
	if err := r.QueryRow(ctx, "SELECT KV_KEYS($1)", pattern).Scan(&raw); err != nil {
		return nil, fmt.Errorf("kv keys %s: %w", pattern, err)
	}
	if raw == nil || *raw == "" {
		return nil, nil
	}
	var keys []string
	if err := json.Unmarshal([]byte(*raw), &keys); err != nil {
		return nil, fmt.Errorf("decode kv keys %s: %w", pattern, err)
	}
	return keys, nil
}

func kvSMembers(ctx context.Context, r pgxRunner, key string) ([]string, error) {
	var raw string
	if err := r.QueryRow(ctx, "SELECT KV_SMEMBERS($1)", key).Scan(&raw); err != nil {
		return nil, fmt.Errorf("kv smembers %s: %w", key, err)
	}
	var members []string
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &members); err != nil {
			return nil, fmt.Errorf("decode kv smembers %s: %w", key, err)
		}
	}
	return members, nil
}

// kvZRangeAll reads a zset's full member/score list. The bounds are inlined
// literals (constants, not user input) because text-protocol parameters
// arrive as text and the engine's KV_ZRANGE wants integer bounds.
func kvZRangeAll(ctx context.Context, r pgxRunner, key string) ([]kvZEntry, error) {
	var raw string
	if err := r.QueryRow(ctx, "SELECT KV_ZRANGE($1, 0, -1)", key).Scan(&raw); err != nil {
		return nil, fmt.Errorf("kv zrange %s: %w", key, err)
	}
	var pairs [][]any
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &pairs); err != nil {
			return nil, fmt.Errorf("decode kv zrange %s: %w", key, err)
		}
	}
	entries := make([]kvZEntry, 0, len(pairs))
	for _, pair := range pairs {
		if len(pair) != 2 {
			return nil, fmt.Errorf("kv zrange %s: entry is not a [member, score] pair", key)
		}
		member, ok := pair[0].(string)
		if !ok {
			return nil, fmt.Errorf("kv zrange %s: member is not a string", key)
		}
		score, ok := pair[1].(float64)
		if !ok {
			return nil, fmt.Errorf("kv zrange %s: score is not a number", key)
		}
		entries = append(entries, kvZEntry{Member: member, Score: score})
	}
	return entries, nil
}

// --- restore half ---

// decodeKVRow strictly decodes one kv-srcmap line (same decoder posture as
// decodeRestoreRow: exact numbers, no trailing JSON, shape-validated).
func decodeKVRow(line []byte) (kvLine, error) {
	dec := json.NewDecoder(strings.NewReader(string(line)))
	dec.UseNumber()
	var row kvLine
	if err := dec.Decode(&row); err != nil {
		return row, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return row, fmt.Errorf("trailing JSON after kv row")
	}
	if !strings.HasPrefix(row.Key, "srcmap:") {
		return row, fmt.Errorf("kv row key %q is outside the srcmap namespace", row.Key)
	}
	if strings.Contains(strings.TrimPrefix(row.Key, "srcmap:"), "\x00") {
		return row, fmt.Errorf("kv row key %q contains a NUL byte", row.Key)
	}
	switch row.Type {
	case "string":
		raw, err := base64.StdEncoding.DecodeString(row.Value)
		if err != nil {
			return row, fmt.Errorf("kv string value for %s is not valid base64: %w", row.Key, err)
		}
		if len(raw) > maxKVValueBytes {
			return row, fmt.Errorf("kv string value for %s exceeds the %d byte restore budget", row.Key, maxKVValueBytes)
		}
	case "set":
		if row.Members == nil {
			return row, fmt.Errorf("kv set row for %s carries no members", row.Key)
		}
		if len(row.Members) > maxKVMembers {
			return row, fmt.Errorf("kv set %s exceeds the %d member restore budget", row.Key, maxKVMembers)
		}
	case "zset":
		if row.Entries == nil {
			return row, fmt.Errorf("kv zset row for %s carries no entries", row.Key)
		}
		if len(row.Entries) > maxKVMembers {
			return row, fmt.Errorf("kv zset %s exceeds the %d member restore budget", row.Key, maxKVMembers)
		}
		for _, e := range row.Entries {
			if e.Member == "" {
				return row, fmt.Errorf("kv zset %s has an empty member", row.Key)
			}
		}
	default:
		return row, fmt.Errorf("kv row for %s has unknown type %q", row.Key, row.Type)
	}
	return row, nil
}

// applyKVSection restores the validated kv-srcmap entry into the (verified
// empty) target and proves completeness per key, by type: every archived
// string must read back byte-identical, every archived set must hold all
// its members again, every archived zset must hold every member with its
// exact score. A KV write path that silently drops a key or member fails
// the restore loudly instead of shipping an incomplete instance. (The
// typed checks are load-bearing: KV_KEYS cannot see collection keys —
// upstream — so a listing-based proof would only vouch for the blobs.)
func applyKVSection(ctx context.Context, db *nucleus.Client, r io.Reader) error {
	kv := db.KV()
	var rows []kvLine
	seen := make(map[string]bool)
	if err := eachKVLine(r, func(row kvLine) error {
		if seen[row.Key] {
			return fmt.Errorf("duplicate kv key %s in archive", row.Key)
		}
		seen[row.Key] = true
		rows = append(rows, row)
		switch row.Type {
		case "string":
			raw, err := base64.StdEncoding.DecodeString(row.Value)
			if err != nil {
				return fmt.Errorf("decode %s: %w", row.Key, err)
			}
			if err := kv.Set(ctx, row.Key, raw); err != nil {
				return fmt.Errorf("kv set %s: %w", row.Key, err)
			}
		case "set":
			for _, m := range row.Members {
				if _, err := kv.SAdd(ctx, row.Key, m); err != nil {
					return fmt.Errorf("kv sadd %s %s: %w", row.Key, m, err)
				}
			}
		case "zset":
			for _, e := range row.Entries {
				if _, err := kv.ZAdd(ctx, row.Key, e.Score, e.Member); err != nil {
					return fmt.Errorf("kv zadd %s %s: %w", row.Key, e.Member, err)
				}
			}
		}
		return nil
	}); err != nil {
		return err
	}

	verify := func(row kvLine) error {
		switch row.Type {
		case "string":
			want, err := base64.StdEncoding.DecodeString(row.Value)
			if err != nil {
				return fmt.Errorf("decode %s: %w", row.Key, err)
			}
			got, err := kv.Get(ctx, row.Key)
			if err != nil {
				return fmt.Errorf("kv completeness read %s: %w", row.Key, err)
			}
			if !bytes.Equal(got, want) {
				return fmt.Errorf("kv completeness check: %s was archived but reads back differently after restore", row.Key)
			}
		case "set":
			members, err := kv.SMembers(ctx, row.Key)
			if err != nil {
				return fmt.Errorf("kv completeness smembers %s: %w", row.Key, err)
			}
			have := make(map[string]bool, len(members))
			for _, m := range members {
				have[m] = true
			}
			for _, m := range row.Members {
				if !have[m] {
					return fmt.Errorf("kv completeness check: set %s is missing member %s after restore", row.Key, m)
				}
			}
		case "zset":
			entries, err := kvZRangeAll(ctx, db.Pool(), row.Key)
			if err != nil {
				return fmt.Errorf("kv completeness zrange %s: %w", row.Key, err)
			}
			have := make(map[string]float64, len(entries))
			for _, e := range entries {
				have[e.Member] = e.Score
			}
			for _, e := range row.Entries {
				got, ok := have[e.Member]
				if !ok || got != e.Score {
					return fmt.Errorf("kv completeness check: zset %s is missing member %s (or its exact score) after restore", row.Key, e.Member)
				}
			}
		}
		return nil
	}
	for _, row := range rows {
		if err := verify(row); err != nil {
			return err
		}
	}
	return nil
}

// eachKVLine streams an archive entry's JSONL lines through fn. The scanner
// budget is larger than the SQL tables': one line carries a whole source-map
// blob, and uploads are not size-capped, so a legitimately archived line can
// be far bigger than a table row.
func eachKVLine(r io.Reader, fn func(kvLine) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1<<20), maxKVValueBytes+(64<<20))
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		row, err := decodeKVRow(line)
		if err != nil {
			// Already validated pre-apply; the spool file is ours.
			return fmt.Errorf("decode kv row (post-validation, should be unreachable): %w", err)
		}
		if err := fn(row); err != nil {
			return err
		}
	}
	return scanner.Err()
}
