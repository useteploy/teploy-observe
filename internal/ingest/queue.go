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
	"strconv"
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
// File layout under dir/:
//
//	current.log   append-only JSON lines, one event per line
//	checkpoint    decimal byte offset; everything <= this has been flushed
type DiskQueue struct {
	mu              sync.Mutex
	dir             string
	name            string
	fsyncInterval   time.Duration
	maxBytes        int64
	file            *os.File
	writer          *bufio.Writer
	offset          int64 // current byte position in current.log
	checkpoint      int64 // bytes <= checkpoint have been flushed
	stopCh          chan struct{}
	stopped         bool
	logger          *slog.Logger
	dirtySinceFlush bool
	// lastErr is the first sticky WAL failure (audit F14). Once set it is
	// never cleared automatically — a WAL-backed deployment that starts
	// losing writes must stop claiming durability, and only a process
	// restart (with replay) re-establishes the contract. Surfaced through
	// LastError for /healthz and admission checks.
	lastErr error
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

// NewDiskQueue opens (or creates) a disk queue under dir/name/. Missing
// directories are created. fsyncInterval is how often we flush OS buffers
// to disk during steady-state writes.
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

	logPath := filepath.Join(full, "current.log")
	f, err := os.OpenFile(logPath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600)
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
	// records into one unparseable JSON line, losing BOTH on replay.
	size, err := repairTornTail(f, info.Size(), logger, name)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("ingest queue: repair torn tail: %w", err)
	}

	cp, err := readCheckpoint(filepath.Join(full, "checkpoint"))
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if cp > size {
		// A checkpoint past the log means the log was replaced (compaction)
		// without the checkpoint surviving — replaying from the stored
		// offset would seek into mid-record bytes of a fresh log and parse
		// garbage. Start from 0 instead; replay dedup drops whatever was
		// already committed. The stale file is removed so the repair
		// survives another crash before the next checkpoint.
		logger.Warn("ingest queue: checkpoint beyond log length, replaying from start",
			"queue", name, "checkpoint", cp, "logBytes", size)
		if err := os.Remove(filepath.Join(full, "checkpoint")); err != nil && !errors.Is(err, os.ErrNotExist) {
			_ = f.Close()
			return nil, fmt.Errorf("ingest queue: remove stale checkpoint: %w", err)
		}
		cp = 0
	}

	q := &DiskQueue{
		dir:           full,
		name:          name,
		fsyncInterval: fsyncInterval,
		maxBytes:      maxBytes,
		file:          f,
		writer:        bufio.NewWriterSize(f, 64*1024),
		offset:        size,
		checkpoint:    cp,
		stopCh:        make(chan struct{}),
		logger:        logger,
	}
	go q.fsyncLoop()
	return q, nil
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
	raw, err := json.Marshal(e)
	if err != nil {
		return 0, err
	}
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
	n, err := q.writer.Write(raw)
	if err == nil {
		err = q.writer.WriteByte('\n')
	}
	if err != nil {
		// A partial line may sit in the bufio buffer; the offset only ever
		// advanced by fully-written bytes, and the torn-tail repair on next
		// open truncates whatever did reach the file. Latch so no further
		// record is appended behind a known-bad write.
		q.offset += int64(n)
		return q.offset, q.setErrLocked(fmt.Errorf("WAL append: %w", err))
	}
	q.offset += int64(n) + 1
	q.dirtySinceFlush = true
	return q.offset, nil
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
// un-written bytes. The bufio flush, fsync, checkpoint-file write, and
// compaction all run while holding q.mu: they cannot race a concurrent
// Append or the fsyncLoop (bufio.Writer is not safe for concurrent use), and
// the offset persisted to the checkpoint file is exactly one this call
// flushed. An Append arriving after the fsync lands beyond that offset and
// stays dirty for the next flush.
//
// Correctness relies on the single-flusher invariant: Buffer serializes Flush
// (and thus Checkpoint) so batches are inserted and checkpointed in WAL order.
func (q *DiskQueue) Checkpoint(target int64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.stopped {
		return errors.New("ingest queue: closed")
	}
	if target <= q.checkpoint {
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
	if err := q.writeCheckpoint(target); err != nil {
		return q.setErrLocked(fmt.Errorf("WAL checkpoint: %w", err))
	}
	// Best-effort compaction: if the file has grown beyond maxBytes and the
	// checkpoint is at the end, truncate it.
	return q.maybeCompact()
}

// writeCheckpoint persists the checkpoint file durably and advances the
// in-memory durability point. The caller must hold q.mu, so the offset
// recorded is exactly one the current Checkpoint call flushed and fsynced.
//
// Audit F17: os.WriteFile + os.Rename left both the new file's data and the
// rename itself un-synced — a power loss could leave a missing, stale, or
// torn checkpoint despite a reported success. The replacement writes a
// unique 0600 temp file, fsyncs it, renames over the target, and fsyncs the
// directory so the rename is durable. (This also repairs a pre-upgrade
// permissive checkpoint mode by replacing the file outright.)
func (q *DiskQueue) writeCheckpoint(offset int64) error {
	path := filepath.Join(q.dir, "checkpoint")
	if err := atomicWrite0600(path, []byte(strconv.FormatInt(offset, 10))); err != nil {
		return err
	}
	q.checkpoint = offset
	q.dirtySinceFlush = false
	return nil
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

// maybeCompact resets the log once it has grown beyond maxBytes and
// everything in it is checkpointed. The caller must hold q.mu.
func (q *DiskQueue) maybeCompact() error {
	if q.offset < q.maxBytes {
		return nil
	}
	if q.checkpoint < q.offset {
		return nil // still have pending data past the checkpoint
	}
	// Everything is flushed — start fresh.
	//
	// Crash-consistent rotation: the persisted checkpoint must be gone
	// BEFORE the log is replaced, or a crash in between leaves a durable
	// checkpoint pointing past the replacement log — replay would then
	// seek into mid-record bytes and parse garbage (the load-time clamp
	// is the backstop for disks already in that state). Worst case — a
	// crash between the two steps — the full old log replays and the
	// committed prefix is dropped by replay dedup.
	if err := os.Remove(filepath.Join(q.dir, "checkpoint")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := syncDir(q.dir); err != nil {
		return err
	}
	q.checkpoint = 0 // the on-disk checkpoint is gone; match it even if the swap below fails
	if err := q.file.Truncate(0); err != nil {
		return err
	}
	if _, err := q.file.Seek(0, 0); err != nil {
		return err
	}
	q.writer.Reset(q.file)
	q.offset = 0
	return nil
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

// Pending reads events appended after the last checkpoint and returns
// them. Used at boot to replay unflushed events into the in-memory buffer.
func (q *DiskQueue) Pending() ([]Event, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.stopped {
		return nil, errors.New("ingest queue: closed")
	}
	if q.checkpoint >= q.offset {
		return nil, nil
	}
	if _, err := q.file.Seek(q.checkpoint, io.SeekStart); err != nil {
		return nil, err
	}
	scanner := bufio.NewScanner(q.file)
	scanner.Buffer(make([]byte, 1<<20), 1<<22)
	var out []Event
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Event
		if err := json.Unmarshal(line, &e); err != nil {
			q.logger.Warn("ingest queue: skipping corrupt record", "err", err, "queue", q.name)
			continue
		}
		out = append(out, e)
	}
	// Rewind file handle to end so subsequent Appends stay in append mode.
	if _, err := q.file.Seek(0, io.SeekEnd); err != nil {
		return out, err
	}
	return out, scanner.Err()
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
// complete lines are already skipped at replay.
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

func readCheckpoint(path string) (int64, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("ingest queue: read checkpoint: %w", err)
	}
	n, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("ingest queue: parse checkpoint: %w", err)
	}
	// Audit F15: a negative checkpoint is corruption, not an offset — a
	// replay seek to it would read from a garbage position.
	if n < 0 {
		return 0, fmt.Errorf("ingest queue: checkpoint is negative (%d)", n)
	}
	return n, nil
}
