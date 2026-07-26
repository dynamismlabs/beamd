package tunnel

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
)

type fakeQUICStream struct {
	mu sync.Mutex

	readErr error

	blockWrite   bool
	writeStarted chan struct{}
	writeRelease chan struct{}
	writeOnce    sync.Once
	releaseOnce  sync.Once
	writing      atomic.Bool

	closeCalls       int
	cancelReadCalls  int
	cancelWriteCalls int
	closeDuringWrite bool
	writeDeadline    time.Time
}

type fakeQUICSessionConn struct {
	ctx    context.Context
	cancel context.CancelCauseFunc

	mu           sync.Mutex
	openStream   func(context.Context) (quicBidiStream, error)
	acceptStream func(context.Context) (quicBidiStream, error)
	closeCalls   []quic.ApplicationErrorCode
	wireCode     quic.ApplicationErrorCode
	wireCodeSet  bool
	blockFirst   bool
	firstEntered chan struct{}
	releaseFirst chan struct{}
}

func newFakeQUICSessionConn() *fakeQUICSessionConn {
	ctx, cancel := context.WithCancelCause(context.Background())
	return &fakeQUICSessionConn{
		ctx:          ctx,
		cancel:       cancel,
		firstEntered: make(chan struct{}),
		releaseFirst: make(chan struct{}),
	}
}

func (c *fakeQUICSessionConn) OpenStreamSync(ctx context.Context) (quicBidiStream, error) {
	if c.openStream != nil {
		return c.openStream(ctx)
	}
	return nil, errors.New("unused")
}
func (c *fakeQUICSessionConn) AcceptStream(ctx context.Context) (quicBidiStream, error) {
	if c.acceptStream != nil {
		return c.acceptStream(ctx)
	}
	return nil, errors.New("unused")
}
func (c *fakeQUICSessionConn) Context() context.Context { return c.ctx }
func (c *fakeQUICSessionConn) CloseWithError(code quic.ApplicationErrorCode, reason string) error {
	c.mu.Lock()
	callIndex := len(c.closeCalls)
	c.closeCalls = append(c.closeCalls, code)
	block := c.blockFirst && callIndex == 0
	c.mu.Unlock()
	if block {
		close(c.firstEntered)
		<-c.releaseFirst
	}
	c.mu.Lock()
	if !c.wireCodeSet {
		c.wireCode = code
		c.wireCodeSet = true
	}
	c.mu.Unlock()
	c.cancel(&quic.ApplicationError{
		ErrorCode:    code,
		ErrorMessage: reason,
	})
	return nil
}
func (c *fakeQUICSessionConn) LocalAddr() net.Addr {
	return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}
}
func (c *fakeQUICSessionConn) RemoteAddr() net.Addr {
	return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 2}
}

func newFakeQUICStream() *fakeQUICStream {
	return &fakeQUICStream{
		writeStarted: make(chan struct{}),
		writeRelease: make(chan struct{}),
	}
}

func (s *fakeQUICStream) Read([]byte) (int, error) {
	return 0, s.readErr
}

func (s *fakeQUICStream) Write(p []byte) (int, error) {
	if !s.blockWrite {
		return len(p), nil
	}
	s.writing.Store(true)
	s.writeOnce.Do(func() { close(s.writeStarted) })
	<-s.writeRelease
	s.writing.Store(false)
	return 0, net.ErrClosed
}

func (s *fakeQUICStream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeCalls++
	s.closeDuringWrite = s.closeDuringWrite || s.writing.Load()
	return nil
}

func (s *fakeQUICStream) CancelRead(quic.StreamErrorCode) {
	s.mu.Lock()
	s.cancelReadCalls++
	s.mu.Unlock()
}

func (s *fakeQUICStream) CancelWrite(quic.StreamErrorCode) {
	s.mu.Lock()
	s.cancelWriteCalls++
	s.mu.Unlock()
	s.releaseOnce.Do(func() { close(s.writeRelease) })
}

func (s *fakeQUICStream) SetDeadline(time.Time) error     { return nil }
func (s *fakeQUICStream) SetReadDeadline(time.Time) error { return nil }
func (s *fakeQUICStream) SetWriteDeadline(deadline time.Time) error {
	s.mu.Lock()
	s.writeDeadline = deadline
	s.mu.Unlock()
	return nil
}

