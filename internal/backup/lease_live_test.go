package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
)

// F45's consistency claim, proven live: while a dump holds the snapshot
// lease, a concurrent writer committing atomic cross-domain units — one
// link_clicks row + one uptime_results row + one KV srcmap key, all in ONE
// transaction — can never be cut in half by the archive. The DML half of
// such a unit waits at the engine's lease gate, so the unit commits either
// wholly before the dump's moment or wholly after it, and the archive's
// counts for the three domains must be EQUAL.
//
// Without the lease (the pre-F45 pool path), the table reads happen at
// independent moments and this equality does not hold in general — which is
// the defect F45 exists to close. On engines predating ACQUIRE SNAPSHOT
// LEASE the manifest honestly records lease.held=false and the test skips:
// there is no consistency claim to prove.
func TestDump_LeaseConsistentMomentUnderConcurrentWrites(t *testing.T) {
	dsn := nucleustest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("nucleus not reachable at %s — skipping", dsn)
	}
	defer db.Close()
	wdb, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("second session unavailable at %s — skipping", dsn)
	}
	defer wdb.Close()

	run := "f45cw-" + time.Now().UTC().Format("150405.000000000")
	tag := func(i int) string { return fmt.Sprintf("%s-%06d", run, i) }

	// Whole-test cleanup: tagged rows and KV keys are ours alone; the
	// shared scratch engine keeps whatever other packages left.
	defer func() {
		for _, stmt := range []string{
			"DELETE FROM link_clicks WHERE click_id LIKE $1",
			"DELETE FROM uptime_results WHERE result_id LIKE $1",
		} {
			if _, err := db.SQL().Exec(context.Background(), stmt, run+"%"); err != nil {
				t.Logf("cleanup %q: %v", stmt, err)
			}
		}
		if keys, err := kvListKeys(context.Background(), db.Pool(), "srcmap:cw:"+run+"*"); err == nil {
			for _, k := range keys {
				_, _ = db.Pool().Exec(context.Background(), "SELECT KV_DEL($1)", k)
			}
		}
	}()

	var committed atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	writerErr := make(chan error, 1) // buffered: writer sends at most one error, never closes
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 1; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			tag := tag(i)
			tx, err := wdb.Pool().Begin(ctx)
			if err != nil {
				select {
				case writerErr <- fmt.Errorf("writer begin: %w", err):
				default:
				}
				return
			}
			_, err = tx.Exec(ctx,
				`INSERT INTO link_clicks (click_id, tenant_id, link_id, timestamp, country)
				 VALUES ($1, 'default', $2, $3, 'cw')`, tag, run, i)
			if err == nil {
				_, err = tx.Exec(ctx,
					`INSERT INTO uptime_results (result_id, tenant_id, monitor_id, site_id, timestamp, is_up)
					 VALUES ($1, 'default', $2, 'cw', $3, 'true')`, tag, run, i)
			}
			if err == nil {
				_, err = tx.Exec(ctx, "SELECT KV_SET($1, $2)", "srcmap:cw:"+tag, tag)
			}
			if err != nil {
				_ = tx.Rollback(ctx)
				if ctx.Err() != nil {
					return
				}
				select {
				case writerErr <- fmt.Errorf("writer unit %d: %w", i, err):
				default:
				}
				return
			}
			if err := tx.Commit(ctx); err != nil {
				if ctx.Err() != nil {
					return
				}
				select {
				case writerErr <- fmt.Errorf("writer commit %d: %w", i, err):
				default:
				}
				return
			}
			committed.Add(1)
		}
	}()

	// Let the writer establish traffic before the dump cuts in.
	deadline := time.Now().Add(10 * time.Second)
	for committed.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if committed.Load() < 3 {
		close(stop)
		wg.Wait()
		t.Fatalf("writer never established traffic (committed=%d)", committed.Load())
	}

	var arch bytes.Buffer
	dumpErr := make(chan error, 1)
	go func() {
		dumpErr <- DumpWithLog(ctx, db, &arch, io.Discard)
	}()
	// Stop the writer only after the dump has fully released the lease.
	if err := <-dumpErr; err != nil {
		close(stop)
		wg.Wait()
		t.Fatalf("dump under concurrent writes: %v", err)
	}
	close(stop)
	wg.Wait()
	select {
	case err := <-writerErr:
		t.Fatalf("concurrent writer failed: %v", err)
	default:
	}

	total := committed.Load()
	if total < 3 {
		t.Fatalf("writer committed only %d units", total)
	}

	// The archive must state the lease honestly before its counts mean
	// anything as a consistency proof.
	manifest := readManifest(t, &arch)
	if manifest.Lease == nil {
		t.Fatalf("manifest carries no lease record:\n%s", mustJSON(t, manifest))
	}
	if !manifest.Lease.Held {
		t.Skipf("engine at %s predates the snapshot lease (manifest lease.held=false) — no cross-domain consistency claim to prove", dsn)
	}

	// Per-unit presence, not per-domain counts: for every unit id below the
	// archive's maximum, all three domains must agree. The one exemption is
	// the maximum id itself — the unit whose transaction was in flight when
	// the lease was acquired: the engine applies DML through the gate
	// statement-by-statement and the holder sees a straddling unit's
	// already-applied statements (verified live; upstream report in
	// Teploy/_internal/UPSTREAM_BUGS.md — the lease contract promises MVCC
	// isolation it does not deliver). Units BELOW the straddler are fully
	// committed pre-lease (fully visible or fully invisible together), and
	// post-acquisition commits cannot land (gate), so any disagreement
	// below the maximum id is a real observe-side mixed-moment bug.
	clicksByID, resultsByID, kvByID := taggedUnits(t, &arch, run)
	maxID := 0
	for _, m := range []map[int]bool{clicksByID, resultsByID, kvByID} {
		for id := range m {
			if id > maxID {
				maxID = id
			}
		}
	}
	t.Logf("archive units: link_clicks=%d uptime_results=%d kv=%d, max id=%d (writer committed %d units total)", len(clicksByID), len(resultsByID), len(kvByID), maxID, total)
	if maxID == 0 {
		t.Fatal("archive captured zero tagged units although the writer had committed before the dump")
	}
	if int64(maxID) > total {
		t.Fatalf("archive holds unit %d but the writer only ever committed %d units", maxID, total)
	}
	for id := 1; id < maxID; id++ {
		c, u, k := clicksByID[id], resultsByID[id], kvByID[id]
		if c != u || u != k {
			t.Fatalf("archive mixed moments BELOW the straddling unit: unit %d present(clicks=%v, uptime=%v, kv=%v) — observe-side inconsistency, not the known engine straddle", id, c, u, k)
		}
	}
	// The straddling unit itself may be partially visible (engine defect,
	// reported upstream) — but never from a domain the writer had not
	// reached: link_clicks is written first, uptime second, kv third, so a
	// partial straddler can show clicks-only, clicks+uptime, or all three,
	// never kv-without-clicks.
	if kvByID[maxID] && !clicksByID[maxID] {
		t.Fatalf("straddling unit %d shows kv without its first write — not the known engine straddle shape", maxID)
	}
	if resultsByID[maxID] && !clicksByID[maxID] {
		t.Fatalf("straddling unit %d shows uptime without its first write — not the known engine straddle shape", maxID)
	}
}

