package ingest

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DiskQueue is an append-only write-ahead log for ingest events. It provides
// crash recovery: events pushed since the last Checkpoint are replayed on the
// next process start.
//
// Durability model: Append writes to an OS-buffered file handle for
// throughput. A background goroutine fsyncs every fsyncInterval. On
// graceful Close, an explicit fsync flushes any buffered data. A SIGKILL may
// therefore lose up to one fsyncInterval's worth of events, which is still
// strictly better than the in-memory-only alternative that loses everything
// buffered since the last flush.
//
// File layout under dir/ (audit F16 — numbered segments, maxBytes is a CAP
// enforced by rolling, not just a compaction threshold):
//
//	current.log     segment 0 — the pre-F16 layout, kept as the lowest
//	                segment so an upgrade needs no rename; writes move to
//	                numbered segments after the first roll
//	wal-000001.log  segment 1..N — sealed when they reach maxBytes; the
//	                highest number is the active segment
//	checkpoint      {"segment":N,"offset":M} (JSON) once writes have moved
//	                past segment 0; a bare decimal offset is the legacy form
//	                and means segment 0. Segments fully below the checkpoint
//	                are deleted (acknowledged-segment GC).
//
// Disk high-water: maxTotalBytes bounds the sum of all segment sizes. When a
// roll would push the queue past the cap, the OLDEST segment is deleted even
// if it is not checkpointed — those events lose their crash-recovery copy
// (they are still in the flush buffer) but ingestion keeps accepting work.
// Every such breach is counted in Stats and logged loudly; /healthz surfaces
// it. This is the operator-visible policy: availability over
// crash-recoverability past the configured cap, never silent.
type DiskQueue struct {
	mu              sync.Mutex
	dir             string
	name            string
	fsyncInterval   time.Duration
	maxBytes        int64
	maxTotalBytes   int64
	file            *os.File
	writer          *bufio.Writer
	segments        []walSegment // ordered lowest..highest; last is active
	activeIdx       int64        // segment number of the active segment
	offset          int64        // logical byte position at the WAL end
	cpSeg           int64        // checkpoint segment number
	cpOff           int64        // checkpoint byte offset within cpSeg
	stopCh          chan struct{}
	stopped         bool
	logger          *slog.Logger
	dirtySinceFlush bool
	// High-water breach accounting (F16), guarded by mu.
	droppedUnackedSegments int64
	droppedUnackedBytes    int64
	// lastErr is the first sticky WAL failure (audit F14). Once set it is
	// never cleared automatically — a WAL-backed deployment that starts
	// losing writes must stop claiming durability, and only a process
	// restart (with replay) re-establishes the contract. Surfaced through
	// LastError for /healthz and admission checks.
	lastErr error
}

// walSegment is one on-disk segment file. base is the logical offset its
// first byte sits at (derived from the sizes of the lower segments).
type walSegment struct {
	idx  int64
	base int64
	size int64
	name string
}

// DefaultWALMaxTotalBytes is the default disk high-water across all WAL
// segments (F16). 512 MiB = eight default-sized (64 MiB) segments.
const DefaultWALMaxTotalBytes = 512 << 20

// QueueStats is the operator-visible WAL state (F16): segment count, total
// bytes on disk, and the breach counters for segments dropped past the
// high-water cap before they were checkpointed.
type QueueStats struct {
	Segments               int   `json:"segments"`
	Bytes                  int64 `json:"bytes"`
	MaxBytes               int64 `json:"max_bytes"`
	MaxTotalBytes          int64 `json:"max_total_bytes"`
	DroppedUnackedSegments int64 `json:"dropped_unacked_segments"`
	DroppedUnackedBytes    int64 `json:"dropped_unacked_bytes"`
}

// Stats returns a snapshot of the WAL state for health surfacing.
func (q *DiskQueue) Stats() QueueStats {
	q.mu.Lock()
	defer q.mu.Unlock()
	var total int64
	for i := range q.segments {
		total += q.segments[i].size
	}
	return QueueStats{
		Segments:               len(q.segments),
		Bytes:                  total,
		MaxBytes:               q.maxBytes,
		MaxTotalBytes:          q.maxTotalBytes,
		DroppedUnackedSegments: q.droppedUnackedSegments,
		DroppedUnackedBytes:    q.droppedUnackedBytes,
	}
}

// WithMaxTotalBytes sets the disk high-water (F16). Values below maxBytes are
// clamped to maxBytes — a cap that forbids even one segment would turn every
// roll into a breach loop.
func (q *DiskQueue) WithMaxTotalBytes(n int64) *DiskQueue {
	q.mu.Lock()
	defer q.mu.Unlock()
	if n >= q.maxBytes && n > 0 {
		q.maxTotalBytes = n
	}
	return q
}

