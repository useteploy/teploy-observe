// Package upgrade downloads, authenticates, and installs Observe releases.
// The service manager remains the sole owner of the server process.
package upgrade

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	releaseRepository = "useteploy/teploy-observe"
	// Public half of the offline Ed25519 release-signing key. Releases contain
	// a raw signature over the exact checksums.txt bytes.
	releasePublicKey = "HzMEb0L9K+7ea7HToYTCLAInCvIaumZ90j6pXGa5Esw="
	maxMetadataSize  = 1 << 20
	maxArchiveSize   = 256 << 20
)

// Version is a semantic version. Suffix excludes the leading hyphen.
type Version struct {
	Major  int
	Minor  int
	Patch  int
	Suffix string
}

func (v Version) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.Suffix != "" {
		s += "-" + v.Suffix
	}
	return s
}

// ParseVersion accepts a bare version, v-prefixed tag, or Observe version
// command output. Build metadata does not affect ordering.
func ParseVersion(s string) (Version, error) {
	s = strings.TrimSpace(s)
	for _, prefix := range []string{"teploy-observe ", "observe "} {
		s = strings.TrimPrefix(s, prefix)
	}
	s = strings.TrimPrefix(s, "v")
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}

	var suffix string
	if i := strings.IndexByte(s, '-'); i >= 0 {
		suffix = s[i+1:]
		s = s[:i]
		if err := validatePrerelease(suffix); err != nil {
			return Version{}, err
		}
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("expected X.Y.Z, got %q", s)
	}
	values := [3]int{}
	for i, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return Version{}, fmt.Errorf("invalid version segment %q", part)
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return Version{}, fmt.Errorf("version segment %d is not numeric: %q", i, part)
		}
		values[i] = n
	}
	return Version{Major: values[0], Minor: values[1], Patch: values[2], Suffix: suffix}, nil
}

func validatePrerelease(s string) error {
	if s == "" {
		return errors.New("empty prerelease")
	}
	for _, identifier := range strings.Split(s, ".") {
		if identifier == "" {
			return errors.New("empty prerelease identifier")
		}
		for _, r := range identifier {
			if (r < '0' || r > '9') && (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') && r != '-' {
				return fmt.Errorf("invalid prerelease identifier %q", identifier)
			}
		}
	}
	return nil
}

// Compare returns -1 if a < b, 0 if a == b, and 1 if a > b.
func Compare(a, b Version) int {
	for _, pair := range [][2]int{{a.Major, b.Major}, {a.Minor, b.Minor}, {a.Patch, b.Patch}} {
		if pair[0] < pair[1] {
			return -1
		}
		if pair[0] > pair[1] {
			return 1
		}
	}
	if a.Suffix == "" && b.Suffix != "" {
		return 1
	}
	if a.Suffix != "" && b.Suffix == "" {
		return -1
	}
	return comparePrerelease(a.Suffix, b.Suffix)
}

func comparePrerelease(a, b string) int {
	ap, bp := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(ap) && i < len(bp); i++ {
		if ap[i] == bp[i] {
			continue
		}
		an, ae := strconv.ParseUint(ap[i], 10, 64)
		bn, be := strconv.ParseUint(bp[i], 10, 64)
		switch {
		case ae == nil && be == nil:
			if an < bn {
				return -1
			}
			return 1
		case ae == nil:
			return -1
		case be == nil:
			return 1
		case ap[i] < bp[i]:
			return -1
		default:
			return 1
		}
	}
	if len(ap) < len(bp) {
		return -1
	}
	if len(ap) > len(bp) {
		return 1
	}
	return 0
}

type ReleaseClient struct {
	HTTPClient      *http.Client
	APIURL          string
	DownloadBaseURL string
	PublicKey       ed25519.PublicKey
}

func NewReleaseClient() *ReleaseClient {
	key, _ := base64.StdEncoding.DecodeString(releasePublicKey)
	return &ReleaseClient{
		HTTPClient:      &http.Client{Timeout: 30 * time.Second},
		APIURL:          "https://api.github.com/repos/" + releaseRepository,
		DownloadBaseURL: "https://github.com/" + releaseRepository + "/releases/download",
		PublicKey:       ed25519.PublicKey(key),
	}
}