func newTestQUICStream(raw quicBidiStream) *quicStream {
	parent := &QUICSession{state: newSessionState()}
	local := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}
	remote := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 2}
	stream := newQUICStreamWithAddresses(raw, parent, local, remote)
	if !parent.state.register(stream) {
		panic("test parent unexpectedly closed")
	}
	return stream
}

func TestQUICConfigContract(t *testing.T) {
	client := ClientQUICConfig()
	server := ServerQUICConfig()
	for name, cfg := range map[string]*quic.Config{"client": client, "server": server} {
		if cfg.HandshakeIdleTimeout != 10*time.Second ||
			cfg.MaxIdleTimeout != 75*time.Second ||
			cfg.KeepAlivePeriod != 0 ||
			cfg.InitialStreamReceiveWindow != 4<<20 ||
			cfg.MaxStreamReceiveWindow != 16<<20 ||
			cfg.InitialConnectionReceiveWindow != 16<<20 ||
			cfg.MaxConnectionReceiveWindow != 64<<20 ||
			cfg.MaxIncomingUniStreams != -1 ||
			cfg.EnableDatagrams ||
			cfg.DisablePathMTUDiscovery {
			t.Errorf("%s config does not match contract: %+v", name, cfg)
		}
	}
	if client.MaxIncomingStreams != 64 {
		t.Errorf("client MaxIncomingStreams = %d, want 64", client.MaxIncomingStreams)
	}
	if server.MaxIncomingStreams != 1 {
		t.Errorf("server MaxIncomingStreams = %d, want 1", server.MaxIncomingStreams)
	}
	if server.Allow0RTT {
		t.Error("server Allow0RTT = true, want false")
	}
}

func TestQUICCloseWriteThenAbortEscalates(t *testing.T) {
	raw := newFakeQUICStream()
	stream := newTestQUICStream(raw)
	if err := stream.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	select {
	case <-stream.Done():
		t.Fatal("CloseWrite alone closed Done")
	default:
	}
	stream.Abort(StreamCanceled)
	awaitClosed(t, stream.Done(), "QUIC stream Done")

	raw.mu.Lock()
	defer raw.mu.Unlock()
	if raw.closeCalls != 1 || raw.cancelWriteCalls != 1 || raw.cancelReadCalls != 1 {
		t.Fatalf("calls close/cancelWrite/cancelRead = %d/%d/%d, want 1/1/1",
			raw.closeCalls, raw.cancelWriteCalls, raw.cancelReadCalls)
	}
}

func completeQUICStreamOrdinarily(stream *quicStream) {
	_ = stream.CloseWrite()
	stream.markReceiveTerminal()
}

func TestQUICOrdinaryCompletionPublishesDoneBeforeUnregister(t *testing.T) {
	stream := newTestQUICStream(newFakeQUICStream())
	state := stream.parent.state

	// Holding the registry mutex stops unregister at the exact boundary under
	// test. Ordinary completion must still publish Stream.Done before blocking
	// on that mutex; the old unregister-then-close order cannot pass this seam.
	state.mu.Lock()
	childReturned := make(chan struct{})
	go func() {
		completeQUICStreamOrdinarily(stream)
		close(childReturned)
	}()
	select {
	case <-stream.Done():
	case <-time.After(time.Second):
		state.mu.Unlock()
		t.Fatal("QUIC Stream.Done was not published before unregister")
	}

	parentReturned := make(chan struct{})
	go func() {
		state.finish(CloseInfo{Reason: "network"})
		close(parentReturned)
	}()
	select {
	case <-state.doneChan():
		state.mu.Unlock()
		t.Fatal("QUIC Session.Done closed while the registry mutex was held")
	default:
	}
	state.mu.Unlock()

	awaitClosed(t, childReturned, "ordinary QUIC completion")
	awaitClosed(t, parentReturned, "QUIC parent finish")
	awaitClosed(t, state.doneChan(), "QUIC Session.Done")
	select {
	case <-stream.Done():
	default:
		t.Fatal("QUIC Session.Done became observable before Stream.Done")
	}
}

func TestQUICOrdinaryCompletionRaceParentFinish(t *testing.T) {
	const iterations = 1_000
	for i := range iterations {
		stream := newTestQUICStream(newFakeQUICStream())
		state := stream.parent.state
		start := make(chan struct{})
		var workers sync.WaitGroup
		workers.Add(2)
		go func() {
			defer workers.Done()
			<-start
			completeQUICStreamOrdinarily(stream)
		}()
		go func() {
			defer workers.Done()
			<-start
			state.finish(CloseInfo{Reason: "network"})
		}()
		close(start)

		<-state.doneChan()
		select {
		case <-stream.Done():
		default:
			t.Fatalf("iteration %d: QUIC Session.Done won before Stream.Done", i)
		}
		workers.Wait()
	}
}

