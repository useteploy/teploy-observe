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
}
