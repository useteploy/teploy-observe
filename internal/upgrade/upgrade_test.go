package upgrade

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestParseVersion(t *testing.T) {
	tests := []struct {
		input string
		want  Version
		err   bool
	}{
		{"observe 1.2.3", Version{Major: 1, Minor: 2, Patch: 3}, false},
		{"teploy-observe 1.2.3", Version{Major: 1, Minor: 2, Patch: 3}, false},
		{"v1.2.3-rc.2+build.7", Version{Major: 1, Minor: 2, Patch: 3, Suffix: "rc.2"}, false},
		{"1.2", Version{}, true},
		{"1.02.3", Version{}, true},
		{"dev", Version{}, true},
		{"1.2.3-", Version{}, true},
	}
	for _, test := range tests {
		got, err := ParseVersion(test.input)
		if test.err {
			if err == nil {
				t.Errorf("ParseVersion(%q) should fail", test.input)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseVersion(%q): %v", test.input, err)
		} else if got != test.want {
			t.Errorf("ParseVersion(%q) = %+v, want %+v", test.input, got, test.want)
		}
	}
}

func TestCompare(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.0.0", "1.0.1", -1},
		{"2.0.0", "1.9.9", 1},
		{"1.0.0-rc.1", "1.0.0", -1},
		{"1.0.0-rc.2", "1.0.0-rc.10", -1},
		{"1.0.0-alpha", "1.0.0-1", 1},
	}
	for _, test := range tests {
		a, _ := ParseVersion(test.a)
		b, _ := ParseVersion(test.b)
		if got := Compare(a, b); got != test.want {
			t.Errorf("Compare(%s, %s) = %d, want %d", test.a, test.b, got, test.want)
		}
	}
}

func TestVerifyChecksums(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	checksums := []byte("abc  observe_1.0.0_linux_x86_64.tar.gz\n")
	signature := ed25519.Sign(privateKey, checksums)
	if err := VerifyChecksums(checksums, signature, publicKey); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	checksums[0] = 'd'
	if err := VerifyChecksums(checksums, signature, publicKey); err == nil {
		t.Fatal("modified checksums should fail verification")
	}
	if err := VerifyChecksums([]byte("abc"), signature[:10], publicKey); err == nil {
		t.Fatal("truncated signature should fail verification")
	}
}

func TestReleasePublicKeyMatchesPackagingAndInstaller(t *testing.T) {
	publicPEM, err := os.ReadFile("../../packaging/release-signing.pub")
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(publicPEM)
	if block == nil {
		t.Fatal("packaged public key is not PEM")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	packagedKey, ok := parsed.(ed25519.PublicKey)
	if !ok {
		t.Fatal("packaged public key is not Ed25519")
	}
	clientKey := NewReleaseClient().PublicKey
	if !bytes.Equal(packagedKey, clientKey) {
		t.Fatal("packaged and embedded release keys differ")
	}
	installer, err := os.ReadFile("../../scripts/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(installer, bytes.TrimSpace(publicPEM)) {
		t.Fatal("installer does not contain the packaged release key")
	}
}

func TestChecksumForRequiresOneExactEntry(t *testing.T) {
	name := "observe_1.0.0_linux_x86_64.tar.gz"
	hash := strings.Repeat("a", 64)
	got, err := checksumFor([]byte(hash+"  "+name+"\n"), name)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(got[:]) != hash {
		t.Fatalf("unexpected hash %x", got)
	}
	if _, err := checksumFor([]byte(hash+"  "+name+"\n"+hash+"  "+name+"\n"), name); err == nil {
		t.Fatal("duplicate entries should fail")
	}
	if _, err := checksumFor([]byte("malformed\n"), name); err == nil {
		t.Fatal("malformed entries should fail")
	}
}

func TestReleaseClientPrepare(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("release does not target Windows")
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	target, _ := ParseVersion("1.2.3")
	asset, err := archiveName(target)
	if err != nil {
		t.Fatal(err)
	}
	archive := testArchive(t, "observe", "#!/bin/sh\necho 'observe 1.2.3'\n", tar.TypeReg)
	hash := sha256.Sum256(archive)
	checksums := []byte(fmt.Sprintf("%x  %s\n", hash, asset))
	signature := ed25519.Sign(privateKey, checksums)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/releases/latest":
			_, _ = w.Write([]byte(`{"tag_name":"v1.2.3"}`))
		case strings.HasSuffix(r.URL.Path, "/checksums.txt"):
			_, _ = w.Write(checksums)
		case strings.HasSuffix(r.URL.Path, "/checksums.txt.sig"):
			_, _ = w.Write(signature)
		case strings.HasSuffix(r.URL.Path, "/"+asset):
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := &ReleaseClient{
		HTTPClient:      server.Client(),
		APIURL:          server.URL + "/api",
		DownloadBaseURL: server.URL + "/download",
		PublicKey:       publicKey,
	}
	current, _ := ParseVersion("1.0.0")
	release, err := client.Prepare(context.Background(), "latest", current)
	if err != nil {
		t.Fatal(err)
	}
	defer release.Close()
	if Compare(release.Version, target) != 0 {
		t.Fatalf("prepared version %s, want %s", release.Version, target)
	}
}

func TestExtractBinaryRejectsUnsafeOrLinkedEntry(t *testing.T) {
	for _, test := range []struct {
		name     string
		entry    string
		typeflag byte
	}{
		{"traversal", "../observe", tar.TypeReg},
		{"symlink", "observe", tar.TypeSymlink},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			archive := filepath.Join(dir, "release.tar.gz")
			if err := os.WriteFile(archive, testArchive(t, test.entry, "data", test.typeflag), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := extractBinary(archive, dir); err == nil {
				t.Fatal("unsafe archive should fail")
			}
		})
	}
}

