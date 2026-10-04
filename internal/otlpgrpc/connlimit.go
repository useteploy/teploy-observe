package otlpgrpc

import (
	"net"
	"sync"
	"sync/atomic"
)

// limitListener caps concurrent connections, in total and per remote IP. A
// connection over either cap is closed immediately after accept (before any
// TLS or HTTP/2 work), so an address that opens thousands of idle sockets
// cannot exhaust file descriptors or the per-connection goroutines.
type limitListener struct {
	net.Listener
	max, maxPerIP int

	mu     sync.Mutex
	total  int
	perIP  map[string]int
	Refuse atomic.Int64
}

func newLimitListener(ln net.Listener, max, maxPerIP int) *limitListener {
	return &limitListener{Listener: ln, max: max, maxPerIP: maxPerIP, perIP: map[string]int{}}
}

func (l *limitListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		ip := remoteHost(c.RemoteAddr())
		l.mu.Lock()
		if (l.max > 0 && l.total >= l.max) || (l.maxPerIP > 0 && l.perIP[ip] >= l.maxPerIP) {
			l.mu.Unlock()
			l.Refuse.Add(1)
			_ = c.Close()
			continue
		}
		l.total++
		l.perIP[ip]++
		l.mu.Unlock()
		return &limitConn{Conn: c, l: l, ip: ip}, nil
	}
}

func remoteHost(a net.Addr) string {
	if a == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(a.String())
	if err != nil {
		return a.String()
	}
	return host
}

type limitConn struct {
	net.Conn
	l    *limitListener
	ip   string
	once sync.Once
}

func (c *limitConn) Close() error {
	c.once.Do(func() {
		c.l.mu.Lock()
		c.l.total--
		if c.l.perIP[c.ip]--; c.l.perIP[c.ip] <= 0 {
			delete(c.l.perIP, c.ip)
		}
		c.l.mu.Unlock()
	})
	return c.Conn.Close()
}
