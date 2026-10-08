package errors

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Instance-local IO seams let regressions fail real spool writes/syncs without
// changing the WAL implementation or claiming that mocks simulate power loss.
type quarantineFile interface {
	Chmod(os.FileMode) error
	Stat() (os.FileInfo, error)
	Write([]byte) (int, error)
	Truncate(int64) error
	Sync() error
	Close() error
}
type quarantineIO struct {
	open    func(string) (quarantineFile, error)
	syncDir func(string) error
}

func syncQuarantineDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}

func (b *ErrorBuffer) quarantineRaw(body []byte, reason error) error {
	b.quarantineMu.Lock()
	defer b.quarantineMu.Unlock()
	b.mu.Lock()
	dir := b.quarantineDir
	b.mu.Unlock()
	if dir == "" {
		return nil
	} // explicitly ephemeral, no WAL checkpoint
	entry := struct {
		TS     string `json:"ts"`
		Reason string `json:"reason"`
		Record []byte `json:"record"`
	}{time.Now().UTC().Format(time.RFC3339), reason.Error(), body}
	line, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	if int64(len(line)) > maxQuarantineBytes {
		return fmt.Errorf("quarantine record exceeds spool capacity")
	}
	path := filepath.Join(dir, "quarantine.log")
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("quarantine spool is not a regular file")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	open := func(path string) (quarantineFile, error) {
		return os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND|syscall.O_NOFOLLOW, 0600)
	}
	syncDir := syncQuarantineDir
	if b.quarantineIO != nil {
		if b.quarantineIO.open != nil {
			open = b.quarantineIO.open
		}
		if b.quarantineIO.syncDir != nil {
			syncDir = b.quarantineIO.syncDir
		}
	}
	f, err := open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("quarantine spool is not a regular file")
	}
	if err := f.Chmod(0600); err != nil {
		return err
	}
	start := info.Size()
	if int64(len(line)) > maxQuarantineBytes-start {
		return fmt.Errorf("quarantine spool full")
	}
	// Failed attempts must not fill the bounded append-only spool with retry
	// duplicates. A failed rollback remains an error/PENDING, never FINAL.
	rollback := func(cause error) error {
		return errors.Join(cause, f.Truncate(start), f.Sync(), syncDir(dir))
	}
	n, err := f.Write(line)
	if err != nil {
		return rollback(err)
	}
	if n != len(line) {
		return rollback(io.ErrShortWrite)
	}
	if err := f.Sync(); err != nil {
		return rollback(err)
	}
	if err := syncDir(dir); err != nil {
		return rollback(err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	return nil
}
