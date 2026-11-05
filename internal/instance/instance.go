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
// That last part means a restored COPY claims to be the original. A copy that will
// run alongside the original has to take a new identity; see Reidentify.
//
// There is no unique constraint to lean on, so two instances booting against
// one empty store can both insert. Everyone then reads back and takes the
// smallest value, which converges on one id.
func EnsureID(ctx context.Context, db *nucleus.Client) (string, error) {
	id, err := readID(ctx, db)
	if err != nil || id != "" {
		return id, err
	}
	generated, err := generateID()
	if err != nil {
		return "", err
	}
	if err := storeID(ctx, db, generated); err != nil {
		return "", err
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

// Reidentify replaces the instance id and returns the old and new values. The new
// value is confirmed by reading it back, because readID picks the smallest stored
// value and a delete the engine did not apply would otherwise leave the old id in
// charge while this reported success. A running server keeps the id it booted with
// until it restarts.
func Reidentify(ctx context.Context, db *nucleus.Client) (oldID, newID string, err error) {
	oldID, err = readID(ctx, db)
	if err != nil {
		return "", "", err
	}
	newID, err = generateID()
	if err != nil {
		return "", "", err
	}
	if _, err := db.SQL().Exec(ctx, "DELETE FROM instance_meta WHERE meta_key = $1", instanceIDKey); err != nil {
		return "", "", fmt.Errorf("instance: clear id: %w", err)
	}
	if err := storeID(ctx, db, newID); err != nil {
		return "", "", err
	}
	got, err := readID(ctx, db)
	if err != nil {
		return "", "", err
	}
	if got != newID {
		return "", "", fmt.Errorf("instance: new id could not be confirmed: store reads %q", got)
	}
	return oldID, newID, nil
}

func generateID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("instance: generate id: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

func storeID(ctx context.Context, db *nucleus.Client, id string) error {
	if _, err := db.SQL().Exec(ctx,
		"INSERT INTO instance_meta (meta_key, meta_value, created_at) VALUES ($1, $2, $3)",
		instanceIDKey, id, dbutil.IntParam(time.Now().UnixMilli()),
	); err != nil {
		return fmt.Errorf("instance: store id: %w", err)
	}
	return nil
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
