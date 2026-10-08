package errors

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/neutron-build/neutron/go/nucleus"
)

// TestFTSSiteScoping verifies audit #147: a busy site's hits must not starve
// another site out of the shared FTS result budget. Requires a live Nucleus.
func TestFTSSiteScoping(t *testing.T) {
	dsn := os.Getenv("OBSERVE_NUCLEUS_URL")
	if dsn == "" {
		t.Skip("no OBSERVE_NUCLEUS_URL")
	}
	db, err := nucleus.Connect(context.Background(), dsn)
	if err != nil {
		t.Skipf("connect: %v", err)
	}
	svc := NewSearchService(db)
	ctx := context.Background()
	uniq := uniqueToken()
	term := "faultterm" + uniq

	// Site A floods the term; site B has one document.
	for i := 0; i < 50; i++ {
		if err := svc.IndexError(ctx, "siteA"+uniq, fmt.Sprintf("A-%d-%s", i, uniq), "Boom", term); err != nil {
			t.Fatalf("index A: %v", err)
		}
	}
	if err := svc.IndexError(ctx, "siteB"+uniq, "B-only-"+uniq, "Boom", term); err != nil {
		t.Fatalf("index B: %v", err)
	}

	// Faceted search returns exactly site B's hit — pre-fix (one global index)
	// site A's 50 hits could fill the top-N and leave B with nothing.
	hits, err := svc.Search(ctx, "siteB"+uniq, term, 10)
	if err != nil {
		t.Fatalf("search B: %v", err)
	}
	if len(hits) != 1 || hits[0].ErrorID != "B-only-"+uniq {
		t.Fatalf("site B expected its 1 hit, got %d (%v) — cross-site starvation", len(hits), hits)
	}
	if hitsA, err := svc.Search(ctx, "siteA"+uniq, term, 10); err != nil || len(hitsA) != 10 {
		t.Fatalf("site A expected 10 (limit) hits, got %d err=%v", len(hitsA), err)
	}
}

// OBS26-89: repeated/interrupted rebuilds reuse event document identities,
// including upgrading an index with the legacy reverse-only mappings.
func TestOBS89ReindexReusesDocumentsAndRemovesLegacyDuplicates(t *testing.T) {
	dsn := os.Getenv("OBSERVE_NUCLEUS_URL")
	if dsn == "" {
		t.Skip("no OBSERVE_NUCLEUS_URL")
	}
	ctx := context.Background()
	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	site, term := "obs89-"+uniqueToken(), "legacyterm"+uniqueToken()
	svc := NewSearchService(db)
	ids := []string{randomID(), randomID()}
	for i, id := range ids {
		_, err := db.SQL().Exec(ctx, `INSERT INTO error_events (error_id,tenant_id,site_id,session_id,issue_id,group_hash,timestamp,error_type,error_value,level) VALUES ($1,'default',$2,'session','issue','hash',$3,'Test',$4,'error')`, id, site, fmt.Sprint(1000+i), term)
		if err != nil {
			t.Fatal(err)
		}
		for j := 0; j < 2; j++ {
			doc, err := db.KV().Incr(ctx, "fts:error:seq")
			if err != nil {
				t.Fatal(err)
			}
			if err := db.KV().Set(ctx, fmt.Sprintf("fts:error:%d", doc), []byte(id)); err != nil {
				t.Fatal(err)
			}
			if _, err := db.FTS().IndexFaceted(ctx, doc, term, "site_id", site); err != nil {
				t.Fatal(err)
			}
		}
	}
	for pass := 0; pass < 2; pass++ {
		p, err := svc.ReindexAll(ctx, site, 1, false, 1, nil)
		if err != nil {
			t.Fatal(err)
		}
		if p.Indexed != 2 {
			t.Fatalf("reindex incomplete: %+v", p)
		}
		docs, err := db.FTS().SearchFilter(ctx, term, "site_id", site, nucleus.WithFTSLimit(10))
		if err != nil {
			t.Fatal(err)
		}
		if len(docs) != 2 {
			t.Fatalf("pass %d document multiplicity=%d", pass, len(docs))
		}
		hits, err := svc.SearchErrors(ctx, site, term, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 2 || hits[0].ErrorID == hits[1].ErrorID {
			t.Fatalf("duplicate hydration: %+v", hits)
		}
	}
}
