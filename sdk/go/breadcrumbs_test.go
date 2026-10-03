package observe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type recorded struct {
	path string
	body map[string]any
	key  string
}

// recServer records POST bodies; /api/v1/flags/evaluate answers flagResp.
func recServer(t *testing.T, flagStatus int, flagBody string, flagDelay time.Duration) (*httptest.Server, func() []recorded) {
	t.Helper()
	var mu sync.Mutex
	var got []recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		mu.Lock()
		got = append(got, recorded{r.URL.Path, m, r.Header.Get("X-API-Key")})
		mu.Unlock()
		if r.URL.Path == "/api/v1/flags/evaluate" {
			time.Sleep(flagDelay)
			w.WriteHeader(flagStatus)
			_, _ = w.Write([]byte(flagBody))
			return
		}
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []recorded {
		mu.Lock()
		defer mu.Unlock()
		return append([]recorded(nil), got...)
	}
}

func newTestClient(t *testing.T, url string, mod func(*Options)) *Client {
	t.Helper()
	o := Options{Endpoint: url, APIKey: "k", SiteID: "s1", LogFlushInterval: time.Hour}
	if mod != nil {
		mod(&o)
	}
	c, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestBreadcrumbRingBoundedAndOrdered(t *testing.T) {
	c := newTestClient(t, "http://127.0.0.1:1", func(o *Options) { o.MaxBreadcrumbs = 3 })
	for i := 0; i < 5; i++ {
		c.AddBreadcrumb(Breadcrumb{Message: fmt.Sprintf("m%d", i)})
	}
	got := c.Breadcrumbs()
	if len(got) != 3 || got[0].Message != "m2" || got[2].Message != "m4" {
		t.Fatalf("ring = %+v", got)
	}
	d := newTestClient(t, "http://127.0.0.1:1", nil)
	for i := 0; i < 150; i++ {
		d.AddBreadcrumb(Breadcrumb{Message: "x"})
	}
	if n := len(d.Breadcrumbs()); n != 100 {
		t.Fatalf("default cap = %d, want 100", n)
	}
}

func TestBreadcrumbConcurrent(t *testing.T) {
	c := newTestClient(t, "http://127.0.0.1:1", func(o *Options) { o.MaxBreadcrumbs = 50 })
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				c.AddBreadcrumb(Breadcrumb{Message: fmt.Sprintf("%d-%d", g, i), Data: map[string]any{"i": i}})
				_ = c.Breadcrumbs()
			}
		}(g)
	}
	wg.Wait()
	if n := len(c.Breadcrumbs()); n != 50 {
		t.Fatalf("len = %d, want 50", n)
	}
}

func TestBreadcrumbNormalizationAndData(t *testing.T) {
	c := newTestClient(t, "http://127.0.0.1:1", nil)
	c.AddBreadcrumb(Breadcrumb{Message: "a", Level: "loud", Data: map[string]any{"n": 1}})
	c.AddBreadcrumb(Breadcrumb{Message: "b", Data: map[string]any{"big": strings.Repeat("y", 5000)}})
	c.AddBreadcrumb(Breadcrumb{Message: "c", Data: map[string]any{"ch": make(chan int)}})
	c.AddBreadcrumb(Breadcrumb{Message: strings.Repeat("é", 400)})
	got := c.Breadcrumbs()
	if got[0].Level != "info" || got[0].Type != "default" || got[0].Category != "default" || got[0].Timestamp < 1_600_000_000_000 {
		t.Fatalf("normalize: %+v", got[0])
	}
	if got[1].Data["_truncated"] != true {
		t.Fatalf("big data: %+v", got[1].Data)
	}
	if got[2].Data["_unserializable"] != true {
		t.Fatalf("unserializable: %+v", got[2].Data)
	}
	if len(got[3].Message) > maxBreadcrumbMessage || strings.ContainsRune(got[3].Message, 0xFFFD) {
		t.Fatalf("message truncation broke utf-8 (len %d)", len(got[3].Message))
	}
	// snapshot is a deep copy
	got[0].Data["n"] = 99
	if c.Breadcrumbs()[0].Data["n"] != float64(1) {
		t.Fatal("snapshot aliased internal data")
	}
}

