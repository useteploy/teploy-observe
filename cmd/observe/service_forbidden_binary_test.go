package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/auth"
	"github.com/useteploy/teploy-observe/internal/nucleustest"
	"github.com/useteploy/teploy-observe/internal/schema"
	"github.com/useteploy/teploy-observe/internal/svctoken"
)

// This runs the REAL binary, because the question is about the real route
// table: main() wires it inline and nothing short of booting it can answer
// whether some route accepts a service token.

// serviceAllowedPrefix is the only namespace a service token may authenticate to.
const serviceAllowedPrefix = "/api/v1/service/"

// publicRoutes answer without any credential by design, so a token has nothing
// to be refused by. Everything else under /api must refuse it. An entry here is
// a claim that the route is public: adding one must be argued, not assumed.
var publicRoutes = map[string]bool{
	"POST /api/v1/auth/login":        true,
	"GET /api/v1/auth/setup":         true,
	"POST /api/v1/auth/setup":        true,
	"GET /api/v1/auth/methods":       true,
	"GET /api/v1/auth/oidc/login":    true,
	"GET /api/v1/auth/oidc/callback": true,
	"GET /api/v1/sso/metadata":       true,
	"POST /api/v1/sso/callback":      true,
	"GET /api/v1/config":             true,
	"POST /api/v1/feedback":          true,
	"POST /api/v1/flags/evaluate":    true,
	"GET /api/v1/surveys/active":     true,
	"POST /api/v1/surveys/expose":    true,
	"POST /api/v1/surveys/respond":   true,
	// CORS preflight: echoes headers, touches nothing, for every caller.
	"OPTIONS /api/v1/{path...}": true,
	// Answers 405 to everyone so an MCP client that guessed the verb gets no HTML.
	"GET /api/mcp":                            true,
	"GET /api/docs":                           true,
	"GET /api/docs/":                          true,
	"GET /api/v1/checkin/{slug}":              true,
	"POST /api/v1/checkin/{slug}":             true,
	"GET /api/v1/checkin/{site_id}/{slug}":    true,
	"POST /api/v1/checkin/{site_id}/{slug}":   true,
	"GET /api/v1/checkin/token/{ping_token}":  true,
	"POST /api/v1/checkin/token/{ping_token}": true,
}

// mustRefuse names what the plan says a service token can never reach, so the
// sweep cannot quietly lose them to a parsing change or a renamed route.
var mustRefuse = []string{
	"GET /api/v1/platform/users",
	"POST /api/v1/platform/users",
	"POST /api/v1/query",
	"POST /api/v1/query/explain",
	"GET /api/v1/query/tables",
	"POST /api/v1/ai/query",
	"DELETE /api/v1/sites/{site_id}",
	"PUT /api/v1/sites/{site_id}/privacy",
	"GET /api/v1/sites",
	"POST /api/v1/sites",
	"POST /api/v1/sites/{site_id}/keys",
	"GET /api/v1/sites/{site_id}/keys",
	"DELETE /api/v1/keys/{key_id}",
	"POST /api/v1/sites/{site_id}/share",
	"GET /api/v1/stats/live",
	"GET /api/v1/export",
	"GET /api/v1/persons",
	"GET /api/v1/audit",
	"GET /api/v1/service-tokens",
	"POST /api/v1/service-tokens",
	"DELETE /api/v1/service-tokens/{token_id}",
	"GET /api/v1/mcp/tokens",
	"POST /api/mcp",
}

var routeParam = regexp.MustCompile(`\{[a-zA-Z_]+(\.\.\.)?\}`)

