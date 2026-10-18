package main

// EV1 end-to-end oracle: the issue alert loop against a real Nucleus and
// the REAL production wiring - obserrors.Service ingest, the combined
// notifier from integrations_wiring.go, the emitter, the webhook kinds
// filter, the signed outbox drain, and the recovery read. Nucleus-gated
// (skips without a scratch engine), same posture as share_read_test.go.
//
// Negative controls asserted here (the programme's EV1 row):
//   - a webhook WITHOUT issue_* in its kinds filter receives NOTHING;
//   - a Slack-type target is NEVER sent a stack;
//   - a burst fires at most ONE issue_spike per issue per hour.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/neutron-build/neutron/go/neutron"
	"github.com/neutron-build/neutron/go/nucleus"

	obserrors "github.com/useteploy/teploy-observe/internal/errors"
	"github.com/useteploy/teploy-observe/internal/integrations"
	"github.com/useteploy/teploy-observe/internal/nucleustest"
	"github.com/useteploy/teploy-observe/internal/platform"
	"github.com/useteploy/teploy-observe/internal/schema"
)

func ev1DB(t *testing.T) *nucleus.Client {
	t.Helper()
	dsn := nucleustest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("nucleus not reachable at %s - skipping integration test", dsn)
	}
	if err := schema.Apply(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func ev1Site(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("ev1-%d", time.Now().UnixNano())
}

func ev1Cleanup(t *testing.T, db *nucleus.Client, site string) {
	t.Helper()
	for _, table := range []string{"error_events", "issues", "webhooks", "notification_outbox"} {
		if _, err := db.SQL().Exec(context.Background(),
			fmt.Sprintf("DELETE FROM %s WHERE site_id = $1", table), site); err != nil {
			t.Fatalf("cleanup %s: %v", table, err)
		}
	}
}

// ev1Sink records every POST per target path.
type ev1Sink struct {
	mu    sync.Mutex
	posts map[string][]ev1Post
}

type ev1Post struct {
	body        string
	delivery    string
	timestamp   string
	signature   string
	contentType string
}

func newEv1Sink() *ev1Sink {
	return &ev1Sink{posts: map[string][]ev1Post{}}
}

func (s *ev1Sink) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		s.mu.Lock()
		s.posts[req.URL.Path] = append(s.posts[req.URL.Path], ev1Post{
			body:        string(body),
			delivery:    req.Header.Get("X-Observe-Delivery"),
			timestamp:   req.Header.Get("X-Observe-Timestamp"),
			signature:   req.Header.Get("X-Observe-Signature"),
			contentType: req.Header.Get("Content-Type"),
		})
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}
}

func (s *ev1Sink) count(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.posts[path])
}

func (s *ev1Sink) bodies(path string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.posts[path]))
	for i, p := range s.posts[path] {
		out[i] = p.body
	}
	return out
}

func (s *ev1Sink) last(path string) ev1Post {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.posts[path])
	if n == 0 {
		return ev1Post{}
	}
	return s.posts[path][n-1]
}

// ev1Harness is the real wiring minus the process: the same constructors
// and the same wireIssueNotifications main() uses.
type ev1Harness struct {
	db       *nucleus.Client
	site     string
	sink     *ev1Sink
	server   *httptest.Server
	errSvc   *obserrors.Service
	issueSvc *obserrors.IssueService
	notify   *platform.Notifier
	emitter  *platform.IssueAlertEmitter
	secrets  map[string]string // webhook name -> signing secret
}

