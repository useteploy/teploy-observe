package platform

// EV1 unit tests for the issue alert surface: the webhook `kinds` filter
// (the negative control that keeps issue traffic opt-in), the Slack
// rendering (never a stack), the payload builder, and the emitter's
// bounded-queue drop. The database-backed end-to-end lives in
// cmd/observe (issue_alerts_e2e_test.go), which can wire the real
// ingest path through the real wiring functions.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// A webhook with no kinds filter receives NO issue kinds - the opt-in is
// the whole point of the filter.
func TestMatchesKind_EmptyFilterOptsOutOfIssueKinds(t *testing.T) {
	for _, kind := range []string{NotifyIssueNew, NotifyIssueRegression, NotifyIssueSpike} {
		if MatchesKind("", kind) {
			t.Errorf("an empty kinds filter must not receive %s", kind)
		}
	}
}

// ...and that same empty filter keeps every pre-066 kind, so no existing
// webhook changes behavior when the column ships.
func TestMatchesKind_EmptyFilterKeepsLegacyKinds(t *testing.T) {
	for _, kind := range []string{NotifyIncidentOpened, NotifyIncidentRecovered, NotifyIncidentRepeat, NotifyCronMissed, NotifyCronRecovered} {
		if !MatchesKind("", kind) {
			t.Errorf("an empty kinds filter lost the pre-066 kind %s", kind)
		}
	}
}

// An explicit filter is exact: listed kinds in, everything else out -
// including the legacy kinds, which is the operator's stated intent.
func TestMatchesKind_ExplicitFilterIsExact(t *testing.T) {
	kinds := "issue_new,issue_regression"
	for _, kind := range []string{NotifyIssueNew, NotifyIssueRegression} {
		if !MatchesKind(kinds, kind) {
			t.Errorf("%s listed but not matched", kind)
		}
	}
	for _, kind := range []string{NotifyIssueSpike, NotifyIncidentOpened, NotifyCronMissed} {
		if MatchesKind(kinds, kind) {
			t.Errorf("%s matched without being listed", kind)
		}
	}
}

// NormalizeKinds: valid lists canonicalize, unknown entries are refused
// (a typo must fail at SAVE time, not silently never match).
func TestNormalizeKinds(t *testing.T) {
	if got, err := NormalizeKinds(" Issue_New , issue_spike,"); err != nil || got != "issue_new,issue_spike" {
		t.Fatalf("normalize: %q %v", got, err)
	}
	if got, err := NormalizeKinds("  "); err != nil || got != "" {
		t.Fatalf("empty: %q %v", got, err)
	}
	if _, err := NormalizeKinds("issue_new,issue_typo"); err == nil {
		t.Fatal("an unknown kind was accepted")
	}
	// The family shorthand is deliberately NOT a kind: exact names only,
	// so the domain stays greppable.
	if _, err := NormalizeKinds("issue_*"); err == nil {
		t.Fatal("the issue_* shorthand was accepted - kinds are exact")
	}
}

// Slack rendering: the stack never crosses the wire to a chat target.
func TestSlackIssueText_NeverContainsStack(t *testing.T) {
	p := IssueNotification{
		Kind: NotifyIssueNew, IssueID: "iss-1", SiteID: "site-1",
		Title: "TypeError: x is not a function", Culprit: "app.ts:42",
		Level: "error", Release: "rel-9", Environment: "production",
		EventCount: 3, UserCount: 2,
		SampleStack: "app/secret-path/deep.ts:42 handleToken Authorization=Bearer eyJhbGciOi.abc.def",
	}
	text, err := slackIssueText(mustJSON(t, p))
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{"sample", "deep.ts", "handleToken", "Bearer", "eyJhbGciOi"} {
		if strings.Contains(text, leaked) {
			t.Fatalf("slack text leaked %q: %s", leaked, text)
		}
	}
	for _, want := range []string{"New issue", "TypeError: x is not a function", "site-1", "rel-9", "production", "Events: 3"} {
		if !strings.Contains(text, want) {
			t.Fatalf("slack text missing %q: %s", want, text)
		}
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// BLOCKER regression (security review B1): Title, Culprit, Release and
// Environment are attacker-influenced through the public browser ingest
// key. Raw into Slack text they carried live control syntax - a title of
// "<!channel>" pings the whole channel, "<https://evil.example|x>" renders
// as a disguised link, and both must arrive HTML-escaped instead. The
// escape used is the SAME one the integrations path uses (exported
// SlackEscape), not a second implementation.
func TestSlackIssueText_EscapesSlackControlSequences(t *testing.T) {
	p := IssueNotification{
		Kind: NotifyIssueNew, IssueID: "iss-1", SiteID: "site-1",
		Title:   `Boom <!channel> <https://evil.example|click me> <@U123>`,
		Culprit: `app.ts:42 <https://evil.example|culprit>`,
		Level:   "error", Release: "rel-<https://evil.example|rel>", Environment: "prod<!here>",
		EventCount: 1, UserCount: 1,
	}
	text, err := slackIssueText(mustJSON(t, p))
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		"<!channel>", "<https://evil.example", "<!here>", "<@U123>",
	} {
		if strings.Contains(text, raw) {
			t.Fatalf("slack text carries live control syntax %q: %s", raw, text)
		}
	}
	for _, want := range []string{
		"&lt;!channel&gt;",
		"&lt;https://evil.example|click me&gt;",
		"&lt;https://evil.example|culprit&gt;",
		"&lt;https://evil.example|rel&gt;",
		"prod&lt;!here&gt;",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("slack text missing escaped form %q: %s", want, text)
		}
	}
}

