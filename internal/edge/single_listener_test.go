package edge

import (
	"crypto/tls"
	"errors"
	"net"
	"testing"
	"time"
)

func TestSingleConnListenerAcceptedConnPreservesConcreteTLSConn(t *testing.T) {
	left, right := net.Pipe()
	t.Cleanup(func() {
		_ = left.Close()
		_ = right.Close()
	})

	raw := tls.Server(left, &tls.Config{})
	listener := &singleConnListener{conn: raw}
	accepted, err := listener.Accept()
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if accepted != raw {
		t.Fatalf("Accept returned %T, want original %T", accepted, raw)
	}
	if _, ok := accepted.(*tls.Conn); !ok {
		t.Fatalf("Accept returned %T, want *tls.Conn", accepted)
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
