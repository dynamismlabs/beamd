package edge

import (
	"errors"
	"net"
)

var errListenerExhausted = errors.New("single-conn listener exhausted")

// singleConnListener hands out exactly one conn (the one it was created
// with), then returns errListenerExhausted on subsequent Accept calls.
// http.Server.Serve uses Accept in a loop; after the second call it
// exits its accept loop but continues serving the conn it already got.
type singleConnListener struct {
	conn   net.Conn
	served bool
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	if l.served {
		return nil, errListenerExhausted
	}
	l.served = true
	return l.conn, nil
}

func (l *singleConnListener) Close() error { return nil }

func (l *singleConnListener) Addr() net.Addr {
	if l.conn != nil {
		return l.conn.LocalAddr()
	}
	return &net.TCPAddr{}
}