// The other half of B1: a very long title must not reach Slack. Slack
// rejects oversized messages, the outbox would burn its retry budget and
// dead-letter the alert - so every field is capped (title at 300 runes,
// the same budget fireSlack gives a headline).
func TestSlackIssueText_CapsAttackerSizedFields(t *testing.T) {
	p := IssueNotification{
		Kind: NotifyIssueSpike, IssueID: "iss-1", SiteID: "site-1",
		Title:   strings.Repeat("A", 10*1024), // a 10 KB title
		Culprit: strings.Repeat("c", 10*1024),
		Level:   "error", Release: strings.Repeat("r", 10*1024), Environment: strings.Repeat("e", 10*1024),
		EventCount: 1, UserCount: 1, WindowCount: 3,
	}
	text, err := slackIssueText(mustJSON(t, p))
	if err != nil {
		t.Fatal(err)
	}
	if got := len([]rune(text)); got > 2000 {
		t.Fatalf("slack text is %d runes - a 10 KB title pushed it past Slack's message budget", got)
	}
	if !strings.Contains(text, "AAA") || !strings.HasSuffix(strings.SplitN(text, "\n", 2)[0], "...*") {
		t.Fatalf("title not capped with the ellipsis marker: %.80q...", text)
	}
	if strings.Contains(text, strings.Repeat("c", 400)) || strings.Contains(text, strings.Repeat("r", 200)) {
		t.Fatal("culprit/release not capped")
	}
}

// ClipStack: byte cap honored, partial line dropped, valid UTF-8.
func TestClipStack(t *testing.T) {
	short := "a.ts:1 f\nb.ts:2 g"
	if got := ClipStack(short); got != short {
		t.Fatalf("short stack modified: %q", got)
	}
	long := strings.Repeat("帧-frame-with-multibyte-runes-"+strings.Repeat("x", 60)+"\n", 80)
	got := ClipStack(long)
	if len(got) > 2000+len("\n[clipped]") {
		t.Fatalf("clipped stack is %d bytes", len(got))
	}
	if !strings.Contains(got, "[clipped]") {
		t.Fatal("clip marker missing")
	}
}

