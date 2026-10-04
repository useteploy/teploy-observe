package sentrycompat

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/useteploy/teploy-observe/internal/auth"
	obserrors "github.com/useteploy/teploy-observe/internal/errors"
)

type fakeKeys struct{}

func (fakeKeys) ValidateAPIKey(_ context.Context, key string) (auth.ValidatedKey, error) {
	switch key {
	case "obs_siteA":
		return auth.ValidatedKey{SiteID: "siteaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Scopes: "telemetry"}, nil
	case "obs_siteB":
		return auth.ValidatedKey{SiteID: "sitebbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Scopes: "telemetry"}, nil
	case "obs_publish":
		return auth.ValidatedKey{SiteID: "siteaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Scopes: "publish"}, nil
	case "obs_down":
		return auth.ValidatedKey{}, fmt.Errorf("%w: boom", auth.ErrAuthUnavailable)
	}
	return auth.ValidatedKey{}, errors.New("auth: invalid api key")
}

const siteA = "siteaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const siteB = "sitebbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

type sinkRec struct {
	site string
	in   obserrors.ErrorInput
}

// fakeSink mimics the buffer's idempotency: same (site,event_id) twice is a
// duplicate.
type fakeSink struct {
	mu   sync.Mutex
	seen map[string]bool
	got  []sinkRec
	err  error
}

func (s *fakeSink) Push(site string, in obserrors.ErrorInput) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	k := site + "|" + in.ProducerID + "|" + in.EventID
	if s.seen == nil {
		s.seen = map[string]bool{}
	}
	if s.seen[k] {
		return obserrors.ErrAdmittedDuplicate
	}
	s.seen[k] = true
	s.got = append(s.got, sinkRec{site, in})
	return nil
}

type denyLimiter struct{ deny bool }

func (d denyLimiter) Allow(string, string) bool { return !d.deny }

func newH(sink *fakeSink, lim Limiter) http.Handler {
	h := &Handler{Keys: fakeKeys{}, Sink: sink, Limiter: lim}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/{project_id}/envelope/{$}", h.Envelope)
	mux.HandleFunc("POST /api/{project_id}/envelope", h.Envelope)
	mux.HandleFunc("POST /api/{project_id}/store/{$}", h.Store)
	return mux
}

func post(h http.Handler, path string, body []byte, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, bytes.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func gz(t testing.TB, b []byte) []byte {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	w.Write(b)
	w.Close()
	return buf.Bytes()
}

func envelope(items ...string) []byte {
	return []byte(`{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc","sent_at":"2026-01-01T00:00:00Z"}` + "\n" + strings.Join(items, "\n") + "\n")
}

func evItem(payload string) string {
	return fmt.Sprintf(`{"type":"event","length":%d}`, len(payload)) + "\n" + payload
}

// ---- real-shaped fixtures (hand-written replicas of SDK output) ----

const pyEvent = `{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc","level":"error","platform":"python","release":"app@1.2.3","environment":"production","server_name":"web-1","timestamp":"2026-01-01T00:00:00.000000Z","sdk":{"name":"sentry.python","version":"2.8.0"},"user":{"id":"u-42","email":"a@b.co","ip_address":"1.2.3.4"},"tags":{"region":"eu"},"fingerprint":["{{ default }}"],"exception":{"values":[{"type":"ZeroDivisionError","value":"division by zero","mechanism":{"type":"excepthook","handled":false},"stacktrace":{"frames":[{"filename":"app/main.py","abs_path":"/srv/app/main.py","function":"<module>","lineno":10,"in_app":true},{"filename":"app/calc.py","abs_path":"/srv/app/calc.py","function":"div","lineno":3,"in_app":true},{"filename":"requests/api.py","abs_path":"/usr/lib/python3/site-packages/requests/api.py","function":"get","lineno":5,"in_app":false}]}}]},"breadcrumbs":{"values":[{"type":"log","category":"app","message":"started","level":"info","timestamp":1767225600.5}]},"contexts":{"runtime":{"name":"CPython","version":"3.12"},"trace":{"trace_id":"771a43a4192642f0b136d5159a501700","span_id":"a1b2c3d4e5f60708"}}}`

const browserEvent = `{"event_id":"11111111111111111111111111111111","platform":"javascript","level":"error","release":"web@9","request":{"url":"https://app.example.com/page?token=SECRET#x","headers":{"User-Agent":"x"}},"exception":{"values":[{"type":"TypeError","value":"x is undefined","mechanism":{"type":"onerror","handled":false},"stacktrace":{"frames":[{"filename":"https://app.example.com/static/vendor.js","function":"a","lineno":1,"colno":100},{"filename":"https://app.example.com/static/app.js","function":"onClick","lineno":1,"colno":2000}]}}]},"breadcrumbs":[{"category":"ui.click","message":"button","timestamp":1767225600.1}],"contexts":{"browser":{"name":"Chrome","version":"120"},"os":{"name":"Windows","version":"11"}},"user":{"id":7}}`

const nodeEvent = `{"event_id":"22222222222222222222222222222222","platform":"node","level":"fatal","exception":{"values":[{"type":"Error","value":"boom","stacktrace":{"frames":[{"filename":"/app/node_modules/express/lib/router.js","function":"handle","lineno":1,"in_app":false},{"filename":"/app/server.js","function":"handler","lineno":42,"colno":9,"in_app":true}]}}]}}`

const goEvent = `{"event_id":"33333333333333333333333333333333","platform":"go","level":"error","message":"plain message","exception":[{"type":"*errors.errorString","value":"oops","stacktrace":{"frames":[{"function":"main.main","module":"main","abs_path":"/src/app/main.go","lineno":12,"in_app":true}]}}]}`

const sessionPayload = `{"sid":"a1b2c3","did":"u","init":true,"started":"2026-01-01T00:00:00Z","status":"ok","attrs":{"release":"x"}}`
const txPayload = `{"type":"transaction","transaction":"/home","spans":[],"start_timestamp":1,"timestamp":2}`

func TestRealFixturesMapped(t *testing.T) {
	sink := &fakeSink{}
	h := newH(sink, nil)
	env := envelope(evItem(pyEvent), evItem(browserEvent), evItem(nodeEvent), evItem(goEvent),
		`{"type":"session"}`+"\n"+sessionPayload, `{"type":"transaction"}`+"\n"+txPayload)
	rr := post(h, "/api/1/envelope/?sentry_key=obs_siteA", env, nil)
	if rr.Code != 200 {
		t.Fatalf("code %d %s", rr.Code, rr.Body)
	}
	if len(sink.got) != 4 {
		t.Fatalf("pushed %d", len(sink.got))
	}
	py := sink.got[0].in
	if sink.got[0].site != siteA || py.SiteID != siteA {
		t.Fatal("site not bound to key")
	}
	if py.ErrorType != "ZeroDivisionError" || py.Handled || py.Mechanism != "excepthook" || py.ReleaseTag != "app@1.2.3" {
		t.Fatalf("py: %+v", py)
	}
	// innermost first: library frame first (reversed), in_app preserved
	if len(py.StackTrace) != 3 || py.StackTrace[0].Filename != "requests/api.py" || py.StackTrace[0].InApp || !py.StackTrace[2].InApp || py.StackTrace[2].Function != "<module>" {
		t.Fatalf("py frames: %+v", py.StackTrace)
	}
	if py.DistinctID != "u-42" || py.EventID != "9ec79c33ec9942ab8353589fcb2e04dc" || py.ProducerID != "sentry" {
		t.Fatalf("identity: %+v", py)
	}
	if len(py.Fingerprint) != 0 {
		t.Fatal("{{ default }} fingerprint must fall back")
	}
	if py.TraceID != "771a43a4192642f0b136d5159a501700" || len(py.Breadcrumbs) != 1 || py.Breadcrumbs[0].Timestamp != 1767225600500 {
		t.Fatalf("trace/crumbs: %+v", py)
	}
	raw, _ := json.Marshal(py.Contexts)
	if strings.Contains(string(raw), "a@b.co") || strings.Contains(string(raw), "1.2.3.4") {
		t.Fatalf("raw user data leaked into contexts: %s", raw)
	}
	br := sink.got[1].in
	if br.Browser != "Chrome 120" || br.OS != "Windows 11" || br.DistinctID != "7" || len(br.Breadcrumbs) != 1 {
		t.Fatalf("browser: %+v", br)
	}
	if !br.StackTrace[0].InApp { // browser frames lack in_app -> inferred
		t.Fatalf("browser in_app not inferred: %+v", br.StackTrace)
	}
	nd := sink.got[2].in
	if nd.Level != "fatal" || nd.StackTrace[0].Function != "handler" || !nd.StackTrace[0].InApp || nd.StackTrace[1].InApp {
		t.Fatalf("node: %+v", nd)
	}
	g := sink.got[3].in
	if g.ErrorType != "*errors.errorString" || g.StackTrace[0].Filename != "/src/app/main.go" {
		t.Fatalf("go: %+v", g)
	}
	st := h.(*http.ServeMux)
	_ = st
}

func TestIgnoredTypesCounted(t *testing.T) {
	sink := &fakeSink{}
	hd := &Handler{Keys: fakeKeys{}, Sink: sink}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/{project_id}/envelope/{$}", hd.Envelope)
	env := envelope(`{"type":"session"}`+"\n"+sessionPayload, `{"type":"transaction"}`+"\n"+txPayload,
		`{"type":"weird_made_up_type_1"}`+"\n{}", `{"type":"weird_made_up_type_2"}`+"\n{}", `{"type":"check_in"}`+"\n{}")
	rr := post(mux, "/api/"+siteA+"/envelope/", env, map[string]string{"X-Sentry-Auth": "Sentry sentry_version=7, sentry_key=obs_siteA, sentry_client=x/1"})
	if rr.Code != 200 || len(sink.got) != 0 {
		t.Fatalf("code %d pushed %d", rr.Code, len(sink.got))
	}
	s := hd.Stats()
	if s.IgnoredByType["session"] != 1 || s.IgnoredByType["transaction"] != 1 || s.IgnoredByType["unknown"] != 2 || s.IgnoredByType["check_in"] != 1 {
		t.Fatalf("stats %+v", s)
	}
	if len(s.IgnoredByType) != 4 {
		t.Fatal("type cardinality not bounded")
	}
}

func TestAuth(t *testing.T) {
	sink := &fakeSink{}
	h := newH(sink, nil)
	body := envelope(evItem(nodeEvent))
	cases := []struct {
		name string
		path string
		hdr  map[string]string
		want int
	}{
		{"missing key", "/api/1/envelope/", nil, 401},
		{"wrong key", "/api/1/envelope/?sentry_key=nope", nil, 401},
		{"publish-only key", "/api/1/envelope/?sentry_key=obs_publish", nil, 403},
		{"auth store down", "/api/1/envelope/?sentry_key=obs_down", nil, 503},
		{"key A project B", "/api/" + siteB + "/envelope/?sentry_key=obs_siteA", nil, 403},
		{"key B project A", "/api/" + siteA + "/envelope/", map[string]string{"X-Sentry-Auth": "Sentry sentry_key=obs_siteB"}, 403},
		{"project traversal-ish", "/api/..%2f" + siteB + "/envelope/?sentry_key=obs_siteA", nil, 403},
		{"ok numeric alias", "/api/1/envelope/?sentry_key=obs_siteA", nil, 200},
		{"ok own site", "/api/" + siteA + "/envelope?sentry_key=obs_siteA", nil, 200},
		{"empty key param", "/api/1/envelope/?sentry_key=", nil, 401},
		{"garbled auth header", "/api/1/envelope/", map[string]string{"X-Sentry-Auth": "Sentry sentry_key"}, 401},
	}
	for _, c := range cases {
		sink.got = nil
		sink.seen = nil
		rr := post(h, c.path, body, c.hdr)
		if rr.Code != c.want {
			t.Errorf("%s: got %d want %d (%s)", c.name, rr.Code, c.want, rr.Body)
		}
		if c.want != 200 && len(sink.got) != 0 {
			t.Errorf("%s: wrote despite failure", c.name)
		}
		if strings.Contains(rr.Body.String(), "obs_") {
			t.Errorf("%s: key echoed", c.name)
		}
	}
}

func TestAuthBeforeParse(t *testing.T) {
	sink := &fakeSink{}
	h := newH(sink, nil)
	// A bomb with a bad key: must be 401, not 413 (the body is never decoded).
	rr := post(h, "/api/1/envelope/?sentry_key=bad", gz(t, bytes.Repeat([]byte("a"), 8<<20)), map[string]string{"Content-Encoding": "gzip"})
	if rr.Code != 401 {
		t.Fatalf("got %d", rr.Code)
	}
}

func TestDSNInEnvelopeHeader(t *testing.T) {
	sink := &fakeSink{}
	h := newH(sink, nil)
	env := []byte(`{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc","dsn":"https://obs_siteA@observe.example.com/1"}` + "\n" + evItem(nodeEvent) + "\n")
	if rr := post(h, "/api/1/envelope/", env, nil); rr.Code != 200 || len(sink.got) != 1 {
		t.Fatalf("code %d pushed %d", rr.Code, len(sink.got))
	}
	// gzip body, DSN of a bad key
	env2 := []byte(`{"dsn":"https://nope@h/1"}` + "\n" + evItem(nodeEvent))
	if rr := post(h, "/api/1/envelope/", gz(t, env2), map[string]string{"Content-Encoding": "gzip"}); rr.Code != 401 {
		t.Fatalf("got %d", rr.Code)
	}
	// header key site A, DSN in header for B is ignored (query/header key wins)
	sink.seen = nil
	env3 := []byte(`{"dsn":"https://obs_siteB@h/1"}` + "\n" + evItem(nodeEvent))
	post(h, "/api/1/envelope/?sentry_key=obs_siteA", env3, nil)
	if sink.got[len(sink.got)-1].site != siteA {
		t.Fatal("header DSN overrode explicit key")
	}
}

func TestEncodings(t *testing.T) {
	sink := &fakeSink{}
	h := newH(sink, nil)
	env := envelope(evItem(nodeEvent))
	var zl bytes.Buffer
	zw := zlib.NewWriter(&zl)
	zw.Write(env)
	zw.Close()
	var raw bytes.Buffer
	fw, _ := flate.NewWriter(&raw, 6)
	fw.Write(env)
	fw.Close()
	cases := []struct {
		name string
		body []byte
		enc  string
		want int
	}{
		{"gzip", gz(t, env), "gzip", 200},
		{"gzip sniffed", gz(t, env), "", 200},
		{"deflate zlib", zl.Bytes(), "deflate", 200},
		{"deflate raw", raw.Bytes(), "deflate", 200},
		{"truncated gzip", gz(t, env)[:20], "gzip", 400},
		{"garbage gzip", []byte("not gzip at all"), "gzip", 400},
		{"unsupported br", env, "br", 415},
	}
	for _, c := range cases {
		sink.seen = nil
		hdr := map[string]string{}
		if c.enc != "" {
			hdr["Content-Encoding"] = c.enc
		}
		if rr := post(h, "/api/1/envelope/?sentry_key=obs_siteA", c.body, hdr); rr.Code != c.want {
			t.Errorf("%s: got %d want %d", c.name, rr.Code, c.want)
		}
	}
}

func TestBombsAndOversize(t *testing.T) {
	sink := &fakeSink{}
	h := newH(sink, nil)
	q := "/api/1/envelope/?sentry_key=obs_siteA"
	// gzip bomb: 64 MiB of zeros compresses to ~64 KiB.
	bomb := gz(t, bytes.Repeat([]byte{0}, 12<<20))
	if rr := post(h, q, bomb, map[string]string{"Content-Encoding": "gzip"}); rr.Code != 413 {
		t.Fatalf("bomb: %d", rr.Code)
	}
	var zl bytes.Buffer
	zw := zlib.NewWriter(&zl)
	zw.Write(bytes.Repeat([]byte{0}, 12<<20))
	zw.Close()
	if rr := post(h, q, zl.Bytes(), map[string]string{"Content-Encoding": "deflate"}); rr.Code != 413 {
		t.Fatalf("deflate bomb: %d", rr.Code)
	}
	// compressed body over cap
	if rr := post(h, q, bytes.Repeat([]byte("x"), MaxEnvelopeBytes+10), nil); rr.Code != 413 {
		t.Fatalf("raw oversize: %d", rr.Code)
	}
	// single oversize item (2 MiB) -> 413, nothing applied
	big := `{"event_id":"44444444444444444444444444444444","message":"` + strings.Repeat("a", 2<<20) + `"}`
	if rr := post(h, q, envelope(evItem(big)), nil); rr.Code != 413 {
		t.Fatalf("oversize item: %d", rr.Code)
	}
	// oversize item next to a good one: good applied, 200
	if rr := post(h, q, envelope(evItem(big), evItem(nodeEvent)), nil); rr.Code != 200 || len(sink.got) != 1 {
		t.Fatalf("mixed: %d pushed %d", rr.Code, len(sink.got))
	}
	if len(sink.got) > 0 && h != nil {
		_ = 0
	}
}

func TestMalformedShapes(t *testing.T) {
	sink := &fakeSink{}
	h := newH(sink, nil)
	q := "/api/1/envelope/?sentry_key=obs_siteA"
	cases := []struct {
		name string
		body []byte
		want int
	}{
		{"empty body", nil, 400},
		{"whitespace body", []byte("\n\n  \n"), 400},
		{"header not json", []byte("garbage\n{}\n"), 400},
		{"header is array", []byte("[1,2]\n"), 400},
		{"header only", []byte(`{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc"}` + "\n"), 200},
		{"header only no newline", []byte(`{}`), 200},
		{"length too long", []byte("{}\n" + `{"type":"event","length":99999}` + "\n{}\n"), 400},
		{"negative length", []byte("{}\n" + `{"type":"event","length":-5}` + "\n{}\n"), 400},
		{"length not a number", []byte("{}\n" + `{"type":"event","length":"x"}` + "\n{}\n"), 400},
		{"item header garbage", []byte("{}\n" + "not json\n{}\n"), 400},
		{"payload not json", []byte("{}\n" + `{"type":"event"}` + "\nnot-json\n"), 400},
		{"event with nothing", []byte("{}\n" + `{"type":"event"}` + "\n{}\n"), 400},
		{"payload is array", []byte("{}\n" + `{"type":"event"}` + "\n[1]\n"), 400},
		{"missing type, ignored", []byte("{}\n" + "{}\n{}\n"), 200},
	}
	for _, c := range cases {
		rr := post(h, q, c.body, nil)
		if rr.Code != c.want {
			t.Errorf("%s: got %d want %d (%s)", c.name, rr.Code, c.want, rr.Body)
		}
	}
	if len(sink.got) != 0 {
		t.Fatal("malformed input was applied")
	}
	// length lies short: payload cut mid-JSON, then trailing bytes parse as a bad header.
	short := `{"event_id":"55555555555555555555555555555555","message":"hello"}`
	body := "{}\n" + fmt.Sprintf(`{"type":"event","length":%d}`, len(short)-10) + "\n" + short + "\n"
	if rr := post(h, q, []byte(body), nil); rr.Code != 400 || len(sink.got) != 0 {
		t.Fatalf("short length: %d", rr.Code)
	}
	// length lies long but within the body: swallows the next header; no 5xx.
	body = "{}\n" + fmt.Sprintf(`{"type":"event","length":%d}`, len(short)+5) + "\n" + short + "\n{}\n"
	if rr := post(h, q, []byte(body), nil); rr.Code >= 500 {
		t.Fatalf("long length: %d", rr.Code)
	}
}

func TestNoLengthItems(t *testing.T) {
	sink := &fakeSink{}
	h := newH(sink, nil)
	body := "{}\n" + `{"type":"event"}` + "\n" + nodeEvent + "\n" + `{"type":"session"}` + "\n" + sessionPayload
	if rr := post(h, "/api/1/envelope/?sentry_key=obs_siteA", []byte(body), nil); rr.Code != 200 || len(sink.got) != 1 {
		t.Fatalf("%d %d", rr.Code, len(sink.got))
	}
}

func TestIdempotentDuplicate(t *testing.T) {
	sink := &fakeSink{}
	h := newH(sink, nil)
	env := envelope(evItem(pyEvent))
	for i := 0; i < 3; i++ {
		if rr := post(h, "/api/1/envelope/?sentry_key=obs_siteA", env, nil); rr.Code != 200 {
			t.Fatal(rr.Code)
		}
	}
	if len(sink.got) != 1 {
		t.Fatalf("applied %d times", len(sink.got))
	}
	// Same event_id from another site is NOT a duplicate (site-scoped).
	post(h, "/api/1/envelope/?sentry_key=obs_siteB", env, nil)
	if len(sink.got) != 2 {
		t.Fatal("dedupe leaked across sites")
	}
}

func TestMappingDeterministic(t *testing.T) {
	a, ida, _ := mapEvent([]byte(pyEvent), "")
	b, idb, _ := mapEvent([]byte(pyEvent), "")
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if !bytes.Equal(ja, jb) || ida != idb {
		t.Fatal("mapping not deterministic (retries would conflict)")
	}
	// no event_id anywhere: stable derived id
	noid := `{"message":"hi"}`
	_, i1, _ := mapEvent([]byte(noid), "")
	_, i2, _ := mapEvent([]byte(noid), "")
	if i1 != i2 || !validEventID.MatchString(i1) {
		t.Fatalf("derived id %q", i1)
	}
	// hostile event id falls back to a valid one
	in, id, _ := mapEvent([]byte(`{"event_id":"../../etc/passwd'--","message":"x"}`), "")
	if !validEventID.MatchString(id) || in.EventID != id {
		t.Fatalf("hostile id kept: %q", id)
	}
}

func TestEventTimestampHonored(t *testing.T) {
	in, _, err := mapEvent([]byte(`{"message":"x","timestamp":1760000000.5}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if in.ClientTimestamp != 1760000000500 {
		t.Fatalf("float-seconds timestamp not mapped: %d", in.ClientTimestamp)
	}
	in, _, _ = mapEvent([]byte(`{"message":"x","timestamp":"2026-10-01T12:00:00Z"}`), "")
	if in.ClientTimestamp <= 0 {
		t.Fatalf("RFC3339 timestamp not mapped: %d", in.ClientTimestamp)
	}
	in, _, _ = mapEvent([]byte(`{"message":"x"}`), "")
	if in.ClientTimestamp != 0 {
		t.Fatalf("absent timestamp must stay zero: %d", in.ClientTimestamp)
	}
}

func TestHundredItems(t *testing.T) {
	sink := &fakeSink{}
	h := newH(sink, nil)
	var items []string
	for i := 0; i < 100; i++ {
		items = append(items, evItem(fmt.Sprintf(`{"event_id":"%032x","message":"m%d"}`, i+1, i)))
	}
	if rr := post(h, "/api/1/envelope/?sentry_key=obs_siteA", envelope(items...), nil); rr.Code != 200 || len(sink.got) != 100 {
		t.Fatalf("%d %d", rr.Code, len(sink.got))
	}
	// 150 events: capped at maxEventsPerEnvelope, still 200
	sink = &fakeSink{}
	h = newH(sink, nil)
	items = nil
	for i := 0; i < 150; i++ {
		items = append(items, evItem(fmt.Sprintf(`{"event_id":"%032x","message":"m%d"}`, i+1, i)))
	}
	if rr := post(h, "/api/1/envelope/?sentry_key=obs_siteA", envelope(items...), nil); rr.Code != 200 || len(sink.got) != maxEventsPerEnvelope {
		t.Fatalf("%d %d", rr.Code, len(sink.got))
	}
	// thousands of tiny items: bounded, no 5xx
	var sb strings.Builder
	sb.WriteString("{}\n")
	for i := 0; i < 5000; i++ {
		sb.WriteString(`{"type":"client_report"}` + "\n{}\n")
	}
	if rr := post(h, "/api/1/envelope/?sentry_key=obs_siteA", []byte(sb.String()), nil); rr.Code >= 500 {
		t.Fatalf("many items: %d", rr.Code)
	}
}

func TestRateLimitAndBackpressure(t *testing.T) {
	sink := &fakeSink{}
	h := newH(sink, denyLimiter{deny: true})
	rr := post(h, "/api/1/envelope/?sentry_key=obs_siteA", envelope(evItem(nodeEvent)), nil)
	if rr.Code != 429 || rr.Header().Get("Retry-After") == "" || !strings.Contains(rr.Header().Get("X-Sentry-Rate-Limits"), "::key") {
		t.Fatalf("%d %v", rr.Code, rr.Header())
	}
	if len(sink.got) != 0 {
		t.Fatal("applied while limited")
	}
	// buffer full -> 429; durability error -> 503 with Retry-After
	sink = &fakeSink{err: obserrors.ErrErrorBufferFull}
	h = newH(sink, nil)
	if rr := post(h, "/api/1/envelope/?sentry_key=obs_siteA", envelope(evItem(nodeEvent)), nil); rr.Code != 429 || rr.Header().Get("X-Sentry-Rate-Limits") == "" {
		t.Fatalf("%d", rr.Code)
	}
	sink = &fakeSink{err: errors.New("disk")}
	h = newH(sink, nil)
	if rr := post(h, "/api/1/envelope/?sentry_key=obs_siteA", envelope(evItem(nodeEvent)), nil); rr.Code != 503 || rr.Header().Get("Retry-After") == "" {
		t.Fatalf("%d", rr.Code)
	}
	// conflicting reuse is acked (no retry loop)
	sink = &fakeSink{err: obserrors.ErrEventIDConflict}
	h = newH(sink, nil)
	if rr := post(h, "/api/1/envelope/?sentry_key=obs_siteA", envelope(evItem(nodeEvent)), nil); rr.Code != 200 {
		t.Fatalf("%d", rr.Code)
	}
}

func TestStoreEndpoint(t *testing.T) {
	sink := &fakeSink{}
	h := newH(sink, nil)
	rr := post(h, "/api/1/store/?sentry_key=obs_siteA", []byte(goEvent), nil)
	if rr.Code != 200 || len(sink.got) != 1 {
		t.Fatalf("%d %s", rr.Code, rr.Body)
	}
	var out map[string]string
	json.Unmarshal(rr.Body.Bytes(), &out)
	if out["id"] != "33333333333333333333333333333333" {
		t.Fatalf("id %v", out)
	}
	if rr := post(h, "/api/1/store/?sentry_key=obs_siteA", []byte("nope"), nil); rr.Code != 400 {
		t.Fatalf("%d", rr.Code)
	}
	if rr := post(h, "/api/1/store/", []byte(goEvent), nil); rr.Code != 401 {
		t.Fatalf("%d", rr.Code)
	}
}

func TestCORSHeaders(t *testing.T) {
	h := newH(&fakeSink{}, nil)
	rr := post(h, "/api/1/envelope/", nil, map[string]string{"Content-Type": "text/plain;charset=UTF-8"})
	if rr.Header().Get("Access-Control-Allow-Origin") != "*" || !strings.Contains(rr.Header().Get("Access-Control-Expose-Headers"), "X-Sentry-Rate-Limits") {
		t.Fatal("error responses must carry CORS headers")
	}
}

func TestOversizedFieldsStayUnderRecordCap(t *testing.T) {
	huge := strings.Repeat("x", 6000)
	var crumbs []string
	for i := 0; i < 500; i++ {
		crumbs = append(crumbs, fmt.Sprintf(`{"message":"%s","data":{"k":"%s"}}`, huge, huge))
	}
	ctxs := `{"a":{"v":"` + strings.Repeat("y", 100000) + `"},"b":{"v":1}}`
	p := fmt.Sprintf(`{"message":"m","breadcrumbs":[%s],"contexts":%s,"extra":%s}`, strings.Join(crumbs, ","), ctxs, ctxs)
	// the raw payload is > MaxItemBytes; shrink to the cap by dropping crumbs
	if len(p) > MaxItemBytes {
		p = fmt.Sprintf(`{"message":"m","breadcrumbs":[%s],"contexts":%s,"extra":%s}`, strings.Join(crumbs[:100], ","), ctxs, ctxs)
	}
	in, _, err := mapEvent([]byte(p), "")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(in)
	if len(raw) > 200<<10 {
		t.Fatalf("mapped record %d bytes risks the 256KiB admission cap", len(raw))
	}
}

func TestConcurrentRace(t *testing.T) {
	sink := &fakeSink{}
	hd := &Handler{Keys: fakeKeys{}, Sink: sink}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/{project_id}/envelope/{$}", hd.Envelope)
	var wg sync.WaitGroup
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				ev := fmt.Sprintf(`{"event_id":"%032x","message":"c"}`, (g%4)*100+i) // overlapping ids
				env := envelope(evItem(ev), `{"type":"session"}`+"\n"+sessionPayload)
				key := "obs_siteA"
				if g%2 == 1 {
					key = "obs_siteB"
				}
				post(mux, "/api/1/envelope/?sentry_key="+key, gz(t, env), map[string]string{"Content-Encoding": "gzip"})
				hd.Stats()
			}
		}(g)
	}
	wg.Wait()
	// sites alternate with g%2 and ids with g%4: 2 id groups x 20 ids x 2 sites = 80
	if len(sink.got) != 80 {
		t.Fatalf("applied %d, want 80 (idempotency under concurrency)", len(sink.got))
	}
	for _, g := range sink.got {
		if g.in.SiteID != g.site {
			t.Fatal("site mismatch")
		}
	}
}

func TestKeyParsers(t *testing.T) {
	if k := authHeaderKey(`Sentry sentry_version=7,sentry_key=abc,sentry_secret=zzz`); k != "abc" {
		t.Fatal(k)
	}
	if authHeaderKey(`Bearer abc`) != "" || authHeaderKey("") != "" {
		t.Fatal("non-sentry scheme accepted")
	}
	if keyFromDSN("https://abc:secret@h/1") != "abc" || keyFromDSN("://bad") != "" || keyFromDSN("https://h/1") != "" {
		t.Fatal("dsn parse")
	}
}