// LastError returns the sticky WAL failure, or nil while the queue is healthy.
func (q *DiskQueue) LastError() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.lastErr
}

func (q *DiskQueue) setErrLocked(err error) error {
	if q.lastErr == nil {
		q.lastErr = err
		q.logger.Error("ingest queue: durability degraded — writes refused until restart", "err", err)
	}
	return q.lastErr
}

const (
	legacySegmentName = "current.log"
	segmentFilePrefix = "wal-"
	segmentFileSuffix = ".log"
)

func segmentFileName(idx int64) string {
	return fmt.Sprintf("%s%06d%s", segmentFilePrefix, idx, segmentFileSuffix)
}

func parseSegmentFileName(name string) (int64, bool) {
	if !strings.HasPrefix(name, segmentFilePrefix) || !strings.HasSuffix(name, segmentFileSuffix) {
		return 0, false
	}
	mid := strings.TrimSuffix(strings.TrimPrefix(name, segmentFilePrefix), segmentFileSuffix)
	n, err := strconv.ParseInt(mid, 10, 64)
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

// NewDiskQueue opens (or creates) a disk queue under dir/name/. Missing
// directories are created. fsyncInterval is how often we flush OS buffers
// to disk during steady-state writes. maxBytes is the per-segment cap
// (F16): an append that would push the active segment past it rolls to a
// fresh segment first.
func NewDiskQueue(dir, name string, fsyncInterval time.Duration, maxBytes int64, logger *slog.Logger) (*DiskQueue, error) {
	full := filepath.Join(dir, name)
	if err := os.MkdirAll(full, 0o700); err != nil {
		return nil, fmt.Errorf("ingest queue: mkdir: %w", err)
	}
	// Audit F18: creation modes do not tighten an existing permissive
	// directory (a pre-upgrade 0755 queue dir stays world-readable), so the
	// mode is enforced explicitly. The queue directory holds raw telemetry;
	// it is operator-owned and not a symlink farm.
	if err := os.Chmod(full, 0o700); err != nil {
		return nil, fmt.Errorf("ingest queue: secure directory: %w", err)
	}

	q := &DiskQueue{
		dir:            full,
		name:           name,
		fsyncInterval:  fsyncInterval,
		maxBytes:       maxBytes,
		maxTotalBytes:  DefaultWALMaxTotalBytes,
		stopCh:         make(chan struct{}),
		logger:         logger,
		activeIdx:      0,
		cpSeg:          0,
		cpOff:          0,
	}

	// Discover the segment set: the legacy current.log (segment 0, if it
	// exists) plus every numbered segment, ordered by number.
	entries, err := os.ReadDir(full)
	if err != nil {
		return nil, fmt.Errorf("ingest queue: readdir: %w", err)
	}
	var segIdxs []int64
	hasLegacy := false
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if e.Name() == legacySegmentName {
			hasLegacy = true
			continue
		}
		if idx, ok := parseSegmentFileName(e.Name()); ok {
			segIdxs = append(segIdxs, idx)
		}
	}
	sort.Slice(segIdxs, func(i, j int) bool { return segIdxs[i] < segIdxs[j] })

	// Open or create the active segment (highest numbered, else the legacy
	// file, else a fresh segment 0 named current.log to keep a never-rolled
	// install byte-compatible with pre-F16 binaries).
	activeName := legacySegmentName
	if len(segIdxs) > 0 {
		q.activeIdx = segIdxs[len(segIdxs)-1]
		activeName = segmentFileName(q.activeIdx)
	}
	f, err := os.OpenFile(filepath.Join(full, activeName), os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("ingest queue: open: %w", err)
	}
	// Audit F18: same repair for the log file — reopen with 0o600 leaves a
	// pre-upgrade 0644 mode in place. Also refuse anything that is not a
	// regular file (a device or symlink the operator did not intend).
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("ingest queue: stat: %w", err)
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("ingest queue: WAL path is not a regular file")
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("ingest queue: secure WAL file: %w", err)
	}

	// A torn final line (crash mid-append) must be repaired before anything
	// else: the next append would continue the partial line and merge two
	// records into one unparseable JSON line, losing BOTH on replay. Only
	// the ACTIVE segment can carry a torn tail — sealed segments were
	// complete when they were rolled past.
	size, err := repairTornTail(f, info.Size(), logger, name)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("ingest queue: repair torn tail: %w", err)
	}

	// Build the ordered segment table with derived logical bases.
	var base int64
	appendSeg := func(idx int64, segName string, segSize int64) {
		q.segments = append(q.segments, walSegment{idx: idx, base: base, size: segSize, name: segName})
		base += segSize
	}
	if hasLegacy {
		legacyInfo, err := os.Stat(filepath.Join(full, legacySegmentName))
		if err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("ingest queue: stat legacy segment: %w", err)
		}
		if q.activeIdx == 0 {
			appendSeg(0, legacySegmentName, size) // active (possibly repaired) size
		} else {
			appendSeg(0, legacySegmentName, legacyInfo.Size())
		}
	}
	for _, idx := range segIdxs {
		if idx == q.activeIdx {
			appendSeg(idx, activeName, size)
			continue
		}
		si, err := os.Stat(filepath.Join(full, segmentFileName(idx)))
		if err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("ingest queue: stat segment %d: %w", idx, err)
		}
		appendSeg(idx, segmentFileName(idx), si.Size())
	}
	// A fresh queue (no legacy file, no numbered segments) still needs its
	// just-created active segment registered as segment 0.
	if len(q.segments) == 0 {
		appendSeg(0, activeName, size)
	}

	cpSeg, cpOff, err := readCheckpoint(filepath.Join(full, "checkpoint"))
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !q.segmentExists(cpSeg) || cpOff > q.segmentSize(cpSeg) {
		// A checkpoint past the log means the log was replaced (or the
		// checkpointed segment was lost) — replaying from the stored
		// position would seek into mid-record bytes of a fresh log and
		// parse garbage. Start from the beginning instead; replay dedup
		// drops whatever was already committed. The stale file is removed
		// so the repair survives another crash before the next checkpoint.
		logger.Warn("ingest queue: checkpoint beyond log, replaying from start",
			"queue", name, "segment", cpSeg, "offset", cpOff)
		if err := os.Remove(filepath.Join(full, "checkpoint")); err != nil && !errors.Is(err, os.ErrNotExist) {
			_ = f.Close()
			return nil, fmt.Errorf("ingest queue: remove stale checkpoint: %w", err)
		}
		cpSeg, cpOff = 0, 0
	}
	// AUD-014 (round 2): an in-range checkpoint that does not sit on a
	// record boundary makes replay seek into the middle of a frame — the
	// same silent-skip corruption as above, just entered differently.
	if err := q.validateCheckpointBoundary(cpSeg, cpOff); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("ingest queue: %w", err)
	}

	q.file = f
	q.writer = bufio.NewWriterSize(f, 64*1024)
	q.offset = q.segmentEnd(q.activeIdx)
	q.cpSeg, q.cpOff = cpSeg, cpOff

	// F16: reclaim acknowledged sealed segments left behind by a crash
	// between checkpoint and GC, and enforce the disk high-water.
	q.gcSegmentsLocked()
	q.enforceCapLocked()

	go q.fsyncLoop()
	return q, nil
}