func TestQUICNormalCloseSendsFINWithoutWriteReset(t *testing.T) {
	raw := newFakeQUICStream()
	stream := newTestQUICStream(raw)
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	awaitClosed(t, stream.Done(), "QUIC stream Done")

	raw.mu.Lock()
	defer raw.mu.Unlock()
	if raw.closeCalls != 1 || raw.cancelWriteCalls != 0 || raw.cancelReadCalls != 1 {
		t.Fatalf("calls close/cancelWrite/cancelRead = %d/%d/%d, want 1/0/1",
			raw.closeCalls, raw.cancelWriteCalls, raw.cancelReadCalls)
	}
	if raw.writeDeadline.IsZero() {
		t.Fatal("Close did not set the immediate write deadline")
	}
}

func TestQUICAbortUnblocksWriterAndPreventsConcurrentClose(t *testing.T) {
	raw := newFakeQUICStream()
	raw.blockWrite = true
	stream := newTestQUICStream(raw)

	writeDone := make(chan error, 1)
	go func() {
		_, err := stream.Write([]byte("blocked"))
		writeDone <- err
	}()
	<-raw.writeStarted

	closeDone := make(chan error, 1)
	go func() { closeDone <- stream.Close() }()
	stream.Abort(StreamCanceled)

	if err := <-writeDone; !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Write error = %v, want net.ErrClosed", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatalf("Close: %v", err)
	}
	raw.mu.Lock()
	defer raw.mu.Unlock()
	if raw.closeDuringWrite {
		t.Fatal("underlying QUIC Close raced an in-flight Write")
	}
	if raw.closeCalls != 0 {
		t.Fatalf("underlying Close calls = %d, want 0 after Abort won", raw.closeCalls)
	}
	if raw.cancelWriteCalls != 1 {
		t.Fatalf("CancelWrite calls = %d, want 1", raw.cancelWriteCalls)
	}
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "deadline" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }
func (timeoutError) Unwrap() error   { return os.ErrDeadlineExceeded }

func TestQUICReadDeadlineDoesNotTerminalizeReceive(t *testing.T) {
	raw := newFakeQUICStream()
	raw.readErr = timeoutError{}
	stream := newTestQUICStream(raw)
	if _, err := stream.Read(nil); err == nil {
		t.Fatal("Read returned nil error")
	}
	if err := stream.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	select {
	case <-stream.Done():
		t.Fatal("deadline timeout marked receive direction terminal")
	default:
	}
	stream.Abort(StreamCanceled)
}

func TestQUICFINAndRemoteEOFCompleteStream(t *testing.T) {
	raw := newFakeQUICStream()
	raw.readErr = io.EOF
	stream := newTestQUICStream(raw)
	if err := stream.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	if _, err := stream.Read(nil); !errors.Is(err, io.EOF) {
		t.Fatalf("Read error = %v, want EOF", err)
	}
	awaitClosed(t, stream.Done(), "QUIC stream Done")
}