func TestBeforeBreadcrumbEditDropPanic(t *testing.T) {
	c := newTestClient(t, "http://127.0.0.1:1", func(o *Options) {
		o.BeforeBreadcrumb = func(b Breadcrumb) *Breadcrumb {
			switch b.Message {
			case "drop":
				return nil
			case "panic":
				panic("hook bug")
			}
			b.Message = strings.ToUpper(b.Message)
			return &b
		}
	})
	c.AddBreadcrumb(Breadcrumb{Message: "keep"})
	c.AddBreadcrumb(Breadcrumb{Message: "drop"})
	c.AddBreadcrumb(Breadcrumb{Message: "panic"})
	got := c.Breadcrumbs()
	if len(got) != 1 || got[0].Message != "KEEP" {
		t.Fatalf("got %+v", got)
	}
}

func TestCaptureExceptionAndMessageCarryBreadcrumbs(t *testing.T) {
	srv, recs := recServer(t, 200, `{}`, 0)
	c := newTestClient(t, srv.URL, nil)
	c.AddBreadcrumb(Breadcrumb{Type: "user", Category: "checkout", Message: "clicked pay", Level: "warning", Data: map[string]any{"cart": 2}})
	if err := c.CaptureException(errors.New("boom")); err != nil {
		t.Fatal(err)
	}
	if err := c.CaptureMessage("heads up", WithLevel("warning")); err != nil {
		t.Fatal(err)
	}
	r := recs()
	if len(r) != 2 {
		t.Fatalf("posts = %d", len(r))
	}
	for i, rec := range r {
		bc, ok := rec.body["breadcrumbs"].([]any)
		if !ok || len(bc) != 1 {
			t.Fatalf("post %d breadcrumbs = %v", i, rec.body["breadcrumbs"])
		}
		b := bc[0].(map[string]any)
		for _, k := range []string{"type", "category", "message", "data", "timestamp", "level"} {
			if _, ok := b[k]; !ok {
				t.Fatalf("breadcrumb missing %q: %v", k, b)
			}
		}
		if b["message"] != "clicked pay" || b["level"] != "warning" {
			t.Fatalf("breadcrumb = %v", b)
		}
	}
	if r[1].body["level"] != "warning" || r[1].body["error_type"] != "Message" || r[1].body["error_value"] != "heads up" {
		t.Fatalf("message payload = %v", r[1].body)
	}
	c.ClearBreadcrumbs()
	_ = c.CaptureException(errors.New("again"))
	if _, has := recs()[2].body["breadcrumbs"]; has {
		t.Fatal("empty breadcrumbs must be omitted")
	}
}

func TestSlogBreadcrumbHandler(t *testing.T) {
	srv, recs := recServer(t, 200, `{}`, 0)
	c := newTestClient(t, srv.URL, nil)
	l := slog.New(c.NewSlogBreadcrumbHandler(slog.LevelWarn, nil))
	l.Info("below threshold")
	l.Warn("slow query", "ms", 420, "err", errors.New("timeout"), slog.Group("db", slog.String("name", "main")))
	l.Error("failed")
	got := c.Breadcrumbs()
	if len(got) != 2 {
		t.Fatalf("crumbs = %+v", got)
	}
	if got[0].Message != "slow query" || got[0].Level != "warning" || got[0].Type != "log" {
		t.Fatalf("crumb0 = %+v", got[0])
	}
	if got[0].Data["err"] != "timeout" || got[0].Data["db.name"] != "main" || got[0].Data["ms"] != float64(420) {
		t.Fatalf("data = %+v", got[0].Data)
	}
	if got[1].Level != "error" {
		t.Fatalf("crumb1 = %+v", got[1])
	}
	// breadcrumb-only handler must not ship logs
	if err := c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, r := range recs() {
		if strings.HasPrefix(r.path, "/api/v1/logs") {
			t.Fatalf("breadcrumb-only handler shipped a log: %v", r.path)
		}
	}
}