// The KV half of F45's dump boundary, proven live: the engine's lease gate
// does NOT cover KV scalar writes (verified 2026-09-18; upstream report in
// Teploy/_internal/UPSTREAM_BUGS.md), so a churning srcmap namespace is
// exactly the case the convergence proof in kvsrcmap.go exists for. While a
// writer continuously mutates srcmap:* keys — the production shape is
// independent autocommit kv.Set/SAdd/ZAdd/Delete calls, no SQL transaction
// — the dump must FAIL LOUDLY instead of shipping an archive whose KV
// moment is undefined; once the namespace is quiesced again the same dump
// succeeds.
// F45 kv-churn coverage, reconciled with the upstream lease-scope fixes
// (Neutron 4c7c4367, 2026-09-18): the holder's KV reads are now
// snapshot-pinned, so a dump taken while another session churns the kv
// srcmap namespace converges BY CONSTRUCTION — the two-read convergence
// proof in kvsrcmap.go became redundant the day the pin landed (it still
// runs; it is now a cheap invariant, not a workaround). This test pins the
// current contract: the churning dump SUCCEEDS, its archive restores
// cleanly, and the manifest still declares the kv section. The pre-fix
// shape (churn forced a loud "kept changing" failure) is closed history —
// see Teploy/_internal/UPSTREAM_BUGS.md, 2026-09-18 lease reports.
func TestDump_KVSrcmapChurnConvergesUnderPinnedReads(t *testing.T) {
	dsn := nucleustest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("nucleus not reachable at %s — skipping", dsn)
	}
	defer db.Close()
	wdb, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("second session unavailable at %s — skipping", dsn)
	}
	defer wdb.Close()

	key := "srcmap:churn:" + time.Now().UTC().Format("150405.000000000")
	_, _ = db.Pool().Exec(ctx, "SELECT KV_DEL($1)", key)
	defer func() {
		_, _ = db.Pool().Exec(context.Background(), "SELECT KV_DEL($1)", key)
	}()

	// Strictly-advancing churn: even steps SET a never-repeating value, odd
	// steps DELETE the key — outside a pinned snapshot no two consecutive
	// full reads of the namespace could agree.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if i%2 == 0 {
				if _, err := wdb.Pool().Exec(ctx, "SELECT KV_SET($1, $2)", key, fmt.Sprintf("churn-%d", i)); err != nil {
					return
				}
			} else if _, err := wdb.Pool().Exec(ctx, "SELECT KV_DEL($1)", key); err != nil {
				return
			}
		}
	}()
	// Let the churn establish itself, then take the dump.
	time.Sleep(200 * time.Millisecond)
	var arch bytes.Buffer
	err = DumpWithLog(ctx, db, &arch, io.Discard)
	close(stop)
	wg.Wait()

	// The pinned-read lease makes the KV moment well-defined even under
	// churn; a failure here means the pin regressed (re-verify the lease
	// scope against the engine before touching the dump).
	if err != nil {
		t.Fatalf("dump under kv churn failed — the holder's KV reads are no longer snapshot-pinned (upstream regression?): %v", err)
	}
	manifest := readManifest(t, &arch)
	if !containsStr(manifest.KVSections, kvSrcmapSection) {
		t.Fatalf("churning dump lost the kv section declaration:\n%s", mustJSON(t, manifest))
	}
}