func TestServiceTokenIsRefusedEverywhereElseOnTheRealBinary(t *testing.T) {
	ctx := context.Background()
	dsn := nucleustest.DSN(t)
	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("nucleus not reachable: %v", err)
	}
	defer db.Close()
	if err := schema.Apply(ctx, db); err != nil {
		t.Fatalf("migrations: %v", err)
	}

	secret := strings.Repeat("b", 48)
	authSvc := auth.NewAuthService(db, secret, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := authSvc.EnsureAdmin(ctx, "svc-sweep-admin", "svc-sweep-password-1234"); err != nil {
		t.Fatalf("ensure admin: %v", err)
	}
	plaintext, tok, err := svctoken.NewStore(db).Create(ctx, "sweep-"+randHex(t), "", "root", allScopes)
	if err != nil {
		t.Fatal(err)
	}

	base, routes := bootObserveBinary(t, dsn, secret)
	client := &http.Client{Timeout: 15 * time.Second}

	seen := map[string]bool{}
	drain := time.After(2 * time.Second)
collect:
	for {
		select {
		case line, ok := <-routes:
			if !ok {
				break collect
			}
			fields := strings.Fields(line)
			if len(fields) >= 2 && strings.HasPrefix(fields[1], "/") {
				seen[fields[0]+" "+fields[1]] = true
			}
		case <-drain:
			break collect
		}
	}
	if len(seen) < 100 {
		t.Fatalf("parsed only %d routes from OBSERVE_LOG_ROUTES", len(seen))
	}
	for _, want := range mustRefuse {
		if !seen[want] {
			t.Errorf("route %q is not in the table: the sweep would not have tested it", want)
		}
	}

	request := func(method, path string, set func(*http.Request)) int {
		req, err := http.NewRequest(method, base+path, strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		set(req)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}
	placements := map[string]func(*http.Request){
		"bearer":        func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+plaintext) },
		"x-api-key":     func(r *http.Request) { r.Header.Set("X-API-Key", plaintext) },
		"x-share-token": func(r *http.Request) { r.Header.Set("X-Share-Token", plaintext) },
	}

	tested := 0
	for route := range seen {
		method, pattern, _ := strings.Cut(route, " ")
		if !strings.HasPrefix(pattern, "/api/") || strings.HasPrefix(pattern, serviceAllowedPrefix) || publicRoutes[route] {
			continue
		}
		path := routeParam.ReplaceAllString(strings.TrimSuffix(pattern, "{$}"), "x")
		tested++
		for name, set := range placements {
			got := request(method, path, set)
			// 401/403 are refusals. 405 cannot happen for a registered pair
			// and 2xx is the failure this test exists to catch.
			if got != http.StatusUnauthorized && got != http.StatusForbidden {
				t.Errorf("%s with the token as %s: status %d, want a refusal", route, name, got)
			}
		}
	}
	t.Logf("swept %d of %d routes", tested, len(seen))

	// The allowed surface works, and leaves a real row in the real audit chain.
	createRef := fmt.Sprintf(`{"external_ref":"sweep-%s"}`, randHex(t))
	req, err := http.NewRequest(http.MethodPost, base+"/api/v1/service/sites", strings.NewReader(createRef))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+plaintext)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("service create on the real binary: %d", resp.StatusCode)
	}
	rows, err := nucleus.Query[struct {
		Result string `db:"result"`
	}](ctx, db.SQL(), "SELECT result FROM audit_events WHERE actor = $1 AND action = $2", tok.ID, "service.sites.create")
	if err != nil {
		t.Fatalf("reading the audit chain: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("audit rows = %+v, want started and success", rows)
	}
}

var (
	observeBinaryOnce sync.Once
	observeBinaryPath string
	observeBinaryErr  error
)

