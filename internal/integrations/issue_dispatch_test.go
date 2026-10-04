package integrations

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/useteploy/teploy-observe/internal/netsafe"
)

// ---- helpers ---------------------------------------------------------------

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) { l.mu.Lock(); defer l.mu.Unlock(); return l.b.Write(p) }
func (l *logBuf) String() string              { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// loopbackAllow lets tests reach httptest servers; the production client uses
// no allowlist.
func loopbackAllow(t *testing.T) netsafe.Allow {
	t.Helper()
	a, err := netsafe.ParseAllow("127.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func testSvc(t *testing.T, client *http.Client) *IntegrationService {
	return &IntegrationService{logger: quietLogger(), client: client}
}

// rewriteTransport sends every request to the test server regardless of host,
// for providers whose endpoint is hard-coded (GitHub, PagerDuty).
type rewriteTransport struct{ target *url.URL }

func (r rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme = r.target.Scheme
	req.URL.Host = r.target.Host
	return http.DefaultTransport.RoundTrip(req)
}

type capture struct {
	mu      sync.Mutex
	path    string
	body    string
	headers http.Header
	user    string
}

func captureServer(t *testing.T, status int) (*httptest.Server, *capture) {
	c := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.path, c.body, c.headers = r.URL.Path, string(b), r.Header.Clone()
		c.user, _, _ = r.BasicAuth()
		c.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, c
}

func sampleEvent(kind, issue string) IssueEvent {
	return IssueEvent{Kind: kind, SiteID: "site-1", IssueID: issue, Title: "TypeError: x is undefined",
		Culprit: "app.js in handleClick", Level: "error", Release: "1.2.3", EventCount: 1}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// ---- per-kind delivery through the real fire path ---------------------------

func TestIssueEventDelivery_PerIntegrationKind(t *testing.T) {
	d := newIssueDispatcher(nil, nil, quietLogger(), "https://obs.example.com/")
	payload := d.payload(sampleEvent(IssueKindNew, "i1"))
	if !strings.HasPrefix(payload.Title, "New issue: ") || payload.URL != "https://obs.example.com/errors" {
		t.Fatalf("payload: %+v", payload)
	}

	t.Run("slack", func(t *testing.T) {
		srv, c := captureServer(t, 200)
		s := testSvc(t, netsafe.ClientWithAllow(5*time.Second, loopbackAllow(t)))
		cfg := fmt.Sprintf(`{"webhook_url":%q}`, srv.URL+"/services/T0/B0/secret")
		if err := s.fireOne(Integration{IntType: "slack", Config: cfg}, payload); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(c.body, "New issue: TypeError") || c.path != "/services/T0/B0/secret" {
			t.Fatalf("slack request: path=%q body=%s", c.path, c.body)
		}
	})

	t.Run("jira", func(t *testing.T) {
		srv, c := captureServer(t, 201)
		s := testSvc(t, netsafe.ClientWithAllow(5*time.Second, loopbackAllow(t)))
		cfg := fmt.Sprintf(`{"base_url":%q,"email":"a@b.c","api_token":"tok-123456","project":"OBS"}`, srv.URL)
		if err := s.fireOne(Integration{IntType: "jira", Config: cfg}, payload); err != nil {
			t.Fatal(err)
		}
		if c.path != "/rest/api/2/issue" || c.user != "a@b.c" || !strings.Contains(c.body, "New issue: TypeError") || !strings.Contains(c.body, `"OBS"`) {
			t.Fatalf("jira request: path=%q user=%q body=%s", c.path, c.user, c.body)
		}
	})

	t.Run("github", func(t *testing.T) {
		srv, c := captureServer(t, 201)
		u, _ := url.Parse(srv.URL)
		s := testSvc(t, &http.Client{Transport: rewriteTransport{u}, Timeout: 5 * time.Second})
		cfg := `{"token":"ghp_secret_value","owner":"o","repo":"r"}`
		if err := s.fireOne(Integration{IntType: "github", Config: cfg}, payload); err != nil {
			t.Fatal(err)
		}
		if c.path != "/repos/o/r/issues" || c.headers.Get("Authorization") != "Bearer ghp_secret_value" || !strings.Contains(c.body, "New issue: TypeError") {
			t.Fatalf("github request: path=%q body=%s", c.path, c.body)
		}
	})

	t.Run("pagerduty", func(t *testing.T) {
		srv, c := captureServer(t, 202)
		u, _ := url.Parse(srv.URL)
		s := testSvc(t, &http.Client{Transport: rewriteTransport{u}, Timeout: 5 * time.Second})
		if err := s.fireOne(Integration{IntType: "pagerduty", Config: `{"routing_key":"rk-abcdef"}`}, payload); err != nil {
			t.Fatal(err)
		}
		if c.path != "/v2/enqueue" || !strings.Contains(c.body, "New issue: TypeError") || !strings.Contains(c.body, `"severity":"error"`) {
			t.Fatalf("pagerduty request: path=%q body=%s", c.path, c.body)
		}
	})

	t.Run("remote failure is an error", func(t *testing.T) {
		srv, _ := captureServer(t, 500)
		s := testSvc(t, netsafe.ClientWithAllow(5*time.Second, loopbackAllow(t)))
		cfg := fmt.Sprintf(`{"webhook_url":%q}`, srv.URL)
		if err := s.fireOne(Integration{IntType: "slack", Config: cfg}, payload); err == nil {
			t.Fatal("a 500 must surface as an error")
		}
	})
}

// The email channel exists (it is not faked); its network leg is covered by the
// SSRF guard tests below. A live SMTP exchange is not exercised here.
func TestEmail_RefusesNonPublicSMTPHost(t *testing.T) {
	s := testSvc(t, nil)
	for _, host := range []string{"127.0.0.1", "localhost", "10.0.0.5", "169.254.169.254", "[::1]"} {
		cfg := fmt.Sprintf(`{"smtp_host":%q,"smtp_port":"25","from":"a@b.c","to":"x@y.z"}`, strings.Trim(host, "[]"))
		err := s.fireEmail(cfg, AlertPayload{Title: "t"})
		if err == nil || !strings.Contains(err.Error(), "non-public") {
			t.Fatalf("host %s: expected non-public refusal, got %v", host, err)
		}
	}
}

func TestPublicSMTPAddr_RebindingMixedAnswerRefused(t *testing.T) {
	mixed := func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}, {IP: net.ParseIP("10.1.2.3")}}, nil
	}
	if _, err := publicSMTPAddr("smtp.evil.example", mixed); err == nil {
		t.Fatal("a name with any private answer must be refused")
	}
	public := func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, nil
	}
	ip, err := publicSMTPAddr("smtp.ok.example", public)
	if err != nil || ip != "93.184.216.34" {
		t.Fatalf("public host: %q %v", ip, err)
	}
}

