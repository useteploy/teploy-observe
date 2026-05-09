package upgrade

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ─── ParseVersion / Compare ─────────────────────────────────────────────────

func TestParseVersion(t *testing.T) {
	cases := []struct {
		in   string
		want Version
		err  bool
	}{
		{"observe 0.1.0", Version{Major: 0, Minor: 1, Patch: 0}, false},
		{"0.1.0", Version{Major: 0, Minor: 1, Patch: 0}, false},
		{"observe 1.2.3-rc1", Version{Major: 1, Minor: 2, Patch: 3, Suffix: "rc1"}, false},
		{"  observe 10.20.30  \n", Version{Major: 10, Minor: 20, Patch: 30}, false},
		{"", Version{}, true},
		{"observe 1.2", Version{}, true},
		{"observe x.y.z", Version{}, true},
	}
	for _, c := range cases {
		got, err := ParseVersion(c.in)
		if c.err {
			if err == nil {
				t.Errorf("ParseVersion(%q): expected error, got %+v", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseVersion(%q) failed: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseVersion(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}
}

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.1.0", "0.1.0", 0},
		{"0.1.0", "0.1.1", -1},
		{"0.1.1", "0.1.0", 1},
		{"0.2.0", "0.1.99", 1},
		{"1.0.0", "0.99.99", 1},
		{"1.0.0-rc1", "1.0.0", -1},
		{"1.0.0", "1.0.0-rc1", 1},
		{"1.0.0-rc1", "1.0.0-rc2", -1},
	}
	for _, c := range cases {
		va, _ := ParseVersion(c.a)
		vb, _ := ParseVersion(c.b)
		if got := Compare(va, vb); got != c.want {
			t.Errorf("Compare(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// ─── PID-file lifecycle ─────────────────────────────────────────────────────

func TestPIDFileLifecycle(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadPID(dir); err == nil {
		t.Fatal("ReadPID before WritePID should fail")
	}
	if err := WritePID(dir); err != nil {
		t.Fatalf("WritePID: %v", err)
	}
	pid, err := ReadPID(dir)
	if err != nil {
		t.Fatalf("ReadPID after WritePID: %v", err)
	}
	if pid != os.Getpid() {
		t.Errorf("ReadPID = %d, want %d", pid, os.Getpid())
	}
	// Path is the documented "observe.pid" under the data dir.
	if _, err := os.Stat(filepath.Join(dir, "observe.pid")); err != nil {
		t.Errorf("PID file not at expected path: %v", err)
	}
	if err := RemovePID(dir); err != nil {
		t.Fatalf("RemovePID: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "observe.pid")); !os.IsNotExist(err) {
		t.Errorf("PID file still present after RemovePID: %v", err)
	}
	// Removing again is a no-op.
	if err := RemovePID(dir); err != nil {
		t.Errorf("RemovePID idempotency: %v", err)
	}
}

func TestWritePIDCreatesDir(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "nested", "data")
	if err := WritePID(dir); err != nil {
		t.Fatalf("WritePID nested: %v", err)
	}
	pid, err := ReadPID(dir)
	if err != nil || pid != os.Getpid() {
		t.Errorf("ReadPID after nested WritePID: pid=%d err=%v", pid, err)
	}
}

func TestWritePIDEmptyDir(t *testing.T) {
	if err := WritePID(""); err == nil {
		t.Error("WritePID(\"\") should fail")
	}
}

// ─── processAlive / WaitForExit ─────────────────────────────────────────────

func TestProcessAliveSelf(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Error("self process should be alive")
	}
	// A PID that is overwhelmingly unlikely to exist.
	if processAlive(2_000_000_000) {
		t.Error("PID 2e9 should not be alive")
	}
}

func TestWaitForExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal-based PID checks are unix-only")
	}
	// Spawn a short-lived child that exits in ~200ms. We must Wait() to
	// reap it — otherwise the kernel keeps the PID as a zombie and
	// kill(pid, 0) keeps returning success, which would hang WaitForExit.
	cmd := exec.Command("sleep", "0.2")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	pid := cmd.Process.Pid
	// Reap on exit so the PID is fully released.
	go func() { _ = cmd.Wait() }()

	start := time.Now()
	if err := WaitForExit(context.Background(), pid, 5*time.Second); err != nil {
		t.Fatalf("WaitForExit: %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("WaitForExit took too long: %s", d)
	}
}

func TestWaitForExitTimeout(t *testing.T) {
	// Self-PID never exits during the test → should hit the deadline quickly.
	err := WaitForExit(context.Background(), os.Getpid(), 200*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestWaitForShutdownPIDFileGone(t *testing.T) {
	// Self-PID is alive, PID file is missing — WaitForShutdown should
	// return immediately because PID-file absence is taken as evidence
	// of graceful exit.
	dir := t.TempDir()
	start := time.Now()
	if err := WaitForShutdown(context.Background(), os.Getpid(), dir, 5*time.Second); err != nil {
		t.Fatalf("WaitForShutdown should return when PID file missing: %v", err)
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("WaitForShutdown took too long: %s", d)
	}
}

func TestWaitForShutdownPIDFilePresent(t *testing.T) {
	// PID file present + process alive → WaitForShutdown should hit the
	// deadline.
	dir := t.TempDir()
	if err := WritePID(dir); err != nil {
		t.Fatal(err)
	}
	err := WaitForShutdown(context.Background(), os.Getpid(), dir, 200*time.Millisecond)
	if err == nil {
		t.Fatal("expected deadline exceeded with PID file still present")
	}
}

func TestWaitForShutdownPIDFileDisappearsMidPoll(t *testing.T) {
	dir := t.TempDir()
	if err := WritePID(dir); err != nil {
		t.Fatal(err)
	}
	// Remove the PID file after 250ms — WaitForShutdown should detect it.
	go func() {
		time.Sleep(250 * time.Millisecond)
		_ = RemovePID(dir)
	}()
	if err := WaitForShutdown(context.Background(), os.Getpid(), dir, 5*time.Second); err != nil {
		t.Fatalf("WaitForShutdown: %v", err)
	}
}

// ─── PreflightTarget ────────────────────────────────────────────────────────

// fakeBinary writes a small shell script that prints `observe <version>` for
// `version` arg, exit 1 otherwise. Returns its absolute path.
func fakeBinary(t *testing.T, version string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "observe-fake")
	body := "#!/bin/sh\nif [ \"$1\" = \"version\" ]; then echo \"observe " + version + "\"; exit 0; fi\nexit 1\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	return path
}

func TestPreflightTargetOK(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake binary needs /bin/sh")
	}
	target := fakeBinary(t, "0.2.0")
	v, err := PreflightTarget(target, "observe 0.1.0")
	if err != nil {
		t.Fatalf("preflight ok case: %v", err)
	}
	if v.Major != 0 || v.Minor != 2 || v.Patch != 0 {
		t.Errorf("got version %+v", v)
	}
}

func TestPreflightTargetSameVersionOK(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake binary needs /bin/sh")
	}
	target := fakeBinary(t, "0.1.0")
	if _, err := PreflightTarget(target, "observe 0.1.0"); err != nil {
		t.Errorf("equal versions should pass: %v", err)
	}
}

func TestPreflightTargetRefusesDowngrade(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake binary needs /bin/sh")
	}
	target := fakeBinary(t, "0.0.9")
	_, err := PreflightTarget(target, "observe 0.1.0")
	if err == nil {
		t.Fatal("downgrade should fail")
	}
	if !strings.Contains(err.Error(), "downgrade") {
		t.Errorf("error should mention downgrade: %v", err)
	}
}

func TestPreflightTargetMissing(t *testing.T) {
	_, err := PreflightTarget("/nonexistent/observe-binary", "observe 0.1.0")
	if err == nil {
		t.Fatal("missing target should fail")
	}
}

func TestPreflightTargetNotExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permissions only")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "not-exec")
	if err := os.WriteFile(path, []byte("not a binary"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := PreflightTarget(path, "observe 0.1.0")
	if err == nil {
		t.Fatal("non-exec target should fail")
	}
}

// ─── SwapBinary / RestoreBinary ─────────────────────────────────────────────

func TestSwapBinaryAndRestore(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "observe")
	newer := filepath.Join(dir, "observe-new")
	if err := os.WriteFile(current, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newer, []byte("NEW"), 0o755); err != nil {
		t.Fatal(err)
	}

	prev, err := SwapBinary(current, newer)
	if err != nil {
		t.Fatalf("SwapBinary: %v", err)
	}
	if !strings.HasSuffix(prev, ".prev") {
		t.Errorf("backup path should end in .prev, got %s", prev)
	}

	got, _ := os.ReadFile(current)
	if string(got) != "NEW" {
		t.Errorf("after swap, current = %q, want NEW", got)
	}
	gotPrev, _ := os.ReadFile(prev)
	if string(gotPrev) != "OLD" {
		t.Errorf("backup contents = %q, want OLD", gotPrev)
	}
	// New path must no longer exist (it was renamed in-place).
	if _, err := os.Stat(newer); !os.IsNotExist(err) {
		t.Errorf("new path should be gone after swap")
	}

	// Restore brings OLD back.
	if err := RestoreBinary(current, prev); err != nil {
		t.Fatalf("RestoreBinary: %v", err)
	}
	got, _ = os.ReadFile(current)
	if string(got) != "OLD" {
		t.Errorf("after restore, current = %q, want OLD", got)
	}
}

func TestSwapBinaryNewMissing(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "observe")
	if err := os.WriteFile(current, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := SwapBinary(current, filepath.Join(dir, "does-not-exist"))
	if err == nil {
		t.Fatal("swap with missing new should fail")
	}
	// And current must be restored to its original contents.
	got, _ := os.ReadFile(current)
	if string(got) != "OLD" {
		t.Errorf("after failed swap, current = %q, want OLD (rolled back)", got)
	}
}

func TestRestoreBinaryNoBackup(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "observe")
	if err := os.WriteFile(current, []byte("NEW"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := RestoreBinary(current, ""); err == nil {
		t.Error("RestoreBinary with empty backup should fail")
	}
	if err := RestoreBinary(current, filepath.Join(dir, "missing.prev")); err == nil {
		t.Error("RestoreBinary with missing backup should fail")
	}
}

// ─── HealthURL / WaitForHealthz ─────────────────────────────────────────────

func TestHealthURL(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{":3000", "http://127.0.0.1:3000/healthz"},
		{"0.0.0.0:3000", "http://0.0.0.0:3000/healthz"},
		{"127.0.0.1:3000", "http://127.0.0.1:3000/healthz"},
		{"http://obs.local:3000", "http://obs.local:3000/healthz"},
		{"http://obs.local:3000/", "http://obs.local:3000/healthz"},
		{"", "http://127.0.0.1:3000/healthz"},
	}
	for _, c := range cases {
		if got := HealthURL(c.in); got != c.want {
			t.Errorf("HealthURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestWaitForHealthzOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()
	if err := WaitForHealthz(context.Background(), srv.URL+"/healthz", 2*time.Second); err != nil {
		t.Fatalf("WaitForHealthz OK: %v", err)
	}
}

func TestWaitForHealthzFlapsThenOK(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits < 3 {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()
	if err := WaitForHealthz(context.Background(), srv.URL+"/healthz", 5*time.Second); err != nil {
		t.Fatalf("WaitForHealthz flap-then-ok: %v", err)
	}
	if hits < 3 {
		t.Errorf("expected at least 3 hits, got %d", hits)
	}
}

func TestWaitForHealthzTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer srv.Close()
	err := WaitForHealthz(context.Background(), srv.URL+"/healthz", 300*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

// ─── SignalProcess ──────────────────────────────────────────────────────────

func TestSignalProcessUnknownPID(t *testing.T) {
	err := SignalProcess(2_000_000_000, os.Interrupt)
	if err == nil {
		t.Fatal("expected error for nonexistent PID")
	}
}

// ─── Env snapshot ───────────────────────────────────────────────────────────

func TestEnvSnapshot(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OBSERVE_TEST_KEY", "value-with-equals=in-it")
	t.Setenv("OBSERVE_OTHER", "second")
	t.Setenv("UNRELATED", "should-not-appear")

	if err := WriteEnv(dir); err != nil {
		t.Fatalf("WriteEnv: %v", err)
	}
	snap, err := ReadEnv(dir)
	if err != nil {
		t.Fatalf("ReadEnv: %v", err)
	}
	got := map[string]string{}
	for _, kv := range snap {
		i := strings.IndexByte(kv, '=')
		got[kv[:i]] = kv[i+1:]
	}
	if got["OBSERVE_TEST_KEY"] != "value-with-equals=in-it" {
		t.Errorf("OBSERVE_TEST_KEY = %q", got["OBSERVE_TEST_KEY"])
	}
	if got["OBSERVE_OTHER"] != "second" {
		t.Errorf("OBSERVE_OTHER = %q", got["OBSERVE_OTHER"])
	}
	if _, ok := got["UNRELATED"]; ok {
		t.Errorf("UNRELATED leaked into snapshot")
	}
}

func TestReadEnvMissing(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadEnv(dir); err == nil {
		t.Error("ReadEnv on missing file should error")
	}
}

// helper: prove WritePID writes a parseable integer.
func TestWritePIDContents(t *testing.T) {
	dir := t.TempDir()
	if err := WritePID(dir); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "observe.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("pid file not numeric: %q", raw)
	}
	if pid != os.Getpid() {
		t.Errorf("got pid %d, want %d", pid, os.Getpid())
	}
}