func TestQUICOpenStreamTimeoutOwnership(t *testing.T) {
	t.Run("adapter timeout", func(t *testing.T) {
		raw := newFakeQUICSessionConn()
		t.Cleanup(func() { raw.cancel(context.Canceled) })
		started := make(chan struct{})
		cause := make(chan error, 1)
		raw.openStream = func(ctx context.Context) (quicBidiStream, error) {
			close(started)
			<-ctx.Done()
			cause <- context.Cause(ctx)
			return nil, ctx.Err()
		}
		session := newQUICSession(raw)
		session.openTimeout = 20 * time.Millisecond

		_, err := session.OpenStream(context.Background())
		if err != ErrOpenTimeout {
			t.Fatalf("OpenStream error = %v, want exact ErrOpenTimeout", err)
		}
		<-started
		if got := <-cause; !errors.Is(got, ErrOpenTimeout) {
			t.Fatalf("derived context cause = %v, want ErrOpenTimeout", got)
		}
		if raw.ctx.Err() != nil {
			t.Fatal("adapter-owned stream timeout closed the healthy QUIC session")
		}
	})

	t.Run("caller cancellation", func(t *testing.T) {
		raw := newFakeQUICSessionConn()
		t.Cleanup(func() { raw.cancel(context.Canceled) })
		started := make(chan struct{})
		raw.openStream = func(ctx context.Context) (quicBidiStream, error) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		session := newQUICSession(raw)
		session.openTimeout = time.Second
		ctx, cancel := context.WithCancel(context.Background())

		result := make(chan error, 1)
		go func() {
			_, err := session.OpenStream(ctx)
			result <- err
		}()
		<-started
		cancel()
		if err := <-result; err != context.Canceled {
			t.Fatalf("OpenStream error = %v, want exact context.Canceled", err)
		}
		if raw.ctx.Err() != nil {
			t.Fatal("caller cancellation closed the healthy QUIC session")
		}
	})

	t.Run("caller deadline", func(t *testing.T) {
		raw := newFakeQUICSessionConn()
		t.Cleanup(func() { raw.cancel(context.Canceled) })
		raw.openStream = func(ctx context.Context) (quicBidiStream, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		session := newQUICSession(raw)
		session.openTimeout = time.Second
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()

		_, err := session.OpenStream(ctx)
		if err != context.DeadlineExceeded {
			t.Fatalf("OpenStream error = %v, want exact context.DeadlineExceeded", err)
		}
		if errors.Is(err, ErrOpenTimeout) {
			t.Fatalf("caller deadline was rewritten as adapter timeout: %v", err)
		}
	})
}

func TestCloseInfoFromQUICClassification(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		code      ErrorCode
		codeValid bool
		remote    bool
		reason    string
		label     string
	}{
		{
			name: "local application",
			err: &quic.ApplicationError{
				ErrorCode:    quic.ApplicationErrorCode(CloseShutdown),
				ErrorMessage: "local shutdown",
			},
			code:      CloseShutdown,
			codeValid: true,
			reason:    "local shutdown",
			label:     "shutdown",
		},
		{
			name: "remote application",
			err: &quic.ApplicationError{
				Remote:       true,
				ErrorCode:    quic.ApplicationErrorCode(CloseAuth),
				ErrorMessage: "peer rejected authentication",
			},
			code:      CloseAuth,
			codeValid: true,
			remote:    true,
			reason:    "peer rejected authentication",
			label:     "protocol",
		},
		{
			name:   "idle timeout",
			err:    &quic.IdleTimeoutError{},
			reason: "idle",
			label:  "idle",
		},
		{
			name:   "stateless reset",
			err:    &quic.StatelessResetError{},
			reason: "network",
			label:  "network",
		},
		{
			name: "no viable path",
			err: &quic.TransportError{
				Remote:    true,
				ErrorCode: quic.NoViablePathError,
			},
			remote: true,
			reason: "network",
			label:  "network",
		},
		{
			name: "transport violation",
			err: &quic.TransportError{
				Remote:    true,
				ErrorCode: quic.ProtocolViolation,
			},
			remote: true,
			reason: "protocol",
			label:  "protocol",
		},
		{
			name:   "version negotiation",
			err:    &quic.VersionNegotiationError{},
			reason: "protocol",
			label:  "protocol",
		},
		{
			name: "socket error",
			err: &net.OpError{
				Op:  "read",
				Net: "udp",
				Err: syscall.ECONNRESET,
			},
			reason: "network",
			label:  "network",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			wrapped := errors.Join(errors.New("outer context"), test.err)
			info := closeInfoFromQUIC(wrapped)
			if info.Code != test.code ||
				info.CodeValid != test.codeValid ||
				info.Remote != test.remote ||
				info.Reason != test.reason {
				t.Fatalf("CloseInfo = %+v, want code=%#x valid=%v remote=%v reason=%q",
					info, test.code, test.codeValid, test.remote, test.reason)
			}
			if info.Cause != wrapped {
				t.Fatalf("Cause = %v, want original wrapped error %v", info.Cause, wrapped)
			}
			if got := CloseReason(info); got != test.label {
				t.Fatalf("CloseReason = %q, want %q", got, test.label)
			}
		})
	}
}

