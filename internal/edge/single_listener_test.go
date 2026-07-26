package edge

import (
	"errors"
	"net"
	"testing"
	"time"
)

type closeWriteTestConn struct {
	net.Conn
	called chan struct{}
	err    error
}

func (c *closeWriteTestConn) CloseWrite() error {
	close(c.called)
	return c.err
}

func TestSingleConnListenerAcceptedConnPreservesCloseWrite(t *testing.T) {
	left, right := net.Pipe()
	t.Cleanup(func() {
		_ = left.Close()
		_ = right.Close()
	})

	wantErr := errors.New("close write sentinel")
	raw := &closeWriteTestConn{
		Conn:   left,
		called: make(chan struct{}),
		err:    wantErr,
	}
	listener := &singleConnListener{conn: raw}
	accepted, err := listener.Accept()
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	t.Cleanup(func() { _ = accepted.Close() })

	closer, ok := accepted.(interface{ CloseWrite() error })
	if !ok {
		t.Fatal("accepted connection does not expose CloseWrite")
	}
	if err := closer.CloseWrite(); !errors.Is(err, wantErr) {
		t.Fatalf("CloseWrite error = %v, want %v", err, wantErr)
	}
	select {
	case <-raw.called:
	default:
		t.Fatal("CloseWrite did not reach the wrapped connection")
	}
}

func TestSingleConnListenerSecondAcceptBlocksUntilClose(t *testing.T) {
	left, right := net.Pipe()
	t.Cleanup(func() {
		_ = left.Close()
		_ = right.Close()
	})

	listener := &singleConnListener{conn: left}
	accepted, err := listener.Accept()
	if err != nil {
		t.Fatalf("first Accept: %v", err)
	}
	t.Cleanup(func() { _ = accepted.Close() })

	second := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		conn, err := listener.Accept()
		if conn != nil {
			_ = conn.Close()
		}
		second <- err
	}()
	<-started

	select {
	case err := <-second:
		t.Fatalf("second Accept returned before Close: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	if err := listener.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-second:
		if !errors.Is(err, errListenerExhausted) {
			t.Fatalf("second Accept error = %v, want errListenerExhausted", err)
		}
	case <-time.After(time.Second):
		t.Fatal("second Accept did not unblock after Close")
	}
}

func TestSingleConnListenerAcceptedConnCloseUnblocksSecondAccept(t *testing.T) {
	left, right := net.Pipe()
	t.Cleanup(func() { _ = right.Close() })

	listener := &singleConnListener{conn: left}
	accepted, err := listener.Accept()
	if err != nil {
		t.Fatalf("first Accept: %v", err)
	}

	second := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		_, err := listener.Accept()
		second <- err
	}()
	<-started

	select {
	case err := <-second:
		t.Fatalf("second Accept returned before accepted conn closed: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	if err := accepted.Close(); err != nil {
		t.Fatalf("accepted conn Close: %v", err)
	}
	select {
	case err := <-second:
		if !errors.Is(err, errListenerExhausted) {
			t.Fatalf("second Accept error = %v, want errListenerExhausted", err)
		}
	case <-time.After(time.Second):
		t.Fatal("second Accept did not unblock after accepted conn Close")
	}
}