func TestSlogMirrorWithBreadcrumbs(t *testing.T) {
	srv, recs := recServer(t, 200, `{}`, 0)
	c := newTestClient(t, srv.URL, nil)
	h := c.NewSlogHandler(slog.LevelError, nil).WithBreadcrumbs(slog.LevelInfo)
	l := slog.New(h)
	l.Info("step one")
	l.Error("oops")
	if n := len(c.Breadcrumbs()); n != 2 {
		t.Fatalf("crumbs = %d", n)
	}
	if err := c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	logs := 0
	for _, r := range recs() {
		if r.path == "/api/v1/logs/batch" {
			logs++
		}
	}
	if logs != 1 {
		t.Fatalf("log batches = %d, want 1 (only the error mirrored)", logs)
	}
}

func TestEvaluateFlagContract(t *testing.T) {
	srv, recs := recServer(t, 200, `{"enabled":true,"variant":"b","reason":"evaluated"}`, 0)
	c := newTestClient(t, srv.URL, nil)
	r := c.EvaluateFlag(context.Background(), "new-checkout", WithFlagUser("u1"), WithFlagAttributes(map[string]string{"plan": "pro"}))
	if !r.Enabled || r.Variant != "b" || r.Source != "server" || r.Err != nil {
		t.Fatalf("result = %+v", r)
	}
	got := recs()[0]
	if got.path != "/api/v1/flags/evaluate" || got.key != "k" {
		t.Fatalf("req = %+v", got)
	}
	want := map[string]any{"site_id": "s1", "flag_key": "new-checkout", "user_id": "u1"}
	for k, v := range want {
		if got.body[k] != v {
			t.Fatalf("body[%s] = %v", k, got.body[k])
		}
	}
	if got.body["context"].(map[string]any)["plan"] != "pro" {
		t.Fatalf("context = %v", got.body["context"])
	}
	if len(recs()) != 1 {
		t.Fatal("flag evaluation must send no other telemetry")
	}
}

func TestEvaluateFlagFailuresUseDefault(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"500", 500, `{"error":"x"}`},
		{"garbage", 200, `not json`},
		{"missing enabled", 200, `{"reason":"evaluated"}`},
		{"unavailable", 200, `{"enabled":false,"reason":"unavailable"}`},
		{"invalid", 200, `{"enabled":false,"reason":"invalid"}`},
		{"redirect", 302, ``},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := recServer(t, tc.status, tc.body, 0)
			c := newTestClient(t, srv.URL, nil)
			r := c.EvaluateFlag(context.Background(), "f", WithFlagDefault(FlagDefault{Enabled: true, Variant: "safe"}))
			if !r.Enabled || r.Variant != "safe" || r.Source != "default" || r.Err == nil {
				t.Fatalf("result = %+v", r)
			}
			if d := c.EvaluateFlag(context.Background(), "f"); d.Enabled {
				t.Fatalf("no default must fail safe disabled: %+v", d)
			}
		})
	}
}

func TestEvaluateFlagTimeoutAndUnreachable(t *testing.T) {
	srv, _ := recServer(t, 200, `{"enabled":true,"reason":"evaluated"}`, 1500*time.Millisecond)
	c := newTestClient(t, srv.URL, nil)
	start := time.Now()
	r := c.EvaluateFlag(context.Background(), "f", WithFlagTimeout(150*time.Millisecond), WithFlagDefault(FlagDefault{Enabled: true}))
	if time.Since(start) > time.Second || r.Source != "default" || !r.Enabled {
		t.Fatalf("timeout result = %+v after %v", r, time.Since(start))
	}
	dead := newTestClient(t, "http://127.0.0.1:1", nil)
	if r := dead.EvaluateFlag(context.Background(), "f", WithFlagTimeout(500*time.Millisecond)); r.Source != "default" || r.Err == nil {
		t.Fatalf("unreachable = %+v", r)
	}
	if r := dead.EvaluateFlag(nil, ""); r.Source != "default" { //nolint:staticcheck // nil ctx is tolerated by design
		t.Fatalf("empty key = %+v", r)
	}
}
