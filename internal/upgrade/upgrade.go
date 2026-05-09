// Package upgrade implements the `observe upgrade` subcommand: a
// zero-downtime, zero-event-loss binary swap.
//
// Flow:
//  1. Pre-flight a target binary (exists, executable, runs `observe version`,
//     reports >= current version).
//  2. SIGTERM the running observe (via PID file or port lookup) so it flushes
//     ingest, fsyncs the WAL queue, and closes the DB.
//  3. Atomically rename the new binary into the position of the old one.
//  4. Spawn the new binary with the same env + working dir, detached.
//  5. Poll /healthz on the same port for up to 60s.
//  6. On any failure, restore the previous binary from a sidecar backup.
//
// The disk WAL queue (internal/ingest.DiskQueue) guarantees that any events
// pushed during steps 2–4 survive the swap — they sit in
// $OBSERVE_DATA_DIR/queue/events/current.log and get replayed by
// Buffer.AttachQueue when the new process boots.
package upgrade

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Version represents a parsed semver-like version: "observe 1.2.3" -> {1,2,3}.
// Pre-release suffixes after a hyphen ("-rc1") are preserved in Suffix and
// compared lexically as a tie-breaker.
type Version struct {
	Major  int
	Minor  int
	Patch  int
	Suffix string
}

// ParseVersion accepts the format `observe X.Y.Z[-suffix]` (with or without
// the leading "observe ") and returns the parsed Version.
func ParseVersion(s string) (Version, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "observe ")
	s = strings.TrimSpace(s)
	if s == "" {
		return Version{}, errors.New("empty version string")
	}
	suffix := ""
	if i := strings.IndexByte(s, '-'); i >= 0 {
		suffix = s[i+1:]
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("expected X.Y.Z, got %q", s)
	}
	v := Version{Suffix: suffix}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return Version{}, fmt.Errorf("version segment %d not numeric: %q", i, p)
		}
		switch i {
		case 0:
			v.Major = n
		case 1:
			v.Minor = n
		case 2:
			v.Patch = n
		}
	}
	return v, nil
}

// Compare returns -1 if a<b, 0 if equal, +1 if a>b.
// A version with a suffix is considered older than the same version without
// one (e.g. 1.0.0-rc1 < 1.0.0), matching semver pre-release semantics.
func Compare(a, b Version) int {
	if a.Major != b.Major {
		return cmpInt(a.Major, b.Major)
	}
	if a.Minor != b.Minor {
		return cmpInt(a.Minor, b.Minor)
	}
	if a.Patch != b.Patch {
		return cmpInt(a.Patch, b.Patch)
	}
	// Pre-release < release.
	if a.Suffix == "" && b.Suffix != "" {
		return 1
	}
	if a.Suffix != "" && b.Suffix == "" {
		return -1
	}
	return strings.Compare(a.Suffix, b.Suffix)
}

func cmpInt(a, b int) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

// PIDFile is the path of the PID file under the data directory.
func PIDFile(dataDir string) string {
	if dataDir == "" {
		dataDir = "."
	}
	return filepath.Join(dataDir, "observe.pid")
}

// EnvFile is the path of the env-snapshot file under the data directory.
// The running observe writes the OBSERVE_* env it was started with so the
// upgrade subcommand can hand the same env to the new binary even when the
// upgrader was invoked from a different shell.
func EnvFile(dataDir string) string {
	if dataDir == "" {
		dataDir = "."
	}
	return filepath.Join(dataDir, "observe.env")
}

// WriteEnv snapshots all OBSERVE_* environment variables to EnvFile.
// One KEY=VALUE per line, no escaping (values are typically URLs/secrets/
// numbers — anything containing a newline is rejected to keep the parser
// trivial). Atomic via tmp+rename.
func WriteEnv(dataDir string) error {
	if dataDir == "" {
		return errors.New("write env: empty data dir")
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fmt.Errorf("write env: mkdir: %w", err)
	}
	var buf strings.Builder
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "OBSERVE_") {
			continue
		}
		if strings.ContainsAny(kv, "\n\r") {
			continue
		}
		buf.WriteString(kv)
		buf.WriteByte('\n')
	}
	path := EnvFile(dataDir)
	tmp := path + ".tmp"
	// Mode 0o600 — env may contain JWT secret + admin password.
	if err := os.WriteFile(tmp, []byte(buf.String()), 0o600); err != nil {
		return fmt.Errorf("write env: %w", err)
	}
	return os.Rename(tmp, path)
}

// ReadEnv loads a snapshotted env file and returns it as a slice suitable
// for exec.Cmd.Env (KEY=VALUE strings). Returns nil, os.ErrNotExist if no
// snapshot is present — caller can fall back to os.Environ().
func ReadEnv(dataDir string) ([]string, error) {
	raw, err := os.ReadFile(EnvFile(dataDir))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		out = append(out, line)
	}
	return out, nil
}

// WritePID writes the current process PID to the PID file. It creates the
// data directory if needed and writes atomically via rename.
func WritePID(dataDir string) error {
	if dataDir == "" {
		return errors.New("write pid: empty data dir")
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fmt.Errorf("write pid: mkdir: %w", err)
	}
	path := PIDFile(dataDir)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		return fmt.Errorf("write pid: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("write pid rename: %w", err)
	}
	return nil
}

// ReadPID reads the PID stored in the PID file. Returns os.ErrNotExist if
// the file is missing.
func ReadPID(dataDir string) (int, error) {
	raw, err := os.ReadFile(PIDFile(dataDir))
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0, fmt.Errorf("read pid: parse: %w", err)
	}
	if pid <= 0 {
		return 0, fmt.Errorf("read pid: invalid pid %d", pid)
	}
	return pid, nil
}