// segmentExists reports whether segment idx is present in the table.
// Callers hold q.mu (or have not yet started concurrency).
func (q *DiskQueue) segmentExists(idx int64) bool {
	for i := range q.segments {
		if q.segments[i].idx == idx {
			return true
		}
	}
	return false
}

// segmentSize returns the size of segment idx, or 0 when absent.
func (q *DiskQueue) segmentSize(idx int64) int64 {
	for i := range q.segments {
		if q.segments[i].idx == idx {
			return q.segments[i].size
		}
	}
	return 0
}

// segmentEnd returns the logical offset at the end of segment idx (its base
// plus its size); 0 when absent.
func (q *DiskQueue) segmentEnd(idx int64) int64 {
	for i := range q.segments {
		if q.segments[i].idx == idx {
			return q.segments[i].base + q.segments[i].size
		}
	}
	return 0
}

// logicalToPos maps a logical offset to (segment, offset-in-segment). The
// target is clamped to the WAL end. Callers hold q.mu.
func (q *DiskQueue) logicalToPos(target int64) (int64, int64) {
	if target > q.offset {
		target = q.offset
	}
	for i := len(q.segments) - 1; i >= 0; i-- {
		s := q.segments[i]
		if target > s.base || (target == s.base && s.size == 0 && i == len(q.segments)-1) {
			return s.idx, target - s.base
		}
	}
	return q.segments[0].idx, 0
}

// cpLogical is the checkpoint's logical offset. Callers hold q.mu.
func (q *DiskQueue) cpLogical() int64 {
	for i := range q.segments {
		if q.segments[i].idx == q.cpSeg {
			return q.segments[i].base + q.cpOff
		}
	}
	return 0
}

// walBatchFrame is the versioned multi-event WAL record written by
// AppendBatch (AUD-010, round 2). A whole admitted batch is one frame, so
// a mid-batch write failure can never leave a partially accepted prefix
// on disk. Pending decodes both this shape and the legacy one-event-per-
// line records older binaries wrote.
type walBatchFrame struct {
	WALVersion int     `json:"wal_version"`
	Events     []Event `json:"events"`
}

const (
	walBatchVersion    = 1
	walMaxFrameBytes   = 8 << 20 // 100 events x 64 KiB event cap, plus envelope
	legacyFrameVersion = 0
)

