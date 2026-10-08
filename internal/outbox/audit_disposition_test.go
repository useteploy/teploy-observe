package outbox

import (
	"context"
	"errors"
	"testing"
)

func TestDispositionFailureStopsFullBatchBeforeNextEffect(t *testing.T) {
	for _, handlerFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "success-mark", true: "attempt-mark"}[handlerFailure], func(t *testing.T) {
			db := o01OutboxFixture(t)
			kind := "fault-" + o01RunID()
			site := "site-" + kind
			store := New(db, nil)
			store.batchSize = 3
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			store.Register(kind, func(context.Context, string, string, []byte) error {
				calls++
				cancel()
				if handlerFailure {
					return errors.New("derive failed")
				}
				return nil
			})
			for i := 0; i < 3; i++ {
				if _, err := store.Enqueue(context.Background(), db.SQL(), site, kind, map[string]string{"fixture": "storage fault"}); err != nil {
					t.Fatal(err)
				}
			}
			n, err := store.ProcessDue(ctx)
			if err == nil || n != 1 || calls != 1 {
				t.Fatalf("failed disposition continued effects: n=%d calls=%d err=%v", n, calls, err)
			}
			t.Cleanup(func() {
				_, _ = db.SQL().Exec(context.Background(), `DELETE FROM derived_outbox WHERE site_id = $1`, site)
			})
		})
	}
}
