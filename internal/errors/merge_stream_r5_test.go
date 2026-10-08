package errors

import (
	"context"
	stderrors "errors"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/neutron-build/neutron/go/nucleus"
	"github.com/useteploy/teploy-observe/internal/schema"
)

// Required native runs fail on unavailable fixtures; optional local runs skip
// only when no URL was supplied. No SQLite substitute or automatic engine start.
func r5Native(t *testing.T) (*nucleus.Client, string) {
	t.Helper()
	dsn := os.Getenv("OBSERVE_NUCLEUS_URL")
	if dsn == "" {
		if os.Getenv("OBSERVE_REQUIRE_NUCLEUS") == "1" {
			t.Fatal("required isolated native fixture unavailable")
		}
		t.Skip("no OBSERVE_NUCLEUS_URL; native acceptance unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if !db.IsNucleus() {
		db.Close()
		t.Fatal("fixture is not Nucleus")
	}
	if err := schema.Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	return db, dsn
}

type r5MergeRows struct {
	pgx.Rows
	values           [][3]string
	pos              int
	scanErr, tailErr error
	closed           bool
}

func (r *r5MergeRows) Next() bool { r.pos++; return r.pos <= len(r.values) }
func (r *r5MergeRows) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	for i, v := range r.values[r.pos-1] {
		*dest[i].(*string) = v
	}
	return nil
}
func (r *r5MergeRows) Err() error { return r.tailErr }
func (r *r5MergeRows) Close()     { r.closed = true }

func TestR5MergeStreamFaultNeverReturnsPartialMappings(t *testing.T) {
	fault := stderrors.New("stream fault")
	for _, mode := range []string{"scan", "tail", "tenant-collision", "complete"} {
		t.Run(mode, func(t *testing.T) {
			rows := &r5MergeRows{values: [][3]string{{"default", "a", "target"}, {"default", "b", "target"}}}
			switch mode {
			case "scan":
				rows.scanErr = fault
			case "tail":
				rows.tailErr = fault
			case "tenant-collision":
				rows.values[1] = [3]string{"other", "a", "different"}
			}
			got, err := readMergeMappings(rows)
			if !rows.closed {
				t.Fatal("stream not closed")
			}
			if mode == "complete" {
				if err != nil || len(got) != 2 {
					t.Fatalf("complete: %v %v", got, err)
				}
			} else if err == nil || got != nil {
				t.Fatalf("fault published partial mapping: %v %v", got, err)
			}
		})
	}
}