// decodeWALLine decodes one newline-delimited WAL record, accepting both
// legacy single-event lines and versioned batch frames.
func decodeWALLine(line []byte) ([]Event, error) {
	var probe struct {
		WALVersion *int    `json:"wal_version"`
		Events     []Event `json:"events"`
	}
	if err := json.Unmarshal(line, &probe); err != nil {
		return nil, err
	}
	if probe.WALVersion == nil {
		// Legacy line: the whole record is one Event. Re-decode to surface
		// type mismatches the probe struct tolerated.
		var e Event
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, err
		}
		return []Event{e}, nil
	}
	switch *probe.WALVersion {
	case walBatchVersion:
		if probe.Events == nil {
			return nil, errors.New("batch frame carries no events")
		}
		return probe.Events, nil
	default:
		return nil, fmt.Errorf("unsupported WAL frame version %d", *probe.WALVersion)
	}
}

// Append writes one event to the write-ahead log and returns the WAL byte
// offset immediately after the written record. The caller must hold this
// offset and pass the batch's maximum offset to Checkpoint after the batch
// is durably stored — checkpointing the live q.offset instead would mark
// later, not-yet-inserted records as durable and drop them on crash.
//
// The caller should still push the event into its in-memory buffer as well —
// DiskQueue is only for durability on crash recovery.
func (q *DiskQueue) Append(e Event) (int64, error) {
	return q.AppendBatch([]Event{e})
}

// AppendBatch writes a whole batch as ONE versioned WAL frame and returns
// the offset after it (AUD-010, round 2). Callers that admitted a batch
// atomically must also be able to journal it atomically: appending events
// one line at a time left a partial batch on disk when a write failed
// mid-batch, so a client retry of the "failed" batch duplicated the
// prefix that had in fact survived.
//
// F16: if the frame would push the active segment past maxBytes, the queue
// rolls to a fresh segment first — maxBytes is a cap, not a threshold that
// only compaction honors.
func (q *DiskQueue) AppendBatch(events []Event) (int64, error) {
	if len(events) == 0 {
		q.mu.Lock()
		defer q.mu.Unlock()
		return q.offset, nil
	}
	frame := walBatchFrame{WALVersion: walBatchVersion, Events: events}
	raw, err := json.Marshal(frame)
	if err != nil {
		return 0, err
	}
	if len(raw) > walMaxFrameBytes {
		return 0, fmt.Errorf("ingest queue: WAL frame too large (%d bytes)", len(raw))
	}
	raw = append(raw, '\n')
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.stopped {
		return q.offset, errors.New("ingest queue: closed")
	}
	if q.lastErr != nil {
		// Audit F14: after a WAL failure the queue is degraded, not best-
		// effort — the caller (Buffer.Push) refuses admission instead of
		// acknowledging events the durability contract cannot back.
		return q.offset, fmt.Errorf("ingest queue: degraded since earlier failure: %w", q.lastErr)
	}
	active := &q.segments[len(q.segments)-1]
	if active.size > 0 && active.size+int64(len(raw)) > q.maxBytes {
		if err := q.rollLocked(); err != nil {
			return q.offset, err
		}
		active = &q.segments[len(q.segments)-1]
	}
	n, err := q.writer.Write(raw)
	if err == nil && n != len(raw) {
		err = io.ErrShortWrite
	}
	if err != nil {
		// A partial frame may sit in the bufio buffer; the offset only ever
		// advanced by fully-written bytes, and the torn-tail repair on next
		// open truncates whatever did reach the file. Latch so no further
		// record is appended behind a known-bad write.
		q.offset += int64(n)
		active.size += int64(n)
		return q.offset, q.setErrLocked(fmt.Errorf("WAL append: %w", err))
	}
	q.offset += int64(n)
	active.size += int64(n)
	q.dirtySinceFlush = true
	return q.offset, nil
}

// rollLocked seals the active segment and opens the next numbered one as
// active, then enforces the disk high-water. Callers hold q.mu.
//
// Ordering: the new segment file is created BEFORE the old handle is
// touched, so a create failure (permissions, fd pressure) leaves the queue
// fully intact with a plain error — no latch, no half-rolled state. A
// flush/sync failure while sealing DOES latch (audit F14: bytes the
// durability model promised are not on disk). An empty next-segment file
// left behind by a sealing failure is harmless: it reopens as an empty
// active segment.
func (q *DiskQueue) rollLocked() error {
	next := q.activeIdx + 1
	f, err := os.OpenFile(filepath.Join(q.dir, segmentFileName(next)), os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("WAL roll create: %w", err)
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return fmt.Errorf("WAL roll secure: %w", err)
	}
	if err := q.writer.Flush(); err != nil {
		_ = f.Close()
		return q.setErrLocked(fmt.Errorf("WAL roll flush: %w", err))
	}
	if err := q.file.Sync(); err != nil {
		_ = f.Close()
		return q.setErrLocked(fmt.Errorf("WAL roll sync: %w", err))
	}
	if err := q.file.Close(); err != nil {
		_ = f.Close()
		return q.setErrLocked(fmt.Errorf("WAL roll close: %w", err))
	}
	if err := syncDir(q.dir); err != nil {
		// The new segment's directory entry is not known-durable; its data
		// fsyncs later through the normal loop. Warn, keep rolling.
		q.logger.Warn("ingest queue: roll directory fsync failed", "queue", q.name, "err", err)
	}
	prev := q.segments[len(q.segments)-1]
	q.file = f
	q.writer = bufio.NewWriterSize(f, 64*1024)
	q.activeIdx = next
	q.segments = append(q.segments, walSegment{
		idx:  next,
		base: prev.base + prev.size,
		name: segmentFileName(next),
	})
	q.enforceCapLocked()
	return nil
}

