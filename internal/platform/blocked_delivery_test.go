package platform

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
	"github.com/useteploy/teploy-observe/internal/netsafe"
	"github.com/useteploy/teploy-observe/internal/nucleustest"
	"github.com/useteploy/teploy-observe/internal/schema"
)

// A name that resolves to a private address cannot be refused at create. The dial
// guard catches it, and that used to look like an ordinary failed delivery.
func TestBlockedDeliveryIsLoggedAndNotRetried(t *testing.T) {
	var logged bytes.Buffer
	svc := NewWebhookService(nil, slog.New(slog.NewTextHandler(&logged, nil)))
	start := time.Now()
	svc.attemptDelivery(deliveryJob{
		id:   "d1",
		hook: Webhook{WebhookID: "w1", Name: "n", WebhookType: "http", URL: "http://localhost:9/x"},
	})
	if strings.Count(logged.String(), "webhook delivery blocked") != 1 {
		t.Fatalf("log = %q", logged.String())
	}
	if elapsed := time.Since(start); elapsed >= 500*time.Millisecond {
		t.Fatalf("a blocked delivery was retried with backoff: took %v", elapsed)
	}
}

func TestOutboxDeliveryReportsBlocked(t *testing.T) {
	var logged bytes.Buffer
	n := &Notifier{client: netsafe.Client(5 * time.Second), logger: slog.New(slog.NewTextHandler(&logged, nil))}
	row := &notifyRow{ID: "d1", Kind: "incident_opened", SiteID: "s", TargetType: "http", TargetURL: "http://localhost:9/x"}
	err := n.deliver(context.Background(), row)
	if !errors.Is(err, netsafe.ErrBlocked) {
		t.Fatalf("deliver: err = %v, want ErrBlocked", err)
	}
	n.noteBlocked(row, err)
	if !strings.Contains(logged.String(), "notification blocked") {
		t.Fatalf("log = %q", logged.String())
	}
	logged.Reset()
	n.noteBlocked(row, errors.New("connection refused"))
	if logged.Len() != 0 {
		t.Fatalf("an ordinary failure must not read as blocked: %q", logged.String())
	}
}

func TestValidateDestinationUsesTheOperatorAllowlist(t *testing.T) {
	t.Setenv("OBSERVE_WEBHOOK_ALLOW_CIDRS", "")
	if err := NewWebhookService(nil, slog.Default()).ValidateDestination("http://100.100.1.1/x"); !errors.Is(err, ErrInvalidWebhook) {
		t.Errorf("no allowlist: err = %v", err)
	}
	t.Setenv("OBSERVE_WEBHOOK_ALLOW_CIDRS", "100.64.0.0/10")
	if err := NewWebhookService(nil, slog.Default()).ValidateDestination("http://100.100.1.1/x"); err != nil {
		t.Errorf("allowlisted: %v", err)
	}
}

// Rows sharing a timestamp must page without a repeat or a gap. triggered_at alone
// leaves their order to the engine, and it can differ between two LIMIT/OFFSET reads.
func TestListHistoryPagesInATotalOrder(t *testing.T) {
	ctx := context.Background()
	db, err := nucleus.Connect(ctx, nucleustest.DSN(t))
	if err != nil {
		t.Skipf("nucleus not reachable: %v", err)
	}
	defer db.Close()
	if err := schema.Apply(ctx, db); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	site := "history-order-" + genID()
	at := dbutil.IntParam(time.Now().UnixMilli())
	want := map[string]bool{}
	for i := 0; i < 12; i++ {
		id := "alert-" + strconv.Itoa(i) + "-" + genID()
		want[id] = true
		if _, err := db.SQL().Exec(ctx,
			`INSERT INTO alert_history (alert_id, tenant_id, rule_id, site_id, triggered_at, metric_value, threshold, status)
			 VALUES ($1, 'default', 'r', $2, $3, '1.00', '1.00', 'triggered')`, id, site, at); err != nil {
			t.Fatal(err)
		}
	}
	svc := NewAlertService(db, nil, nil, nil, nil)
	var last string
	seen := map[string]bool{}
	for offset := 0; offset < 12; offset += 3 {
		page, err := svc.ListHistory(ctx, site, 3, offset)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) != 3 {
			t.Fatalf("offset %d: %d rows", offset, len(page))
		}
		for _, e := range page {
			if seen[e.AlertID] {
				t.Fatalf("offset %d repeated %s", offset, e.AlertID)
			}
			if last != "" && e.AlertID >= last {
				t.Fatalf("offset %d: %s after %s is not descending", offset, e.AlertID, last)
			}
			seen[e.AlertID], last = true, e.AlertID
		}
	}
	if len(seen) != len(want) {
		t.Fatalf("paged %d of %d rows", len(seen), len(want))
	}
}