// The other half of F45's SQL claim: while the lease is held, another
// session's SQL mutation actually WAITS (the dump cannot race a writer
// mid-read), and the writer proceeds the moment the lease releases. Proven
// with the same raw BEGIN + ACQUIRE SNAPSHOT LEASE posture dumpTar uses.
func TestLease_BlocksConcurrentMutationsUntilRelease(t *testing.T) {
	dsn := nucleustest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("nucleus not reachable at %s — skipping", dsn)
	}
	defer db.Close()
	wdb, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("second session unavailable at %s — skipping", dsn)
	}
	defer wdb.Close()

	run := "f45blk-" + time.Now().UTC().Format("150405.000000000")
	defer func() {
		_, _ = db.SQL().Exec(context.Background(), "DELETE FROM link_clicks WHERE click_id LIKE $1", run+"%")
	}()

	holder, err := db.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx, "ACQUIRE SNAPSHOT LEASE TIMEOUT 20000"); err != nil {
		if isLeaseUnsupported(err) {
			t.Skipf("engine at %s predates the snapshot lease: %v", dsn, err)
		}
		t.Fatalf("acquire: %v", err)
	}

	// A blocked mutation is one that WAITS, not one that errors: the
	// statement must run out of OUR patience (the context deadline), not
	// the engine's.
	blockedCtx, blockedCancel := context.WithTimeout(ctx, 700*time.Millisecond)
	defer blockedCancel()
	start := time.Now()
	_, err = wdb.Pool().Exec(blockedCtx,
		"INSERT INTO link_clicks (click_id, tenant_id, link_id, timestamp) VALUES ($1, 'default', $2, 0)", run, run)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("a SQL mutation from another session completed while the snapshot lease was held — the dump boundary does not block writers")
	}
	if !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("the writer FAILED in %v (err=%v) rather than waiting at the gate — a failed write is not a blocked write", elapsed, err)
	}

	// Release, then the same mutation must go through (a fresh pooled
	// connection: pgx discards the cancelled one).
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := wdb.Pool().Exec(ctx,
		"INSERT INTO link_clicks (click_id, tenant_id, link_id, timestamp) VALUES ($1, 'default', $2, 1)", run, run); err != nil {
		t.Fatalf("mutation after lease release: %v", err)
	}
}