func newEv1Harness(t *testing.T, spikeMin string) *ev1Harness {
	t.Helper()
	db := ev1DB(t)
	site := ev1Site(t)
	t.Cleanup(func() { ev1Cleanup(t, db, site) })

	// The production client refuses private destinations; the receiver is
	// httptest on 127.0.0.1, so declare loopback the way an operator
	// would. Must be set before NewNotifier reads it.
	t.Setenv("OBSERVE_WEBHOOK_ALLOW_CIDRS", "127.0.0.0/8,::1/128")
	t.Setenv("OBSERVE_ISSUE_ALERT_SETTLE_MS", "50")
	if spikeMin != "" {
		t.Setenv("OBSERVE_ISSUE_SPIKE_MIN", spikeMin)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sink := newEv1Sink()
	server := httptest.NewServer(sink.handler())
	t.Cleanup(server.Close)

	issueSvc := obserrors.NewIssueService(db)
	errSvc := obserrors.NewService(db, issueSvc, nil, nil)
	integrationSvc := integrations.NewIntegrationService(db, logger)
	webhookSvc := platform.NewWebhookService(db, logger)
	notifier := platform.NewNotifier(db, logger, nil, nil)
	emitter := platform.NewIssueAlertEmitter(notifier, webhookSvc.List, issueSnapshotReader(issueSvc), "https://observe.example", logger)

	h := &ev1Harness{
		db: db, site: site, sink: sink, server: server,
		errSvc: errSvc, issueSvc: issueSvc, notify: notifier, emitter: emitter,
		secrets: map[string]string{},
	}

	// The REAL wiring: one callback feeding the integrations dispatcher
	// (unchanged) and the outbox emitter (new). No integrations are
	// configured in this fixture, so the dispatcher is a no-op sink.
	wireIssueNotifications(issueSvc, integrationSvc, "", emitter)

	// Three targets: full-fidelity http (opted in), the negative-control
	// http target (default kinds - must receive NOTHING), and a Slack
	// target (opted in, must never see a stack).
	for _, w := range []struct {
		name, path, wtype, kinds string
	}{
		{"full", "/full", "http", "issue_new,issue_regression,issue_spike"},
		{"plain", "/plain", "http", ""},
		{"slack", "/slack", "slack", "issue_new,issue_regression,issue_spike"},
	} {
		hook, err := webhookSvc.Create(context.Background(), site, w.name, w.wtype, server.URL+w.path, "", w.kinds)
		if err != nil {
			t.Fatalf("create webhook %s: %v", w.name, err)
		}
		h.secrets[w.path] = hook.Secret
	}

	emitter.Start()
	t.Cleanup(emitter.Stop)
	return h
}

func (h *ev1Harness) drain(t *testing.T) {
	t.Helper()
	if _, err := h.notify.DrainDue(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}
}

func (h *ev1Harness) wait(t *testing.T, path string, n int) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if h.sink.count(path) >= n {
			return
		}
		// The emitter's enqueue races the ingest transaction by design
		// (settle delay); drain whatever has landed, then re-check.
		h.drain(t)
		time.Sleep(50 * time.Millisecond)
	}
	h.drain(t)
	if got := h.sink.count(path); got < n {
		t.Fatalf("target %s received %d posts, want >= %d (bodies so far: %v)", path, got, n, h.sink.bodies(path))
	}
}

func ev1Input(site string) obserrors.ErrorInput {
	return obserrors.ErrorInput{
		SiteID:      site,
		ErrorType:   "TypeError",
		ErrorValue:  "x is not a function",
		Level:       "error",
		ReleaseTag:  "rel-1",
		Environment: "production",
		StackTrace: []obserrors.StackFrame{
			{Filename: "vendor/polyfill.js", Function: "wrapped", Lineno: 9},
			{Filename: "app/orders.ts", Function: "OrderController.create", Lineno: 42, InApp: true},
			{Filename: "app/token.ts", Function: "leak Authorization=Bearer abcdefghijklmnop", Lineno: 7, InApp: true},
		},
	}
}