func testEmitter(t *testing.T, hooks func(context.Context, string) ([]Webhook, error)) *IssueAlertEmitter {
	t.Helper()
	t.Setenv("OBSERVE_ISSUE_ALERT_QUEUE", "2")
	t.Setenv("OBSERVE_ISSUE_ALERT_SETTLE_MS", "0")
	// A zero-value Notifier satisfies Notify's non-nil guard; the drain is
	// never started in these tests, so it is never used.
	return NewIssueAlertEmitter(&Notifier{}, hooks, nil, "https://observe.example", slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func sampleIssueAlertEvent(kind string) IssueAlertEvent {
	return IssueAlertEvent{
		Kind: kind, IssueID: "iss-1", SiteID: "site-1", Fingerprint: "gh-1",
		Title: "Boom", Culprit: "app.ts:1", Level: "error", Release: "rel-1",
		Environment: "production", SampleStack: "app.ts:1 boom", At: time.Now().UTC(),
	}
}

// The negative control for routing: a webhook without issue kinds in its
// filter is never routed an issue event; one with the kind is; a
// severity filter still applies on top.
func TestIssueAlertEmitter_RouteHooksFiltersByKindAndSeverity(t *testing.T) {
	hooks := func(context.Context, string) ([]Webhook, error) {
		return []Webhook{
			{WebhookID: "plain", WebhookType: "http", URL: "https://a.example", Kinds: ""},
			{WebhookID: "opted", WebhookType: "http", URL: "https://b.example", Kinds: "issue_new,issue_regression"},
			{WebhookID: "crit", WebhookType: "http", URL: "https://c.example", Kinds: "issue_new", Severities: "critical"},
			{WebhookID: "slack", WebhookType: "slack", URL: "https://d.example", Kinds: "issue_new"},
		}, nil
	}
	e := testEmitter(t, hooks)

	got := e.routeHooks(context.Background(), sampleIssueAlertEvent(NotifyIssueNew))
	if len(got) != 2 {
		t.Fatalf("routed %d hooks, want 2 (opted + slack; plain has no issue kinds, crit wants critical severity)", len(got))
	}
	if got[0].WebhookID != "opted" || got[1].WebhookID != "slack" {
		t.Fatalf("routed hooks: %+v", got)
	}
}

// The payload builder: shape, snapshot re-read values, service default,
// stack re-clip.
func TestIssueAlertEmitter_BuildPayload(t *testing.T) {
	e := testEmitter(t, nil)
	ev := sampleIssueAlertEvent(NotifyIssueRegression)
	ev.SampleStack = strings.Repeat("l\n", 1500) // > 2 KB after build
	snap := &IssueSnapshot{
		IssueID: "iss-1", SiteID: "site-1", GroupHash: "gh-real", Title: "Boom (real)",
		Level: "error", Status: "open", EventCount: 41, UserCount: 7, RegressionCount: 2,
		ReleaseTag: "rel-0", FingerprintVersion: 1,
		FirstSeen: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		LastSeen:  time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC),
	}
	var p IssueNotification
	if err := json.Unmarshal([]byte(e.buildPayload(ev, snap)), &p); err != nil {
		t.Fatal(err)
	}
	if p.Kind != NotifyIssueRegression || p.IssueID != "iss-1" || p.SiteID != "site-1" {
		t.Fatalf("identity: %+v", p)
	}
	if p.Service != "site-1" {
		t.Fatalf("service default: %q", p.Service)
	}
	if p.Fingerprint != "gh-real" || p.EventCount != 41 || p.UserCount != 7 || p.RegressionCount != 2 {
		t.Fatalf("snapshot fields: %+v", p)
	}
	if p.FirstRelease != "rel-0" || p.Release != "rel-1" {
		t.Fatalf("release pair: %q / %q", p.FirstRelease, p.Release)
	}
	if p.FirstSeen != "2026-10-01T00:00:00Z" || p.LastSeen != "2026-10-17T12:00:00Z" {
		t.Fatalf("seen window: %q %q", p.FirstSeen, p.LastSeen)
	}
	if len(p.SampleStack) > 2000 {
		t.Fatalf("stack not re-clipped: %d bytes", len(p.SampleStack))
	}
	if p.IssueURL != "https://observe.example/errors" {
		t.Fatalf("issue url: %q", p.IssueURL)
	}
	// delivery_seq is per issue and monotonic across kinds.
	if p.DeliverySeq != 1 {
		t.Fatalf("first seq = %d, want 1", p.DeliverySeq)
	}
	var q IssueNotification
	if err := json.Unmarshal([]byte(e.buildPayload(sampleIssueAlertEvent(NotifyIssueNew), snap)), &q); err != nil {
		t.Fatal(err)
	}
	if q.DeliverySeq != 2 {
		t.Fatalf("second seq = %d, want 2", q.DeliverySeq)
	}
}

// The load-bearing ingest guarantee: a full queue drops and counts rather
// than blocking. Notify must stay non-blocking even with the drain never
// running.
func TestIssueAlertEmitter_FullQueueDropsAndCountsNeverBlocks(t *testing.T) {
	e := testEmitter(t, nil) // queue capacity 2, drain NOT started
	// Fill, then hammer well past capacity from several goroutines: every
	// call must return immediately (no blocking send), drops must equal
	// the overflow exactly, and the queued two must be intact.
	e.Notify(sampleIssueAlertEvent(NotifyIssueNew))
	e.Notify(sampleIssueAlertEvent(NotifyIssueNew))
	var wg sync.WaitGroup
	start := time.Now()
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				e.Notify(sampleIssueAlertEvent(NotifyIssueSpike))
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)
	if elapsed > time.Second {
		t.Fatalf("Notify blocked for %v - ingest must never wait on alerts", elapsed)
	}
	if got := e.Stats().Dropped; got != 200 {
		t.Fatalf("dropped = %d, want 200", got)
	}
	if got := e.Stats().QueueDepth; got != 2 {
		t.Fatalf("queue depth = %d, want 2 (the originally queued pair)", got)
	}
	// Unknown kinds are ignored outright, not dropped.
	e.Notify(IssueAlertEvent{Kind: NotifyIncidentOpened, IssueID: "x"})
	if got := e.Stats().Dropped; got != 200 {
		t.Fatalf("non-issue kind changed the drop count: %d", got)
	}
}

