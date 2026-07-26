package client

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"math/big"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dynamismlabs/beamd/internal/proto"
	"github.com/dynamismlabs/beamd/internal/tunnel"
)

// fakeEdge is a minimal in-process edge for unit-testing the client. It
// completes the TLS(ALPN beam/1) + yamux + hello handshake, then hands each
// accepted control stream to a per-test script that owns what the edge sends
// next. This lets tests drive control-plane behavior — torn lines, error
// replies, mismatched replies — that a real edge won't emit.
type fakeEdge struct {
	ln     net.Listener
	addr   string
	script func(ctrl tunnel.Stream, sess tunnel.Session, br *bufio.Reader, hello proto.Hello)

	mu    sync.Mutex
	conns int
}

func newFakeEdge(t *testing.T, script func(tunnel.Stream, tunnel.Session, *bufio.Reader, proto.Hello)) *fakeEdge {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", fakeEdgeTLS(t))
	if err != nil {
		t.Fatalf("fake edge listen: %v", err)
	}
	fe := &fakeEdge{ln: ln, addr: ln.Addr().String(), script: script}
	go fe.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return fe
}

func (fe *fakeEdge) serve() {
	for {
		c, err := fe.ln.Accept()
		if err != nil {
			return
		}
		go fe.handle(c)
	}
}

func (fe *fakeEdge) handle(c net.Conn) {
	defer c.Close()
	if err := c.(*tls.Conn).Handshake(); err != nil {
		return
	}
	sess, err := tunnel.NewYamuxServer(c, 0)
	if err != nil {
		return
	}
	defer sess.CloseWithError(tunnel.CloseNormal, "fake edge closed")
	ctrl, err := sess.AcceptStream(context.Background())
	if err != nil {
		return
	}
	br := bufio.NewReader(ctrl)
	typ, line, err := proto.Read(br)
	if err != nil || typ != proto.TypeHello {
		return
	}
	var hello proto.Hello
	_ = json.Unmarshal(line, &hello)
	if err := proto.Write(ctrl, &proto.HelloOK{
		Type: proto.TypeHelloOK, Slug: "turing",
		BaseDomain: "test.example.com", ProtoVersion: proto.ProtoVersion,
	}); err != nil {
		return
	}
	fe.mu.Lock()
	fe.conns++
	fe.mu.Unlock()
	if fe.script != nil {
		fe.script(ctrl, sess, br, hello)
	}
}

func (fe *fakeEdge) connCount() int {
	fe.mu.Lock()
	defer fe.mu.Unlock()
	return fe.conns
}

func fakeEdgeTLS(t *testing.T) *tls.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "fake-edge"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		NextProtos:   []string{ALPNBeam},
	}
}

