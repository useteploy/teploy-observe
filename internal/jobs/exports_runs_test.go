package jobs

// O14 slice 2 oracle: the scheduled-export run ledger - durable history,
// outbox-pattern retry (visible backoff, attempt budget, dead letters),
// stable run-keyed S3 objects, the restoration manifest sidecar, frozen
// run inputs, and the create-time destination scoping. At the real engine
// with a fake S3 endpoint (force-path-style PUTs against httptest),
// fail-not-skip under OBSERVE_REQUIRE_NUCLEUS.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
	"github.com/useteploy/teploy-observe/internal/schema"
)

// fakeS3 is the S3-compatible receiver: it records every PUT (key + body)
// and can fail the first N requests with a 500.
type fakeS3 struct {
	mu        sync.Mutex
	puts      []fakePut
	failFirst int
}

type fakePut struct {
	key  string
	body string
}

func (f *fakeS3) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.puts = append(f.puts, fakePut{key: r.URL.Path, body: ""})
		n := len(f.puts)
		fail := f.failFirst
		f.mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.puts[n-1].body = string(body)
		f.mu.Unlock()
		if fail > 0 && n <= fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}

func (f *fakeS3) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.puts)
}

func (f *fakeS3) dataPuts() []fakePut {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []fakePut{}
	for _, p := range f.puts {
		if !strings.HasSuffix(p.key, ".manifest.json") {
			out = append(out, p)
		}
	}
	return out
}

func (f *fakeS3) manifestPuts() []fakePut {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []fakePut{}
	for _, p := range f.puts {
		if strings.HasSuffix(p.key, ".manifest.json") {
			out = append(out, p)
		}
	}
	return out
}

