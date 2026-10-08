package errors

import (
	"context"
	stderrors "errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"
)

func r5SearchEvent(t *testing.T, db *nucleus.Client, site, id, issue, term string, ts int) {
	t.Helper()
	_, err := db.SQL().Exec(context.Background(), `INSERT INTO error_events (error_id,tenant_id,site_id,session_id,issue_id,group_hash,timestamp,error_type,error_value,level) VALUES ($1,'default',$2,'session',$3,'hash',CAST($4 AS BIGINT),'R5',$5,'error')`, id, site, issue, strconv.Itoa(ts), term)
	if err != nil {
		t.Fatal(err)
	}
}
func r5LegacyDocument(t *testing.T, db *nucleus.Client, site, id, text string) int64 {
	t.Helper()
	ctx := context.Background()
	doc, err := db.KV().Incr(ctx, "fts:error:seq")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.KV().Set(ctx, fmt.Sprintf("fts:error:%d", doc), []byte(id)); err != nil {
		t.Fatal(err)
	}
	ok, err := db.FTS().IndexFaceted(ctx, doc, text, "site_id", site)
	if err != nil || !ok {
		t.Fatalf("legacy index: %v %v", ok, err)
	}
	return doc
}
func r5ReopenSearch(t *testing.T, db *nucleus.Client, dsn string) *nucleus.Client {
	t.Helper()
	db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fresh, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fresh.Close() })
	return fresh
}

// Actual KV/FTS effects are retained across new clients/services. These are
// graceful connection reopens, not engine crashes or power-loss proof. Hooks
// stop production paths at storage boundaries; no mock index supplies survivors.
func TestR5NativeCanonicalSurvivesIdentityIndexRemovalAndCursorFaults(t *testing.T) {
	_, dsn := r5Native(t)
	fault := stderrors.New("r5 boundary fault")
	for _, allocationFault := range []string{"forward", "reverse", "index"} {
		for _, adoptionFault := range []string{"reverse", "index", "remove", "cursor"} {
			t.Run(allocationFault+"-"+adoptionFault, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				db, err := nucleus.Connect(ctx, dsn)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { db.Close() })
				site, term := "r5-fts-"+uniqueToken(), "r5survivorterm"+uniqueToken()
				id := randomID()
				r5SearchEvent(t, db, site, id, "issue", term, 1000)
				r5LegacyDocument(t, db, site, id, term)
				// When the first forward write refuses, two old documents exercise the
				// later removal boundary. Reverse/index failures use the EXACT last-old-
				// document schedule from SR4-02 (new identity, no canonical index).
				if allocationFault == "forward" {
					r5LegacyDocument(t, db, site, id, term)
				}
				svc := NewSearchService(db)
				svc.before = func(stage string) error {
					if stage == allocationFault {
						return fault
					}
					return nil
				}
				if err := svc.IndexError(ctx, site, id, "R5", term); !stderrors.Is(err, fault) {
					t.Fatalf("allocation fault not reached: %v", err)
				}
				canonicalBefore, err := db.KV().Get(ctx, eventDocumentKey(site, id))
				if err != nil {
					t.Fatal(err)
				}
				if allocationFault == "forward" && canonicalBefore != nil {
					t.Fatal("forward refusal published identity")
				}
				if allocationFault != "forward" && canonicalBefore == nil {
					t.Fatal("fixture did not leave interrupted canonical allocation")
				}
				db = r5ReopenSearch(t, db, dsn)
				svc = NewSearchService(db)
				cursorCalls := 0
				svc.before = func(stage string) error {
					if stage == "cursor" {
						cursorCalls++
					}
					if stage == adoptionFault && (stage != "cursor" || cursorCalls == 2) {
						return fault
					}
					return nil
				}
				if _, err := svc.ReindexAll(ctx, site, 1, false, 1, nil); !stderrors.Is(err, fault) {
					t.Fatalf("adoption fault not reached: %v", err)
				}
				for reopen := 0; reopen < 2; reopen++ {
					db = r5ReopenSearch(t, db, dsn)
					docs, err := db.FTS().SearchFilter(ctx, term, "site_id", site, nucleus.WithFTSLimit(20))
					if err != nil || len(docs) == 0 {
						t.Fatalf("last indexed survivor lost after %s/%s reopen %d: %v %v", allocationFault, adoptionFault, reopen, docs, err)
					}
					svc = NewSearchService(db)
					hits, err := svc.SearchErrors(ctx, site, term, 1)
					if err != nil || len(hits) != 1 || hits[0].ErrorID != id {
						t.Fatalf("survivor hydration: %+v %v", hits, err)
					}
				}
				// Retry must reuse the published identity and retire redundant docs only
				// after a successful canonical index. A second successful pass is stable.
				for pass := 0; pass < 2; pass++ {
					p, err := svc.ReindexAll(ctx, site, 1, false, 1, nil)
					if err != nil || p.Indexed != 1 {
						t.Fatalf("repair: %+v %v", p, err)
					}
					docs, err := db.FTS().SearchFilter(ctx, term, "site_id", site, nucleus.WithFTSLimit(20))
					if err != nil || len(docs) != 1 {
						t.Fatalf("canonical multiplicity: %+v %v", docs, err)
					}
					canonical, err := db.KV().Get(ctx, eventDocumentKey(site, id))
					if err != nil {
						t.Fatal(err)
					}
					if canonicalBefore != nil && string(canonical) != string(canonicalBefore) {
						t.Fatal("retry changed durable identity")
					}
					if strconv.FormatInt(docs[0].DocID, 10) != string(canonical) {
						t.Fatal("survivor is not canonical")
					}
				}
			})
		}
	}
}

