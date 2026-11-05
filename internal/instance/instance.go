// Package instance owns the stable identity of one Observe deployment.
package instance

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
)

const instanceIDKey = "instance_id"

// EnsureID returns this deployment's instance id, generating and persisting it
// on first boot. It lives in the store rather than the data directory because
// the store is what makes an instance this instance: a replaced container
// keeps it, and restoring a backup restores it.
//
// There is no unique constraint to lean on, so two instances booting against
// one empty store can both insert. Everyone then reads back and takes the
// smallest value, which converges on one id.
func EnsureID(ctx context.Context, db *nucleus.Client) (string, error) {
	id, err := readID(ctx, db)
	if err != nil || id != "" {
		return id, err
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("instance: generate id: %w", err)
	}
	if _, err := db.SQL().Exec(ctx,
		"INSERT INTO instance_meta (meta_key, meta_value, created_at) VALUES ($1, $2, $3)",
		instanceIDKey, hex.EncodeToString(raw), dbutil.IntParam(time.Now().UnixMilli()),
	); err != nil {
		return "", fmt.Errorf("instance: store id: %w", err)
	}
	id, err = readID(ctx, db)
	if err != nil {
		return "", err
	}
	if id == "" {
		return "", fmt.Errorf("instance: stored id is not readable")
	}
	return id, nil
}

func readID(ctx context.Context, db *nucleus.Client) (string, error) {
	rows, err := nucleus.Query[struct {
		Value string `db:"meta_value"`
	}](ctx, db.SQL(), "SELECT meta_value FROM instance_meta WHERE meta_key = $1", instanceIDKey)
	if err != nil {
		return "", fmt.Errorf("instance: read id: %w", err)
	}
	if len(rows) == 0 {
		return "", nil
	}
	values := make([]string, len(rows))
	for i, r := range rows {
		values[i] = r.Value
	}
	sort.Strings(values)
	return values[0], nil
}
