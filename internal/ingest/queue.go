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
	mu             sync.Mutex
	dir            string
	name           string
	fsyncInterval  time.Duration
	maxBytes       int64
	file           *os.File
	writer         *bufio.Writer
	offset         int64 // current byte position in current.log
	checkpoint     int64 // bytes <= checkpoint have been flushed
	stopCh         chan struct{}
	stopped        bool
	logger         *slog.Logger
	dirtySinceFlush bool
}

// NewDiskQueue opens (or creates) a disk queue under dir/name/. Missing
// directories are created. fsyncInterval is how often we flush OS buffers
// to disk during steady-state writes.
func NewDiskQueue(dir, name string, fsyncInterval time.Duration, maxBytes int64, logger *slog.Logger) (*DiskQueue, error) {
	full := filepath.Join(dir, name)
	if err := os.MkdirAll(full, 0o755); err != nil {
		return nil, fmt.Errorf("ingest queue: mkdir: %w", err)
	}

	logPath := filepath.Join(full, "current.log")
	f, err := os.OpenFile(logPath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("ingest queue: open: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("ingest queue: stat: %w", err)
	}

	cp, err := readCheckpoint(filepath.Join(full, "checkpoint"))
	if err != nil {
		_ = f.Close()
		return nil, err
	}

	q := &DiskQueue{
		dir:           full,
		name:          name,
		fsyncInterval: fsyncInterval,
		maxBytes:      maxBytes,
		file:          f,
		writer:        bufio.NewWriterSize(f, 64*1024),
		offset:        info.Size(),
		checkpoint:    cp,
		stopCh:        make(chan struct{}),
		logger:        logger,
	}
	go q.fsyncLoop()
	return q, nil
}

// Append writes one event to the write-ahead log. The caller should still
// push the event into its in-memory buffer as well — DiskQueue is only for
// durability on crash recovery.
func (q *DiskQueue) Append(e Event) error {
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.stopped {
		return errors.New("ingest queue: closed")
	}
	n, err := q.writer.Write(raw)
	if err != nil {
		return err
	}
	if err := q.writer.WriteByte('\n'); err != nil {
		return err
	}
	q.offset += int64(n) + 1
	q.dirtySinceFlush = true
	return nil
}

// Checkpoint records that every byte <= offset has been successfully
// flushed to the database. Called from Buffer.Flush after a successful
// SQL insert.
func (q *DiskQueue) Checkpoint() error {
	q.mu.Lock()
	off := q.offset
	q.mu.Unlock()

	if err := q.writer.Flush(); err != nil {
		return err
	}
	if err := q.file.Sync(); err != nil {
		return err
	}
	return q.writeCheckpoint(off)
}

func (q *DiskQueue) writeCheckpoint(offset int64) error {
	path := filepath.Join(q.dir, "checkpoint")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(offset, 10)), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	q.mu.Lock()
	q.checkpoint = offset
	q.dirtySinceFlush = false
	q.mu.Unlock()
	// Best-effort compaction: if the file has grown beyond maxBytes and the
	// checkpoint is at the end, truncate it.
	return q.maybeCompact()
}

func (q *DiskQueue) maybeCompact() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.offset < q.maxBytes {
		return nil
	}
	if q.checkpoint < q.offset {
		return nil // still have pending data past the checkpoint
	}
	// Everything is flushed — start fresh.
	if err := q.file.Truncate(0); err != nil {
		return err
	}
	if _, err := q.file.Seek(0, 0); err != nil {
		return err
	}
	q.writer.Reset(q.file)
	q.offset = 0
	q.checkpoint = 0
	_ = os.Remove(filepath.Join(q.dir, "checkpoint"))
	return nil
}

// Pending reads events appended after the last checkpoint and returns
// them. Used at boot to replay unflushed events into the in-memory buffer.
func (q *DiskQueue) Pending() ([]Event, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
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
			if dirty {
				_ = q.writer.Flush()
				_ = q.file.Sync()
			}
			q.mu.Unlock()
		}
	}
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
	return n, nil
}