func runsFixture(t *testing.T) *nucleus.Client {
	t.Helper()
	dsn := nucleustest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("nucleus not reachable at %s — skipping integration test", dsn)
	}
	if err := schema.Apply(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func newRunsService(db *nucleus.Client, recv *fakeS3, srvURL string) *ExportService {
	svc := NewExportService(db, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	svc.maxAttempts = 3
	svc.backoffBase = 0 // immediate retry - the drain policy, not sleeps
	return svc
}

// TestO14ExportRunRetryAndHistory: one transient S3 failure is retried
// under the SAME object key (an at-least-once retry overwrites its own
// partial upload), the run ledger records the attempt, and a fresh service
// instance (restart) reads the same history.
func TestO14ExportRunRetryAndHistory(t *testing.T) {
	t.Setenv("OBSERVE_SECRET_KEY", "0123456789abcdef0123456789abcdef")
	db := runsFixture(t)
	ctx := context.Background()

	recv := &fakeS3{failFirst: 1}
	srv := httptest.NewServer(recv.handler())
	defer srv.Close()

	svc := newRunsService(db, recv, srv.URL)
	e, err := svc.Create(ctx, CreateInput{
		Name: "retrytest", SQL: "SELECT 1 AS n", Cron: "@daily", Format: "ndjson",
		Destination: S3Destination{
			Region: "us-east-1", Bucket: "obs",
			Endpoint: srv.URL, ForcePathStyle: true, AccessKeyID: "k", SecretAccessKey: "s",
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.SQL().Exec(context.Background(), `DELETE FROM scheduled_exports WHERE export_id = $1`, e.ExportID)
		_, _ = db.SQL().Exec(context.Background(), `DELETE FROM scheduled_export_runs WHERE export_id = $1`, e.ExportID)
	})

	// Drain the run directly: attempt 1 fails against the 500ing receiver,
	// the failure is recorded on the row with a due-now retry.
	runID, err := svc.enqueueRun(ctx, e.ExportID, "manual")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := svc.DrainRun(ctx, runID); err == nil {
		t.Fatalf("first drain reported success against a failing receiver")
	}
	row, err := svc.runByID(ctx, runID)
	if err != nil || row == nil {
		t.Fatalf("read run: %v %v", err, row)
	}
	if row.Attempts != 1 || row.FinishedAt != 0 || row.NextAttemptAt < 0 {
		t.Fatalf("after first failure: attempts=%d finished=%d next=%d, want 1/0/due", row.Attempts, row.FinishedAt, row.NextAttemptAt)
	}
	if row.LastError == "" {
		t.Fatalf("failure kept no last_error")
	}

	// Retry (the drain-due pass a later tick / restart would run): the SAME
	// run finishes, writing the SAME object key again.
	if err := svc.DrainDue(ctx); err != nil {
		t.Fatalf("drain due: %v", err)
	}
	row, err = svc.runByID(ctx, runID)
	if err != nil || row == nil {
		t.Fatalf("read run after retry: %v %v", err, row)
	}
	if row.FinishedAt == 0 {
		t.Fatalf("run not finished after the successful retry: %+v", row)
	}
	if row.Attempts != 1 {
		t.Fatalf("successful retry bumped attempts to %d, want 1", row.Attempts)
	}
	if !strings.HasPrefix(row.LastError, "sha256:") {
		t.Fatalf("finished run's last_error = %q, want the body digest record", row.LastError)
	}

	data := recv.dataPuts()
	if len(data) != 2 {
		t.Fatalf("data PUTs = %d, want 2 (failed attempt + successful retry, same key)", len(data))
	}
	if data[0].key != data[1].key {
		t.Fatalf("retry wrote a different key: %q vs %q - the run id must key the object", data[0].key, data[1].key)
	}
	if !strings.Contains(data[0].key, runID) {
		t.Fatalf("object key %q does not carry the run id %q", data[0].key, runID)
	}

	// The manifest sidecar exists, carries the run identity, and its digest
	// matches the uploaded body.
	manifests := recv.manifestPuts()
	if len(manifests) != 1 {
		t.Fatalf("manifest PUTs = %d, want 1 (only on success)", len(manifests))
	}
	var m exportManifest
	if err := json.Unmarshal([]byte(manifests[0].body), &m); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if m.RunID != runID || m.ExportID != e.ExportID || m.Rows != 1 || len(m.Columns) != 1 || m.Columns[0] != "n" {
		t.Fatalf("manifest = %+v, want run/export identity + one row/column", m)
	}
	// The manifest's digest must match the successfully uploaded body.
	bodySum := sha256.Sum256([]byte(data[1].body))
	if m.BodySHA256 != hex.EncodeToString(bodySum[:]) {
		t.Fatalf("manifest digest %q does not match the uploaded body %q", m.BodySHA256, hex.EncodeToString(bodySum[:]))
	}

	// A fresh service instance (the restarted process) reads the same
	// history - the ledger is the table, not memory.
	fresh := newRunsService(db, recv, srv.URL)
	runs, err := fresh.ListRuns(ctx, e.ExportID, 10)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs) != 1 || runs[0].RunID != runID || runs[0].FinishedAt == 0 {
		t.Fatalf("restart view = %+v, want one finished run", runs)
	}

	// The legacy summary mirrored for existing readers.
	sum, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, x := range sum {
		if x.ExportID == e.ExportID && (x.LastStatus != "ok" || x.LastRows != 1) {
			t.Fatalf("mirrored summary = status %q rows %d, want ok/1", x.LastStatus, x.LastRows)
		}
	}
}

// TestO14ExportRunDeadLetter: exhausting the attempt budget writes the
// durable dead-letter sentinel (-1); a later drain pass - even with a
// LARGER budget - never retries it, and the last_error stays inspectable.
func TestO14ExportRunDeadLetter(t *testing.T) {
	t.Setenv("OBSERVE_SECRET_KEY", "0123456789abcdef0123456789abcdef")
	db := runsFixture(t)
	ctx := context.Background()

	recv := &fakeS3{failFirst: 999}
	srv := httptest.NewServer(recv.handler())
	defer srv.Close()

	svc := newRunsService(db, recv, srv.URL)
	svc.maxAttempts = 2
	e, err := svc.Create(ctx, CreateInput{
		Name: "deadtest", SQL: "SELECT 1", Cron: "@daily",
		Destination: S3Destination{
			Region: "us-east-1", Bucket: "obs",
			Endpoint: srv.URL, ForcePathStyle: true, AccessKeyID: "k", SecretAccessKey: "s",
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.SQL().Exec(context.Background(), `DELETE FROM scheduled_exports WHERE export_id = $1`, e.ExportID)
		_, _ = db.SQL().Exec(context.Background(), `DELETE FROM scheduled_export_runs WHERE export_id = $1`, e.ExportID)
	})

	runID, err := svc.enqueueRun(ctx, e.ExportID, "manual")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := svc.DrainRun(ctx, runID); err == nil {
			t.Fatalf("drain %d succeeded against an always-failing receiver", i)
		}
	}
	row, _ := svc.runByID(ctx, runID)
	if row == nil || row.NextAttemptAt != -1 || row.Attempts != 2 {
		t.Fatalf("dead letter = %+v, want next=-1 attempts=2", row)
	}
	if row.LastError == "" {
		t.Fatalf("dead letter kept no last_error")
	}

	// A restarted process with a BIGGER budget must not resurrect it: dead
	// is a durable row state, not a property of the current worker's config.
	fresh := newRunsService(db, recv, srv.URL)
	fresh.maxAttempts = 10
	if err := fresh.DrainDue(ctx); err != nil {
		t.Fatalf("drain due: %v", err)
	}
	after, _ := fresh.runByID(ctx, runID)
	if after.Attempts != 2 || after.NextAttemptAt != -1 {
		t.Fatalf("larger-budget process retried the dead letter: %+v", after)
	}
	if got := recv.count(); got != 2 {
		t.Fatalf("total PUTs = %d, want 2 (the budgeted attempts only)", got)
	}
}

// TestO14ExportRunFreezesInputs: the run row snapshots sql/format/dest at
// enqueue; editing the export definition afterwards cannot change what the
// already-enqueued run executes.
func TestO14ExportRunFreezesInputs(t *testing.T) {
	t.Setenv("OBSERVE_SECRET_KEY", "0123456789abcdef0123456789abcdef")
	db := runsFixture(t)
	ctx := context.Background()

	recv := &fakeS3{}
	srv := httptest.NewServer(recv.handler())
	defer srv.Close()

	svc := newRunsService(db, recv, srv.URL)
	e, err := svc.Create(ctx, CreateInput{
		Name: "freeze", SQL: "SELECT 111 AS v", Cron: "@daily",
		Destination: S3Destination{
			Region: "us-east-1", Bucket: "obs",
			Endpoint: srv.URL, ForcePathStyle: true, AccessKeyID: "k", SecretAccessKey: "s",
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.SQL().Exec(context.Background(), `DELETE FROM scheduled_exports WHERE export_id = $1`, e.ExportID)
		_, _ = db.SQL().Exec(context.Background(), `DELETE FROM scheduled_export_runs WHERE export_id = $1`, e.ExportID)
	})

	runID, err := svc.enqueueRun(ctx, e.ExportID, "cron")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Redefine the export AFTER the run is enqueued (a new row version in
	// the plain-mergetree append table).
	_, err = db.SQL().Exec(ctx,
		`INSERT INTO scheduled_exports
		 (export_id, tenant_id, name, sql, format, cron, destination_type, destination_cfg,
		  enabled, last_run_at, last_status, last_error, last_rows, created_at, updated_at)
		 SELECT export_id, tenant_id, name, 'SELECT 222 AS v', format, cron, destination_type, destination_cfg,
		        enabled, last_run_at, last_status, last_error, last_rows, created_at, $2
		 FROM scheduled_exports WHERE export_id = $1 ORDER BY updated_at DESC LIMIT 1`,
		e.ExportID, fmt.Sprintf("%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("redefine: %v", err)
	}

	if err := svc.DrainRun(ctx, runID); err != nil {
		t.Fatalf("drain: %v", err)
	}
	data := recv.dataPuts()
	if len(data) == 0 || !strings.Contains(data[len(data)-1].body, "111") {
		t.Fatalf("run executed a query it was not enqueued with: %q", data[len(data)-1].body)
	}
}

// TestO14ExportDestinationScoping: create-time destination validation
// (region/bucket required; explicit endpoint must be clean http(s) with no
// userinfo).
func TestO14ExportDestinationScoping(t *testing.T) {
	cases := []struct {
		name string
		dest S3Destination
		want bool // want accepted
	}{
		{"aws minimal", S3Destination{Region: "us-east-1", Bucket: "b"}, true},
		{"r2 endpoint", S3Destination{Region: "auto", Bucket: "b", Endpoint: "https://abc.r2.cloudflarestorage.com"}, true},
		{"minio http lan", S3Destination{Region: "us-east-1", Bucket: "b", Endpoint: "http://10.0.0.5:9000"}, true},
		{"missing region", S3Destination{Bucket: "b"}, false},
		{"missing bucket", S3Destination{Region: "us-east-1"}, false},
		{"ftp endpoint", S3Destination{Region: "r", Bucket: "b", Endpoint: "ftp://x"}, false},
		{"userinfo endpoint", S3Destination{Region: "r", Bucket: "b", Endpoint: "https://key:secret@host"}, false},
		{"hostless endpoint", S3Destination{Region: "r", Bucket: "b", Endpoint: "https://"}, false},
	}
	for _, tc := range cases {
		err := tc.dest.validate()
		if (err == nil) != tc.want {
			t.Fatalf("%s: validate = %v, want accepted=%v", tc.name, err, tc.want)
		}
	}
}