// PreparedRelease is a verified release binary in a temporary directory.
type PreparedRelease struct {
	Version    Version
	BinaryPath string
	cleanup    func()
}

func (r *PreparedRelease) Close() {
	if r != nil && r.cleanup != nil {
		r.cleanup()
	}
}

func (c *ReleaseClient) Prepare(ctx context.Context, requested string, current Version) (*PreparedRelease, error) {
	tag, target, err := c.resolveVersion(ctx, requested)
	if err != nil {
		return nil, err
	}
	if Compare(target, current) < 0 {
		return nil, fmt.Errorf("refusing downgrade %s -> %s", current, target)
	}
	if Compare(target, current) == 0 {
		return nil, fmt.Errorf("version %s is already installed", target)
	}

	asset, err := archiveName(target)
	if err != nil {
		return nil, err
	}
	base := strings.TrimRight(c.DownloadBaseURL, "/") + "/" + tag
	checksums, err := c.getLimited(ctx, base+"/checksums.txt", maxMetadataSize)
	if err != nil {
		return nil, fmt.Errorf("download checksums: %w", err)
	}
	signature, err := c.getLimited(ctx, base+"/checksums.txt.sig", ed25519.SignatureSize+1)
	if err != nil {
		return nil, fmt.Errorf("download checksum signature: %w", err)
	}
	if err := VerifyChecksums(checksums, signature, c.PublicKey); err != nil {
		return nil, err
	}
	wantHash, err := checksumFor(checksums, asset)
	if err != nil {
		return nil, err
	}

	dir, err := os.MkdirTemp("", "observe-update-*")
	if err != nil {
		return nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	archivePath := filepath.Join(dir, asset)
	if err := c.downloadArchive(ctx, base+"/"+asset, archivePath, wantHash); err != nil {
		cleanup()
		return nil, err
	}
	binaryPath, err := extractBinary(archivePath, dir)
	if err != nil {
		cleanup()
		return nil, err
	}
	actual, err := BinaryVersion(binaryPath)
	if err != nil {
		cleanup()
		return nil, err
	}
	if Compare(actual, target) != 0 {
		cleanup()
		return nil, fmt.Errorf("release tag %s contains binary version %s", target, actual)
	}
	return &PreparedRelease{Version: target, BinaryPath: binaryPath, cleanup: cleanup}, nil
}

func (c *ReleaseClient) resolveVersion(ctx context.Context, requested string) (string, Version, error) {
	if requested != "" && requested != "latest" {
		v, err := ParseVersion(requested)
		if err != nil {
			return "", Version{}, err
		}
		return "v" + v.String(), v, nil
	}
	body, err := c.getLimited(ctx, strings.TrimRight(c.APIURL, "/")+"/releases/latest", maxMetadataSize)
	if err != nil {
		return "", Version{}, fmt.Errorf("resolve latest release: %w", err)
	}
	var response struct {
		TagName string `json:"tag_name"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return "", Version{}, fmt.Errorf("decode latest release: %w", err)
	}
	v, err := ParseVersion(response.TagName)
	if err != nil {
		return "", Version{}, fmt.Errorf("invalid latest release tag %q: %w", response.TagName, err)
	}
	if response.TagName != "v"+v.String() {
		return "", Version{}, fmt.Errorf("latest release tag is not canonical: %q", response.TagName)
	}
	return response.TagName, v, nil
}

func (c *ReleaseClient) getLimited(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned %s", url, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("response exceeds size limit")
	}
	return data, nil
}

func VerifyChecksums(checksums, signature []byte, publicKey ed25519.PublicKey) error {
	if len(publicKey) != ed25519.PublicKeySize {
		return errors.New("invalid embedded release public key")
	}
	if len(signature) != ed25519.SignatureSize {
		return fmt.Errorf("invalid checksum signature length: %d", len(signature))
	}
	if !ed25519.Verify(publicKey, checksums, signature) {
		return errors.New("checksum signature verification failed")
	}
	return nil
}

func checksumFor(checksums []byte, asset string) ([sha256.Size]byte, error) {
	var found [sha256.Size]byte
	matches := 0
	for _, line := range strings.Split(strings.TrimSpace(string(checksums)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return found, fmt.Errorf("malformed checksum line %q", line)
		}
		name := strings.TrimPrefix(fields[1], "*")
		if name != asset {
			continue
		}
		raw, err := hex.DecodeString(fields[0])
		if err != nil || len(raw) != sha256.Size {
			return found, fmt.Errorf("invalid checksum for %s", asset)
		}
		copy(found[:], raw)
		matches++
	}
	if matches != 1 {
		return found, fmt.Errorf("expected one checksum for %s, found %d", asset, matches)
	}
	return found, nil
}

func archiveName(v Version) (string, error) {
	osName := runtime.GOOS
	if osName != "linux" && osName != "darwin" {
		return "", fmt.Errorf("unsupported operating system %s", osName)
	}
	arch := runtime.GOARCH
	if arch == "amd64" {
		arch = "x86_64"
	}
	if arch != "x86_64" && arch != "arm64" {
		return "", fmt.Errorf("unsupported architecture %s", runtime.GOARCH)
	}
	return fmt.Sprintf("observe_%s_%s_%s.tar.gz", v, osName, arch), nil
}

func (c *ReleaseClient) downloadArchive(ctx context.Context, url, path string, want [sha256.Size]byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("download release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download release: %s", resp.Status)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	hash := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(f, hash), io.LimitReader(resp.Body, maxArchiveSize+1))
	closeErr := f.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if n > maxArchiveSize {
		return errors.New("release archive exceeds size limit")
	}
	if !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), hex.EncodeToString(want[:])) {
		return errors.New("release archive checksum mismatch")
	}
	return nil
}

func extractBinary(archivePath, destination string) (string, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("open release archive: %w", err)
	}
	defer gz.Close()

	output := filepath.Join(destination, "observe")
	found := false
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("read release archive: %w", err)
		}
		clean := filepath.Clean(header.Name)
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("unsafe archive path %q", header.Name)
		}
		if filepath.Base(clean) != "observe" {
			continue
		}
		if header.Typeflag != tar.TypeReg || found {
			return "", errors.New("release archive must contain exactly one regular observe binary")
		}
		out, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
		if err != nil {
			return "", err
		}
		n, copyErr := io.Copy(out, io.LimitReader(tr, maxArchiveSize+1))
		closeErr := out.Close()
		if copyErr != nil {
			return "", copyErr
		}
		if closeErr != nil {
			return "", closeErr
		}
		if n > maxArchiveSize {
			return "", errors.New("observe binary exceeds size limit")
		}
		found = true
	}
	if !found {
		return "", errors.New("release archive does not contain observe")
	}
	return output, nil
}

func BinaryVersion(path string) (Version, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "version").Output()
	if err != nil {
		return Version{}, fmt.Errorf("candidate failed version check: %w", err)
	}
	v, err := ParseVersion(string(out))
	if err != nil {
		return Version{}, fmt.Errorf("candidate version check: %w", err)
	}
	return v, nil
}

type ServiceManager interface {
	IsActive(context.Context) error
	Stop(context.Context) error
	Start(context.Context) error
}

type SystemdManager struct {
	Unit string
}

func (m SystemdManager) run(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "systemctl", append(args, m.Unit)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl %s %s: %w: %s", strings.Join(args, " "), m.Unit, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (m SystemdManager) IsActive(ctx context.Context) error {
	return m.run(ctx, "is-active", "--quiet")
}
func (m SystemdManager) Stop(ctx context.Context) error  { return m.run(ctx, "stop") }
func (m SystemdManager) Start(ctx context.Context) error { return m.run(ctx, "start") }

func (m SystemdManager) VerifyExecutable(ctx context.Context, expected string) error {
	cmd := exec.CommandContext(ctx, "systemctl", "show", "--property=MainPID", "--value", m.Unit)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("read %s MainPID: %w: %s", m.Unit, err, strings.TrimSpace(string(out)))
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || pid <= 0 {
		return fmt.Errorf("%s has invalid MainPID %q", m.Unit, strings.TrimSpace(string(out)))
	}
	actual, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return fmt.Errorf("resolve %s executable: %w", m.Unit, err)
	}
	expected, err = filepath.EvalSymlinks(expected)
	if err != nil {
		return fmt.Errorf("resolve updater executable: %w", err)
	}
	actual, err = filepath.EvalSymlinks(actual)
	if err != nil {
		return fmt.Errorf("resolve service executable: %w", err)
	}
	if actual != expected {
		return fmt.Errorf("%s runs %s, not %s", m.Unit, actual, expected)
	}
	return nil
}

type ApplyOptions struct {
	CurrentPath    string
	CandidatePath  string
	CurrentVersion Version
	TargetVersion  Version
	HealthURL      string
	LockPath       string
	Manager        ServiceManager
	Logger         *slog.Logger
	HealthTimeout  time.Duration
}

// Apply stops the service through its manager, atomically replaces the binary,
// starts it, and verifies the exact running version. Any failure after the swap
// restores and restarts the prior version.
func Apply(ctx context.Context, options ApplyOptions) error {
	if options.Manager == nil {
		return errors.New("service manager is required")
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	if options.HealthTimeout == 0 {
		options.HealthTimeout = 60 * time.Second
	}
	lock, err := acquireLock(options.LockPath)
	if err != nil {
		return err
	}
	defer releaseLock(lock)
	installedVersion, err := BinaryVersion(options.CurrentPath)
	if err != nil {
		return fmt.Errorf("verify installed version under upgrade lock: %w", err)
	}
	if Compare(options.TargetVersion, installedVersion) < 0 {
		return fmt.Errorf("refusing downgrade %s -> %s", installedVersion, options.TargetVersion)
	}
	if Compare(options.TargetVersion, installedVersion) == 0 {
		return fmt.Errorf("version %s is already installed", installedVersion)
	}
	options.CurrentVersion = installedVersion
	if err := options.Manager.IsActive(ctx); err != nil {
		return fmt.Errorf("observe service must be active before upgrade: %w", err)
	}
	if owner, ok := options.Manager.(interface {
		VerifyExecutable(context.Context, string) error
	}); ok {
		if err := owner.VerifyExecutable(ctx, options.CurrentPath); err != nil {
			return fmt.Errorf("verify service ownership: %w", err)
		}
	}

	stage, err := stageBinary(options.CurrentPath, options.CandidatePath)
	if err != nil {
		return fmt.Errorf("stage candidate: %w", err)
	}
	defer os.Remove(stage)
	if err := options.Manager.Stop(ctx); err != nil {
		return fmt.Errorf("stop observe: %w", err)
	}
	backup, err := SwapBinary(options.CurrentPath, stage)
	if err != nil {
		if startErr := options.Manager.Start(ctx); startErr != nil {
			return fmt.Errorf("%v; restart previous version failed: %w", err, startErr)
		}
		return err
	}
	rollback := func(cause error) error {
		if err := options.Manager.Stop(ctx); err != nil {
			return fmt.Errorf("%v; rollback stop failed: %w", cause, err)
		}
		if err := RestoreBinary(options.CurrentPath, backup); err != nil {
			return fmt.Errorf("%v; rollback restore failed: %w", cause, err)
		}
		if err := options.Manager.Start(ctx); err != nil {
			return fmt.Errorf("%v; rollback restart failed: %w", cause, err)
		}
		if err := WaitForVersionHealth(ctx, options.HealthURL, options.CurrentVersion, options.HealthTimeout); err != nil {
			return fmt.Errorf("%v; rollback health check failed: %w", cause, err)
		}
		return fmt.Errorf("%v; previous version %s restored and healthy", cause, options.CurrentVersion)
	}
	if err := options.Manager.Start(ctx); err != nil {
		return rollback(fmt.Errorf("start candidate: %w", err))
	}
	if err := WaitForVersionHealth(ctx, options.HealthURL, options.TargetVersion, options.HealthTimeout); err != nil {
		return rollback(fmt.Errorf("candidate health check: %w", err))
	}
	options.Logger.Info("upgrade: previous binary retained", "path", backup)
	return nil
}

func stageBinary(currentPath, candidatePath string) (string, error) {
	src, err := os.Open(candidatePath)
	if err != nil {
		return "", err
	}
	defer src.Close()
	stage, err := os.CreateTemp(filepath.Dir(currentPath), ".observe-update-*")
	if err != nil {
		return "", err
	}
	path := stage.Name()
	ok := false
	defer func() {
		_ = stage.Close()
		if !ok {
			_ = os.Remove(path)
		}
	}()
	if _, err := io.Copy(stage, src); err != nil {
		return "", err
	}
	if err := stage.Chmod(0o755); err != nil {
		return "", err
	}
	if err := stage.Sync(); err != nil {
		return "", err
	}
	if err := stage.Close(); err != nil {
		return "", err
	}
	ok = true
	return path, nil
}

// SwapBinary preserves the current inode as .prev, then atomically renames the
// staged candidate over the live path. The live path is never absent.
func SwapBinary(currentPath, stagedPath string) (string, error) {
	backup := currentPath + ".prev"
	_ = os.Remove(backup)
	if err := os.Link(currentPath, backup); err != nil {
		return "", fmt.Errorf("backup current binary: %w", err)
	}
	if err := os.Rename(stagedPath, currentPath); err != nil {
		_ = os.Remove(backup)
		return "", fmt.Errorf("install candidate: %w", err)
	}
	if err := syncDir(filepath.Dir(currentPath)); err != nil {
		_ = os.Rename(backup, currentPath)
		return "", fmt.Errorf("sync installed candidate: %w", err)
	}
	return backup, nil
}

func RestoreBinary(currentPath, backupPath string) error {
	if backupPath == "" {
		return errors.New("restore: no backup path")
	}
	if err := os.Rename(backupPath, currentPath); err != nil {
		return fmt.Errorf("restore previous binary: %w", err)
	}
	return syncDir(filepath.Dir(currentPath))
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func HealthURL(addr string) string {
	if addr == "" {
		addr = ":3000"
	}
	if strings.HasPrefix(addr, ":") {
		return "http://127.0.0.1" + addr + "/healthz"
	}
	if strings.HasPrefix(addr, "0.0.0.0:") {
		addr = "127.0.0.1:" + strings.TrimPrefix(addr, "0.0.0.0:")
	}
	if strings.HasPrefix(addr, "[::]:") {
		addr = "127.0.0.1:" + strings.TrimPrefix(addr, "[::]:")
	}
	if strings.HasPrefix(addr, "http://") || strings.HasPrefix(addr, "https://") {
		return strings.TrimRight(addr, "/") + "/healthz"
	}
	return "http://" + addr + "/healthz"
}

func WaitForVersionHealth(ctx context.Context, url string, expected Version, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{
		Timeout:   2 * time.Second,
		Transport: &http.Transport{Proxy: nil},
	}
	consecutive := 0
	lastErr := errors.New("health endpoint was not reached")
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			var body struct {
				Status  string `json:"status"`
				Version string `json:"version"`
			}
			decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK && decodeErr == nil && body.Status == "ok" {
				actual, versionErr := ParseVersion(body.Version)
				if versionErr == nil && Compare(actual, expected) == 0 {
					consecutive++
					if consecutive == 3 {
						return nil
					}
				} else {
					consecutive = 0
					lastErr = fmt.Errorf("health endpoint reports version %q, expected %s", body.Version, expected)
				}
			} else {
				consecutive = 0
				lastErr = fmt.Errorf("health endpoint returned %s", resp.Status)
			}
		} else {
			consecutive = 0
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("health check did not stabilize within %s: %w", timeout, lastErr)
}

func acquireLock(path string) (*os.File, error) {
	if path == "" {
		return nil, errors.New("upgrade lock path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, errors.New("another Observe upgrade is already running")
	}
	return f, nil
}

func releaseLock(f *os.File) {
	if f == nil {
		return
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}

// ManagedInstall returns the external manager that owns an executable path.
func ManagedInstall(path string) string {
	path = filepath.ToSlash(path)
	switch {
	case strings.Contains(path, "/Cellar/"), strings.Contains(path, "/homebrew/"), strings.Contains(path, "/linuxbrew/"):
		return "homebrew"
	case fileExists("/.dockerenv"), os.Getenv("KUBERNETES_SERVICE_HOST") != "":
		return "container"
	default:
		return ""
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
