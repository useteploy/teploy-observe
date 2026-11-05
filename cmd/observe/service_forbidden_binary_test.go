package main

import (
	"bufio"
	"context"
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

	dir := t.TempDir()
	bin := filepath.Join(dir, "observe")
	build := exec.Command("go", "build", "-p", "2", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	addr := freeLoopbackAddr(t)
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		"OBSERVE_ADDR="+addr,
		"OBSERVE_NUCLEUS_URL="+dsn,
		"OBSERVE_JWT_SECRET="+secret,
		"OBSERVE_SESSION_SALT=sweep-salt-sweep-salt-sweep-salt",
		"OBSERVE_DATA_DIR="+dir,
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
			routes <- sc.Text()
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
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("observe did not come up: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}

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

func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}