func TestPublicSMTPAddr_PrivateRelayAllowlist(t *testing.T) {
	private := func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("10.1.2.3")}}, nil
	}
	if _, err := publicSMTPAddr("relay.tailnet.example", private); err == nil {
		t.Fatal("private host must be refused without the allowlist")
	}
	old := smtpPrivateHosts
	smtpPrivateHosts = map[string]bool{"relay.tailnet.example": true}
	defer func() { smtpPrivateHosts = old }()
	ip, err := publicSMTPAddr("relay.tailnet.example", private)
	if err != nil || ip != "10.1.2.3" {
		t.Fatalf("allowlisted relay: %q %v", ip, err)
	}
	// The allowlist is exact-name: it does not open the door for others.
	if _, err := publicSMTPAddr("other.tailnet.example", private); err == nil {
		t.Fatal("allowlist must not cover sibling names")
	}
}

// ---- SSRF -------------------------------------------------------------------

func TestSSRF_StrictClientBlocksLoopbackAndPrivate(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	s := testSvc(t, netsafe.Client(3*time.Second)) // exactly what production uses

	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	for _, target := range []string{
		srv.URL,                          // loopback IP literal
		"http://localhost:" + port,       // name resolving to loopback (dial-time check)
		"http://10.0.0.1:" + port,        // RFC1918
		"http://169.254.169.254/latest/", // cloud metadata
		"http://[::1]:" + port,           // v6 loopback
		"http://100.64.0.1:" + port,      // CGNAT / tailscale
	} {
		slack := fmt.Sprintf(`{"webhook_url":%q}`, target)
		if err := s.fireOne(Integration{IntType: "slack", Config: slack}, AlertPayload{Title: "t"}); err == nil {
			t.Fatalf("slack to %s must be blocked", target)
		}
		jira := fmt.Sprintf(`{"base_url":%q,"project":"P"}`, target)
		if err := s.fireOne(Integration{IntType: "jira", Config: jira}, AlertPayload{Title: "t"}); err == nil {
			t.Fatalf("jira to %s must be blocked", target)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("blocked destinations must never be contacted, got %d hits", hits.Load())
	}
}

func TestSSRF_RedirectToMetadataBlocked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer srv.Close()
	// Even with loopback allowed for the first hop, the redirect hop is
	// link-local, which no allowlist can open.
	s := testSvc(t, netsafe.ClientWithAllow(3*time.Second, loopbackAllow(t)))
	cfg := fmt.Sprintf(`{"webhook_url":%q}`, srv.URL)
	if err := s.fireOne(Integration{IntType: "slack", Config: cfg}, AlertPayload{Title: "t"}); err == nil {
		t.Fatal("redirect to the metadata address must fail")
	}
}