// One seeded error creates exactly one issue_new, delivered signed to the
// opted-in webhook only. The negative-control webhook (no issue kinds)
// receives nothing; the Slack target gets a rendered text without the
// stack; the payload carries the grouping fingerprint, release,
// environment, counts and a scrubbed, 2 KB-clipped stack.
func TestEV1_SeededErrorOneIssueNew(t *testing.T) {
	h := newEv1Harness(t, "1000") // spike floor high: this test must not fire one
	ctx := context.Background()

	if _, err := h.errSvc.IngestErrorEvent(ctx, ev1Input(h.site)); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	h.wait(t, "/full", 1)
	h.drain(t)

	if got := h.sink.count("/plain"); got != 0 {
		t.Fatalf("NEGATIVE CONTROL FAILED: webhook without issue_* kinds received %d posts: %v", got, h.sink.bodies("/plain"))
	}
	if got := h.sink.count("/full"); got != 1 {
		t.Fatalf("issue_new posts = %d, want exactly 1", got)
	}

	// The signed delivery: headers and a verifiable HMAC over
	// timestamp + "." + body with the webhook's own secret.
	post := h.sink.last("/full")
	if post.delivery == "" || post.timestamp == "" || post.signature == "" {
		t.Fatalf("delivery headers missing: %+v", post)
	}
	if post.contentType != "application/json" {
		t.Fatalf("content type: %q", post.contentType)
	}
	mac := hmac.New(sha256.New, []byte(h.secrets["/full"]))
	mac.Write([]byte(post.timestamp))
	mac.Write([]byte("."))
	mac.Write([]byte(post.body))
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if post.signature != want {
		t.Fatalf("X-Observe-Signature mismatch: got %s want %s", post.signature, want)
	}

	var p platform.IssueNotification
	if err := json.Unmarshal([]byte(post.body), &p); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if p.Kind != platform.NotifyIssueNew {
		t.Fatalf("kind = %s, want issue_new", p.Kind)
	}
	if p.IssueID == "" || p.SiteID != h.site || p.Service != h.site {
		t.Fatalf("identity: %+v", p)
	}
	if p.Fingerprint == "" || len(p.Fingerprint) != 32 {
		t.Fatalf("fingerprint: %q", p.Fingerprint)
	}
	if p.Release != "rel-1" || p.Environment != "production" || p.Title == "" || p.Culprit == "" {
		t.Fatalf("context fields: %+v", p)
	}
	if p.EventCount != 1 {
		t.Fatalf("event_count = %d, want 1 (re-read after settle)", p.EventCount)
	}
	if p.FirstSeen == "" || p.LastSeen == "" || p.OccurredAt == "" || p.DeliverySeq != 1 {
		t.Fatalf("times/seq: %+v", p)
	}
	// Scrubbed stack: in-app frames only, secret gone, budget honored.
	if !strings.Contains(p.SampleStack, "app/orders.ts:42 OrderController.create") {
		t.Fatalf("sample stack missing in-app frame: %q", p.SampleStack)
	}
	if strings.Contains(p.SampleStack, "abcdefghijklmnop") || strings.Contains(p.SampleStack, "vendor/polyfill.js") {
		t.Fatalf("sample stack leaked secret or library frame: %q", p.SampleStack)
	}
	if len(p.SampleStack) > platform.IssueStackMaxBytes {
		t.Fatalf("sample stack is %d bytes, want <= %d", len(p.SampleStack), platform.IssueStackMaxBytes)
	}

	// Slack target: rendered text, and the stack NEVER crossed the wire.
	if got := h.sink.count("/slack"); got != 1 {
		t.Fatalf("slack posts = %d, want 1", got)
	}
	var slackBody struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(h.sink.last("/slack").body), &slackBody); err != nil {
		t.Fatalf("slack body: %v (%s)", err, h.sink.last("/slack").body)
	}
	// The culprit is a one-frame summary and fine to show; what must NEVER
	// reach a chat target is the STACK: frame lines ("file:line function"),
	// the token.ts frame (which exists only in the stack), and the secret.
	for _, leaked := range []string{"app/orders.ts:42", "app/token.ts", "token.ts:7", "abcdefghijklmnop", "sample_stack"} {
		if strings.Contains(slackBody.Text, leaked) || strings.Contains(h.sink.last("/slack").body, leaked) {
			t.Fatalf("NEGATIVE CONTROL FAILED: slack target received stack content (%q): %s", leaked, h.sink.last("/slack").body)
		}
	}
	if !strings.Contains(slackBody.Text, "New issue") || !strings.Contains(slackBody.Text, "production") {
		t.Fatalf("slack text: %q", slackBody.Text)
	}
}

