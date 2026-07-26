package edge

import (
	"errors"
	"net"
	"sync"
)

var errListenerExhausted = errors.New("single-conn listener exhausted")

// singleConnListener hands out exactly one conn (the one it was created
// with), then blocks the next Accept until Close. Keeping Serve blocked for
// the accepted connection's lifetime lets shutdown account for the server.
//
// Accept deliberately returns the original connection without a wrapper.
// net/http requires the concrete *tls.Conn type to recognize negotiated h2
// and install its HTTP/2 handler.
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
		conn := l.conn
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