// --- Drain settle discipline (SF1, review round 2) ---

// drainTestEmitter builds an emitter whose enqueue path counts intents
// in memory (no store), with one always-routing hook.
func drainTestEmitter(t *testing.T, settleMs int, hooks func(context.Context, string) ([]Webhook, error)) (*IssueAlertEmitter, *int) {
	t.Helper()
	t.Setenv("OBSERVE_ISSUE_ALERT_QUEUE", "1024")
	t.Setenv("OBSERVE_ISSUE_ALERT_SETTLE_MS", strconv.Itoa(settleMs))
	e := NewIssueAlertEmitter(&Notifier{}, hooks, nil, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	enqueued := 0
	e.enqueueIntent = func(context.Context, NotificationIntent) (string, error) {
		enqueued++
		return "nfy-test", nil
	}
	return e, &enqueued
}

func oneHook(context.Context, string) ([]Webhook, error) {
	return []Webhook{{WebhookID: "w1", WebhookType: "http", URL: "https://sink.example", Kinds: "issue_new"}}, nil
}

// The review's headline number: 100 queued events whose settle window has
// already elapsed must drain in ~zero time, not 100 x settle (the old
// drain slept the FULL settle delay per event serially - a 1024-deep
// burst delayed every site behind it by ~34 minutes at 2s settle).
func TestIssueAlertEmitter_BacklogDrainsWithoutPerEventSettle(t *testing.T) {
	const settleMs = 2000
	e, enqueued := drainTestEmitter(t, settleMs, oneHook)
	past := time.Now().UTC().Add(-time.Minute) // settle long elapsed
	start := time.Now()
	const n = 100
	for i := 0; i < n; i++ {
		ev := sampleIssueAlertEvent(NotifyIssueNew)
		ev.At = past
		e.deliver(context.Background(), ev)
	}
	elapsed := time.Since(start)
	if *enqueued != n {
		t.Fatalf("enqueued %d intents, want %d", *enqueued, n)
	}
	// 100 x settle would be 200s; anything under 3 x settle (6s, generous
	// CI headroom over the microseconds this actually takes) proves the
	// per-event sleep is gone.
	if elapsed > time.Duration(3*settleMs)*time.Millisecond {
		t.Fatalf("draining %d settled events took %v - the drain is still sleeping per event (budget %v)", n, elapsed, 3*settleMs*time.Millisecond)
	}
}

// An event with NO opted-in webhook must not consume a settle wait at
// all: most events on most sites route nowhere, and parking the single
// drain goroutine on them was pure added latency for every other site.
func TestIssueAlertEmitter_UnroutedEventSkipsTheSettleWait(t *testing.T) {
	e, enqueued := drainTestEmitter(t, 3000, func(context.Context, string) ([]Webhook, error) {
		return nil, nil
	})
	ev := sampleIssueAlertEvent(NotifyIssueNew) // At = now: not yet settled
	start := time.Now()
	e.deliver(context.Background(), ev)
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("unrouted event waited %v - routing must happen BEFORE the settle wait", elapsed)
	}
	if *enqueued != 0 {
		t.Fatalf("unrouted event enqueued %d intents", *enqueued)
	}
}

// The settle still happens for a routed, not-yet-settled event: the
// re-read of the issue needs the triggering event's row to have landed.
func TestIssueAlertEmitter_RoutedEventWaitsForItsOwnDeadline(t *testing.T) {
	e, enqueued := drainTestEmitter(t, 250, oneHook)
	ev := sampleIssueAlertEvent(NotifyIssueNew) // At = now: deadline = now+250ms
	start := time.Now()
	e.deliver(context.Background(), ev)
	if elapsed := time.Since(start); elapsed < 200*time.Millisecond {
		t.Fatalf("routed event returned after %v - the settle wait was skipped", elapsed)
	}
	if *enqueued != 1 {
		t.Fatalf("enqueued %d intents, want 1", *enqueued)
	}
}
