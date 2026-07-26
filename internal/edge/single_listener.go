package edge

import (
	"errors"
	"net"
	"sync"
)

var errListenerExhausted = errors.New("single-conn listener exhausted")

// singleConnListener hands out exactly one conn (the one it was created
// with), then returns errListenerExhausted on subsequent Accept calls.
// http.Server.Serve uses Accept in a loop; after the second call it
// exits its accept loop but continues serving the conn it already got.
type singleConnListener struct {
	conn net.Conn

	mu        sync.Mutex
	served    bool
	closed    chan struct{}
	isClosed  bool
	closeOnce sync.Once
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	if l.isClosed {
		l.mu.Unlock()
		return nil, errListenerExhausted
	}
	if !l.served {
		l.served = true
		if l.closed == nil {
			l.closed = make(chan struct{})
		}
		conn := &singleListenerConn{Conn: l.conn, listener: l}
		l.mu.Unlock()
		return conn, nil
	}
	if l.closed == nil {
		l.closed = make(chan struct{})
	}
	closed := l.closed
	l.mu.Unlock()
	<-closed
	return nil, errListenerExhausted
}

func (l *singleConnListener) Close() error {
	l.mu.Lock()
	l.isClosed = true
	if l.closed == nil {
		l.closed = make(chan struct{})
	}
	closed := l.closed
	l.mu.Unlock()
	l.closeOnce.Do(func() { close(closed) })
	return nil
}

func (l *singleConnListener) Addr() net.Addr {
	if l.conn != nil {
		return l.conn.LocalAddr()
	}
	return &net.TCPAddr{}
}

// singleListenerConn wakes the listener's blocked second Accept when the one
// accepted connection ends. This keeps http.Server.Serve alive for the actual
// connection lifetime while still allowing it to return naturally afterward.
type singleListenerConn struct {
	net.Conn
	listener *singleConnListener
	once     sync.Once
}

// CloseWrite preserves the half-close capability of the accepted TLS
// connection through this listener wrapper. net/http uses that capability
// when a handler replies before consuming a large request body: it sends the
// complete response, half-closes the socket, and briefly keeps the read side
// open so an HTTP/1.1 client observes the response instead of only the RST from
// unread request data.
func (c *singleListenerConn) CloseWrite() error {
	if closer, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return closer.CloseWrite()
	}
	return nil
}

func (c *singleListenerConn) Close() error {
	var err error
	c.once.Do(func() {
		err = c.Conn.Close()
		_ = c.listener.Close()
	})
	return err
}
