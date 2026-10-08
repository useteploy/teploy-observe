package audit

import (
	"context"
	"fmt"
	"github.com/neutron-build/neutron/go/nucleus"
	"github.com/useteploy/teploy-observe/internal/dbutil"
	"strconv"
)

// Typed read seam for the production verifier, including its SQL domain check.
// Tests can drive the real Verify loop and control its watermark interleaving.
type verificationReader interface {
	invalid(context.Context) (*verificationHead, error)
	head(context.Context) (int64, error)
	checkpoint(context.Context) (*Checkpoint, error)
	page(context.Context, int64, int64, int) ([]AuditEvent, error)
}
type verificationHead struct {
	Seq int64 `db:"seq"`
}
type nucleusVerificationReader struct{ s *Service }

func (r nucleusVerificationReader) invalid(ctx context.Context) (*verificationHead, error) {
	rows, err := nucleus.Query[verificationHead](ctx, r.s.db.SQL(), "SELECT CAST(seq AS BIGINT) AS seq FROM audit_events WHERE CAST(seq AS BIGINT) <= 0 OR CAST(seq AS BIGINT) IS NULL LIMIT 1")
	if err != nil {
		return nil, err
	}
	if len(rows) > 0 {
		return &rows[0], nil
	}
	return nil, nil
}
func (r nucleusVerificationReader) head(ctx context.Context) (int64, error) {
	rows, err := nucleus.Query[verificationHead](ctx, r.s.db.SQL(), "SELECT CAST(seq AS BIGINT) AS seq FROM audit_events ORDER BY CAST(seq AS BIGINT) DESC LIMIT 1")
	if err != nil {
		return 0, err
	}
	if len(rows) > 0 {
		return rows[0].Seq, nil
	}
	return 0, nil
}
func (r nucleusVerificationReader) checkpoint(ctx context.Context) (*Checkpoint, error) {
	return r.s.latestCheckpoint(ctx)
}
func (r nucleusVerificationReader) page(ctx context.Context, last, through int64, limit int) ([]AuditEvent, error) {
	return nucleus.Query[AuditEvent](ctx, r.s.db.SQL(), "SELECT "+auditColumns+" FROM audit_events WHERE CAST(seq AS BIGINT) > $1 AND CAST(seq AS BIGINT) <= $2 ORDER BY CAST(seq AS BIGINT) ASC LIMIT "+strconv.Itoa(limit), dbutil.IntParam(last), dbutil.IntParam(through))
}

// Caller holds the writer lock for this bounded snapshot only. The full chain
// scan uses the captured watermark and permits concurrent appends.
func verificationSnapshot(ctx context.Context, r verificationReader) (int64, *Checkpoint, *verificationHead, error) {
	invalid, err := r.invalid(ctx)
	if err != nil {
		return 0, nil, nil, err
	}
	if invalid != nil {
		return 0, nil, invalid, nil
	}
	head, err := r.head(ctx)
	if err != nil {
		return 0, nil, nil, err
	}
	cp, err := r.checkpoint(ctx)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("audit: latest checkpoint read: %w", err)
	}
	return head, cp, nil, nil
}