func TestQUICConcurrentCloseSendsOnlyWinningCode(t *testing.T) {
	raw := newFakeQUICSessionConn()
	raw.blockFirst = true
	session := newQUICSession(raw)

	firstDone := make(chan error, 1)
	go func() {
		firstDone <- session.CloseWithError(CloseProtocol, "first")
	}()
	select {
	case <-raw.firstEntered:
	case <-time.After(time.Second):
		t.Fatal("first raw close did not start")
	}

	secondDone := make(chan error, 1)
	go func() {
		secondDone <- session.CloseWithError(CloseShutdown, "second")
	}()
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatalf("losing CloseWithError: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("losing CloseWithError blocked on the raw connection")
	}
	close(raw.releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatalf("winning CloseWithError: %v", err)
	}

	raw.mu.Lock()
	calls := append([]quic.ApplicationErrorCode(nil), raw.closeCalls...)
	wireCode := raw.wireCode
	raw.mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("raw CloseWithError calls = %v, want exactly one", calls)
	}
	info := session.CloseInfo()
	if !info.CodeValid || info.Code != CloseProtocol {
		t.Fatalf("local CloseInfo = %+v, want protocol winner", info)
	}
	if ErrorCode(wireCode) != info.Code {
		t.Fatalf("wire close code = %#x, local code = %#x", wireCode, info.Code)
	}
}

func TestQUICClosePreservesAlreadyTerminalRemoteEvent(t *testing.T) {
	raw := newFakeQUICSessionConn()
	raw.cancel(&quic.ApplicationError{
		Remote:       true,
		ErrorCode:    quic.ApplicationErrorCode(CloseShutdown),
		ErrorMessage: "remote shutdown",
	})
	// Deliberately omit the watcher so CloseWithError itself must observe the
	// already-terminal raw context rather than relying on goroutine scheduling.
	session := &QUICSession{raw: raw, state: newSessionState()}
	if err := session.CloseWithError(CloseProtocol, "late cleanup"); err != nil {
		t.Fatalf("CloseWithError: %v", err)
	}
	info := session.CloseInfo()
	if !info.CodeValid || info.Code != CloseShutdown || !info.Remote {
		t.Fatalf("CloseInfo = %+v, want remote shutdown", info)
	}
	raw.mu.Lock()
	closeCalls := len(raw.closeCalls)
	raw.mu.Unlock()
	if closeCalls != 0 {
		t.Fatalf("raw close calls = %d, want zero after remote terminal event", closeCalls)
	}
}

func TestQUICRepeatedConcurrentTerminalCalls(t *testing.T) {
	t.Run("stream", func(t *testing.T) {
		raw := newFakeQUICStream()
		stream := newTestQUICStream(raw)
		const calls = 128
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(calls)
		for i := range calls {
			go func() {
				defer wg.Done()
				<-start
				switch i % 3 {
				case 0:
					_ = stream.Close()
				case 1:
					_ = stream.CloseWrite()
				default:
					stream.Abort(StreamCanceled)
				}
			}()
		}
		close(start)
		wg.Wait()
		awaitClosed(t, stream.Done(), "repeatedly terminalized QUIC stream")

		raw.mu.Lock()
		defer raw.mu.Unlock()
		if raw.closeCalls > 1 {
			t.Fatalf("underlying graceful close calls = %d, want at most 1", raw.closeCalls)
		}
		if raw.cancelWriteCalls != 1 {
			t.Fatalf("underlying write-reset calls = %d, want exactly 1", raw.cancelWriteCalls)
		}
		if raw.cancelReadCalls == 0 {
			t.Fatal("underlying receive direction was never canceled")
		}
	})

	t.Run("session", func(t *testing.T) {
		raw := newFakeQUICSessionConn()
		session := newQUICSession(raw)
		const calls = 128
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(calls)
		for i := range calls {
			go func() {
				defer wg.Done()
				<-start
				code := CloseProtocol
				if i%2 == 0 {
					code = CloseShutdown
				}
				if err := session.CloseWithError(code, "concurrent close"); err != nil {
					t.Errorf("CloseWithError: %v", err)
				}
			}()
		}
		close(start)
		wg.Wait()
		awaitClosed(t, session.Done(), "repeatedly terminalized QUIC session")

		raw.mu.Lock()
		closeCalls := len(raw.closeCalls)
		raw.mu.Unlock()
		if closeCalls != 1 {
			t.Fatalf("raw CloseWithError calls = %d, want exactly 1", closeCalls)
		}
		info := session.CloseInfo()
		if !info.CodeValid || info.Remote ||
			(info.Code != CloseProtocol && info.Code != CloseShutdown) {
			t.Fatalf("CloseInfo = %+v, want one local winning application close", info)
		}
	})
}

type fakeResolver struct {
	ips   []net.IPAddr
	err   error
	block bool
}

func (r *fakeResolver) LookupIPAddr(ctx context.Context, _ string) ([]net.IPAddr, error) {
	if r.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return r.ips, r.err
}