func dialFake(t *testing.T, fe *fakeEdge, opts Options) *Client {
	t.Helper()
	opts.InsecureSkipVerify = true
	if opts.HeartbeatInterval == 0 {
		opts.HeartbeatInterval = time.Hour // keep heartbeats out of the way unless a test wants them
	}
	c, err := Connect(context.Background(), fe.addr, "tok", opts)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// A register that the edge accepts returns the URL and populates identity.
func TestClient_RegisterSuccess(t *testing.T) {
	fe := newFakeEdge(t, func(ctrl tunnel.Stream, _ tunnel.Session, br *bufio.Reader, _ proto.Hello) {
		for {
			typ, line, err := proto.Read(br)
			if err != nil {
				return
			}
			if typ == proto.TypeRegister {
				var reg proto.Register
				_ = json.Unmarshal(line, &reg)
				_ = proto.Write(ctrl, &proto.Registered{
					Type: proto.TypeRegistered, Name: reg.Name,
					URL: "https://" + reg.Name + ".turing.test.example.com",
				})
			}
		}
	})
	c := dialFake(t, fe, Options{RegisterTimeout: time.Second})

	url, err := c.Register("api", 3000)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if url != "https://api.turing.test.example.com" {
		t.Errorf("url = %q", url)
	}
	if c.Slug() != "turing" || c.BaseDomain() != "test.example.com" {
		t.Errorf("identity not populated: slug=%q base=%q", c.Slug(), c.BaseDomain())
	}
}

// A torn / unparseable control line must tear down the session so manage()
// reconnects — otherwise the client zombies (data plane up, control dead, every
// register times out forever). Regression for the readControl teardown fix.
func TestClient_TornControlLineReconnects(t *testing.T) {
	var round int32
	fe := newFakeEdge(t, func(ctrl tunnel.Stream, sess tunnel.Session, _ *bufio.Reader, _ proto.Hello) {
		if atomic.AddInt32(&round, 1) == 1 {
			_, _ = ctrl.Write([]byte("this is not json\n")) // garbage → client must drop the session
		}
		<-sess.Done()
	})
	c := dialFake(t, fe, Options{ReconnectInitial: 20 * time.Millisecond, ReconnectMax: 50 * time.Millisecond})
	_ = c

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && fe.connCount() < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	if fe.connCount() < 2 {
		t.Errorf("torn control line did not trigger a reconnect (conns=%d)", fe.connCount())
	}
}

// A failed register must not linger in intended state (or the next reconnect's
// replay resurrects it as a ghost tunnel). Regression for the rollback fix.
func TestClient_RegisterRollsBackIntendedOnError(t *testing.T) {
	fe := newFakeEdge(t, func(ctrl tunnel.Stream, _ tunnel.Session, br *bufio.Reader, _ proto.Hello) {
		for {
			typ, line, err := proto.Read(br)
			if err != nil {
				return
			}
			if typ == proto.TypeRegister {
				var reg proto.Register
				_ = json.Unmarshal(line, &reg)
				// The edge now names the register the error is about.
				_ = proto.Write(ctrl, &proto.Error{Type: proto.TypeError, Code: proto.CodeNameTaken, Name: reg.Name, Message: "taken"})
			}
		}
	})
	c := dialFake(t, fe, Options{RegisterTimeout: time.Second})

	if _, err := c.Register("api", 3000); err == nil {
		t.Fatal("register should have failed with name_taken")
	}
	if got := c.Intended(); len(got) != 0 {
		t.Errorf("failed register must not persist intended state, got %v", got)
	}
}

// An `error` naming a DIFFERENT register must be dropped, not delivered to the
// register in flight — otherwise a late error from an already-timed-out
// register fails the next one and orphans a live tunnel. Regression for the
// error-reply correlation fix (proto.Error.Name).
func TestClient_MismatchedErrorReplyDropped(t *testing.T) {
	fe := newFakeEdge(t, func(ctrl tunnel.Stream, _ tunnel.Session, br *bufio.Reader, _ proto.Hello) {
		for {
			typ, _, err := proto.Read(br)
			if err != nil {
				return
			}
			if typ == proto.TypeRegister {
				// Reply with an error for the WRONG name.
				_ = proto.Write(ctrl, &proto.Error{Type: proto.TypeError, Code: proto.CodeNameTaken, Name: "wrong", Message: "taken"})
			}
		}
	})
	c := dialFake(t, fe, Options{RegisterTimeout: 300 * time.Millisecond})

	_, err := c.Register("api", 3000)
	if err == nil {
		t.Fatal("register should not succeed")
	}
	// It must TIME OUT (mismatched error dropped), not surface the wrong error.
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("register should have timed out (mismatched error dropped), got: %v", err)
	}
}

// A `registered` reply for a DIFFERENT name must not satisfy the pending
// register (which would cache the wrong URL). It should be dropped and the
// register should time out. Regression for the reply-name correlation fix.
func TestClient_MismatchedRegisteredReplyDropped(t *testing.T) {
	fe := newFakeEdge(t, func(ctrl tunnel.Stream, _ tunnel.Session, br *bufio.Reader, _ proto.Hello) {
		for {
			typ, _, err := proto.Read(br)
			if err != nil {
				return
			}
			if typ == proto.TypeRegister {
				_ = proto.Write(ctrl, &proto.Registered{Type: proto.TypeRegistered, Name: "wrong", URL: "https://wrong"})
			}
		}
	})
	c := dialFake(t, fe, Options{RegisterTimeout: 300 * time.Millisecond})

	if url, err := c.Register("api", 3000); err == nil {
		t.Fatalf("register must time out on a mismatched-name reply, got url=%q", url)
	}
}