// enforceCapLocked applies the disk high-water (F16): while the total
// segment bytes exceed maxTotalBytes and more than one segment remains,
// delete the oldest. A fully-checkpointed deletion is normal GC; deleting a
// segment that still holds unacknowledged records is a POLICY BREACH —
// those events lose their crash-recovery copy (they remain in the flush
// buffer) — and is counted and logged. Callers hold q.mu.
func (q *DiskQueue) enforceCapLocked() {
	var total int64
	for i := range q.segments {
		total += q.segments[i].size
	}
	for total > q.maxTotalBytes && len(q.segments) > 1 {
		oldest := q.segments[0]
		// Strictly below the checkpoint. A checkpoint sitting at a
		// segment's END still names that segment — deleting it would make
		// the next open clamp the checkpoint to zero and replay the whole
		// log through the best-effort dedup, which can double-count on a
		// lookup failure. The boundary segment is reclaimed once the
		// checkpoint advances into a later one.
		acked := q.cpSeg > oldest.idx
		if err := os.Remove(filepath.Join(q.dir, oldest.name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			q.logger.Error("ingest queue: high-water GC failed to remove a segment", "queue", q.name, "segment", oldest.name, "err", err)
			return
		}
		if err := syncDir(q.dir); err != nil {
			q.logger.Error("ingest queue: high-water GC dir sync failed", "queue", q.name, "err", err)
		}
		q.segments = q.segments[1:]
		// Bases of the survivors shift down by the removed size; the
		// checkpoint is stored as (segment, offset) so its own mapping is
		// unaffected — only recompute the survivors' bases.
		for i := range q.segments {
			q.segments[i].base -= oldest.size
		}
		total -= oldest.size
		if !acked {
			q.droppedUnackedSegments++
			q.droppedUnackedBytes += oldest.size
			q.logger.Error("ingest queue: WAL disk high-water breached — dropped an UNACKNOWLEDGED segment to keep accepting writes; those events lost their crash-recovery copy",
				"queue", q.name, "segment", oldest.name, "bytes", oldest.size,
				"maxTotalBytes", q.maxTotalBytes,
				"policy", "raise OBSERVE_WAL_MAX_TOTAL_BYTES or drain the database faster")
		}
	}
}

// gcSegmentsLocked deletes sealed segments fully below the checkpoint
// (acknowledged-segment GC, F16). Never touches the active segment.
// Callers hold q.mu.
func (q *DiskQueue) gcSegmentsLocked() {
	removed := true
	for removed {
		removed = false
		if len(q.segments) <= 1 {
			return
		}
		oldest := q.segments[0]
		// Strictly below the checkpoint (see enforceCapLocked for why a
		// checkpoint at a segment's end does not make it reclaimable).
		if q.cpSeg > oldest.idx {
			if err := os.Remove(filepath.Join(q.dir, oldest.name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				q.logger.Warn("ingest queue: acknowledged-segment GC failed", "queue", q.name, "segment", oldest.name, "err", err)
				return
			}
			if err := syncDir(q.dir); err != nil {
				q.logger.Warn("ingest queue: acknowledged-segment GC dir sync failed", "queue", q.name, "err", err)
			}
			q.segments = q.segments[1:]
			for i := range q.segments {
				q.segments[i].base -= oldest.size
			}
			removed = true
		}
	}
}

// Offset returns the current WAL byte position. Used by Buffer.AttachQueue to
// seed its high-water mark so the first post-replay checkpoint advances to the
// end of the replayed region (otherwise replayed events would re-replay on the
// next crash).
func (q *DiskQueue) Offset() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.offset
}

// Checkpoint records that every byte <= target has been successfully flushed to
// the database. Called from Buffer.Flush after a successful SQL insert with the
// maximum WAL offset of the inserted batch.
//
// The checkpoint only ever advances (monotonic clamp): a stale or out-of-order
// call can never roll it backward past data already confirmed durable. target
// is also clamped to the current offset so a bogus value can't skip past
// un-written bytes. The bufio flush, fsync, checkpoint-file write, and segment
// GC all run while holding q.mu: they cannot race a concurrent Append or the
// fsyncLoop (bufio.Writer is not safe for concurrent use), and the offset
// persisted to the checkpoint file is exactly one this call flushed. An Append
// arriving after the fsync lands beyond that offset and stays dirty for the
// next flush.
//
// Correctness relies on the single-flusher invariant: Buffer serializes Flush
// (and thus Checkpoint) so batches are inserted and checkpointed in WAL order.
//
// F16: after advancing the checkpoint, acknowledged sealed segments are
// deleted; an active segment that is fully checkpointed and has reached
// maxBytes is rolled away (reclaimed by the same GC) instead of truncated.
func (q *DiskQueue) Checkpoint(target int64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.stopped {
		return errors.New("ingest queue: closed")
	}
	if target <= q.cpLogical() {
		return nil // already durable up to (or past) target
	}
	if target > q.offset {
		target = q.offset
	}
	if err := q.writer.Flush(); err != nil {
		return q.setErrLocked(fmt.Errorf("WAL flush: %w", err))
	}
	if err := q.file.Sync(); err != nil {
		return q.setErrLocked(fmt.Errorf("WAL sync: %w", err))
	}
	// The checkpoint write must happen inside the SAME lock hold as the
	// flush above. Writing it after releasing q.mu let an Append land
	// between the two: the append set dirtySinceFlush, the checkpoint
	// write then cleared it, and the fsync loop skipped bytes it had
	// never flushed — losing appends the durability model promises are
	// on disk within one fsyncInterval.
	seg, off := q.logicalToPos(target)
	if err := q.writeCheckpoint(seg, off); err != nil {
		return q.setErrLocked(fmt.Errorf("WAL checkpoint: %w", err))
	}
	q.cpSeg, q.cpOff = seg, off
	q.gcSegmentsLocked()
	// Fully-checkpointed over-sized active segment: roll it away so a
	// database that stalled for a while does not leave those bytes on disk
	// forever after recovery. The checkpoint is then rewritten at the head
	// of the fresh active segment (the same logical position) and the
	// rolled segment is reclaimed immediately — a crash between the roll
	// and the rewrite still replays correctly off the old checkpoint, which
	// names a segment that only this GC path removes.
	active := &q.segments[len(q.segments)-1]
	if active.size >= q.maxBytes && q.cpSeg == active.idx && q.cpOff >= active.size {
		if err := q.rollLocked(); err != nil {
			// Non-fatal for the data (it is checkpointed), but reported: a
			// create failure here is transient and retried by the next
			// checkpoint; a seal failure latched the queue per F14.
			q.logger.Warn("ingest queue: post-checkpoint segment reclaim failed", "err", err)
		} else {
			if err := q.writeCheckpoint(q.activeIdx, 0); err != nil {
				return q.setErrLocked(fmt.Errorf("WAL checkpoint (post-roll): %w", err))
			}
			q.cpSeg, q.cpOff = q.activeIdx, 0
			q.gcSegmentsLocked()
		}
	}
	return nil
}

// checkpointFile is the JSON form written once writes live in numbered
// segments (F16). A bare decimal remains readable (legacy: segment 0).
type checkpointFile struct {
	Segment int64 `json:"segment"`
	Offset  int64 `json:"offset"`
}

// writeCheckpoint persists the checkpoint file durably and advances the
// in-memory durability point. The caller must hold q.mu, so the position
// recorded is exactly one the current Checkpoint call flushed and fsynced.
//
// Audit F17: os.WriteFile + os.Rename left both the new file's data and the
// rename itself un-synced — a power loss could leave a missing, stale, or
// torn checkpoint despite a reported success. The replacement writes a
// unique 0600 temp file, fsyncs it, renames over the target, and fsyncs the
// directory so the rename is durable. (This also repairs a pre-upgrade
// permissive checkpoint mode by replacing the file outright.)
//
// While the active segment is 0 (no roll has ever happened) the legacy
// decimal form is kept so a pre-F16 binary can still open the queue.
func (q *DiskQueue) writeCheckpoint(seg, off int64) error {
	var data []byte
	if seg == 0 && !q.segmentExistsNonLegacy() {
		data = []byte(strconv.FormatInt(off, 10))
	} else {
		b, err := json.Marshal(checkpointFile{Segment: seg, Offset: off})
		if err != nil {
			return err
		}
		data = b
	}
	path := filepath.Join(q.dir, "checkpoint")
	if err := atomicWrite0600(path, data); err != nil {
		return err
	}
	q.dirtySinceFlush = false
	return nil
}

// segmentExistsNonLegacy reports whether any numbered segment exists.
// Callers hold q.mu.
func (q *DiskQueue) segmentExistsNonLegacy() bool {
	for i := range q.segments {
		if q.segments[i].idx > 0 {
			return true
		}
	}
	return false
}

// atomicWrite0600 durably replaces path with data via write-to-temp, fsync,
// rename, and a directory fsync. Assumes an application-owned directory on a
// filesystem with supported sync semantics; it is not a symlink sandbox.
func atomicWrite0600(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".observe-checkpoint-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if n, writeErr := f.Write(data); writeErr != nil || n != len(data) {
		if writeErr == nil {
			writeErr = io.ErrShortWrite
		}
		return errors.Join(writeErr, f.Close())
	}
	if err := f.Sync(); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := f.Chmod(0o600); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}