func TestSSRF_NonHTTPSchemeRejected(t *testing.T) {
	s := testSvc(t, netsafe.Client(time.Second))
	for _, u := range []string{"file:///etc/passwd", "gopher://x/", "http://user:pw@example.com/"} {
		err := s.fireOne(Integration{IntType: "slack", Config: fmt.Sprintf(`{"webhook_url":%q}`, u)}, AlertPayload{})
		if err == nil {
			t.Fatalf("%s must be rejected", u)
		}
		if strings.Contains(err.Error(), "pw@") || strings.Contains(err.Error(), "passwd") {
			t.Fatalf("error echoes the URL: %v", err)
		}
	}
}

// ---- redaction ---------------------------------------------------------------

func TestRedaction_DeliveryErrorNeverCarriesWebhookSecret(t *testing.T) {
	const secret = "T000/B000/XXXXsuperSecretTokenXXXX"
	s := testSvc(t, netsafe.Client(2*time.Second))
	hook := "http://localhost:1/services/" + secret
	cfg := fmt.Sprintf(`{"webhook_url":%q}`, hook)
	err := s.fireOne(Integration{IntType: "slack", Config: cfg}, AlertPayload{Title: "t"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), secret) {
		t.Fatalf("precondition: raw transport error should embed the URL, got %v", err)
	}
	red := redactDeliveryError(err, cfg)
	if strings.Contains(red, secret) || strings.Contains(red, "/services/") {
		t.Fatalf("redacted error still leaks the secret: %s", red)
	}
	if !strings.Contains(red, "localhost") {
		t.Fatalf("redacted error lost the host: %s", red)
	}
}

func TestRedaction_ConfigValuesMaskedInPlainErrors(t *testing.T) {
	cfg := `{"api_token":"tok-supersecret","password":"pw-hunter22","routing_key":"rk-abcdef","base_url":"https://j.example.com"}`
	err := errors.New("remote said: bad credentials tok-supersecret / pw-hunter22 / rk-abcdef")
	red := redactDeliveryError(err, cfg)
	for _, s := range []string{"tok-supersecret", "pw-hunter22", "rk-abcdef"} {
		if strings.Contains(red, s) {
			t.Fatalf("%s leaked: %s", s, red)
		}
	}
}

func TestRedaction_DispatcherLogsCarryNoSecretsOrConfig(t *testing.T) {
	lb := &logBuf{}
	logger := slog.New(slog.NewTextHandler(lb, &slog.HandlerOptions{Level: slog.LevelDebug}))
	intg := Integration{IntegrationID: "int-1", SiteID: "site-1", IntType: "slack",
		Config: `{"webhook_url":"https://hooks.slack.com/services/SECRETSECRET"}`}
	d := newIssueDispatcher(
		func(context.Context, string) ([]Integration, error) { return []Integration{intg}, nil },
		func(_ context.Context, i Integration, _ AlertPayload) error {
			return fmt.Errorf("Post %q: boom", "https://hooks.slack.com/services/SECRETSECRET")
		}, logger, "")
	d.Notify(sampleEvent(IssueKindNew, "i1"))
	waitFor(t, "failure counted", func() bool { return d.Stats().Failed == 1 })
	if strings.Contains(lb.String(), "SECRETSECRET") || strings.Contains(lb.String(), "hooks.slack.com") {
		t.Fatalf("dispatcher log leaked destination: %s", lb.String())
	}
}

