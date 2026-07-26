package edge

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dynamismlabs/beamd/internal/auth"
	"github.com/dynamismlabs/beamd/internal/certs"
	"github.com/dynamismlabs/beamd/internal/config"
	"github.com/dynamismlabs/beamd/internal/proto"
	"github.com/dynamismlabs/beamd/internal/reqlog"
	"github.com/dynamismlabs/beamd/internal/tunnel"
)

type lifecyclePipeStream struct {
	net.Conn
	done      chan struct{}
	closeOnce sync.Once
}

type lifecycleRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f lifecycleRoundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type lifecycleBlockingSink struct {
	capSink
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *lifecycleBlockingSink) Record(event reqlog.RequestEvent) {
	s.once.Do(func() { close(s.started) })
	<-s.release
	s.capSink.Record(event)
}

type lifecycleHijackWriter struct {
	header http.Header
	conn   net.Conn
}

func (w *lifecycleHijackWriter) Header() http.Header { return w.header }

func (w *lifecycleHijackWriter) Write(p []byte) (int, error) { return len(p), nil }

func (w *lifecycleHijackWriter) WriteHeader(int) {}

func (w *lifecycleHijackWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.conn, bufio.NewReadWriter(bufio.NewReader(w.conn), bufio.NewWriter(w.conn)), nil
}

func newLifecyclePipeStream() (*lifecyclePipeStream, net.Conn) {
	server, client := net.Pipe()
	return &lifecyclePipeStream{Conn: server, done: make(chan struct{})}, client
}

func (s *lifecyclePipeStream) CloseWrite() error { return s.Close() }

func (s *lifecyclePipeStream) Abort(tunnel.ErrorCode) {
	_ = s.Close()
}

func (s *lifecyclePipeStream) Done() <-chan struct{} { return s.done }

func (s *lifecyclePipeStream) Close() error {
	err := s.Conn.Close()
	s.closeOnce.Do(func() { close(s.done) })
	return err
}

type lifecycleSession struct {
	kind    tunnel.Kind
	control tunnel.Stream

	acceptCalls   atomic.Int32
	acceptStarted chan struct{}
	acceptOnce    sync.Once
	// When set, a post-close AcceptStream deliberately holds its return so
	// lifecycle tests can prove the session handler joins the unexpected-stream
	// helper rather than merely asking the transport to close.
	acceptDoneRelease  <-chan struct{}
	acceptDoneObserved chan struct{}
	acceptDoneOnce     sync.Once

	closed    atomic.Bool
	done      chan struct{}
	closeOnce sync.Once
	closeMu   sync.Mutex
	closeInfo tunnel.CloseInfo
}

func newLifecycleSession(kind tunnel.Kind, control tunnel.Stream) *lifecycleSession {
	return &lifecycleSession{
		kind:          kind,
		control:       control,
		acceptStarted: make(chan struct{}),
		done:          make(chan struct{}),
	}
}

func (s *lifecycleSession) Kind() tunnel.Kind { return s.kind }

func (s *lifecycleSession) OpenStream(context.Context) (tunnel.Stream, error) {
	return nil, errors.New("unexpected OpenStream")
}

func (s *lifecycleSession) AcceptStream(ctx context.Context) (tunnel.Stream, error) {
	call := s.acceptCalls.Add(1)
	if call == 1 && s.control != nil {
		return s.control, nil
	}
	s.acceptOnce.Do(func() { close(s.acceptStarted) })
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.done:
		if s.acceptDoneObserved != nil {
			s.acceptDoneOnce.Do(func() { close(s.acceptDoneObserved) })
		}
		if s.acceptDoneRelease != nil {
			<-s.acceptDoneRelease
		}
		return nil, tunnel.ErrSessionClosed
	}
}

func (s *lifecycleSession) Done() <-chan struct{} { return s.done }

func (s *lifecycleSession) IsClosed() bool { return s.closed.Load() }

func (s *lifecycleSession) CloseInfo() tunnel.CloseInfo {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	return s.closeInfo
}

func (s *lifecycleSession) CloseWithError(code tunnel.ErrorCode, reason string) error {
	s.closeOnce.Do(func() {
		s.closeMu.Lock()
		s.closeInfo = tunnel.CloseInfo{
			Code:      code,
			CodeValid: true,
			Reason:    reason,
		}
		s.closeMu.Unlock()
		s.closed.Store(true)
		if s.control != nil {
			s.control.Abort(code)
		}
		close(s.done)
	})
	return nil
}

func (s *lifecycleSession) LocalAddr() net.Addr  { return admissionAddr("local") }
func (s *lifecycleSession) RemoteAddr() net.Addr { return admissionAddr("remote") }