// syncDir fsyncs a directory so a remove within it is durable before the
// steps that depend on that ordering run.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// StreamPending replays events appended after the last checkpoint WITHOUT
// materializing the whole backlog (F16): each decoded WAL frame is handed to
// fn and becomes garbage once it returns. Used at boot to replay unflushed
// events into the in-memory buffer; the callback performs the chunked
// already-committed dedup so neither the frame list nor the event-id lookup
// is bounded by the entire backlog.
//
// AUD-014 (round 2): a complete-but-corrupt record is a replay ERROR, not
// a skip (see Pending). A non-nil error from fn aborts the walk.
func (q *DiskQueue) StreamPending(fn func(events []Event) error) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.stopped {
		return errors.New("ingest queue: closed")
	}
	if q.cpLogical() >= q.offset {
		return nil
	}
	// Appends land in a userspace bufio buffer; replay must see every
	// logically-appended record, so push them to the file before scanning.
	// (Crash-recovery still only promises what the fsync loop durable-wrote;
	// this flush covers in-process replay, which replay also serves.)
	if err := q.writer.Flush(); err != nil {
		return fmt.Errorf("ingest queue: flushing buffered appends for replay (queue %s): %w", q.name, err)
	}
	for _, seg := range q.segments {
		start := int64(0)
		if seg.idx < q.cpSeg {
			continue // entirely below the checkpoint
		}
		if seg.idx == q.cpSeg {
			start = q.cpOff
		}
		if start >= seg.size {
			continue
		}
		// Read through a fresh handle: the active segment's write handle is
		// positioned for appends and must not be seeked around under
		// concurrent writers (this hold of q.mu excludes them, but the
		// invariant should not depend on it for reads).
		f, err := os.Open(filepath.Join(q.dir, seg.name))
		if err != nil {
			return fmt.Errorf("ingest queue: opening WAL segment %s for replay: %w", seg.name, err)
		}
		if _, err := f.Seek(start, io.SeekStart); err != nil {
			_ = f.Close()
			return err
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 1<<20), walMaxFrameBytes+1<<20)
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}
			events, err := decodeWALLine(line)
			if err != nil {
				_ = f.Close()
				return fmt.Errorf("ingest queue: WAL corruption — refusing to skip a complete record (queue %s, segment %s): %w", q.name, seg.name, err)
			}
			for _, e := range events {
				if e.EventID == "" || e.SiteID == "" {
					_ = f.Close()
					return fmt.Errorf("ingest queue: WAL record is missing its event/site identity (queue %s, segment %s)", q.name, seg.name)
				}
			}
			if err := fn(events); err != nil {
				_ = f.Close()
				return fmt.Errorf("ingest queue: replay consumer failed (queue %s): %w", q.name, err)
			}
		}
		scanErr := scanner.Err()
		if scanErr == nil {
			scanErr = f.Close()
		} else {
			_ = f.Close()
		}
		if scanErr != nil {
			return fmt.Errorf("ingest queue: reading WAL (queue %s, segment %s): %w", q.name, seg.name, scanErr)
		}
	}
	return nil
}