// ---- dispatcher behaviour ------------------------------------------------------

type recorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *recorder) add(s string) { r.mu.Lock(); r.calls = append(r.calls, s); r.mu.Unlock() }
func (r *recorder) n() int       { r.mu.Lock(); defer r.mu.Unlock(); return len(r.calls) }

func fixedList(intgs ...Integration) func(context.Context, string) ([]Integration, error) {
	return func(_ context.Context, site string) ([]Integration, error) {
		var out []Integration
		for _, i := range intgs {
			if i.SiteID == site {
				out = append(out, i)
			}
		}
		return out, nil
	}
}

func TestDispatcher_NewAndRegressionReachEverySiteIntegration(t *testing.T) {
	rec := &recorder{}
	d := newIssueDispatcher(
		fixedList(
			Integration{IntegrationID: "a", SiteID: "site-1", IntType: "slack"},
			Integration{IntegrationID: "b", SiteID: "site-1", IntType: "pagerduty"},
			Integration{IntegrationID: "other", SiteID: "site-2", IntType: "slack"},
		),
		func(_ context.Context, i Integration, p AlertPayload) error {
			rec.add(i.IntegrationID + ":" + p.RuleName)
			return nil
		}, quietLogger(), "")
	d.Notify(sampleEvent(IssueKindNew, "i1"))
	d.Notify(sampleEvent(IssueKindRegression, "i2"))
	waitFor(t, "4 deliveries", func() bool { return rec.n() == 4 })
	got := strings.Join(rec.calls, ",")
	for _, want := range []string{"a:new_issue", "b:new_issue", "a:regression", "b:regression"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %s in %s", want, got)
		}
	}
	if strings.Contains(got, "other") {
		t.Fatalf("another site's integration fired: %s", got)
	}
	if s := d.Stats(); s.Delivered != 4 {
		t.Fatalf("stats: %+v", s)
	}
}

func TestDispatcher_CooldownSuppressesFlapping(t *testing.T) {
	rec := &recorder{}
	d := newIssueDispatcher(
		fixedList(Integration{IntegrationID: "a", SiteID: "site-1", IntType: "slack"}),
		func(_ context.Context, i Integration, p AlertPayload) error { rec.add(p.RuleName); return nil },
		quietLogger(), "")
	clock := time.Unix(1_700_000_000, 0)
	var cm sync.Mutex
	d.now = func() time.Time { cm.Lock(); defer cm.Unlock(); return clock }

	for i := 0; i < 5; i++ { // resolve/regress flapping: five events for one issue
		d.Notify(sampleEvent(IssueKindRegression, "flappy"))
	}
	d.Notify(sampleEvent(IssueKindNew, "other-issue"))
	waitFor(t, "2 deliveries", func() bool { return rec.n() == 2 })
	time.Sleep(50 * time.Millisecond)
	if rec.n() != 2 {
		t.Fatalf("flapping issue was not collapsed: %d deliveries", rec.n())
	}
	if d.Stats().Suppressed != 4 {
		t.Fatalf("suppressed: %+v", d.Stats())
	}

	cm.Lock()
	clock = clock.Add(IssueNotifyCooldown + time.Second)
	cm.Unlock()
	d.Notify(sampleEvent(IssueKindRegression, "flappy"))
	waitFor(t, "delivery after cooldown", func() bool { return rec.n() == 3 })
}

func TestDispatcher_OverflowDropsOldestAndNeverBlocks(t *testing.T) {
	gate := make(chan struct{})
	defer close(gate)
	var listed atomic.Int32
	d := newIssueDispatcher(
		func(ctx context.Context, site string) ([]Integration, error) {
			listed.Add(1)
			select {
			case <-gate:
			case <-ctx.Done():
			}
			return nil, nil
		},
		func(context.Context, Integration, AlertPayload) error { return nil }, quietLogger(), "")

	// Occupy every worker, then flood well past the queue bound.
	for i := 0; i < IssueNotifyWorkers; i++ {
		d.Notify(sampleEvent(IssueKindNew, fmt.Sprintf("w%d", i)))
	}
	waitFor(t, "workers busy", func() bool { return int(listed.Load()) == IssueNotifyWorkers })

	start := time.Now()
	const flood = IssueNotifyQueueSize + 50
	for i := 0; i < flood; i++ {
		d.Notify(sampleEvent(IssueKindNew, fmt.Sprintf("f%d", i)))
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("Notify blocked the caller for %v with all workers busy", el)
	}
	s := d.Stats()
	if s.Pending != IssueNotifyQueueSize || s.DroppedOverflow != 50 {
		t.Fatalf("expected queue at bound with 50 dropped, got %+v", s)
	}
	d.mu.Lock()
	oldest, newest := d.q[0].IssueID, d.q[len(d.q)-1].IssueID
	d.mu.Unlock()
	if oldest != "f50" || newest != fmt.Sprintf("f%d", flood-1) {
		t.Fatalf("drop-oldest violated: oldest=%s newest=%s", oldest, newest)
	}
}