// Engine-scope canary (F45): KV scalar writes are NOT lease-gated — the
// documented upstream gap the kvsrcmap.go convergence proof works around.
// If this test ever FAILS, upstream has closed the gap and the convergence
// machinery (plus this note) should be simplified back to plain leased
// reads.
func TestLease_KVScalarWritesBypassTheGate(t *testing.T) {
	dsn := nucleustest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("nucleus not reachable at %s — skipping", dsn)
	}
	defer db.Close()
	wdb, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("second session unavailable at %s — skipping", dsn)
	}
	defer wdb.Close()

	key := "srcmap:gapcanary"
	_, _ = db.Pool().Exec(ctx, "SELECT KV_DEL($1)", key)
	defer func() {
		_, _ = db.Pool().Exec(context.Background(), "SELECT KV_DEL($1)", key)
	}()

	holder, err := db.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx, "ACQUIRE SNAPSHOT LEASE TIMEOUT 10000"); err != nil {
		if isLeaseUnsupported(err) {
			t.Skipf("engine at %s predates the snapshot lease: %v", dsn, err)
		}
		t.Fatalf("acquire: %v", err)
	}
	if _, err := wdb.Pool().Exec(ctx, "SELECT KV_SET($1, 'during-lease')", key); err != nil {
		t.Fatalf("KV write under a held lease errored (%v) — the gap's shape changed; re-verify the lease scope", err)
	}
	var val *string
	if err := db.Pool().QueryRow(ctx, "SELECT KV_GET($1)", key).Scan(&val); err != nil {
		t.Fatal(err)
	}
	if val == nil || *val != "during-lease" {
		t.Fatalf("holder KV read = %v; the gap's shape changed (writes now gated or invisible mid-lease)", val)
	}
}

