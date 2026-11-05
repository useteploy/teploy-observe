package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/config"
	"github.com/useteploy/teploy-observe/internal/instance"
	"github.com/useteploy/teploy-observe/internal/svctoken"
)

// runReidentify gives a restored copy its own identity. A backup carries the
// instance id and the service tokens, so without this a copy answers discovery as
// the original and accepts the original's tokens: a client holding one could
// provision against the copy believing it is the original.
//
// Run it between `observe restore` and starting the server. It asks for --yes
// because on the original instance it would disconnect every client.
func runReidentify(logger *slog.Logger, cfg config.Config, args []string) {
	if len(args) != 1 || args[0] != "--yes" {
		fmt.Fprintln(os.Stderr, "observe reidentify gives this database a new instance_id and revokes every service token.")
		fmt.Fprintln(os.Stderr, "It is for a restored copy. On the original instance it disconnects every client.")
		fmt.Fprintln(os.Stderr, "Run it with --yes to proceed.")
		os.Exit(2)
	}
	db := connectForCLI(cfg, logger)
	defer db.Close()
	oldID, newID, revoked, err := reidentify(context.Background(), db)
	if err != nil {
		logger.Error("reidentify failed; run it again", "err", err, "tokens_revoked", revoked)
		os.Exit(1)
	}
	logger.Info("instance reidentified", "old_instance_id", oldID, "new_instance_id", newID, "tokens_revoked", revoked)
}

// reidentify revokes first. If it stops partway the copy has lost its tokens but
// not yet its id, which fails safe, and running it again finishes the job.
func reidentify(ctx context.Context, db *nucleus.Client) (oldID, newID string, revoked int, err error) {
	revoked, err = svctoken.NewStore(db).RevokeAll(ctx)
	if err != nil {
		return "", "", revoked, err
	}
	oldID, newID, err = instance.Reidentify(ctx, db)
	return oldID, newID, revoked, err
}
