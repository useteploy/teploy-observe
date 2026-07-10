package errors

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/neutron-dev/neutron-go/nucleus"
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