func TestDispatcher_SlowReceiverTimesOutAndFreesWorker(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	d := newIssueDispatcher(
		fixedList(Integration{IntegrationID: "a", SiteID: "site-1", IntType: "slack"}),
		func(context.Context, Integration, AlertPayload) error { <-block; return nil },
		quietLogger(), "")
	d.deliverTimeout = 50 * time.Millisecond
	d.Notify(sampleEvent(IssueKindNew, "i1"))
	waitFor(t, "timeout counted", func() bool { return d.Stats().TimedOut == 1 })
}

func TestDispatcher_PanicAndListErrorAreContained(t *testing.T) {
	d := newIssueDispatcher(
		fixedList(Integration{IntegrationID: "a", SiteID: "site-1", IntType: "slack"}),
		func(context.Context, Integration, AlertPayload) error { panic("kaboom") },
		quietLogger(), "")
	d.Notify(sampleEvent(IssueKindNew, "i1"))
	waitFor(t, "panic counted as failure", func() bool { return d.Stats().Failed == 1 })

	d2 := newIssueDispatcher(
		func(context.Context, string) ([]Integration, error) { return nil, errors.New("db down") },
		nil, quietLogger(), "")
	d2.Notify(sampleEvent(IssueKindNew, "i1"))
	waitFor(t, "list error counted", func() bool { return d2.Stats().ListErrors == 1 })
}

func TestDispatcher_PayloadSanitisesAttackerText(t *testing.T) {
	d := newIssueDispatcher(nil, nil, quietLogger(), "")
	ev := sampleEvent(IssueKindNew, "i1")
	ev.Title = "bad\r\nBcc: evil@example.com\x00" + strings.Repeat("x", 500)
	p := d.payload(ev)
	if strings.ContainsAny(p.Title, "\r\n\x00") || len([]rune(p.Title)) > 230 {
		t.Fatalf("title not sanitised: %q", p.Title)
	}
	if p.Severity != "error" {
		t.Fatalf("severity: %s", p.Severity)
	}
	ev.Level = "fatal"
	if d.payload(ev).Severity != "critical" {
		t.Fatal("fatal must map to critical")
	}
	// The payload must still pass the email header-injection check.
	if strings.ContainsAny(p.Title, "\r\n") {
		t.Fatal("CR/LF would reach the SMTP Subject")
	}
}

func TestDispatcher_ShutdownDrainsAndRefusesNew(t *testing.T) {
	rec := &recorder{}
	d := newIssueDispatcher(
		fixedList(Integration{IntegrationID: "a", SiteID: "site-1", IntType: "slack"}),
		func(context.Context, Integration, AlertPayload) error {
			time.Sleep(20 * time.Millisecond)
			rec.add("x")
			return nil
		},
		quietLogger(), "")
	d.Notify(sampleEvent(IssueKindNew, "i1"))
	if err := d.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rec.n() != 1 {
		t.Fatalf("queued delivery not drained: %d", rec.n())
	}
	d.Notify(sampleEvent(IssueKindNew, "i2"))
	if d.Stats().Enqueued != 1 {
		t.Fatal("Notify after Shutdown must be refused")
	}
}

func TestDispatcher_IgnoresIncompleteEvents(t *testing.T) {
	d := newIssueDispatcher(nil, nil, quietLogger(), "")
	d.Notify(IssueEvent{Kind: IssueKindNew})
	if d.Stats().Enqueued != 0 {
		t.Fatal("event without site/issue must be ignored")
	}
}