func TestQUICDialerNumericRetriesAndOriginalSNI(t *testing.T) {
	resolver := &fakeResolver{ips: []net.IPAddr{
		{IP: net.IPv4(192, 0, 2, 1)},
		{IP: net.IPv4(192, 0, 2, 2)},
	}}
	dialer := NewQUICDialerWithResolver(resolver)
	var addresses []string
	dialer.dialAddr = func(_ context.Context, address string, tlsConfig *tls.Config, _ *quic.Config) (*quic.Conn, error) {
		addresses = append(addresses, address)
		if tlsConfig.ServerName != "edge.example.test" {
			t.Fatalf("ServerName = %q, want original host", tlsConfig.ServerName)
		}
		if len(addresses) == 1 {
			return nil, &net.OpError{Op: "dial", Net: "udp", Err: syscall.ECONNREFUSED}
		}
		return nil, errors.New("terminal handshake result")
	}
	_, err := dialer.Dial(context.Background(), "edge.example.test:443", &tls.Config{})
	var failure *DialFailure
	if !errors.As(err, &failure) || failure.Category != DialTerminal {
		t.Fatalf("Dial error = %v, want terminal DialFailure", err)
	}
	if len(addresses) != 2 ||
		addresses[0] != "192.0.2.1:443" ||
		addresses[1] != "192.0.2.2:443" {
		t.Fatalf("numeric attempts = %v", addresses)
	}
}

func TestQUICDialerDNSUsesCandidateContext(t *testing.T) {
	dialer := NewQUICDialerWithResolver(&fakeResolver{block: true})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := dialer.Dial(ctx, "edge.example.test:443", &tls.Config{})
	var failure *DialFailure
	if !errors.As(err, &failure) || failure.Category != DialTimeout {
		t.Fatalf("Dial error = %v, want timeout DialFailure", err)
	}
}

func TestQUICDialerCancellationJoinsMultiAddressAttempt(t *testing.T) {
	resolver := &fakeResolver{ips: []net.IPAddr{
		{IP: net.IPv4(192, 0, 2, 1)},
		{IP: net.IPv4(192, 0, 2, 2)},
		{IP: net.IPv4(192, 0, 2, 3)},
	}}
	dialer := NewQUICDialerWithResolver(resolver)
	var attempts atomic.Int64
	var active atomic.Int64
	var maxActive atomic.Int64
	secondStarted := make(chan struct{})
	secondExited := make(chan struct{})
	dialer.dialAddr = func(ctx context.Context, _ string, _ *tls.Config, _ *quic.Config) (*quic.Conn, error) {
		attempt := attempts.Add(1)
		nowActive := active.Add(1)
		for {
			seen := maxActive.Load()
			if nowActive <= seen || maxActive.CompareAndSwap(seen, nowActive) {
				break
			}
		}
		defer active.Add(-1)
		if attempt == 1 {
			return nil, &net.OpError{Op: "dial", Net: "udp", Err: syscall.ECONNREFUSED}
		}
		if attempt != 2 {
			t.Fatalf("unexpected background/third dial attempt %d", attempt)
		}
		close(secondStarted)
		defer close(secondExited)
		<-ctx.Done()
		return nil, ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := dialer.Dial(ctx, "edge.example.test:443", &tls.Config{})
		result <- err
	}()
	<-secondStarted
	cancel()
	err := <-result
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Dial error = %v, want context.Canceled", err)
	}
	select {
	case <-secondExited:
	default:
		t.Fatal("Dial returned before the canceled address attempt exited")
	}
	if got := active.Load(); got != 0 {
		t.Fatalf("active dial attempts after return = %d, want 0", got)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("dial attempts = %d, want exactly 2", got)
	}
	if got := maxActive.Load(); got != 1 {
		t.Fatalf("concurrent dial attempts = %d, want exactly 1", got)
	}
}

func TestQUICDialerCanceledAttemptsDoNotLeakUDPSockets(t *testing.T) {
	before, err := os.ReadDir("/dev/fd")
	if err != nil {
		t.Skipf("open file-descriptor accounting unavailable: %v", err)
	}
	blackhole, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen UDP blackhole: %v", err)
	}
	defer blackhole.Close()

	// quic.DialAddr owns a fresh UDP socket per attempt. Repetition makes an
	// otherwise one-descriptor leak unambiguous while retaining a small
	// allowance for platform/runtime descriptor bookkeeping.
	dialer := NewQUICDialer()
	for range 16 {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
		_, dialErr := dialer.Dial(ctx, blackhole.LocalAddr().String(), &tls.Config{InsecureSkipVerify: true})
		cancel()
		if dialErr == nil {
			t.Fatal("dial to non-QUIC UDP blackhole unexpectedly succeeded")
		}
	}
	after, err := os.ReadDir("/dev/fd")
	if err != nil {
		t.Fatalf("count final open file descriptors: %v", err)
	}
	if growth := len(after) - len(before); growth > 2 {
		t.Fatalf("open descriptors grew by %d after canceled QUIC attempts; sockets were not reclaimed", growth)
	}
}

