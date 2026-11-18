package claim

import (
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
)

// handshakes is the claim server's error log. A load balancer's TCP health probe opens a connection and closes it
// without sending a byte, which net/http logs as "http: TLS handshake error from <address>: EOF". handshakes drops
// exactly those, counting them through probed, and passes every other line to log. It knows how much each open
// connection read because countingListener registers every connection it accepts, by remote address, until the
// connection is closed; net/http writes the line before it closes the connection.
type handshakes struct {
	log    *slog.Logger
	probed func()

	mu   sync.Mutex
	open map[string]*countingConn
}

// The line net/http writes for a failed handshake, and its end when the connection closed at a TLS record boundary;
// a close within a record ends it in "unexpected EOF" instead.
const (
	handshakeError = "http: TLS handshake error from "
	handshakeEOF   = ": EOF"
)

// Write takes one line of the server's error log.
func (h *handshakes) Write(p []byte) (int, error) {
	line := strings.TrimSuffix(string(p), "\n")
	if addr, ok := strings.CutPrefix(line, handshakeError); ok {
		if addr, ok := strings.CutSuffix(addr, handshakeEOF); ok && h.readNothing(addr) {
			h.probed()
			return len(p), nil
		}
	}
	h.log.Warn("claim endpoint", "error", line)
	return len(p), nil
}

// readNothing reports whether the open connection from addr has read no byte.
func (h *handshakes) readNothing(addr string) bool {
	h.mu.Lock()
	c := h.open[addr]
	h.mu.Unlock()
	return c != nil && c.read.Load() == 0
}

func (h *handshakes) track(c net.Conn) *countingConn {
	cc := &countingConn{Conn: c, h: h, addr: c.RemoteAddr().String()}
	h.mu.Lock()
	h.open[cc.addr] = cc
	h.mu.Unlock()
	return cc
}

func (h *handshakes) forget(c *countingConn) {
	h.mu.Lock()
	if h.open[c.addr] == c {
		delete(h.open, c.addr)
	}
	h.mu.Unlock()
}

// countingListener registers each connection it accepts with handshakes.
type countingListener struct {
	net.Listener
	h *handshakes
}

func (l *countingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return l.h.track(c), nil
}

// countingConn counts the bytes read from a connection, until it is closed.
type countingConn struct {
	net.Conn
	h    *handshakes
	addr string
	read atomic.Int64
}

func (c *countingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.read.Add(int64(n))
	return n, err
}

func (c *countingConn) Close() error {
	c.h.forget(c)
	return c.Conn.Close()
}