func testArchive(t *testing.T, name, contents string, typeflag byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	header := &tar.Header{Name: name, Mode: 0o755, Size: int64(len(contents)), Typeflag: typeflag}
	if typeflag != tar.TypeReg {
		header.Size = 0
	}
	if err := tw.WriteHeader(header); err != nil {
		t.Fatal(err)
	}
	if typeflag == tar.TypeReg {
		if _, err := tw.Write([]byte(contents)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestSwapBinaryAndRestore(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "observe")
	staged := filepath.Join(dir, "observe-new")
	if err := os.WriteFile(current, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	backup, err := SwapBinary(current, staged)
	if err != nil {
		t.Fatal(err)
	}
	assertFileContents(t, current, "new")
	assertFileContents(t, backup, "old")
	if err := RestoreBinary(current, backup); err != nil {
		t.Fatal(err)
	}
	assertFileContents(t, current, "old")
}

type fakeManager struct {
	active    bool
	startFail int
	stops     int
	starts    int
}

type rejectingOwner struct {
	*fakeManager
}

func (m rejectingOwner) VerifyExecutable(context.Context, string) error {
	return fmt.Errorf("wrong executable")
}

func (m *fakeManager) IsActive(context.Context) error {
	if !m.active {
		return fmt.Errorf("inactive")
	}
	return nil
}

func (m *fakeManager) Stop(context.Context) error {
	m.stops++
	m.active = false
	return nil
}

func (m *fakeManager) Start(context.Context) error {
	m.starts++
	if m.startFail > 0 {
		m.startFail--
		return fmt.Errorf("start failed")
	}
	m.active = true
	return nil
}

func TestApplySuccess(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "observe")
	candidate := filepath.Join(dir, "candidate")
	writeTestBinary(t, current, "1.0.0")
	writeTestBinary(t, candidate, "1.1.0")
	manager := &fakeManager{active: true}
	server := versionServer(t, current, manager)
	defer server.Close()

	err := Apply(context.Background(), ApplyOptions{
		CurrentPath: current, CandidatePath: candidate,
		CurrentVersion: mustVersion(t, "1.0.0"), TargetVersion: mustVersion(t, "1.1.0"),
		HealthURL: server.URL, LockPath: filepath.Join(dir, "upgrade.lock"),
		Manager: manager, HealthTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertBinaryVersion(t, current, "1.1.0")
	if manager.stops != 1 || manager.starts != 1 {
		t.Fatalf("stops=%d starts=%d", manager.stops, manager.starts)
	}
}

func TestApplyRejectsServiceForAnotherExecutable(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "observe")
	candidate := filepath.Join(dir, "candidate")
	writeTestBinary(t, current, "1.0.0")
	writeTestBinary(t, candidate, "1.1.0")
	manager := &fakeManager{active: true}
	err := Apply(context.Background(), ApplyOptions{
		CurrentPath: current, CandidatePath: candidate,
		CurrentVersion: mustVersion(t, "1.0.0"), TargetVersion: mustVersion(t, "1.1.0"),
		HealthURL: "http://127.0.0.1:1/healthz", LockPath: filepath.Join(dir, "upgrade.lock"),
		Manager: rejectingOwner{manager},
	})
	if err == nil || !strings.Contains(err.Error(), "verify service ownership") {
		t.Fatalf("expected ownership error, got %v", err)
	}
	if manager.stops != 0 || manager.starts != 0 {
		t.Fatalf("service changed before ownership verification")
	}
}

func TestApplyRechecksInstalledVersionUnderLock(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "observe")
	candidate := filepath.Join(dir, "candidate")
	writeTestBinary(t, current, "1.2.0")
	writeTestBinary(t, candidate, "1.1.0")
	manager := &fakeManager{active: true}
	err := Apply(context.Background(), ApplyOptions{
		CurrentPath: current, CandidatePath: candidate,
		CurrentVersion: mustVersion(t, "1.0.0"), TargetVersion: mustVersion(t, "1.1.0"),
		HealthURL: "http://127.0.0.1:1/healthz", LockPath: filepath.Join(dir, "upgrade.lock"),
		Manager: manager,
	})
	if err == nil || !strings.Contains(err.Error(), "refusing downgrade 1.2.0 -> 1.1.0") {
		t.Fatalf("expected locked downgrade rejection, got %v", err)
	}
	if manager.stops != 0 || manager.starts != 0 {
		t.Fatalf("service changed before locked version check")
	}
}

func TestApplyRestoresAndRestartsOnStartFailure(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "observe")
	candidate := filepath.Join(dir, "candidate")
	writeTestBinary(t, current, "1.0.0")
	writeTestBinary(t, candidate, "1.1.0")
	manager := &fakeManager{active: true, startFail: 1}
	server := versionServer(t, current, manager)
	defer server.Close()

	err := Apply(context.Background(), ApplyOptions{
		CurrentPath: current, CandidatePath: candidate,
		CurrentVersion: mustVersion(t, "1.0.0"), TargetVersion: mustVersion(t, "1.1.0"),
		HealthURL: server.URL, LockPath: filepath.Join(dir, "upgrade.lock"),
		Manager: manager, HealthTimeout: 2 * time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "restored and healthy") {
		t.Fatalf("expected successful rollback error, got %v", err)
	}
	assertBinaryVersion(t, current, "1.0.0")
	if !manager.active || manager.starts != 2 {
		t.Fatalf("rollback did not restart service: active=%v starts=%d", manager.active, manager.starts)
	}
}

func TestApplyRestoresAndRestartsOnWrongRunningVersion(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "observe")
	candidate := filepath.Join(dir, "candidate")
	writeTestBinary(t, current, "1.0.0")
	writeTestBinary(t, candidate, "1.1.0")
	manager := &fakeManager{active: true}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		version, _ := BinaryVersion(current)
		reported := version.String()
		if reported == "1.1.0" {
			reported = "9.9.9"
		}
		_, _ = fmt.Fprintf(w, `{"status":"ok","version":%q}`, reported)
	}))
	defer server.Close()

	err := Apply(context.Background(), ApplyOptions{
		CurrentPath: current, CandidatePath: candidate,
		CurrentVersion: mustVersion(t, "1.0.0"), TargetVersion: mustVersion(t, "1.1.0"),
		HealthURL: server.URL, LockPath: filepath.Join(dir, "upgrade.lock"),
		Manager: manager, HealthTimeout: 1200 * time.Millisecond,
	})
	if err == nil || !strings.Contains(err.Error(), "restored and healthy") {
		t.Fatalf("expected health-gated rollback, got %v", err)
	}
	assertBinaryVersion(t, current, "1.0.0")
	if !manager.active || manager.starts != 2 {
		t.Fatalf("rollback did not restart service: active=%v starts=%d", manager.active, manager.starts)
	}
}