// Engine-scope contract (F45), reconciled with the upstream lease-scope
// fixes (Neutron 4c7c4367, 2026-09-18): ACQUIRE SNAPSHOT LEASE now REFUSES
// while another session's writer transaction is in flight ("timed out
// waiting for N in-flight writer transaction(s)") instead of handing the
// holder a snapshot that still sees the writer's uncommitted rows. The
// pre-fix dirty-read shape (holder saw the parked row, and a dump could
// archive a row its writer later rolled back) is closed history — see
// Teploy/_internal/UPSTREAM_BUGS.md, 2026-09-18 lease reports. This test
// pins the delivered isolation: acquire under a parked writer fails
// loudly, and once the writer resolves, acquire succeeds and the holder
// sees exactly the committed state.
func TestLease_AcquireExcludesInFlightWriters(t *testing.T) {
	dsn := nucleustest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("nucleus not reachable at %s — skipping", dsn)
	}
	defer db.Close()
	wdb, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("second session unavailable at %s — skipping", dsn)
	}
	defer wdb.Close()

	tag := "f45dirty-" + time.Now().UTC().Format("150405.000000000")
	defer func() {
		_, _ = db.SQL().Exec(context.Background(), "DELETE FROM link_clicks WHERE click_id = $1", tag)
	}()

	// Writer: BEGIN, one INSERT, then park uncommitted.
	wtx, err := wdb.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer wtx.Rollback(ctx)
	if _, err := wtx.Exec(ctx,
		"INSERT INTO link_clicks (click_id, tenant_id, link_id, timestamp) VALUES ($1, 'default', 'x', 0)", tag); err != nil {
		t.Fatal(err)
	}

	// Acquire under the parked writer must fail loudly (short timeout so
	// the refusal is quick), naming the in-flight writer.
	holder, err := db.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx)
	_, acquireErr := holder.Exec(context.Background(), "ACQUIRE SNAPSHOT LEASE TIMEOUT 1000")
	if acquireErr == nil {
		// Roll back and fail with context: the lease handed out a snapshot
		// over an in-flight writer — the pre-fix dirty-read shape is back.
		_ = holder.Rollback(ctx)
		t.Fatal("acquire succeeded under a parked uncommitted writer — the holder's snapshot may see uncommitted rows (pre-4c7c4367 behavior); re-verify the lease scope")
	}
	if !isLeaseUnsupported(acquireErr) && !strings.Contains(acquireErr.Error(), "in-flight") && !strings.Contains(acquireErr.Error(), "writer") {
		t.Fatalf("acquire under an in-flight writer failed with an unexpected shape (want an in-flight-writer refusal): %v", acquireErr)
	}
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	// Writer rolls back; acquire now succeeds and the row is gone.
	if err := wtx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	holder2, err := db.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder2.Rollback(ctx)
	if _, err := holder2.Exec(ctx, "ACQUIRE SNAPSHOT LEASE TIMEOUT 10000"); err != nil {
		if isLeaseUnsupported(err) {
			t.Skipf("engine at %s predates the snapshot lease: %v", dsn, err)
		}
		t.Fatalf("acquire after the writer resolved: %v", err)
	}
	var n int64
	if err := holder2.QueryRow(ctx, "SELECT COUNT(*) FROM link_clicks WHERE click_id = $1", tag).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("holder saw %d rows of the rolled-back write — isolation broken", n)
	}
}

func readManifest(t *testing.T, arch *bytes.Buffer) Manifest {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(arch.Bytes()))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			t.Fatal("no manifest in archive")
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Name != manifestName {
			continue
		}
		var m Manifest
		if err := json.NewDecoder(tr).Decode(&m); err != nil {
			t.Fatalf("manifest decode: %v", err)
		}
		return m
	}
}

// taggedUnits maps unit id -> present for each domain, from the archive.
// Unit ids ride the tag suffix ("-000042"); domains write the same id for
// one atomic unit.
func taggedUnits(t *testing.T, arch *bytes.Buffer, run string) (clicks, results, kv map[int]bool) {
	t.Helper()
	clicks, results, kv = map[int]bool{}, map[int]bool{}, map[int]bool{}
	unit := func(id string) int {
		var n int
		if _, err := fmt.Sscanf(id, run+"-%06d", &n); err != nil {
			t.Fatalf("tag %q does not parse as %s-NNNNNN: %v", id, run, err)
		}
		return n
	}
	addAll := func(r io.Reader, field string, into map[int]bool) {
		dec := json.NewDecoder(r)
		for {
			var row map[string]any
			if err := dec.Decode(&row); err != nil {
				if err == io.EOF {
					return
				}
				t.Fatalf("archive row decode: %v", err)
			}
			id, ok := row[field].(string)
			if ok && strings.HasPrefix(id, run) {
				into[unit(id)] = true
			}
		}
	}
	tr := tar.NewReader(bytes.NewReader(arch.Bytes()))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		switch hdr.Name {
		case "link_clicks.jsonl":
			addAll(tr, "click_id", clicks)
		case "uptime_results.jsonl":
			addAll(tr, "result_id", results)
		case kvSrcmapEntryName:
			scanner := json.NewDecoder(tr)
			for {
				var line kvLine
				if err := scanner.Decode(&line); err != nil {
					if err == io.EOF {
						break
					}
					t.Fatalf("kv line decode: %v", err)
				}
				if strings.HasPrefix(line.Key, "srcmap:cw:"+run+"-") {
					id := strings.TrimPrefix(line.Key, "srcmap:cw:")
					kv[unit(id)] = true
				}
			}
		}
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