type channelTunnelListener struct {
	sessions  chan tunnel.Session
	closed    chan struct{}
	closeOnce sync.Once
}

func newChannelTunnelListener() *channelTunnelListener {
	return &channelTunnelListener{
		sessions: make(chan tunnel.Session, 64),
		closed:   make(chan struct{}),
	}
}

func (l *channelTunnelListener) Accept(ctx context.Context) (tunnel.Session, error) {
	select {
	case session := <-l.sessions:
		return session, nil
	case <-l.closed:
		return nil, net.ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (l *channelTunnelListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return nil
}

func (l *channelTunnelListener) Addr() net.Addr { return admissionAddr("listener") }

type queuedTunnelListener struct {
	session   tunnel.Session
	closed    chan struct{}
	closeOnce sync.Once
	delivered atomic.Bool
}

func newQueuedTunnelListener(session tunnel.Session) *queuedTunnelListener {
	return &queuedTunnelListener{
		session: session,
		closed:  make(chan struct{}),
	}
}

func (l *queuedTunnelListener) Accept(ctx context.Context) (tunnel.Session, error) {
	if l.delivered.Load() {
		return nil, net.ErrClosed
	}
	select {
	case <-l.closed:
		if l.delivered.CompareAndSwap(false, true) {
			return l.session, nil
		}
		return nil, net.ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (l *queuedTunnelListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return nil
}

func (l *queuedTunnelListener) Addr() net.Addr { return admissionAddr("listener") }

type queuedNetListener struct {
	conn      net.Conn
	closed    chan struct{}
	closeOnce sync.Once
	delivered atomic.Bool
}

func newQueuedNetListener(conn net.Conn) *queuedNetListener {
	return &queuedNetListener{
		conn:   conn,
		closed: make(chan struct{}),
	}
}

func (l *queuedNetListener) Accept() (net.Conn, error) {
	if l.delivered.Load() {
		return nil, net.ErrClosed
	}
	<-l.closed
	if l.delivered.CompareAndSwap(false, true) {
		return l.conn, nil
	}
	return nil, net.ErrClosed
}

func (l *queuedNetListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return nil
}

func (l *queuedNetListener) Addr() net.Addr { return admissionAddr("listener") }

func newLifecycleTestEdge() *Edge {
	cfg := &config.Server{
		BaseDomain:             "edge.test",
		ListenHTTPS:            "127.0.0.1:0",
		DisableQUIC:            true,
		MaxStreamsPerSession:   config.DefaultMaxStreamsPerSession,
		MaxStreamsTotal:        config.DefaultMaxStreamsTotal,
		MaxPreAuthSessions:     config.DefaultMaxPreAuthSessions,
		MaxSessionsTotal:       config.DefaultMaxSessionsTotal,
		YamuxStreamWindowBytes: config.DefaultYamuxStreamWindowBytes,
	}
	return &Edge{
		cfg:               cfg,
		version:           "test",
		tokens:            auth.NewMemoryStore(map[string]string{"token": "scope"}),
		heartbeatTimeout:  time.Hour,
		sessions:          make(map[*Session]struct{}),
		routes:            make(map[string]*Route),
		pubSrvs:           make(map[*http.Server]struct{}),
		rawConns:          make(map[net.Conn]struct{}),
		preauth:           make(map[tunnel.Session]struct{}),
		hijacked:          make(map[net.Conn]struct{}),
		tlsHandshakeSlots: make(chan struct{}, 128),
		preAuthSlots:      make(chan struct{}, config.DefaultMaxPreAuthSessions),
		authSlots:         make(chan struct{}, config.DefaultMaxSessionsTotal),
		globalStreamSlots: make(chan struct{}, config.DefaultMaxStreamsTotal),
		metrics:           newMetrics(),
		traffic:           newTrafficStore(""),
		reqSink:           reqlog.NopSink{},
		reqHeartbeat:      time.Hour,
		firstSession:      make(chan struct{}),
		shutdown:          make(chan struct{}),
		shutdownDone:      make(chan struct{}),
	}
}

func waitForLifecycleCondition(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func waitForLifecycleGroup(t *testing.T, description string, group *sync.WaitGroup) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		group.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func TestOpenRouteStreamRejectsStaleIdentityAfterLeaseReservation(t *testing.T) {
	stream := newAdmissionStream()
	transport := &admissionSession{stream: stream}
	e, sess := newAdmissionEdge(transport, 1, 1)

	// Capture the route exactly as openRouteStream does, then replace it before
	// entering admission. The helper must reserve watcher ownership and both
	// leases, reject the now-stale pointer, and release everything exactly once.
	route := e.routes["app.test"]
	replacement := &Route{session: sess, name: "app"}
	e.mu.Lock()
	e.routes["app.test"] = replacement
	e.mu.Unlock()

	_, err := e.openRouteStreamForRoute(context.Background(), "app.test", route)
	if !errors.Is(err, errStaleRoute) {
		t.Fatalf("openRouteStream error = %v, want errStaleRoute", err)
	}
	waitAdmissionReleased(t, e, sess)
	waitForLifecycleGroup(t, "edge stream watcher", &e.streamWG)
	waitForLifecycleGroup(t, "session stream watcher", &sess.streamWG)
	if got := stream.writes.String(); got != "" {
		t.Fatalf("stale route opened a stream and wrote %q", got)
	}
}

func TestRawTLSHandshakeCapacityRejects129thAndReleasesSlot(t *testing.T) {
	e := newLifecycleTestEdge()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	results := make(chan error, 1)
	e.acceptWG.Add(1)
	go e.acceptTCP(ln, &tls.Config{}, results)

	var clients []net.Conn
	t.Cleanup(func() {
		for _, client := range clients {
			_ = client.Close()
		}
		_ = ln.Close()
	})
	for range 128 {
		client, dialErr := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
		if dialErr != nil {
			t.Fatalf("dial admitted TLS handshake: %v", dialErr)
		}
		clients = append(clients, client)
	}
	waitForLifecycleCondition(t, "128 active raw TLS handshakes", func() bool {
		e.lifecycleMu.Lock()
		rawCount := len(e.rawConns)
		e.lifecycleMu.Unlock()
		return len(e.tlsHandshakeSlots) == 128 && rawCount == 128
	})

	rejectedAt := time.Now()
	overflow, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
	if err != nil {
		t.Fatalf("dial 129th TLS handshake: %v", err)
	}
	_ = overflow.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
	var one [1]byte
	_, readErr := overflow.Read(one[:])
	_ = overflow.Close()
	if readErr == nil {
		t.Fatal("129th raw TLS handshake remained open")
	}
	var netErr net.Error
	if errors.As(readErr, &netErr) && netErr.Timeout() {
		t.Fatalf("129th raw TLS handshake was not rejected within 250ms: %v", readErr)
	}
	if elapsed := time.Since(rejectedAt); elapsed > 250*time.Millisecond {
		t.Fatalf("129th raw TLS handshake rejection took %v", elapsed)
	}
	if got := e.metrics.capacityRejected[0].Load(); got != 1 {
		t.Fatalf("tls_handshake rejection metric = %d, want 1", got)
	}

	_ = clients[0].Close()
	waitForLifecycleCondition(t, "released raw TLS handshake slot", func() bool {
		return len(e.tlsHandshakeSlots) == 127
	})

	replacement, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
	if err != nil {
		t.Fatalf("dial after raw TLS slot release: %v", err)
	}
	clients = append(clients, replacement)
	waitForLifecycleCondition(t, "replacement raw TLS handshake admission", func() bool {
		e.lifecycleMu.Lock()
		rawCount := len(e.rawConns)
		e.lifecycleMu.Unlock()
		return len(e.tlsHandshakeSlots) == 128 && rawCount == 128
	})

	for _, client := range clients {
		_ = client.Close()
	}
	_ = ln.Close()
	select {
	case acceptErr := <-results:
		if acceptErr != nil {
			t.Fatalf("acceptTCP: %v", acceptErr)
		}
	case <-time.After(time.Second):
		t.Fatal("acceptTCP did not stop")
	}
	waitForLifecycleGroup(t, "raw TLS handlers", &e.handlerWG)
	if got := len(e.tlsHandshakeSlots); got != 0 {
		t.Fatalf("raw TLS slots after drain = %d, want 0", got)
	}
}

func TestPreauthCapacityRejects33rdAndReleasesSlot(t *testing.T) {
	e := newLifecycleTestEdge()
	listener := newChannelTunnelListener()
	e.quicLn = listener
	results := make(chan error, 1)
	e.acceptWG.Add(1)
	go e.acceptQUIC(listener, results)

	admitted := make([]*lifecycleSession, 0, 32)
	for range 32 {
		session := newLifecycleSession(tunnel.KindQUIC, nil)
		admitted = append(admitted, session)
		listener.sessions <- session
	}
	waitForLifecycleCondition(t, "32 pre-authentication sessions", func() bool {
		if len(e.preAuthSlots) != 32 {
			return false
		}
		for _, session := range admitted {
			if session.acceptCalls.Load() == 0 {
				return false
			}
		}
		return true
	})

	rejected := newLifecycleSession(tunnel.KindQUIC, nil)
	rejectedAt := time.Now()
	listener.sessions <- rejected
	select {
	case <-rejected.Done():
	case <-time.After(250 * time.Millisecond):
		t.Fatal("33rd pre-authentication session was not rejected within 250ms")
	}
	if elapsed := time.Since(rejectedAt); elapsed > 250*time.Millisecond {
		t.Fatalf("33rd pre-authentication rejection took %v", elapsed)
	}
	if rejected.acceptCalls.Load() != 0 {
		t.Fatal("33rd pre-authentication session was dispatched")
	}
	if info := rejected.CloseInfo(); !info.CodeValid || info.Code != tunnel.CloseCapacity {
		t.Fatalf("33rd close info = %+v, want CloseCapacity", info)
	}
	if got := len(e.preAuthSlots); got != 32 {
		t.Fatalf("pre-authentication slots after rejection = %d, want 32", got)
	}
	if got := e.metrics.capacityRejected[1].Load(); got != 1 {
		t.Fatalf("preauth_session rejection metric = %d, want 1", got)
	}

	_ = admitted[0].CloseWithError(tunnel.CloseNormal, "release one")
	waitForLifecycleCondition(t, "released pre-authentication slot", func() bool {
		return len(e.preAuthSlots) == 31
	})

	replacement := newLifecycleSession(tunnel.KindQUIC, nil)
	listener.sessions <- replacement
	select {
	case <-replacement.acceptStarted:
	case <-time.After(time.Second):
		t.Fatal("replacement pre-authentication session was not dispatched")
	}
	if got := len(e.preAuthSlots); got != 32 {
		t.Fatalf("pre-authentication slots after replacement = %d, want 32", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := e.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case acceptErr := <-results:
		if acceptErr != nil {
			t.Fatalf("acceptQUIC: %v", acceptErr)
		}
	case <-time.After(time.Second):
		t.Fatal("acceptQUIC did not stop")
	}
	if got := len(e.preAuthSlots); got != 0 {
		t.Fatalf("pre-authentication slots after shutdown = %d, want 0", got)
	}
}

func startAuthenticatedLifecycleSession(
	t *testing.T,
	e *Edge,
) (*lifecycleSession, net.Conn, string, []byte) {
	t.Helper()
	control, peer := newLifecyclePipeStream()
	session := newLifecycleSession(tunnel.KindQUIC, control)
	if !e.acquirePreauth(session) {
		t.Fatal("could not acquire pre-authentication slot")
	}
	e.sessionWG.Add(1)
	go func() {
		defer e.sessionWG.Done()
		e.handleTunnelSession(session)
	}()
	if err := proto.Write(peer, &proto.Hello{
		Type:         proto.TypeHello,
		Token:        "token",
		ProtoVersion: proto.ProtoVersion,
	}); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	typ, line, err := proto.Read(bufio.NewReader(peer))
	if err != nil {
		t.Fatalf("read hello response: %v", err)
	}
	return session, peer, typ, line
}

func TestAuthenticatedCapacityRejects9thAndReleasesSlot(t *testing.T) {
	e := newLifecycleTestEdge()
	sessions := make([]*lifecycleSession, 0, 9)
	peers := make([]net.Conn, 0, 9)
	t.Cleanup(func() {
		for _, session := range sessions {
			_ = session.CloseWithError(tunnel.CloseNormal, "test cleanup")
		}
		for _, peer := range peers {
			_ = peer.Close()
		}
		waitForLifecycleGroup(t, "authenticated session handlers", &e.sessionWG)
	})

	for i := range 8 {
		session, peer, typ, _ := startAuthenticatedLifecycleSession(t, e)
		sessions = append(sessions, session)
		peers = append(peers, peer)
		if typ != proto.TypeHelloOK {
			t.Fatalf("session %d response type = %q, want hello_ok", i+1, typ)
		}
	}
	waitForLifecycleCondition(t, "eight authenticated slots", func() bool {
		return len(e.authSlots) == 8 && len(e.preAuthSlots) == 0
	})

	rejectedAt := time.Now()
	rejected, rejectedPeer, typ, line := startAuthenticatedLifecycleSession(t, e)
	sessions = append(sessions, rejected)
	peers = append(peers, rejectedPeer)
	if typ != proto.TypeError {
		t.Fatalf("ninth response type = %q, want error", typ)
	}
	var protocolErr proto.Error
	if err := json.Unmarshal(line, &protocolErr); err != nil {
		t.Fatalf("decode ninth response: %v", err)
	}
	if protocolErr.Code != proto.CodeOverLimit {
		t.Fatalf("ninth response code = %q, want %q", protocolErr.Code, proto.CodeOverLimit)
	}
	select {
	case <-rejected.Done():
	case <-time.After(250 * time.Millisecond):
		t.Fatal("ninth authenticated session was not closed within 250ms")
	}
	if elapsed := time.Since(rejectedAt); elapsed > 250*time.Millisecond {
		t.Fatalf("ninth authenticated rejection took %v", elapsed)
	}
	if info := rejected.CloseInfo(); !info.CodeValid || info.Code != tunnel.CloseCapacity {
		t.Fatalf("ninth close info = %+v, want CloseCapacity", info)
	}
	waitForLifecycleCondition(t, "ninth pre-authentication slot release", func() bool {
		return len(e.preAuthSlots) == 0
	})
	if got := len(e.authSlots); got != 8 {
		t.Fatalf("authenticated slots after rejection = %d, want 8", got)
	}
	if got := e.metrics.capacityRejected[2].Load(); got != 1 {
		t.Fatalf("authenticated_session rejection metric = %d, want 1", got)
	}

	_ = sessions[0].CloseWithError(tunnel.CloseNormal, "release one")
	_ = peers[0].Close()
	waitForLifecycleCondition(t, "released authenticated slot", func() bool {
		return len(e.authSlots) == 7
	})

	replacement, replacementPeer, typ, _ := startAuthenticatedLifecycleSession(t, e)
	sessions = append(sessions, replacement)
	peers = append(peers, replacementPeer)
	if typ != proto.TypeHelloOK {
		t.Fatalf("replacement response type = %q, want hello_ok", typ)
	}
	if got := len(e.authSlots); got != 8 {
		t.Fatalf("authenticated slots after replacement = %d, want 8", got)
	}
}

func TestQueuedAcceptResultsAreClosedDuringShutdown(t *testing.T) {
	t.Run("quic", func(t *testing.T) {
		e := newLifecycleTestEdge()
		session := newLifecycleSession(tunnel.KindQUIC, nil)
		listener := newQueuedTunnelListener(session)
		e.quicLn = listener
		results := make(chan error, 1)
		e.acceptWG.Add(1)
		go e.acceptQUIC(listener, results)

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := e.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
		select {
		case <-session.Done():
		default:
			t.Fatal("queued QUIC accept result was not closed")
		}
		if session.acceptCalls.Load() != 0 {
			t.Fatal("queued QUIC accept result was dispatched")
		}
		if got := len(e.preAuthSlots); got != 0 {
			t.Fatalf("queued QUIC accept leaked %d pre-authentication slots", got)
		}
		select {
		case acceptErr := <-results:
			if acceptErr != nil {
				t.Fatalf("acceptQUIC: %v", acceptErr)
			}
		default:
			t.Fatal("QUIC accept loop did not exit")
		}
	})

	t.Run("tcp", func(t *testing.T) {
		e := newLifecycleTestEdge()
		server, peer := net.Pipe()
		defer peer.Close()
		listener := newQueuedNetListener(server)
		e.ln = listener
		results := make(chan error, 1)
		e.acceptWG.Add(1)
		go e.acceptTCP(listener, &tls.Config{}, results)

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := e.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
		_ = peer.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
		var one [1]byte
		_, err := peer.Read(one[:])
		if err == nil {
			t.Fatal("queued TCP accept result remained open")
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			t.Fatalf("queued TCP accept result was not closed: %v", err)
		}
		if got := len(e.tlsHandshakeSlots); got != 0 {
			t.Fatalf("queued TCP accept leaked %d TLS handshake slots", got)
		}
		select {
		case acceptErr := <-results:
			if acceptErr != nil {
				t.Fatalf("acceptTCP: %v", acceptErr)
			}
		default:
			t.Fatal("TCP accept loop did not exit")
		}
	})
}

func TestDisabledQUICStartsWithOccupiedUDPAndDoesNotTouchKeys(t *testing.T) {
	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy UDP: %v", err)
	}
	defer udp.Close()

	dataDir := t.TempDir()
	cfg := &config.Server{
		BaseDomain:   "edge.test",
		ListenHTTPS:  "127.0.0.1:0",
		ListenQUIC:   udp.LocalAddr().String(),
		DisableQUIC:  true,
		DataDir:      dataDir,
		MetricsToken: "metrics",
	}
	manager, err := certs.NewSelfSignedManager(cfg.BaseDomain)
	if err != nil {
		t.Fatalf("cert manager: %v", err)
	}
	e := New(cfg, "test", auth.NewMemoryStore(nil), manager)
	serveResult := make(chan error, 1)
	go func() { serveResult <- e.Serve() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = e.Shutdown(ctx)
	})

	var tcpAddress string
	waitForLifecycleCondition(t, "TCP readiness with occupied UDP", func() bool {
		e.lifecycleMu.Lock()
		defer e.lifecycleMu.Unlock()
		if e.ln == nil {
			return false
		}
		tcpAddress = e.ln.Addr().String()
		return true
	})
	tcpConn, err := net.DialTimeout("tcp", tcpAddress, time.Second)
	if err != nil {
		t.Fatalf("TCP readiness dial: %v", err)
	}
	_ = tcpConn.Close()

	e.lifecycleMu.Lock()
	quicListener, quicIO := e.quicLn, e.quicIO
	e.lifecycleMu.Unlock()
	if quicListener != nil || quicIO != nil {
		t.Fatalf("disabled QUIC constructed runtime state: listener=%v io=%v", quicListener, quicIO)
	}
	for _, name := range []string{
		"quic-stateless-reset.key",
		"quic-token-generator.key",
	} {
		if _, err := os.Stat(filepath.Join(dataDir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("disabled QUIC touched %s: %v", name, err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := e.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case err := <-serveResult:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve did not return after shutdown")
	}
}

func TestShutdownWriteFailureStillClaimsShutdownCloseCode(t *testing.T) {
	control := newAdmissionStream()
	control.writeErr = errors.New("synthetic shutdown notification failure")
	transport := newLifecycleSession(tunnel.KindQUIC, nil)
	sess := &Session{
		transport: transport,
		kind:      tunnel.KindQUIC,
		control:   control,
		id:        "shutdown-write-failure",
	}
	e := newLifecycleTestEdge()
	e.sessions[sess] = struct{}{}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := e.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	info := transport.CloseInfo()
	if !info.CodeValid || info.Code != tunnel.CloseShutdown || info.Reason != "edge shutting down" {
		t.Fatalf("final CloseInfo = %+v, want authoritative shutdown close", info)
	}
}

func TestUpgradeLosingShutdownRaceEmitsOneOrdinaryTerminalEvent(t *testing.T) {
	e := newLifecycleTestEdge()
	sink := &lifecycleBlockingSink{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	var releaseSinkOnce sync.Once
	releaseSink := func() { releaseSinkOnce.Do(func() { close(sink.release) }) }
	defer releaseSink()
	e.reqSink = sink
	e.proxies = make(map[string]*httputil.ReverseProxy)

	const host = "ws.scope.edge.test"
	e.routes[host] = &Route{
		session: &Session{slug: "scope"},
		name:    "ws",
	}

	roundTripStarted := make(chan struct{})
	releaseUpgrade := make(chan struct{})
	backend, backendPeer := net.Pipe()
	defer backendPeer.Close()
	e.proxies[host] = &httputil.ReverseProxy{
		Director: func(*http.Request) {},
		Transport: lifecycleRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
			close(roundTripStarted)
			<-releaseUpgrade
			return &http.Response{
				Status:     "101 Switching Protocols",
				StatusCode: http.StatusSwitchingProtocols,
				Proto:      "HTTP/1.1",
				ProtoMajor: 1,
				ProtoMinor: 1,
				Header: http.Header{
					"Connection": {"Upgrade"},
					"Upgrade":    {"websocket"},
				},
				Body:    backend,
				Request: req,
			}, nil
		}),
		ErrorHandler: func(http.ResponseWriter, *http.Request, error) {},
	}

	public, publicPeer := net.Pipe()
	defer publicPeer.Close()
	writer := &lifecycleHijackWriter{
		header: make(http.Header),
		conn:   public,
	}
	req := httptest.NewRequest(http.MethodGet, "https://"+host+"/socket", nil)
	req.Host = host
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")

	handlerDone := make(chan struct{})
	go func() {
		e.handler(writer, req)
		close(handlerDone)
	}()
	select {
	case <-roundTripStarted:
	case <-time.After(time.Second):
		t.Fatal("upgrade did not reach RoundTrip after proxy admission")
	}

	e.lifecycleMu.Lock()
	e.shuttingDown = true
	e.lifecycleMu.Unlock()

	proxyDrained := make(chan struct{})
	go func() {
		e.proxyWG.Wait()
		close(proxyDrained)
	}()
	close(releaseUpgrade)

	select {
	case <-sink.started:
	case <-time.After(time.Second):
		t.Fatal("shutdown-raced upgrade did not begin its terminal request event")
	}
	select {
	case <-proxyDrained:
		t.Fatal("proxy ownership released before the terminal request event completed")
	case <-time.After(20 * time.Millisecond):
	}
	releaseSink()

	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("shutdown-raced upgrade handler did not return")
	}
	select {
	case <-proxyDrained:
	case <-time.After(time.Second):
		t.Fatal("proxy ownership was not released after the terminal request event")
	}
	events := sink.events()
	if len(events) != 1 {
		t.Fatalf("terminal request events = %d, want exactly one: %+v", len(events), events)
	}
	if events[0].IsWebSocket ||
		events[0].ConnectionID != "" ||
		events[0].Outcome != reqlog.OutcomeOK {
		t.Fatalf("terminal request event = %+v, want one ordinary ok event", events[0])
	}
	e.lifecycleMu.Lock()
	tracked := len(e.hijacked)
	e.lifecycleMu.Unlock()
	if tracked != 0 {
		t.Fatalf("tracked hijacked connections = %d, want 0", tracked)
	}

	// No heartbeat owner was installed, so a delayed duplicate WS-final event
	// must not appear after the ordinary handler event.
	time.Sleep(20 * time.Millisecond)
	if got := len(sink.events()); got != 1 {
		t.Fatalf("terminal request events after settling = %d, want exactly one", got)
	}
}

func TestShutdownTerminatesStalledWorkers(t *testing.T) {
	t.Run("raw_tls", func(t *testing.T) {
		e := newLifecycleTestEdge()
		raw, peer := net.Pipe()
		defer peer.Close()

		e.tlsHandshakeSlots <- struct{}{}
		e.lifecycleMu.Lock()
		e.rawConns[raw] = struct{}{}
		e.handlerWG.Add(1)
		e.lifecycleMu.Unlock()
		go e.handleTCP(raw, &tls.Config{})

		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
		defer cancel()
		err := e.Shutdown(ctx)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Shutdown error = %v, want context deadline", err)
		}
		waitForLifecycleGroup(t, "stalled raw TLS handler", &e.handlerWG)
		if got := len(e.tlsHandshakeSlots); got != 0 {
			t.Fatalf("raw TLS slots after forced shutdown = %d, want 0", got)
		}
		e.lifecycleMu.Lock()
		rawCount := len(e.rawConns)
		e.lifecycleMu.Unlock()
		if rawCount != 0 {
			t.Fatalf("tracked raw connections after shutdown = %d, want 0", rawCount)
		}
	})

	t.Run("preauth", func(t *testing.T) {
		e := newLifecycleTestEdge()
		session := newLifecycleSession(tunnel.KindQUIC, nil)
		if !e.acquirePreauth(session) {
			t.Fatal("acquire pre-authentication slot")
		}
		e.sessionWG.Add(1)
		go func() {
			defer e.sessionWG.Done()
			e.handleTunnelSession(session)
		}()
		select {
		case <-session.acceptStarted:
		case <-time.After(time.Second):
			t.Fatal("pre-authentication handler did not stall in AcceptStream")
		}

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := e.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
		if info := session.CloseInfo(); !info.CodeValid || info.Code != tunnel.CloseShutdown {
			t.Fatalf("pre-authentication close info = %+v, want CloseShutdown", info)
		}
		waitForLifecycleGroup(t, "pre-authentication handler", &e.sessionWG)
		if got := len(e.preAuthSlots); got != 0 {
			t.Fatalf("pre-authentication slots after shutdown = %d, want 0", got)
		}
	})

	t.Run("hijacked", func(t *testing.T) {
		e := newLifecycleTestEdge()
		raw, peer := net.Pipe()
		defer peer.Close()

		e.proxyWG.Add(1)
		var tracked net.Conn
		tracked = e.startWSHeartbeat(reqMeta{started: time.Now()}, raw, func() {
			e.lifecycleMu.Lock()
			delete(e.hijacked, tracked)
			e.lifecycleMu.Unlock()
			e.proxyWG.Done()
		})
		e.lifecycleMu.Lock()
		e.hijacked[tracked] = struct{}{}
		e.lifecycleMu.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
		defer cancel()
		err := e.Shutdown(ctx)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Shutdown error = %v, want context deadline", err)
		}
		waitForLifecycleGroup(t, "hijacked proxy worker", &e.proxyWG)
		counting, ok := tracked.(*wsCountingConn)
		if !ok {
			t.Fatalf("tracked connection type = %T, want *wsCountingConn", tracked)
		}
		select {
		case <-counting.done:
		default:
			t.Fatal("hijacked heartbeat worker remained live")
		}
		e.lifecycleMu.Lock()
		hijackedCount := len(e.hijacked)
		e.lifecycleMu.Unlock()
		if hijackedCount != 0 {
			t.Fatalf("tracked hijacked connections after shutdown = %d, want 0", hijackedCount)
		}
	})

	t.Run("ordinary_proxy", func(t *testing.T) {
		e := newLifecycleTestEdge()
		serverConn, clientConn := net.Pipe()
		defer clientConn.Close()

		started := make(chan struct{})
		proxyDone := make(chan struct{})
		e.proxyWG.Add(1)
		srv := &http.Server{Handler: http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
			close(started)
			defer func() {
				e.proxyWG.Done()
				close(proxyDone)
			}()
			<-request.Context().Done()
		})}
		e.mu.Lock()
		e.pubSrvs[srv] = struct{}{}
		e.mu.Unlock()
		serveDone := make(chan struct{})
		go func() {
			_ = srv.Serve(&singleConnListener{conn: serverConn})
			close(serveDone)
		}()
		go func() {
			_, _ = io.WriteString(clientConn, "GET / HTTP/1.1\r\nHost: edge.test\r\n\r\n")
		}()
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("ordinary proxy worker did not start")
		}

		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
		defer cancel()
		err := e.Shutdown(ctx)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Shutdown error = %v, want context deadline", err)
		}
		select {
		case <-proxyDone:
		case <-time.After(time.Second):
			t.Fatal("forced server close did not terminate the ordinary proxy worker")
		}
		select {
		case <-serveDone:
		case <-time.After(time.Second):
			t.Fatal("ordinary HTTP server goroutine remained live")
		}
		waitForLifecycleGroup(t, "ordinary proxy worker", &e.proxyWG)
	})
}