// Seeds historical accepted rows directly, then admits a new merge through the
// actual service. Tests the original SQL cap, not merely an in-memory scope.
func TestR5NativeMergeBelowAtAboveOldBoundAndUnmerge(t *testing.T) {
	db, dsn := r5Native(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for _, n := range []int{9999, 10000, 10001} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			site := "r5-merge-" + uniqueToken()
			const target = "target"
			source := func(i int) string { return fmt.Sprintf("source-%05d", i) }
			exec := func(q string, args ...any) {
				t.Helper()
				if _, err := db.SQL().Exec(ctx, q, args...); err != nil {
					t.Fatal(err)
				}
			}
			// Populate 10,001 sources in bounded statements, without O(n^2) admissions.
			for base := 0; base < n; base += 200 {
				var mergeVals, eventVals, issueVals []string
				end := base + 200
				if end > n {
					end = n
				}
				for i := base; i < end; i++ {
					id := source(i)
					mergeVals = append(mergeVals, fmt.Sprintf("('default','%s','%s','target','true',1)", site, id))
					eventVals = append(eventVals, fmt.Sprintf("('e-%s-%d','default','%s','%s','%s','hash',%d,'T','boom','error','r1','%s')", site, i, site, id, id, i+1000, id))
					issueVals = append(issueVals, fmt.Sprintf("('%s','default','%s','%s','T','error','open',1,1,1)", id, site, id))
				}
				exec("INSERT INTO issue_merges (tenant_id,site_id,source_issue_id,target_issue_id,active,version) VALUES " + strings.Join(mergeVals, ","))
				exec("INSERT INTO error_events (error_id,tenant_id,site_id,session_id,issue_id,group_hash,timestamp,error_type,error_value,level,release_tag,distinct_id) VALUES " + strings.Join(eventVals, ","))
				exec("INSERT INTO issues (issue_id,tenant_id,site_id,group_hash,title,level,status,first_seen,last_seen,version) VALUES " + strings.Join(issueVals, ","))
			}
			for _, id := range []string{target, "additional"} {
				exec(`INSERT INTO issues (issue_id,tenant_id,site_id,group_hash,title,level,status,first_seen,last_seen,version) VALUES ($1,'default',$2,$1,'T','error','open',1,999999,1)`, id, site)
			}
			svc := NewIssueService(db)
			clock := time.Now()
			svc.merge.now = func() time.Time { return clock }
			check := func(want int) {
				t.Helper()
				mappings, err := svc.merge.store.activeMerges(ctx, site)
				if err != nil || len(mappings) != want {
					t.Fatalf("SQL active mappings=%d want=%d err=%v", len(mappings), want, err)
				}
				ids := svc.issueScope(ctx, site, target)
				expected := []string{target}
				if want == n+1 {
					expected = append(expected, "additional")
				}
				for i := 0; i < n; i++ {
					expected = append(expected, source(i))
				}
				if !reflect.DeepEqual(ids, expected) {
					t.Fatalf("unstable/incomplete SQL scope: got=%d want=%d", len(ids), len(expected))
				}

				got, err := svc.GetIssue(ctx, source(n-1), site)
				if err != nil || got == nil || got.IssueID != target || got.EventCount != int64(n) || got.UserCount != int64(n) || len(got.MergedSources) != want || len(got.Releases) != 1 || got.Releases[0].EventCount != int64(n) {
					t.Fatalf("detail/count/users/releases: %+v err=%v", got, err)
				}
				events, err := svc.LatestEvents(ctx, target, site, n+1)
				if err != nil || len(events) != n {
					t.Fatalf("latest events=%d want=%d err=%v", len(events), n, err)
				}
				listed, err := svc.ListIssues(ctx, site, "open", n+10, 0)
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, issue := range listed {
					if issue.IssueID == target {
						found = true
						if issue.EventCount != int64(n) || len(issue.MergedSources) != want {
							t.Fatalf("list scope: %+v", issue)
						}
					} else if _, hidden := mappings[issue.IssueID]; hidden {
						t.Fatal("accepted source reappeared")
					}
				}
				if !found {
					t.Fatal("target absent from list")
				}
				if count := svc.lifecycle().scopeCount(ctx, site, ids); count != int64(n) {
					t.Fatalf("lifecycle lost scope: %d", count)
				}
			}
			check(n)
			clock = clock.Add(mergeCacheTTL + time.Second)
			check(n)
			if err := svc.MergeIssues(ctx, site, "additional", target, "r5"); err != nil {
				t.Fatal(err)
			}
			check(n + 1)
			if err := svc.UpdateStatus(ctx, target, site, "resolved", 0); err != nil {
				t.Fatal(err)
			}
			var notifications []IssueEvent
			svc.SetNotifier(func(_ context.Context, ev IssueEvent) { notifications = append(notifications, ev) })
			attributed, err := svc.ResolveIssue(ctx, site, source(n-1), "T", "", "error", "r1", 9999999)
			if err != nil || attributed != target || len(notifications) != 1 || notifications[0].IssueID != target || notifications[0].EventCount != int64(n+1) {
				t.Fatalf("wide source lifecycle attribution: %s %+v %v", attributed, notifications, err)
			}
			// Unmerge changes lifecycle scope immediately, without waiting count TTL.
			if err := svc.UnmergeIssue(ctx, site, source(n-1), "r5"); err != nil {
				t.Fatal(err)
			}
			if count := svc.mergedCount(ctx, svc.lifecycle(), site, target); count != int64(n-1) {
				t.Fatalf("unmerge cached old count: %d", count)
			}
			got, err := svc.GetIssue(ctx, target, site)
			if err != nil || got == nil || got.EventCount != int64(n-1) || got.UserCount != int64(n-1) || len(got.MergedSources) != n {
				t.Fatalf("unmerge detail: %+v %v", got, err)
			}
			restored, err := svc.GetIssue(ctx, source(n-1), site)
			if err != nil || restored == nil || restored.IssueID != source(n-1) || restored.EventCount != 1 {
				t.Fatalf("restored source: %+v %v", restored, err)
			}
			// Another service/process cache refresh reads the complete SQL state.
			other, err := nucleus.Connect(ctx, dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			fresh := NewIssueService(other)
			if ids := fresh.issueScope(ctx, site, target); len(ids) != n+1 {
				t.Fatalf("fresh scope=%d", len(ids))
			}
		})
	}
}

// Simulate an invalidation while an older read is in flight without scheduling
// another worker. A pre-write snapshot can serve its reader, but cannot seed
// the next cache TTL after that write.
type r5InvalidatingMergeStore struct {
	*fakeMergeStore
	afterRead func()
}

func (f *r5InvalidatingMergeStore) activeMerges(ctx context.Context, site string) (map[string]string, error) {
	result, err := f.fakeMergeStore.activeMerges(ctx, site)
	if f.afterRead != nil {
		once := f.afterRead
		f.afterRead = nil
		once()
	}
	return result, err
}
func TestR5MergeInvalidatedReadCannotReseedCache(t *testing.T) {
	f := newFakeStore(map[string][]string{"A": {"src", "tgt"}})
	f.merges["A"] = map[string]string{"src": "tgt"}
	store := &r5InvalidatingMergeStore{fakeMergeStore: f}
	m := newMergeState(store)
	store.afterRead = func() {
		if err := f.putMerge(context.Background(), "A", "src", "tgt", false, ""); err != nil {
			t.Fatal(err)
		}
		m.invalidate("A")
	}
	if got := m.siteMerges(context.Background(), "A"); len(got) != 1 {
		t.Fatal("fixture lacks old in-flight snapshot")
	}
	if got := m.siteMerges(context.Background(), "A"); len(got) != 0 {
		t.Fatal("invalidated old snapshot reseeded cache")
	}
}
func TestR5MergeScopeChangeInvalidatesLifecycleCountBeforeTTL(t *testing.T) {
	lc := newFakeLifecycle()
	svc, _ := mergedSvc(t, lc, map[string]string{"src": "tgt"})
	svc.mergedCount(context.Background(), lc, "A", "tgt")
	if err := svc.UnmergeIssue(context.Background(), "A", "src", ""); err != nil {
		t.Fatal(err)
	}
	svc.mergedCount(context.Background(), lc, "A", "tgt")
	if lc.counts != 2 {
		t.Fatalf("membership change reused old lifecycle count: calls=%d", lc.counts)
	}
}