// Resolve, then re-error: exactly one issue_regression (and the reopen is
// visible in the recovery read).
func TestEV1_ResolveThenReErrorOneRegression(t *testing.T) {
	h := newEv1Harness(t, "1000")
	ctx := context.Background()

	issueID, err := h.errSvc.IngestErrorEvent(ctx, ev1Input(h.site))
	if err != nil {
		t.Fatalf("ingest 1: %v", err)
	}
	h.wait(t, "/full", 1)

	if err := h.issueSvc.UpdateStatus(ctx, issueID, h.site, "resolved", 0); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := h.errSvc.IngestErrorEvent(ctx, ev1Input(h.site)); err != nil {
		t.Fatalf("ingest 2: %v", err)
	}
	h.wait(t, "/full", 2)
	h.drain(t)

	if got := h.sink.count("/plain"); got != 0 {
		t.Fatalf("NEGATIVE CONTROL FAILED: kinds-less webhook received %d posts", got)
	}
	if got := h.sink.count("/full"); got != 2 {
		t.Fatalf("posts = %d, want exactly 2 (one new, one regression)", got)
	}
	var p platform.IssueNotification
	if err := json.Unmarshal([]byte(h.sink.last("/full").body), &p); err != nil {
		t.Fatal(err)
	}
	if p.Kind != platform.NotifyIssueRegression {
		t.Fatalf("kind = %s, want issue_regression", p.Kind)
	}
	if p.RegressionCount != 1 {
		t.Fatalf("regression_count = %d, want 1", p.RegressionCount)
	}
	if p.EventCount < 2 {
		t.Fatalf("event_count = %d, want >= 2", p.EventCount)
	}
	if p.DeliverySeq != 2 {
		t.Fatalf("delivery_seq = %d, want 2 (per-issue monotonic)", p.DeliverySeq)
	}

	// The recovery read the regression feeds: status open again, both
	// events counted, one in the release.
	stats, err := h.issueSvc.IssueSince(ctx, h.site, issueID, time.Now().Add(-time.Hour).UnixMilli(), "rel-1")
	if err != nil || stats == nil {
		t.Fatalf("IssueSince: %v %+v", err, stats)
	}
	if stats.Status != "open" || stats.EventsSince != 2 || stats.EventsSinceInRelease != 2 || stats.LatestRelease != "rel-1" {
		t.Fatalf("recovery read: %+v", stats)
	}
}

// A burst fires at most ONE issue_spike per issue per hour, however long
// it keeps up.
func TestEV1_BurstAtMostOneSpikePerHour(t *testing.T) {
	h := newEv1Harness(t, "5") // low floor so the test burst crosses it
	ctx := context.Background()

	in := ev1Input(h.site)
	if _, err := h.errSvc.IngestErrorEvent(ctx, in); err != nil {
		t.Fatalf("ingest seed: %v", err)
	}
	// The burst: well past the floor, then more inside the same hour.
	for i := 0; i < 20; i++ {
		if _, err := h.errSvc.IngestErrorEvent(ctx, in); err != nil {
			t.Fatalf("ingest %d: %v", i, err)
		}
	}
	h.wait(t, "/full", 2) // one issue_new + one issue_spike
	for i := 0; i < 20; i++ {
		if _, err := h.errSvc.IngestErrorEvent(ctx, in); err != nil {
			t.Fatalf("ingest continued %d: %v", i, err)
		}
	}
	h.wait(t, "/full", 2)
	h.drain(t)

	spikes := 0
	for _, body := range h.sink.bodies("/full") {
		var p platform.IssueNotification
		if err := json.Unmarshal([]byte(body), &p); err != nil {
			t.Fatal(err)
		}
		if p.Kind == platform.NotifyIssueSpike {
			spikes++
			if p.WindowCount < 5 {
				t.Fatalf("spike window_count = %d, want >= 5", p.WindowCount)
			}
		}
	}
	if spikes != 1 {
		t.Fatalf("issue_spike notifications = %d, want exactly 1 (at most one per issue per hour)", spikes)
	}
	if got := h.sink.count("/plain"); got != 0 {
		t.Fatalf("NEGATIVE CONTROL FAILED: kinds-less webhook received %d posts", got)
	}
}