func TestR5NativeLegacyDuplicatesDoNotSpendUniqueSiteResultBudget(t *testing.T) {
	db, _ := r5Native(t)
	ctx := context.Background()
	site, foreign, term := "r5-budget-"+uniqueToken(), "r5-foreign-"+uniqueToken(), "r5budgetterm"+uniqueToken()
	svc := NewSearchService(db)
	a, b := randomID(), randomID()
	r5SearchEvent(t, db, site, a, "issue-a", term, 1000)
	long := term + " " + strings.Repeat("padding ", 200)
	r5SearchEvent(t, db, site, b, "issue-b", long, 1001)
	for i := 0; i < 12; i++ {
		r5LegacyDocument(t, db, site, a, term)
	}
	r5LegacyDocument(t, db, site, b, long)
	for i := 0; i < 25; i++ {
		if err := svc.IndexError(ctx, foreign, fmt.Sprintf("foreign-%s-%d", foreign, i), "R5", term); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"issue-a", "issue-b"} {
		if _, err := db.SQL().Exec(ctx, `INSERT INTO issues (issue_id,tenant_id,site_id,group_hash,title,status,first_seen,last_seen,version) VALUES ($1,'default',$2,$1,'R5','open','1','1',1)`, id, site); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := db.FTS().SearchFilter(ctx, term, "site_id", site, nucleus.WithFTSLimit(2))
	if err != nil || len(raw) != 2 {
		t.Fatalf("raw budget fixture: %+v %v", raw, err)
	}
	for _, doc := range raw {
		id, err := db.KV().Get(ctx, fmt.Sprintf("fts:error:%d", doc.DocID))
		if err != nil || string(id) != a {
			t.Fatalf("fixture must crowd B out with duplicate A hits: %s %v", id, err)
		}
	}
	hits, err := svc.SearchErrors(ctx, site, term, 2)
	if err != nil || len(hits) != 2 || hits[0].ErrorID == hits[1].ErrorID || hits[0].SiteID != site || hits[1].SiteID != site {
		t.Fatalf("unique site budget: %+v %v", hits, err)
	}
	issues, err := svc.SearchIssues(ctx, site, term, 2)
	if err != nil || len(issues) != 2 || issues[0].IssueID == issues[1].IssueID {
		t.Fatalf("issue budget: %+v %v", issues, err)
	}
	// Scoped rebuild cannot remove or re-facet the foreign site's documents.
	foreignBefore, err := db.FTS().SearchFilter(ctx, term, "site_id", foreign, nucleus.WithFTSLimit(30))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReindexAll(ctx, site, 1, false, 1, nil); err != nil {
		t.Fatal(err)
	}
	foreignAfter, err := db.FTS().SearchFilter(ctx, term, "site_id", foreign, nucleus.WithFTSLimit(30))
	if err != nil || len(foreignBefore) != 25 || len(foreignAfter) != 25 {
		t.Fatalf("foreign completeness: before=%d after=%d err=%v", len(foreignBefore), len(foreignAfter), err)
	}
	docs, err := db.FTS().SearchFilter(ctx, term, "site_id", site, nucleus.WithFTSLimit(30))
	if err != nil || len(docs) != 2 {
		t.Fatalf("deduped docs: %+v %v", docs, err)
	}
}

// Multiple unique events of one issue must not hide the next matching issue.
func TestR5NativeSearchIssueBudgetRefillsAfterGrouping(t *testing.T) {
	db, _ := r5Native(t)
	ctx := context.Background()
	site, term := "r5-group-"+uniqueToken(), "r5groupterm"+uniqueToken()
	svc := NewSearchService(db)
	for i := 0; i < 12; i++ {
		id := randomID()
		text := term
		issue := "many"
		if i == 11 {
			issue = "last"
			text += " " + strings.Repeat("padding ", 200)
		}
		r5SearchEvent(t, db, site, id, issue, text, 1000+i)
		if err := svc.IndexError(ctx, site, id, "R5", text); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"many", "last"} {
		if _, err := db.SQL().Exec(ctx, `INSERT INTO issues (issue_id,tenant_id,site_id,group_hash,title,status,first_seen,last_seen,version) VALUES ($1,'default',$2,$1,'R5','open','1','1',1)`, id, site); err != nil {
			t.Fatal(err)
		}
	}
	issues, err := svc.SearchIssues(ctx, site, term, 2)
	if err != nil || len(issues) != 2 {
		t.Fatalf("grouping consumed unique issue budget: %+v %v", issues, err)
	}
}