func TestQUICAttemptResolutionIsOneWinner(t *testing.T) {
	t.Run("observer wins", func(t *testing.T) {
		observed := make(chan error, 1)
		attempt := newQUICAttempt(func(err error) { observed <- err })
		ctx, cancel := context.WithCancelCause(context.Background())
		watchDone := make(chan struct{})
		go func() {
			attempt.watch(ctx)
			close(watchDone)
		}()

		wantErr := errors.New("handshake failed before accept")
		cancel(wantErr)
		select {
		case got := <-observed:
			if !errors.Is(got, wantErr) {
				t.Fatalf("observed error = %v, want %v", got, wantErr)
			}
		case <-time.After(time.Second):
			t.Fatal("handshake failure was not observed")
		}
		<-watchDone
		if attempt.markAccepted() {
			t.Fatal("acceptance won after the observer resolved the attempt as failed")
		}
	})

	t.Run("acceptance wins", func(t *testing.T) {
		observed := make(chan error, 1)
		attempt := newQUICAttempt(func(err error) { observed <- err })
		if !attempt.markAccepted() {
			t.Fatal("first acceptance did not win")
		}
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(errors.New("connection closed after acceptance"))
		attempt.watch(ctx)
		select {
		case err := <-observed:
			t.Fatalf("accepted connection also emitted a handshake error: %v", err)
		default:
		}
	})
}

func TestQUICDialerTerminalCertificateFailureDoesNotRetry(t *testing.T) {
	resolver := &fakeResolver{ips: []net.IPAddr{
		{IP: net.IPv4(192, 0, 2, 1)},
		{IP: net.IPv4(192, 0, 2, 2)},
	}}
	dialer := NewQUICDialerWithResolver(resolver)
	attempts := 0
	dialer.dialAddr = func(context.Context, string, *tls.Config, *quic.Config) (*quic.Conn, error) {
		attempts++
		return nil, x509.UnknownAuthorityError{Cert: &x509.Certificate{}}
	}
	_, err := dialer.Dial(context.Background(), "edge.example.test:443", &tls.Config{})
	var failure *DialFailure
	if !errors.As(err, &failure) || failure.Category != DialTerminal {
		t.Fatalf("Dial error = %v, want terminal DialFailure", err)
	}
	if attempts != 1 {
		t.Fatalf("dial attempts = %d, want 1", attempts)
	}
}

func TestQUICDialerRetriesAddressSpecificTransportFailures(t *testing.T) {
	for _, code := range []quic.TransportErrorCode{
		quic.NoViablePathError,
		quic.ConnectionRefused,
	} {
		t.Run(code.String(), func(t *testing.T) {
			resolver := &fakeResolver{ips: []net.IPAddr{
				{IP: net.IPv4(192, 0, 2, 1)},
				{IP: net.IPv4(192, 0, 2, 2)},
			}}
			dialer := NewQUICDialerWithResolver(resolver)
			attempts := 0
			dialer.dialAddr = func(context.Context, string, *tls.Config, *quic.Config) (*quic.Conn, error) {
				attempts++
				if attempts == 1 {
					return nil, &quic.TransportError{ErrorCode: code}
				}
				return nil, x509.UnknownAuthorityError{Cert: &x509.Certificate{}}
			}
			_, err := dialer.Dial(context.Background(), "edge.example.test:443", &tls.Config{})
			var failure *DialFailure
			if !errors.As(err, &failure) || failure.Category != DialTerminal {
				t.Fatalf("Dial error = %v, want second address's terminal failure", err)
			}
			if attempts != 2 {
				t.Fatalf("dial attempts = %d, want 2", attempts)
			}
		})
	}
}

