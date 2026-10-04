package otlpgrpc

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	logspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"

	"github.com/useteploy/teploy-observe/internal/auth"
	"github.com/useteploy/teploy-observe/internal/authguard"
)

// With a bad key, an oversize message must be refused on its HEADERS as
// Unauthenticated. If auth ran after the message was read (the old unary
// interceptor), the receive cap would fire first and the code would be
// ResourceExhausted: the cap is 1 KiB and the message 4 KiB here.
func TestUnauthenticatedOversizeIsRejectedBeforeDecode(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.MaxRecvMsgSize = 1024 })
	big := logsReq(strings.Repeat("a", 4096))
	_, err := logspb.NewLogsServiceClient(h.conn).Export(withKey("obs_nope"), big)
	if code(err) != codes.Unauthenticated {
		t.Fatalf("bad key + oversize: got %v, want Unauthenticated (auth must precede decode)", err)
	}
	_, err = logspb.NewLogsServiceClient(h.conn).Export(context.Background(), big)
	if code(err) != codes.Unauthenticated {
		t.Fatalf("no key + oversize: got %v", err)
	}
	if h.sink.count() != 0 {
		t.Fatal("unauthenticated message reached ingest")
	}
}

// A multi-MiB unauthenticated message under the default cap is refused too,
// over a real listener, without reaching the sink.
func TestUnauthenticatedMultiMiBMessageRefused(t *testing.T) {
	h := newHarness(t, nil)
	big := logsReq(strings.Repeat("a", 3<<20))
	_, err := logspb.NewLogsServiceClient(h.conn).Export(withKey("obs_nope"), big)
	if code(err) != codes.Unauthenticated {
		t.Fatalf("got %v", err)
	}
	if h.sink.count() != 0 {
		t.Fatal("reached ingest")
	}
}

func TestDefaultsAndClamps(t *testing.T) {
	s, err := New(Config{}, Deps{Keys: &fakeKeys{}})
	if err != nil {
		t.Fatal(err)
	}
	if s.cfg.MaxRecvMsgSize != 4<<20 || s.cfg.MaxHeaderListSize != DefaultMaxHeaderListSize ||
		s.cfg.MaxConcurrentStreams == 0 || s.cfg.MaxConnections == 0 || s.cfg.MaxConnsPerIP == 0 {
		t.Fatalf("defaults: %+v", s.cfg)
	}
	s, _ = New(Config{MaxRecvMsgSize: 64 << 20}, Deps{Keys: &fakeKeys{}})
	if s.cfg.MaxRecvMsgSize != MaxRecvMsgSizeLimit {
		t.Fatalf("not clamped to the 10 MiB ceiling: %d", s.cfg.MaxRecvMsgSize)
	}
	s, _ = New(Config{MaxRecvMsgSize: 8 << 20}, Deps{Keys: &fakeKeys{}})
	if s.cfg.MaxRecvMsgSize != 8<<20 {
		t.Fatalf("override ignored: %d", s.cfg.MaxRecvMsgSize)
	}
}

func TestOversizeHeadersRefused(t *testing.T) {
	h := newHarness(t, nil)
	ctx := metadata.AppendToOutgoingContext(withKey("obs_good"), "x-pad", strings.Repeat("p", 64<<10))
	_, err := logspb.NewLogsServiceClient(h.conn).Export(ctx, logsReq("x"))
	if err == nil {
		t.Fatal("64 KiB of headers accepted; MaxHeaderListSize not applied")
	}
	if h.sink.count() != 0 {
		t.Fatal("reached ingest")
	}
}

func TestPeerIPReachesLimiterFromTap(t *testing.T) {
	h := newHarness(t, nil)
	if _, err := logspb.NewLogsServiceClient(h.conn).Export(withKey("obs_good"), logsReq("x")); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.limiter.site.Load().(string); got != "site-a|127.0.0.1" {
		t.Fatalf("limiter saw %q", got)
	}
}

func TestConnectionCapPerIP(t *testing.T) {
	srv, err := New(Config{MaxConnsPerIP: 2, MaxConnections: 10}, Deps{Keys: &fakeKeys{}})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("loopback unavailable: %v", err)
	}
	srv.Serve(ln)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()
	var conns []net.Conn
	for i := 0; i < 3; i++ {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		conns = append(conns, c)
	}
	// The refused connection is closed by the server: its read ends promptly.
	closed := 0
	for _, c := range conns {
		_ = c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		buf := make([]byte, 1)
		if _, err := c.Read(buf); err != nil {
			if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
				closed++
			}
		}
	}
	if closed != 1 {
		t.Fatalf("closed %d connections, want exactly the 3rd refused", closed)
	}
	// Releasing one frees a slot.
	_ = conns[0].Close()
	time.Sleep(100 * time.Millisecond)
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, err := c.Read(make([]byte, 1)); err != nil {
		if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
			t.Fatalf("connection after release was refused: %v", err)
		}
	}
}

type countingKeys struct{ calls atomic.Int64 }

func (c *countingKeys) ValidateAPIKey(_ context.Context, key string) (auth.ValidatedKey, error) {
	c.calls.Add(1)
	if key == "obs_good" {
		return auth.ValidatedKey{SiteID: "site-a", Scopes: "telemetry"}, nil
	}
	return auth.ValidatedKey{}, fmt.Errorf("auth: invalid api key")
}

func TestRandomKeysAreNegativeCachedAndIPLimited(t *testing.T) {
	ck := &countingKeys{}
	guard := authguard.New(ck, authguard.Config{FailuresPerMinute: 15})
	sk := &sink{}
	srv, err := New(Config{}, Deps{Keys: guard, Ingester: sk.ingesters()})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("loopback unavailable: %v", err)
	}
	srv.Serve(ln)
	conn, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()
	lc := logspb.NewLogsServiceClient(conn)

	for i := 0; i < 5; i++ {
		if _, err := lc.Export(withKey("obs_same_bad"), logsReq("x")); code(err) != codes.Unauthenticated {
			t.Fatalf("got %v", err)
		}
	}
	if ck.calls.Load() != 1 {
		t.Fatalf("store lookups=%d for one repeated bad key", ck.calls.Load())
	}
	exhausted := 0
	for i := 0; i < 40; i++ {
		_, err := lc.Export(withKey(fmt.Sprintf("rand-%d", i)), logsReq("x"))
		if code(err) == codes.ResourceExhausted {
			exhausted++
			_ = retryDelayOf(t, err)
		}
	}
	if exhausted == 0 {
		t.Fatal("no ResourceExhausted after sustained random-key attempts")
	}
	if ck.calls.Load() > 20 {
		t.Fatalf("store lookups=%d: limiter did not stop the flood", ck.calls.Load())
	}
	// A good key from another source address would still work; from this
	// blocked address it is refused until the window passes (documented).
	if sk.count() != 0 {
		t.Fatal("reached ingest")
	}
}
