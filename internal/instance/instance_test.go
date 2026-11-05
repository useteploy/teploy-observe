package instance

import (
	"context"
	"testing"

	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/nucleustest"
	"github.com/useteploy/teploy-observe/internal/schema"
)

func TestEnsureIDIsStable(t *testing.T) {
	ctx := context.Background()
	db, err := nucleus.Connect(ctx, nucleustest.DSN(t))
	if err != nil {
		t.Skipf("nucleus not reachable: %v", err)
	}
	defer db.Close()
	if err := schema.Apply(ctx, db); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	first, err := EnsureID(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 32 {
		t.Fatalf("instance id = %q", first)
	}
	second, err := EnsureID(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("instance id changed across calls: %q then %q", first, second)
	}

	// Several rounds: a new id can sort either side of the old one, and the
	// smallest stored value wins, so a surviving old row would show up here.
	for i := 0; i < 8; i++ {
		before, err := EnsureID(ctx, db)
		if err != nil {
			t.Fatal(err)
		}
		oldID, newID, err := Reidentify(ctx, db)
		if err != nil {
			t.Fatal(err)
		}
		if oldID != before || newID == before || len(newID) != 32 {
			t.Fatalf("reidentify: before %q, old %q, new %q", before, oldID, newID)
		}
		after, err := EnsureID(ctx, db)
		if err != nil {
			t.Fatal(err)
		}
		if after != newID {
			t.Fatalf("EnsureID = %q after reidentify to %q", after, newID)
		}
	}
}