// RemovePID best-effort deletes the PID file. Missing-file is not an error.
func RemovePID(dataDir string) error {
	err := os.Remove(PIDFile(dataDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// processAlive returns true if the given PID is currently a live process.
// On unix, kill(pid, 0) returns nil for live and ESRCH for dead.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if err := p.Signal(syscall.Signal(0)); err != nil {
		return false
	}
	return true
}

// SignalProcess sends sig to pid. Returns os.ErrNotExist if the process is gone.
func SignalProcess(pid int, sig os.Signal) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := p.Signal(sig); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrNotExist
		}
		return err
	}
	return nil
}

// WaitForExit polls until the given PID is no longer alive or timeout
// elapses. Returns nil on exit, context.DeadlineExceeded on timeout.
//
// On Unix, kill(pid, 0) keeps returning "alive" for processes that have
// exited but not yet been reaped by their parent (zombies). To avoid
// hanging in that case, callers should prefer WaitForShutdown which also
// checks for the absence of the PID file the running observe maintains.
func WaitForExit(ctx context.Context, pid int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if !processAlive(pid) {
			return nil
		}
		if time.Now().After(deadline) {
			return context.DeadlineExceeded
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// WaitForShutdown polls until the running observe is verifiably gone:
// either its process is no longer alive OR its PID file (which observe
// removes during graceful shutdown) has disappeared. Use this from the
// upgrade orchestrator — pure WaitForExit can hang on zombie children
// when the upgrader's invoking shell happens to be observe's parent.
func WaitForShutdown(ctx context.Context, pid int, dataDir string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	pidPath := PIDFile(dataDir)
	for {
		if !processAlive(pid) {
			return nil
		}
		// Graceful exit removes the PID file as the last cleanup step.
		if _, err := os.Stat(pidPath); errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if time.Now().After(deadline) {
			return context.DeadlineExceeded
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// WaitForHealthz polls the given URL until it returns 200 or timeout
// elapses. Used to confirm the new binary is serving before declaring the
// upgrade successful.
func WaitForHealthz(ctx context.Context, url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	for {
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("healthz never returned 200 within %s", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// PreflightTarget validates that the target binary exists, is executable,
// and reports a version >= currentVersion when invoked with `version`.
// Returns the parsed target version on success.
func PreflightTarget(targetPath, currentVersion string) (Version, error) {
	info, err := os.Stat(targetPath)
	if err != nil {
		return Version{}, fmt.Errorf("preflight: stat target: %w", err)
	}
	if info.IsDir() {
		return Version{}, fmt.Errorf("preflight: target is a directory: %s", targetPath)
	}
	if info.Mode().Perm()&0o111 == 0 {
		return Version{}, fmt.Errorf("preflight: target not executable: %s", targetPath)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, targetPath, "version").Output()
	if err != nil {
		return Version{}, fmt.Errorf("preflight: target failed `version`: %w", err)
	}
	targetV, err := ParseVersion(string(out))
	if err != nil {
		return Version{}, fmt.Errorf("preflight: target version parse: %w", err)
	}
	currentV, err := ParseVersion(currentVersion)
	if err != nil {
		return Version{}, fmt.Errorf("preflight: current version parse: %w", err)
	}
	if Compare(targetV, currentV) < 0 {
		return Version{}, fmt.Errorf("preflight: refusing downgrade %s -> %s", currentVersion, out)
	}
	return targetV, nil
}

// SwapBinary moves currentPath to currentPath+".prev" and renames newPath
// into currentPath. Both renames are atomic when source and dest are on the
// same filesystem. Returns the path to the saved previous binary so the
// caller can restore on failure.
func SwapBinary(currentPath, newPath string) (prevBackup string, err error) {
	prevBackup = currentPath + ".prev"
	// Remove any stale .prev from a previous failed swap.
	_ = os.Remove(prevBackup)
	if err := os.Rename(currentPath, prevBackup); err != nil {
		return "", fmt.Errorf("swap: backup current: %w", err)
	}
	if err := os.Rename(newPath, currentPath); err != nil {
		// Roll back the first rename so we don't end up with no binary.
		_ = os.Rename(prevBackup, currentPath)
		return "", fmt.Errorf("swap: install new: %w", err)
	}
	return prevBackup, nil
}

// RestoreBinary undoes a SwapBinary by renaming the saved backup back over
// the current path. Used by the upgrade orchestrator on healthz timeout.
func RestoreBinary(currentPath, prevBackup string) error {
	if prevBackup == "" {
		return errors.New("restore: no backup path provided")
	}
	if _, err := os.Stat(prevBackup); err != nil {
		return fmt.Errorf("restore: backup missing: %w", err)
	}
	// Move the failed new binary aside (best effort) so the rename can land.
	failed := currentPath + ".failed"
	_ = os.Remove(failed)
	if err := os.Rename(currentPath, failed); err == nil {
		// Best-effort cleanup of the failed binary.
		defer os.Remove(failed)
	}
	if err := os.Rename(prevBackup, currentPath); err != nil {
		return fmt.Errorf("restore: rename backup: %w", err)
	}
	return nil
}

// HealthURL converts a bind address (":3000", "0.0.0.0:3000",
// "127.0.0.1:3000") into a healthz URL the upgrader can poll.
func HealthURL(addr string) string {
	if addr == "" {
		addr = ":3000"
	}
	if strings.HasPrefix(addr, ":") {
		return "http://127.0.0.1" + addr + "/healthz"
	}
	if strings.HasPrefix(addr, "http://") || strings.HasPrefix(addr, "https://") {
		return strings.TrimRight(addr, "/") + "/healthz"
	}
	return "http://" + addr + "/healthz"
}
