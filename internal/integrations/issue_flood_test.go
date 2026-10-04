package integrations

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/useteploy/teploy-observe/internal/netsafe"
)

// A telemetry-key holder emitting distinct fingerprints makes each event a
// new issue; the per-integration and per-site buckets must bound delivery and
// emit exactly one bounded summary.
func TestDispatcher_FingerprintFloodIsRateLimitedWithOneSummary(t *testing.T) {
	var mu sync.Mutex
	var rules []string
	d := newIssueDispatcher(
		fixedList(Integration{IntegrationID: "a", SiteID: "s", IntType: "slack"}),
		func(_ context.Context, _ Integration, p AlertPayload) error {
			mu.Lock()
			rules = append(rules, p.RuleName)
			mu.Unlock()
			return nil
		}, quietLogger(), "")
	d.perHour = 10
	clock := time.Unix(1_700_000_000, 0)
	var cm sync.Mutex
	d.now = func() time.Time { cm.Lock(); defer cm.Unlock(); return clock }

	for i := 0; i < 200; i++ {
		d.Notify(IssueEvent{Kind: IssueKindNew, SiteID: "s", IssueID: fmt.Sprintf("flood-%d", i), Title: "x"})
	}
	waitFor(t, "queue drained", func() bool { return d.Stats().Pending == 0 && d.Stats().SuppressedRate+d.Stats().Delivered >= 190 })
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	issue, summary := 0, 0
	for _, r := range rules {
		if r == "issue_rate_limited" {
			summary++
		} else {
			issue++
		}
	}
	st := d.Stats()
	if issue != 10 {
		t.Fatalf("delivered %d issue notifications, want burst 10 (stats %+v)", issue, st)
	}
	if summary != 1 {
		t.Fatalf("got %d summaries, want exactly 1 (stats %+v)", summary, st)
	}
	if st.SuppressedRate == 0 || st.RateSummaries != 1 {
		t.Fatalf("stats: %+v", st)
	}
}

func TestDispatcher_RateLimitRefillsAndIsPerIntegration(t *testing.T) {
	rec := &recorder{}
	d := newIssueDispatcher(
		fixedList(
			Integration{IntegrationID: "a", SiteID: "s1", IntType: "slack"},
			Integration{IntegrationID: "b", SiteID: "s2", IntType: "slack"}),
		func(_ context.Context, i Integration, p AlertPayload) error {
			rec.add(i.IntegrationID + p.RuleName)
			return nil
		},
		quietLogger(), "")
	d.perHour = 3
	clock := time.Unix(1_700_000_000, 0)
	var cm sync.Mutex
	d.now = func() time.Time { cm.Lock(); defer cm.Unlock(); return clock }
	for i := 0; i < 10; i++ {
		d.Notify(IssueEvent{Kind: IssueKindNew, SiteID: "s1", IssueID: fmt.Sprintf("i%d", i)})
	}
	waitFor(t, "s1 burst", func() bool { return d.Stats().SuppressedRate >= 7 })
	// A different site is unaffected by s1's flood.
	d.Notify(IssueEvent{Kind: IssueKindNew, SiteID: "s2", IssueID: "other"})
	waitFor(t, "s2 delivered", func() bool {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		return strings.Contains(strings.Join(rec.calls, ","), "bnew_issue")
	})
	// One token per 20 minutes at 3/hour.
	cm.Lock()
	clock = clock.Add(21 * time.Minute)
	cm.Unlock()
	before := d.Stats().Delivered
	d.Notify(IssueEvent{Kind: IssueKindNew, SiteID: "s1", IssueID: "after-refill"})
	waitFor(t, "refilled token used", func() bool { return d.Stats().Delivered == before+1 })
}

func TestDispatcher_RateLimitEnvAndUnlimited(t *testing.T) {
	t.Setenv("OBSERVE_ISSUE_NOTIFY_PER_HOUR", "0")
	rec := &recorder{}
	d := newIssueDispatcher(
		fixedList(Integration{IntegrationID: "a", SiteID: "s", IntType: "slack"}),
		func(_ context.Context, _ Integration, _ AlertPayload) error { rec.add("x"); return nil },
		quietLogger(), "")
	for i := 0; i < 50; i++ {
		d.Notify(IssueEvent{Kind: IssueKindNew, SiteID: "s", IssueID: fmt.Sprintf("i%d", i)})
	}
	waitFor(t, "all delivered", func() bool { return rec.n() == 50 })
	t.Setenv("OBSERVE_ISSUE_NOTIFY_PER_HOUR", "7")
	if issueNotifyPerHourFromEnv() != 7 {
		t.Fatal("env not honoured")
	}
	t.Setenv("OBSERVE_ISSUE_NOTIFY_PER_HOUR", "junk")
	if issueNotifyPerHourFromEnv() != DefaultIssueNotifyPerHour {
		t.Fatal("bad env must fall back")
	}
}

func TestSlackEscapeNeutralisesControlSequences(t *testing.T) {
	in := "<!channel> <!here> <@U123> <https://evil.example|click> `code` & more"
	out := slackEscape(in, 1000)
	for _, bad := range []string{"<!channel>", "<!here>", "<@U123>", "<https", "`"} {
		if strings.Contains(out, bad) {
			t.Fatalf("%q survived in %q", bad, out)
		}
	}
	if !strings.Contains(out, "&lt;!channel&gt;") || !strings.Contains(out, "&amp; more") {
		t.Fatalf("unexpected escape: %q", out)
	}
	if got := slackEscape(strings.Repeat("a", 5000), 100); len([]rune(got)) > 103 {
		t.Fatalf("not truncated: %d", len(got))
	}
}

func TestFireSlackEscapesAttackerFieldsOnTheWire(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
	}))
	defer srv.Close()
	cfg := fmt.Sprintf(`{"webhook_url":%q}`, srv.URL)
	// httptest listens on loopback; use the allow-listed client.
	svc := testSvc(t, netsafe.ClientWithAllow(5*time.Second, loopbackAllow(t)))
	err := svc.fireSlack(cfg, AlertPayload{Title: "New issue: <!channel> `x`", Message: "see <@U1> <http://a|b>"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "<!channel>") || strings.Contains(body, "<@U1>") || strings.Contains(body, "<http") {
		t.Fatalf("control sequences reached Slack: %s", body)
	}
}

func TestCleanStripsBidiAndZeroWidth(t *testing.T) {
	if got := clean("a\u202ebc\u200bd\ufeff", 50); got != "abcd" {
		t.Fatalf("got %q", got)
	}
}