func TestQUICSessionIntegrationAndRemoteCloseInfo(t *testing.T) {
	serverTLS := testServerTLSConfig(t)
	dataDir := t.TempDir()
	listener, transport, err := ListenQUIC("127.0.0.1:0", serverTLS, dataDir, nil)
	if err != nil {
		t.Fatalf("ListenQUIC: %v", err)
	}
	t.Cleanup(func() { _ = transport.Close() })
	for _, name := range []string{statelessResetKeyFile, tokenGeneratorKeyFile} {
		path := filepath.Join(dataDir, name)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %o, want 0600", name, info.Mode().Perm())
		}
		data, err := os.ReadFile(path)
		if err != nil || len(data) != 32 {
			t.Fatalf("%s length/error = %d/%v, want 32/nil", name, len(data), err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	acceptCh := make(chan struct {
		session Session
		err     error
	}, 1)
	go func() {
		session, err := listener.Accept(ctx)
		acceptCh <- struct {
			session Session
			err     error
		}{session, err}
	}()

	client, err := NewQUICDialer().Dial(ctx, listener.Addr().String(), &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("QUIC Dial: %v", err)
	}
	accepted := <-acceptCh
	if accepted.err != nil {
		t.Fatalf("QUIC Accept: %v", accepted.err)
	}
	server := accepted.session

	clientStream, serverStream := openSessionStreamPair(t, client, server)
	go func() {
		_ = clientStream.CloseWrite()
	}()
	request, err := io.ReadAll(serverStream)
	if err != nil || string(request) != "request" {
		t.Fatalf("server request = %q, %v", request, err)
	}
	go func() {
		_, _ = serverStream.Write([]byte("response"))
		_ = serverStream.CloseWrite()
	}()
	response, err := io.ReadAll(clientStream)
	if err != nil || string(response) != "response" {
		t.Fatalf("client response = %q, %v", response, err)
	}
	awaitClosed(t, clientStream.Done(), "client QUIC stream Done")
	awaitClosed(t, serverStream.Done(), "server QUIC stream Done")

	if err := server.CloseWithError(CloseProtocol, "bad control"); err != nil {
		t.Fatalf("server CloseWithError: %v", err)
	}
	awaitClosed(t, client.Done(), "client QUIC session Done")
	info := client.CloseInfo()
	if !info.CodeValid || info.Code != CloseProtocol || !info.Remote || info.Reason != "bad control" {
		t.Fatalf("client CloseInfo = %+v, want remote protocol close", info)
	}
}

func TestQUICKeysReuseAndRejectMalformed(t *testing.T) {
	dataDir := t.TempDir()
	first, err := loadQUICServerKeys(dataDir)
	if err != nil {
		t.Fatalf("first loadQUICServerKeys: %v", err)
	}
	second, err := loadQUICServerKeys(dataDir)
	if err != nil {
		t.Fatalf("second loadQUICServerKeys: %v", err)
	}
	if first != second {
		t.Fatal("persisted QUIC keys were not reused")
	}
	if err := os.WriteFile(filepath.Join(dataDir, statelessResetKeyFile), []byte("short"), 0o600); err != nil {
		t.Fatalf("write malformed key: %v", err)
	}
	if _, err := loadQUICServerKeys(dataDir); err == nil {
		t.Fatal("malformed persisted QUIC key was silently replaced")
	}
}

func TestQUICKeysRejectInsecureMode(t *testing.T) {
	dataDir := t.TempDir()
	if _, err := loadQUICServerKeys(dataDir); err != nil {
		t.Fatalf("loadQUICServerKeys: %v", err)
	}
	path := filepath.Join(dataDir, statelessResetKeyFile)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod key: %v", err)
	}
	if _, err := loadQUICServerKeys(dataDir); err == nil {
		t.Fatal("persisted QUIC key with mode 0644 was accepted")
	}
}

func openSessionStreamPair(t *testing.T, opener, accepter Session) (Stream, Stream) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	acceptCh := make(chan struct {
		stream Stream
		err    error
	}, 1)
	go func() {
		stream, err := accepter.AcceptStream(ctx)
		acceptCh <- struct {
			stream Stream
			err    error
		}{stream, err}
	}()
	opened, err := opener.OpenStream(ctx)
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	if _, err := opened.Write([]byte("request")); err != nil {
		t.Fatalf("activate QUIC stream: %v", err)
	}
	accepted := <-acceptCh
	if accepted.err != nil {
		t.Fatalf("AcceptStream: %v", accepted.err)
	}
	return opened, accepted.stream
}

func testServerTLSConfig(t *testing.T) *tls.Config {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "beamd test"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{{
			Certificate: [][]byte{der},
			PrivateKey:  privateKey,
		}},
	}
}