// Pending reads events appended after the last checkpoint and returns
// them. Used at boot to replay unflushed events into the in-memory buffer.
// Materializes the full backlog — new callers should prefer StreamPending
// (F16), which bounds replay memory to one frame at a time.
func (q *DiskQueue) Pending() ([]Event, error) {
	var out []Event
	err := q.StreamPending(func(events []Event) error {
		out = append(out, events...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Close stops the fsync goroutine and fsyncs any buffered writes.
func (q *DiskQueue) Close() error {
	q.mu.Lock()
	if q.stopped {
		q.mu.Unlock()
		return nil
	}
	q.stopped = true
	close(q.stopCh)
	err := q.writer.Flush()
	if err == nil {
		err = q.file.Sync()
	}
	if cerr := q.file.Close(); cerr != nil && err == nil {
		err = cerr
	}
	q.mu.Unlock()
	return err
}

func (q *DiskQueue) fsyncLoop() {
	t := time.NewTicker(q.fsyncInterval)
	defer t.Stop()
	for {
		select {
		case <-q.stopCh:
			return
		case <-t.C:
			q.mu.Lock()
			if q.stopped {
				q.mu.Unlock()
				return
			}
			dirty := q.dirtySinceFlush
			if dirty && q.lastErr == nil {
				// Audit F14: a background flush/sync failure is latched
				// exactly like an append failure — silently ignoring it let
				// a WAL-backed deployment keep acknowledging events that
				// were never on disk.
				if err := q.writer.Flush(); err != nil {
					q.setErrLocked(fmt.Errorf("WAL flush: %w", err))
				} else if err := q.file.Sync(); err != nil {
					q.setErrLocked(fmt.Errorf("WAL sync: %w", err))
				}
			}
			q.mu.Unlock()
		}
	}
}

// repairTornTail truncates an unterminated final line — the signature of a
// crash mid-append — back to the last record boundary, and returns the
// (possibly shortened) log length. Every Append writes exactly one
// newline-terminated JSON line, so a missing terminator is by definition a
// torn record: without this repair the next append continues the partial
// line and the two records merge into one unparseable JSON line, losing
// BOTH on replay. A newline-terminated tail is left alone; corrupt but
// complete lines fail replay loudly (see Pending).
func repairTornTail(f *os.File, size int64, logger *slog.Logger, name string) (int64, error) {
	if size == 0 {
		return 0, nil
	}
	var last [1]byte
	if _, err := f.ReadAt(last[:], size-1); err != nil {
		return size, err
	}
	if last[0] == '\n' {
		return size, nil
	}
	// Scan backwards for the last record boundary.
	const scanChunk = 4096
	buf := make([]byte, scanChunk)
	end := size
	for end > 0 {
		start := end - scanChunk
		if start < 0 {
			start = 0
		}
		n, err := f.ReadAt(buf[:end-start], start)
		if err != nil && n == 0 {
			return size, err
		}
		for i := n - 1; i >= 0; i-- {
			if buf[i] == '\n' {
				keep := start + int64(i) + 1
				if err := f.Truncate(keep); err != nil {
					return size, err
				}
				logger.Warn("ingest queue: truncated torn tail from crashed append",
					"queue", name, "droppedBytes", size-keep)
				return keep, nil
			}
		}
		end = start
	}
	// No newline anywhere: the whole file is one torn record.
	if err := f.Truncate(0); err != nil {
		return size, err
	}
	logger.Warn("ingest queue: truncated torn tail from crashed append",
		"queue", name, "droppedBytes", size)
	return 0, nil
}

// validateCheckpointBoundary verifies that a nonzero checkpoint offset
// lands immediately after a newline — i.e. on a WAL record boundary
// (AUD-014, round 2). F16: the check runs against the checkpoint's own
// segment file.
func (q *DiskQueue) validateCheckpointBoundary(seg, off int64) error {
	if off <= 0 {
		return nil
	}
	var name string
	for i := range q.segments {
		if q.segments[i].idx == seg {
			name = q.segments[i].name
		}
	}
	if name == "" {
		return fmt.Errorf("checkpoint references missing segment %d", seg)
	}
	f, err := os.Open(filepath.Join(q.dir, name))
	if err != nil {
		return fmt.Errorf("checkpoint boundary open: %w", err)
	}
	defer f.Close()
	var prev [1]byte
	if _, err := f.ReadAt(prev[:], off-1); err != nil {
		return fmt.Errorf("checkpoint boundary read: %w", err)
	}
	if prev[0] != '\n' {
		return fmt.Errorf("checkpoint (segment %d, offset %d) is not at a WAL record boundary (corrupt or foreign checkpoint file)", seg, off)
	}
	return nil
}

// readCheckpoint loads the durability point. Both forms are accepted
// (F16): the JSON {"segment","offset"} written once the queue rolls, and
// the legacy bare decimal (an offset into segment 0 / current.log).
func readCheckpoint(path string) (seg, off int64, err error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("ingest queue: read checkpoint: %w", err)
	}
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "{") {
		var cf checkpointFile
		if err := json.Unmarshal([]byte(trimmed), &cf); err != nil {
			return 0, 0, fmt.Errorf("ingest queue: parse checkpoint: %w", err)
		}
		if cf.Segment < 0 {
			return 0, 0, fmt.Errorf("ingest queue: checkpoint segment is negative (%d)", cf.Segment)
		}
		return cf.Segment, cf.Offset, nil
	}
	n, perr := strconv.ParseInt(trimmed, 10, 64)
	if perr != nil {
		return 0, 0, fmt.Errorf("ingest queue: parse checkpoint: %w", perr)
	}
	// Audit F15: a negative checkpoint is corruption, not an offset — a
	// replay seek to it would read from a garbage position.
	if n < 0 {
		return 0, 0, fmt.Errorf("ingest queue: checkpoint is negative (%d)", n)
	}
	return 0, n, nil
}