func TestHealthURL(t *testing.T) {
	tests := map[string]string{
		":3000":        "http://127.0.0.1:3000/healthz",
		"0.0.0.0:3000": "http://127.0.0.1:3000/healthz",
		"[::]:3000":    "http://127.0.0.1:3000/healthz",
		"127.0.0.1:9":  "http://127.0.0.1:9/healthz",
	}
	for input, want := range tests {
		if got := HealthURL(input); got != want {
			t.Errorf("HealthURL(%q) = %q, want %q", input, got, want)
		}
	}
}

func versionServer(t *testing.T, current string, manager *fakeManager) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !manager.active {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		version, err := BinaryVersion(current)
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = fmt.Fprintf(w, `{"status":"ok","version":%q}`, version.String())
	}))
}

func mustVersion(t *testing.T, value string) Version {
	t.Helper()
	v, err := ParseVersion(value)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func writeTestBinary(t *testing.T, path, version string) {
	t.Helper()
	contents := "#!/bin/sh\necho 'observe " + version + "'\n"
	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
}

func assertFileContents(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != want {
		t.Fatalf("%s contains %q, want %q", path, data, want)
	}
}

func assertBinaryVersion(t *testing.T, path, want string) {
	t.Helper()
	got, err := BinaryVersion(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != want {
		t.Fatalf("%s reports %s, want %s", path, got, want)
	}
}