func TestShutdownJoinsSessionHelpersBeforeSessionCompletion(t *testing.T) {
	e := newLifecycleTestEdge()
	control, peer := newLifecyclePipeStream()
	defer peer.Close()

	releaseUnexpected := make(chan struct{})
	unexpectedSawClose := make(chan struct{})
	session := newLifecycleSession(tunnel.KindQUIC, control)
	session.acceptDoneRelease = releaseUnexpected
	session.acceptDoneObserved = unexpectedSawClose
	if !e.acquirePreauth(session) {
		t.Fatal("acquire pre-authentication slot")
	}
	e.sessionWG.Add(1)
	go func() {
		defer e.sessionWG.Done()
		e.handleTunnelSession(session)
	}()

	reader := bufio.NewReader(peer)
	if err := proto.Write(peer, &proto.Hello{
		Type:         proto.TypeHello,
		Token:        "token",
		ProtoVersion: proto.ProtoVersion,
	}); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	typ, line, err := proto.Read(reader)
	if err != nil {
		t.Fatalf("read hello response: %v", err)
	}
	if typ != proto.TypeHelloOK {
		t.Fatalf("hello response type = %q line = %q, want hello_ok", typ, line)
	}
	select {
	case <-session.acceptStarted:
	case <-time.After(time.Second):
		t.Fatal("unexpected-stream helper did not start")
	}

	// Drain the bounded shutdown control write so the test reaches transport
	// close immediately rather than waiting on net.Pipe's synchronous Write.
	shutdownControl := make(chan error, 1)
	go func() {
		typ, line, readErr := proto.Read(reader)
		if readErr != nil {
			shutdownControl <- readErr
			return
		}
		var message proto.Error
		if decodeErr := json.Unmarshal(line, &message); decodeErr != nil {
			shutdownControl <- decodeErr
			return
		}
		if typ != proto.TypeError || message.Code != proto.CodeShutdown {
			shutdownControl <- errors.New("unexpected shutdown control message")
			return
		}
		shutdownControl <- nil
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	shutdownResult := make(chan error, 1)
	go func() { shutdownResult <- e.Shutdown(ctx) }()
	select {
	case err := <-shutdownControl:
		if err != nil {
			t.Fatalf("read shutdown control message: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown control message was not delivered")
	}
	select {
	case <-unexpectedSawClose:
	case <-time.After(time.Second):
		t.Fatal("unexpected-stream helper did not observe session close")
	}

	// Session.Done is already observable, but the adapter's AcceptStream cleanup
	// is deliberately held. dropSession must therefore retain the auth lease,
	// sessionWG must remain live, and Shutdown must not return.
	select {
	case <-session.Done():
	default:
		t.Fatal("transport did not reach Done before helper cleanup")
	}
	if got := len(e.authSlots); got != 1 {
		t.Fatalf("authenticated slots while helper is blocked = %d, want 1", got)
	}
	sessionHandlersDone := make(chan struct{})
	go func() {
		e.sessionWG.Wait()
		close(sessionHandlersDone)
	}()
	select {
	case <-sessionHandlersDone:
		t.Fatal("sessionWG completed before auxiliary helper cleanup")
	case <-shutdownResult:
		t.Fatal("Shutdown returned before auxiliary helper cleanup")
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseUnexpected)
	select {
	case err := <-shutdownResult:
		if err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not finish after auxiliary helper cleanup")
	}
	select {
	case <-sessionHandlersDone:
	case <-time.After(time.Second):
		t.Fatal("sessionWG did not finish after auxiliary helper cleanup")
	}
	if got := len(e.authSlots); got != 0 {
		t.Fatalf("authenticated slots after helper cleanup = %d, want 0", got)
	}
}