// The recovery read handler fails closed: missing ts, missing release and
// an unknown issue are 400/400/404, never a zero-valued 200.
func TestEV1_IssueSinceHandlerFailsClosed(t *testing.T) {
	h := newEv1Harness(t, "1000")
	ctx := context.Background()

	in := ev1Input(h.site)
	issueID, err := h.errSvc.IngestErrorEvent(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.errSvc.IngestErrorEvent(ctx, in); err != nil { // second event, same release
		t.Fatal(err)
	}
	in2 := in
	in2.ReleaseTag = "rel-2"
	if _, err := h.errSvc.IngestErrorEvent(ctx, in2); err != nil { // third event, fix release
		t.Fatal(err)
	}

	handler := issueSinceHandler(h.issueSvc)
	since := time.Now().Add(-time.Hour).UnixMilli()

	stats, err := handler(ctx, issueSinceInput{IssueID: issueID, SiteID: h.site, Ts: since, Release: "rel-2"})
	if err != nil {
		t.Fatalf("happy path: %v", err)
	}
	if stats.Status != "open" || stats.EventsSince != 3 || stats.EventsSinceInRelease != 1 || stats.LatestRelease != "rel-2" {
		t.Fatalf("stats: %+v", stats)
	}
	if stats.LastSeen.IsZero() {
		t.Fatal("last_seen is zero")
	}

	for name, in := range map[string]issueSinceInput{
		"missing ts":      {IssueID: issueID, SiteID: h.site, Release: "rel-2"},
		"zero ts":         {IssueID: issueID, SiteID: h.site, Ts: -5, Release: "rel-2"},
		"missing release": {IssueID: issueID, SiteID: h.site, Ts: since},
		"missing site":    {IssueID: issueID, Ts: since, Release: "rel-2"},
		"unknown issue":   {IssueID: "nope", SiteID: h.site, Ts: since, Release: "rel-2"},
		"foreign site":    {IssueID: issueID, SiteID: "someone-elses", Ts: since, Release: "rel-2"},
	} {
		_, err := handler(ctx, in)
		if err == nil {
			t.Fatalf("%s: accepted", name)
		}
		var appErr *neutron.AppError
		if !errors.As(err, &appErr) {
			t.Fatalf("%s: error is not an AppError: %v", name, err)
		}
		want := 400
		if name == "unknown issue" || name == "foreign site" {
			want = 404
		}
		if appErr.Status != want {
			t.Fatalf("%s: status = %d, want %d", name, appErr.Status, want)
		}
	}
}

// The share posture of the new route is a security decision that lives
// inline in main(); guard it the same cheap way the trace routes do
// (TestOnlyAggregateTraceReadsAreShareable).
func TestEV1_OnlyTheRecoveryReadIsShareableOnErrors(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	if !strings.Contains(text, `errorsShared := r.Group("/api/v1/errors", jwtOrShareMW(jwtMW, shareSvc))`) {
		t.Fatal("the recovery read is no longer share-readable - teploy-ship's recovery sweep (EV7) depends on it")
	}
	if !strings.Contains(text, `neutron.Get(errorsShared, "/issues/{issue_id}/since", issueSinceHandler(issueSvc)`) {
		t.Error("/errors/issues/{id}/since must be registered on the shared group")
	}
	// No other errors route may accept a share token: everything else on
	// the errors surface returns payloads (stacks, URLs, user values).
	for _, route := range []string{"/search", "/{issue_id}/events", "/{issue_id}/session"} {
		if strings.Contains(text, `neutron.Get(errorsShared, "`+route+`"`) {
			t.Errorf("%s returns error payloads and must not accept a share token", route)
		}
	}
}