// bootObserveBinary builds once per test run, starts the real server against dsn on
// a free port, and returns its base URL and its route-table output.
func bootObserveBinary(t *testing.T, dsn, secret string) (string, <-chan string) {
	t.Helper()
	observeBinaryOnce.Do(func() {
		dir, err := os.MkdirTemp("", "observe-binary-test")
		if err != nil {
			observeBinaryErr = err
			return
		}
		observeBinaryPath = filepath.Join(dir, "observe")
		out, err := exec.Command("go", "build", "-p", "2", "-o", observeBinaryPath, ".").CombinedOutput()
		if err != nil {
			observeBinaryErr = fmt.Errorf("build: %w\n%s", err, out)
		}
	})
	if observeBinaryErr != nil {
		t.Fatal(observeBinaryErr)
	}
	addr := freeLoopbackAddr(t)
	cmd := exec.Command(observeBinaryPath)
	cmd.Env = append(os.Environ(),
		"OBSERVE_ADDR="+addr,
		"OBSERVE_NUCLEUS_URL="+dsn,
		"OBSERVE_JWT_SECRET="+secret,
		"OBSERVE_SESSION_SALT=sweep-salt-sweep-salt-sweep-salt",
		"OBSERVE_DATA_DIR="+t.TempDir(),
		"OBSERVE_LOG_ROUTES=1",
		"OBSERVE_PUBLIC_URL=http://"+addr,
		"OBSERVE_INGEST_ADDR=",
		"OBSERVE_OTLP_GRPC_ADDR=",
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	})
	routes := make(chan string, 2048)
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			select {
			case routes <- sc.Text():
			default:
			}
		}
		close(routes)
	}()
	base := "http://" + addr
	client := &http.Client{Timeout: 15 * time.Second}
	deadline := time.Now().Add(120 * time.Second)
	for {
		resp, err := client.Get(base + "/.well-known/teploy.json")
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK && strings.Contains(string(body), `"product":"observe"`) {
				return base, routes
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("observe did not come up: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// The documented path, whole: a token creates a site, mints a key, an SDK configured
// from nothing but the environment contract writes, and the rows land under that
// site. Each SDK's own default site used to turn every one of these writes into a 403.
func TestMintedKeyReachesTheStoreThroughTheEnvContract(t *testing.T) {
	ctx := context.Background()
	dsn := nucleustest.DSN(t)
	db, err := nucleus.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("nucleus not reachable: %v", err)
	}
	defer db.Close()
	if err := schema.Apply(ctx, db); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	plaintext, _, err := svctoken.NewStore(db).Create(ctx, "e2e-"+randHex(t), "", "root",
		[]string{svctoken.ScopeSitesCreate, svctoken.ScopeKeysMint})
	if err != nil {
		t.Fatal(err)
	}
	base, _ := bootObserveBinary(t, dsn, strings.Repeat("e", 48))

	post := func(path, body string, out any) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, base+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+plaintext)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("POST %s: status %d", path, resp.StatusCode)
		}
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
	var created struct {
		Site struct {
			SiteID string `json:"site_id"`
		} `json:"site"`
	}
	post("/api/v1/service/sites", `{"external_ref":"app:e2e-`+randHex(t)+`"}`, &created)
	site := created.Site.SiteID
	var minted struct {
		Key string `json:"key"`
	}
	post("/api/v1/service/sites/"+site+"/keys", `{"label":"e2e"}`, &minted)

	// The server's and this test's own configuration must not reach the SDK.
	sdkEnv := func(extra ...string) []string {
		env := []string{}
		for _, kv := range os.Environ() {
			if !strings.HasPrefix(kv, "OBSERVE_") && !strings.HasPrefix(kv, "OTEL_") {
				env = append(env, kv)
			}
		}
		return append(env, extra...)
	}
	run := func(name string, cmd *exec.Cmd) {
		t.Helper()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", name, err, out)
		}
	}
	waitForRow := func(what, query, tag string) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for {
			rows, err := nucleus.Query[struct {
				SiteID string `db:"site_id"`
			}](ctx, db.SQL(), query, tag)
			if err != nil {
				t.Fatalf("%s: %v", what, err)
			}
			if len(rows) > 0 {
				if rows[0].SiteID != site {
					t.Fatalf("%s stored under site %q, want %q", what, rows[0].SiteID, site)
				}
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s %q never reached the store", what, tag)
			}
			time.Sleep(250 * time.Millisecond)
		}
	}
	const errorQuery = "SELECT site_id FROM error_events WHERE error_value = $1"
	const logQuery = "SELECT site_id FROM logs WHERE message = $1"

	observeEnv := []string{"OBSERVE_ENDPOINT=" + base, "OBSERVE_API_KEY=" + minted.Key}
	otelEnv := []string{
		"OTEL_EXPORTER_OTLP_ENDPOINT=" + base,
		"OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf",
		"OTEL_EXPORTER_OTLP_HEADERS=x-api-key=" + minted.Key,
		"OTEL_SERVICE_NAME=e2e",
		"OTEL_RESOURCE_ATTRIBUTES=service.version=1.2.3",
	}

	goRun := func(tag string, env []string) {
		t.Helper()
		cmd := exec.Command("go", "run", "./examples/envcontract", tag)
		cmd.Dir = filepath.Join("..", "..", "sdk", "go")
		cmd.Env = sdkEnv(env...)
		run("go sdk", cmd)
		waitForRow("go error", errorQuery, tag)
		waitForRow("go log", logQuery, tag)
	}
	goRun("envcontract-go-observe-"+randHex(t), observeEnv)
	goRun("envcontract-go-otel-"+randHex(t), otelEnv)

	python, err := exec.LookPath("python3")
	if err != nil {
		t.Log("python3 not found; the Python half of this test did not run")
		return
	}
	const pythonProgram = `
import sys
import observe_sdk
tag = sys.argv[1]
client = observe_sdk.Client()
client.capture_message(tag)
client.info(tag)
client.flush(timeout=10.0)
client.close(timeout=10.0)
lost = client.stats()["dropped"]
if lost:
    sys.exit("dropped: %s" % lost)
`
	pyRun := func(tag string, env []string) {
		t.Helper()
		cmd := exec.Command(python, "-c", pythonProgram, tag)
		cmd.Env = sdkEnv(append(env, "PYTHONPATH="+filepath.Join("..", "..", "sdk", "python"))...)
		run("python sdk", cmd)
		waitForRow("python error", errorQuery, tag)
		waitForRow("python log", logQuery, tag)
	}
	pyRun("envcontract-py-observe-"+randHex(t), observeEnv)
	pyRun("envcontract-py-otel-"+randHex(t), otelEnv)

	// The analytics path, which every SDK's non-browser user agent is filtered out
	// of by IsBot, so this one is a plain request shaped like a browser's: a key and
	// no site. The wrong site is refused rather than stored.
	sendEvent := func(body string) int {
		req, err := http.NewRequest(http.MethodPost, base+"/api/v1/events", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-API-Key", minted.Key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 Chrome/120.0 Safari/537.36")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := sendEvent(`{"site_id":"default","event_type":"pageview","url":"https://x.example/","pathname":"/"}`); got != http.StatusForbidden {
		t.Fatalf("event for another site: status %d, want 403", got)
	}
	if got := sendEvent(`{"event_type":"pageview","url":"https://x.example/","pathname":"/"}`); got/100 != 2 {
		t.Fatalf("event with no site: status %d", got)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		rows, err := nucleus.Query[struct {
			SiteID string `db:"site_id"`
		}](ctx, db.SQL(), "SELECT site_id FROM events WHERE site_id = $1 LIMIT 1", site)
		if err != nil {
			t.Fatalf("events: %v", err)
		}
		if len(rows) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the event never reached the store under the key's site")
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}
