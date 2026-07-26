package e2e

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/dynamismlabs/beamd/internal/edge"
	"github.com/dynamismlabs/beamd/internal/proto"
	"github.com/dynamismlabs/beamd/internal/tunnel"
)

func dialProtocolTestSession(t *testing.T, address string) tunnel.Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var (
		session tunnel.Session
		err     error
	)
	if e2eTransport(t) == "quic" {
		session, err = tunnel.NewQUICDialer().Dial(ctx, address, &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec // local self-signed E2E edge
		})
	} else {
		raw, dialErr := (&tls.Dialer{Config: &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec // local self-signed E2E edge
			NextProtos:         []string{edge.ALPNBeam},
		}}).DialContext(ctx, "tcp", address)
		if dialErr != nil {
			err = dialErr
		} else {
			tlsConn := raw.(*tls.Conn)
			if got := tlsConn.ConnectionState().NegotiatedProtocol; got != edge.ALPNBeam {
				_ = tlsConn.Close()
				t.Fatalf("negotiated ALPN = %q, want %q", got, edge.ALPNBeam)
			}
			session, err = tunnel.NewYamuxClient(tlsConn, tunnel.DefaultStreamWindow)
		}
	}
	if err != nil {
		t.Fatalf("dial %s protocol session: %v", e2eTransport(t), err)
	}
	t.Cleanup(func() {
		_ = session.CloseWithError(tunnel.CloseNormal, "test cleanup")
		select {
		case <-session.Done():
		case <-time.After(time.Second):
			t.Errorf("protocol test session did not close")
		}
	})
	return session
}

func openProtocolControl(t *testing.T, session tunnel.Session, version int) (tunnel.Stream, string, []byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	control, err := session.OpenStream(ctx)
	if err != nil {
		return nil, "", nil, err
	}
	if err := proto.Write(control, &proto.Hello{
		Type:         proto.TypeHello,
		Token:        "T1",
		ProtoVersion: version,
	}); err != nil {
		return control, "", nil, err
	}
	typ, line, err := proto.Read(bufio.NewReader(control))
	return control, typ, line, err
}

func TestTransportProtocol_EdgeRejectsBadVersion(t *testing.T) {
	_, edgeAddr := startEdge(t, map[string]string{"T1": "turing"})
	session := dialProtocolTestSession(t, edgeAddr)
	_, typ, line, readErr := openProtocolControl(t, session, proto.ProtoVersion+1)
	if readErr == nil {
		if typ != proto.TypeError {
			t.Fatalf("bad-version reply type = %q, want error", typ)
		}
		var rejection proto.Error
		if err := json.Unmarshal(line, &rejection); err != nil {
			t.Fatalf("decode bad-version reply: %v", err)
		}
		if rejection.Code != proto.CodeBadVersion {
			t.Fatalf("bad-version code = %q, want %q", rejection.Code, proto.CodeBadVersion)
		}
	}

	select {
	case <-session.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("bad protocol version did not terminate the session")
	}
	if readErr != nil && session.Kind() == tunnel.KindQUIC {
		info := session.CloseInfo()
		if !info.CodeValid || !info.Remote || info.Code != tunnel.CloseProtocol {
			t.Fatalf("QUIC bad-version CloseInfo = %+v, want remote protocol close (read error %v)", info, readErr)
		}
	}
}

func TestTransportProtocol_SecondAgentStreamAndControlCloseAreTerminal(t *testing.T) {
	_, edgeAddr := startEdge(t, map[string]string{"T1": "turing"})
	session := dialProtocolTestSession(t, edgeAddr)
	control, typ, line, err := openProtocolControl(t, session, proto.ProtoVersion)
	if err != nil {
		t.Fatalf("hello: %v", err)
	}
	if typ != proto.TypeHelloOK {
		t.Fatalf("hello reply type = %q line = %q, want hello_ok", typ, line)
	}

	// yamux has no peer-advertised incoming-stream credit, so a live-control
	// second stream directly exercises the edge's application invariant. QUIC
	// enforces MaxIncomingStreams=1 on the wire; the close race below exercises
	// its transition as that sole control stream becomes terminal.
	if session.Kind() == tunnel.KindYamux {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		unexpected, openErr := session.OpenStream(ctx)
		cancel()
		if openErr != nil {
			t.Fatalf("open unexpected yamux stream: %v", openErr)
		}
		_, _ = unexpected.Write([]byte("unexpected"))
	}

	// Race a fresh agent-side open against control EOF. The only acceptable
	// outcomes are that the open is rejected or briefly succeeds and is reset;
	// in either case the whole protocol session must become terminal promptly.
	openResult := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		stream, openErr := session.OpenStream(ctx)
		if openErr == nil {
			_, _ = stream.Write([]byte("raced"))
			stream.Abort(tunnel.StreamCanceled)
		}
		openResult <- openErr
	}()
	_ = control.Close()

	select {
	case openErr := <-openResult:
		if openErr != nil &&
			!errors.Is(openErr, tunnel.ErrSessionClosed) &&
			!errors.Is(openErr, net.ErrClosed) &&
			!errors.Is(openErr, context.Canceled) {
			var netErr net.Error
			if !errors.As(openErr, &netErr) {
				t.Fatalf("raced second-stream error = %v", openErr)
			}
		}
	case <-time.After(3 * time.Second):
		t.Fatal("second-stream/control-close race hung")
	}
	select {
	case <-session.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("second agent stream/control EOF did not terminate session")
	}
}
