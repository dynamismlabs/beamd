package edge

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dynamismlabs/beamd/internal/config"
	"github.com/dynamismlabs/beamd/internal/reqlog"
	"github.com/dynamismlabs/beamd/internal/tunnel"
)

type admissionAddr string

func (a admissionAddr) Network() string { return "test" }
func (a admissionAddr) String() string  { return string(a) }

type admissionStream struct {
	mu            sync.Mutex
	writes        bytes.Buffer
	readErr       error
	writeErr      error
	blockWrite    bool
	writeStarted  chan struct{}
	writeRelease  chan struct{}
	writeOnce     sync.Once
	releaseOnce   sync.Once
	abortCalls    int
	aborted       bool
	closed        bool
	deadline      time.Time
	writeDeadline []time.Time
	done          chan struct{}
	doneOnce      sync.Once
}

func newAdmissionStream() *admissionStream {
	return &admissionStream{
		writeStarted: make(chan struct{}),
		writeRelease: make(chan struct{}),
		done:         make(chan struct{}),
	}
}

func (s *admissionStream) Read([]byte) (int, error) {
	s.mu.Lock()
	err := s.readErr
	s.mu.Unlock()
	if err != nil {
		return 0, err
	}
	return 0, io.EOF
}
func (s *admissionStream) Write(p []byte) (int, error) {
	s.mu.Lock()
	err := s.writeErr
	block := s.blockWrite
	s.mu.Unlock()
	if err != nil {
		return 0, err
	}
	if block {
		s.writeOnce.Do(func() { close(s.writeStarted) })
		<-s.writeRelease
		s.mu.Lock()
		aborted := s.aborted
		err = s.writeErr
		s.mu.Unlock()
		if err != nil {
			return 0, err
		}
		if aborted {
			return 0, net.ErrClosed
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes.Write(p)
}
func (s *admissionStream) CloseWrite() error { return s.Close() }
func (s *admissionStream) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return nil
}
func (s *admissionStream) Abort(tunnel.ErrorCode) {
	s.mu.Lock()
	s.aborted = true
	s.abortCalls++
	s.mu.Unlock()
	s.releaseOnce.Do(func() { close(s.writeRelease) })
	s.finish()
}
func (s *admissionStream) Done() <-chan struct{} { return s.done }
func (s *admissionStream) finish() {
	s.doneOnce.Do(func() { close(s.done) })
}
func (s *admissionStream) LocalAddr() net.Addr  { return admissionAddr("local") }
func (s *admissionStream) RemoteAddr() net.Addr { return admissionAddr("remote") }
func (s *admissionStream) SetDeadline(deadline time.Time) error {
	s.mu.Lock()
	s.deadline = deadline
	s.mu.Unlock()
	return nil
}
func (s *admissionStream) SetReadDeadline(time.Time) error { return nil }
func (s *admissionStream) SetWriteDeadline(deadline time.Time) error {
	s.mu.Lock()
	s.writeDeadline = append(s.writeDeadline, deadline)
	s.mu.Unlock()
	return nil
}

type admissionSession struct {
	stream      tunnel.Stream
	err         error
	acceptDelay time.Duration
	blockOpen   bool
	openStarted chan struct{}
	openOnce    sync.Once
	closed      bool
	done        chan struct{}
	closeCalled chan struct{}
	closeOnce   sync.Once
}

func (s *admissionSession) Kind() tunnel.Kind { return tunnel.KindQUIC }
func (s *admissionSession) OpenStream(ctx context.Context) (tunnel.Stream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.blockOpen {
		s.openOnce.Do(func() { close(s.openStarted) })
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-s.Done():
			return nil, tunnel.ErrSessionClosed
		}
	}
	return s.stream, s.err
}
func (s *admissionSession) AcceptStream(ctx context.Context) (tunnel.Stream, error) {
	if s.acceptDelay > 0 {
		timer := time.NewTimer(s.acceptDelay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return s.stream, s.err
}
func (s *admissionSession) Done() <-chan struct{} {
	if s.done == nil {
		s.done = make(chan struct{})
	}
	return s.done
}
func (s *admissionSession) IsClosed() bool              { return s.closed }
func (s *admissionSession) CloseInfo() tunnel.CloseInfo { return tunnel.CloseInfo{} }
func (s *admissionSession) CloseWithError(tunnel.ErrorCode, string) error {
	s.closed = true
	if s.closeCalled != nil {
		s.closeOnce.Do(func() { close(s.closeCalled) })
	}
	return nil
}
func (s *admissionSession) LocalAddr() net.Addr  { return admissionAddr("local") }
func (s *admissionSession) RemoteAddr() net.Addr { return admissionAddr("remote") }

type authLifecycleSession struct {
	closed      atomic.Bool
	closeOnce   sync.Once
	closeCalled chan struct{}
	done        chan struct{}
}

type blockingTrafficRecorder struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

type proxyCancellationStream struct {
	net.Conn
	aborted   chan struct{}
	done      chan struct{}
	abortOnce sync.Once
	doneOnce  sync.Once
}

func newProxyCancellationStream(conn net.Conn) *proxyCancellationStream {
	return &proxyCancellationStream{
		Conn:    conn,
		aborted: make(chan struct{}),
		done:    make(chan struct{}),
	}
}

func (s *proxyCancellationStream) CloseWrite() error { return nil }
func (s *proxyCancellationStream) Done() <-chan struct{} {
	return s.done
}
func (s *proxyCancellationStream) Abort(tunnel.ErrorCode) {
	s.abortOnce.Do(func() { close(s.aborted) })
	_ = s.Conn.Close()
	s.doneOnce.Do(func() { close(s.done) })
}
func (s *proxyCancellationStream) Close() error {
	err := s.Conn.Close()
	s.doneOnce.Do(func() { close(s.done) })
	return err
}

func (r *blockingTrafficRecorder) RecordTraffic(string, string, int64, int64) {
	r.once.Do(func() { close(r.started) })
	<-r.release
}

func newAuthLifecycleSession() *authLifecycleSession {
	return &authLifecycleSession{
		closeCalled: make(chan struct{}),
		done:        make(chan struct{}),
	}
}

func (s *authLifecycleSession) Kind() tunnel.Kind { return tunnel.KindQUIC }
func (s *authLifecycleSession) OpenStream(context.Context) (tunnel.Stream, error) {
	return nil, errors.New("unused")
}
func (s *authLifecycleSession) AcceptStream(context.Context) (tunnel.Stream, error) {
	return nil, errors.New("unused")
}
func (s *authLifecycleSession) Done() <-chan struct{} { return s.done }
func (s *authLifecycleSession) IsClosed() bool        { return s.closed.Load() }
func (s *authLifecycleSession) CloseInfo() tunnel.CloseInfo {
	return tunnel.CloseInfo{Reason: "other"}
}
func (s *authLifecycleSession) CloseWithError(tunnel.ErrorCode, string) error {
	s.closed.Store(true)
	s.closeOnce.Do(func() { close(s.closeCalled) })
	return nil
}
func (s *authLifecycleSession) LocalAddr() net.Addr  { return admissionAddr("local") }
func (s *authLifecycleSession) RemoteAddr() net.Addr { return admissionAddr("remote") }

func newAdmissionEdge(session *admissionSession, streamCapacity, globalCapacity int) (*Edge, *Session) {
	e := &Edge{
		routes:            make(map[string]*Route),
		proxies:           make(map[string]*httputil.ReverseProxy),
		globalStreamSlots: make(chan struct{}, globalCapacity),
		metrics:           newMetrics(),
		traffic:           newTrafficStore(""),
	}
	sess := &Session{
		transport:   session,
		kind:        tunnel.KindQUIC,
		slug:        "slug",
		streamSlots: make(chan struct{}, streamCapacity),
	}
	e.routes["app.test"] = &Route{session: sess, name: "app"}
	return e, sess
}

func newProxyCancellationEdge(session *admissionSession) (*Edge, *Session) {
	e, sess := newAdmissionEdge(session, 1, 1)
	e.cfg = &config.Server{MaxRequestBodyBytes: 32 << 20}
	e.reqSink = reqlog.NopSink{}
	return e, sess
}

func waitAdmissionReleased(t *testing.T, e *Edge, sess *Session) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(e.globalStreamSlots) == 0 && len(sess.streamSlots) == 0 &&
			sess.activeStreams.Load() == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("lease did not release: global=%d session=%d active=%d",
		len(e.globalStreamSlots), len(sess.streamSlots), sess.activeStreams.Load())
}

func TestOpenRouteStreamLeaseHeldUntilStreamDone(t *testing.T) {
	stream := newAdmissionStream()
	transport := &admissionSession{stream: stream}
	e, sess := newAdmissionEdge(transport, 1, 1)

	conn, err := e.openRouteStream(context.Background(), "app.test")
	if err != nil {
		t.Fatalf("openRouteStream: %v", err)
	}
	if got := stream.writes.String(); got != "app\n" {
		t.Fatalf("prefix = %q, want app newline", got)
	}
	if len(e.globalStreamSlots) != 1 || len(sess.streamSlots) != 1 {
		t.Fatal("stream did not hold both admission leases")
	}

	if err := conn.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if len(e.globalStreamSlots) != 1 || len(sess.streamSlots) != 1 {
		t.Fatal("net.Conn.Close released a lease before Stream.Done")
	}

	stream.finish()
	waitAdmissionReleased(t, e, sess)
}

func TestOpenRouteStreamBoundsAndClearsPrefixWriteDeadline(t *testing.T) {
	stream := newAdmissionStream()
	transport := &admissionSession{stream: stream}
	e, sess := newAdmissionEdge(transport, 1, 1)
	started := time.Now()

	conn, err := e.openRouteStream(context.Background(), "app.test")
	if err != nil {
		t.Fatalf("openRouteStream: %v", err)
	}
	stream.mu.Lock()
	deadlines := append([]time.Time(nil), stream.writeDeadline...)
	stream.mu.Unlock()
	if len(deadlines) != 2 {
		t.Fatalf("prefix write deadline calls = %d, want set then clear", len(deadlines))
	}
	if deadlines[0].IsZero() {
		t.Fatal("prefix write deadline was not bounded")
	}
	if elapsed := deadlines[0].Sub(started); elapsed < 4*time.Second || elapsed > 6*time.Second {
		t.Fatalf("prefix write deadline after %s, want approximately 5s", elapsed)
	}
	if !deadlines[1].IsZero() {
		t.Fatalf("prefix write deadline was not cleared: %v", deadlines[1])
	}

	_ = conn.Close()
	stream.finish()
	waitAdmissionReleased(t, e, sess)
}

func TestProxyPreservesVisitorCancellation(t *testing.T) {
	t.Run("while opening stream", func(t *testing.T) {
		transport := &admissionSession{
			blockOpen:   true,
			openStarted: make(chan struct{}),
		}
		e, sess := newProxyCancellationEdge(transport)
		ctx, cancel := context.WithCancel(context.Background())
		req := httptest.NewRequest(http.MethodGet, "https://app.test/wait", nil).WithContext(ctx)
		handlerDone := make(chan struct{})
		go func() {
			e.handler(httptest.NewRecorder(), req)
			close(handlerDone)
		}()

		select {
		case <-transport.openStarted:
		case <-time.After(time.Second):
			t.Fatal("proxy did not begin opening a tunnel stream")
		}
		cancel()
		select {
		case <-handlerDone:
		case <-time.After(250 * time.Millisecond):
			t.Fatal("visitor cancellation did not unblock OpenStream")
		}
		waitAdmissionReleased(t, e, sess)
	})

	t.Run("while writing prefix", func(t *testing.T) {
		stream := newAdmissionStream()
		stream.blockWrite = true
		transport := &admissionSession{stream: stream}
		e, sess := newProxyCancellationEdge(transport)
		ctx, cancel := context.WithCancel(context.Background())
		req := httptest.NewRequest(http.MethodGet, "https://app.test/wait", nil).WithContext(ctx)
		handlerDone := make(chan struct{})
		go func() {
			e.handler(httptest.NewRecorder(), req)
			close(handlerDone)
		}()

		select {
		case <-stream.writeStarted:
		case <-time.After(time.Second):
			t.Fatal("proxy did not begin writing the tunnel prefix")
		}
		cancel()
		select {
		case <-handlerDone:
		case <-time.After(250 * time.Millisecond):
			t.Fatal("visitor cancellation did not unblock the prefix write")
		}
		select {
		case <-stream.Done():
		case <-time.After(250 * time.Millisecond):
			t.Fatal("visitor cancellation did not terminate the blocked prefix stream")
		}
		stream.mu.Lock()
		aborted := stream.aborted
		stream.mu.Unlock()
		if !aborted {
			t.Fatal("prefix cancellation did not abort the tunnel stream")
		}
		waitAdmissionReleased(t, e, sess)
	})

	t.Run("after dial aborts instead of graceful close", func(t *testing.T) {
		edgeConn, agentConn := net.Pipe()
		stream := newProxyCancellationStream(edgeConn)
		transport := &admissionSession{stream: stream}
		e, sess := newProxyCancellationEdge(transport)
		requestSeen := make(chan struct{})
		agentDone := make(chan struct{})
		go func() {
			defer close(agentDone)
			defer agentConn.Close()
			br := bufio.NewReader(agentConn)
			prefix, err := br.ReadString('\n')
			if err != nil || prefix != "app\n" {
				return
			}
			request, err := http.ReadRequest(br)
			if err != nil {
				return
			}
			_ = request.Body.Close()
			close(requestSeen)
			_, _ = io.Copy(io.Discard, br)
		}()

		ctx, cancel := context.WithCancel(context.Background())
		req := httptest.NewRequest(http.MethodGet, "https://app.test/wait", nil).WithContext(ctx)
		handlerDone := make(chan struct{})
		go func() {
			e.handler(httptest.NewRecorder(), req)
			close(handlerDone)
		}()
		select {
		case <-requestSeen:
		case <-time.After(time.Second):
			t.Fatal("request did not cross the established tunnel stream")
		}

		cancel()
		select {
		case <-stream.aborted:
		case <-time.After(250 * time.Millisecond):
			t.Fatal("visitor cancellation gracefully closed instead of aborting the stream")
		}
		select {
		case <-handlerDone:
		case <-time.After(250 * time.Millisecond):
			t.Fatal("visitor cancellation did not unblock the proxy")
		}
		select {
		case <-agentDone:
		case <-time.After(time.Second):
			t.Fatal("aborted tunnel did not unblock the remote stream")
		}
		waitAdmissionReleased(t, e, sess)
	})
}

func TestOpenRouteStreamCapacityIsImmediateAndScoped(t *testing.T) {
	stream := newAdmissionStream()
	transport := &admissionSession{stream: stream}

	e, sess := newAdmissionEdge(transport, 1, 1)
	e.globalStreamSlots <- struct{}{}
	start := time.Now()
	_, err := e.openRouteStream(context.Background(), "app.test")
	if !errors.Is(err, tunnel.ErrCapacity) {
		t.Fatalf("global capacity error = %v, want ErrCapacity", err)
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("global capacity rejection queued instead of returning immediately")
	}
	<-e.globalStreamSlots

	sess.streamSlots <- struct{}{}
	_, err = e.openRouteStream(context.Background(), "app.test")
	if !errors.Is(err, tunnel.ErrCapacity) {
		t.Fatalf("session capacity error = %v, want ErrCapacity", err)
	}
	if len(e.globalStreamSlots) != 0 {
		t.Fatal("session rejection leaked the already-acquired global lease")
	}
}

func TestOpenRouteStreamCancellationAbortsAndReleases(t *testing.T) {
	stream := newAdmissionStream()
	transport := &admissionSession{stream: stream}
	e, sess := newAdmissionEdge(transport, 1, 1)
	ctx, cancel := context.WithCancel(context.Background())

	if _, err := e.openRouteStream(ctx, "app.test"); err != nil {
		t.Fatalf("openRouteStream: %v", err)
	}
	cancel()
	waitAdmissionReleased(t, e, sess)
	stream.mu.Lock()
	aborted := stream.aborted
	stream.mu.Unlock()
	if !aborted {
		t.Fatal("request cancellation did not abort the transport stream")
	}
}

func TestOpenRouteStreamPrefixFailureReleasesAfterDone(t *testing.T) {
	stream := newAdmissionStream()
	stream.writeErr = errors.New("prefix write failed")
	transport := &admissionSession{stream: stream}
	e, sess := newAdmissionEdge(transport, 1, 1)

	if _, err := e.openRouteStream(context.Background(), "app.test"); err == nil {
		t.Fatal("prefix failure unexpectedly succeeded")
	}
	waitAdmissionReleased(t, e, sess)
}

func TestPreauthControlUsesOneTotalDeadline(t *testing.T) {
	stream := newAdmissionStream()
	transport := &admissionSession{
		stream:      stream,
		acceptDelay: 50 * time.Millisecond,
	}
	const budget = 200 * time.Millisecond
	started := time.Now()
	control, cancel, err := acceptPreauthControl(transport, budget)
	if err != nil {
		t.Fatalf("acceptPreauthControl: %v", err)
	}
	defer cancel()
	if control != stream {
		t.Fatal("acceptPreauthControl returned the wrong stream")
	}

	stream.mu.Lock()
	deadline := stream.deadline
	stream.mu.Unlock()
	if deadline.IsZero() {
		t.Fatal("control stream deadline was not set")
	}
	// The deadline is anchored before AcceptStream. Restarting a full budget
	// after the deliberate delay would land around started+250ms.
	if latest := started.Add(budget + 25*time.Millisecond); deadline.After(latest) {
		t.Fatalf("pre-auth deadline = %v, want no later than %v", deadline, latest)
	}
}

func TestOpenRouteStreamCancellationDuringPrefixAbortsPromptly(t *testing.T) {
	stream := newAdmissionStream()
	stream.blockWrite = true
	transport := &admissionSession{stream: stream}
	e, sess := newAdmissionEdge(transport, 1, 1)
	ctx, cancel := context.WithCancel(context.Background())

	result := make(chan error, 1)
	go func() {
		_, err := e.openRouteStream(ctx, "app.test")
		result <- err
	}()
	select {
	case <-stream.writeStarted:
	case <-time.After(time.Second):
		t.Fatal("prefix write did not block")
	}
	started := time.Now()
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("openRouteStream error = %v, want context.Canceled", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("cancellation waited for the five-second prefix deadline")
	}
	if time.Since(started) > 250*time.Millisecond {
		t.Fatal("prefix cancellation was not prompt")
	}
	waitAdmissionReleased(t, e, sess)
}

func TestLeasedConnAbortsOnNonEOFIOError(t *testing.T) {
	for _, direction := range []string{"read", "write"} {
		t.Run(direction, func(t *testing.T) {
			stream := newAdmissionStream()
			transport := &admissionSession{stream: stream}
			e, sess := newAdmissionEdge(transport, 1, 1)
			conn, err := e.openRouteStream(context.Background(), "app.test")
			if err != nil {
				t.Fatalf("openRouteStream: %v", err)
			}

			wantErr := errors.New(direction + " failed")
			stream.mu.Lock()
			if direction == "read" {
				stream.readErr = wantErr
			} else {
				stream.writeErr = wantErr
			}
			stream.mu.Unlock()
			if direction == "read" {
				_, err = conn.Read(nil)
			} else {
				_, err = conn.Write([]byte("request"))
			}
			if !errors.Is(err, wantErr) {
				t.Fatalf("%s error = %v, want %v", direction, err, wantErr)
			}
			waitAdmissionReleased(t, e, sess)
			stream.mu.Lock()
			abortCalls := stream.abortCalls
			stream.mu.Unlock()
			if abortCalls == 0 {
				t.Fatalf("%s error did not abort the transport stream", direction)
			}
		})
	}
}

func TestAuthenticatedCapacityHeldUntilSessionDone(t *testing.T) {
	transport := newAuthLifecycleSession()
	sess := &Session{
		transport:   transport,
		kind:        tunnel.KindQUIC,
		slug:        "slug",
		id:          "session",
		streamSlots: make(chan struct{}, 1),
	}
	e := &Edge{
		sessions:  map[*Session]struct{}{sess: {}},
		routes:    make(map[string]*Route),
		proxies:   make(map[string]*httputil.ReverseProxy),
		authSlots: make(chan struct{}, 1),
		metrics:   newMetrics(),
	}
	e.authSlots <- struct{}{}

	dropped := make(chan struct{})
	go func() {
		e.dropSession(sess)
		close(dropped)
	}()
	select {
	case <-transport.closeCalled:
	case <-time.After(time.Second):
		t.Fatal("dropSession did not begin transport close")
	}
	select {
	case <-dropped:
		t.Fatal("dropSession returned before Session.Done")
	default:
	}
	if got := len(e.authSlots); got != 1 {
		t.Fatalf("authenticated slots held = %d, want 1 before Session.Done", got)
	}

	close(transport.done)
	select {
	case <-dropped:
	case <-time.After(time.Second):
		t.Fatal("dropSession did not finish after Session.Done")
	}
	if got := len(e.authSlots); got != 0 {
		t.Fatalf("authenticated slots held = %d, want 0 after Session.Done", got)
	}
}

func TestSessionCloseJoinsLeasedWatcherBeforeCapacityRelease(t *testing.T) {
	stream := newAdmissionStream()
	transport := &admissionSession{
		stream:      stream,
		done:        make(chan struct{}),
		closeCalled: make(chan struct{}),
	}
	e, sess := newAdmissionEdge(transport, 1, 1)
	e.sessions = map[*Session]struct{}{sess: {}}
	e.authSlots = make(chan struct{}, 1)
	e.authSlots <- struct{}{}
	recorder := &blockingTrafficRecorder{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	e.trafficSinks = []TrafficRecorder{recorder}

	conn, err := e.openRouteStream(context.Background(), "app.test")
	if err != nil {
		t.Fatalf("openRouteStream: %v", err)
	}
	if _, err := conn.Write([]byte("request")); err != nil {
		t.Fatalf("request write: %v", err)
	}

	dropped := make(chan struct{})
	go func() {
		e.dropSession(sess)
		close(dropped)
	}()
	select {
	case <-transport.closeCalled:
	case <-time.After(time.Second):
		t.Fatal("dropSession did not begin transport close")
	}
	// Adapter contract ordering: every child Done closes before Session.Done.
	stream.finish()
	close(transport.done)
	select {
	case <-recorder.started:
	case <-time.After(time.Second):
		t.Fatal("leased watcher did not begin traffic finalization")
	}

	select {
	case <-dropped:
		t.Fatal("session capacity released while traffic finalization was blocked")
	default:
	}
	if got := sess.activeStreams.Load(); got != 1 {
		t.Fatalf("active streams = %d, want 1 until watcher finalizes", got)
	}
	if got := len(e.authSlots); got != 1 {
		t.Fatalf("authenticated slots held = %d, want 1 until watcher finalizes", got)
	}

	close(recorder.release)
	select {
	case <-dropped:
	case <-time.After(time.Second):
		t.Fatal("dropSession did not finish after watcher finalization")
	}
	if got := sess.activeStreams.Load(); got != 0 {
		t.Fatalf("active streams = %d, want 0 after watcher finalizes", got)
	}
	if got := len(e.authSlots); got != 0 {
		t.Fatalf("authenticated slots held = %d, want 0 after watcher finalizes", got)
	}
}

func TestGracefulShutdownAllowsAlreadyAdmittedProxyToOpenStream(t *testing.T) {
	stream := newAdmissionStream()
	transport := &admissionSession{stream: stream}
	e, sess := newAdmissionEdge(transport, 1, 1)
	e.shutdown = make(chan struct{})
	e.shutdownDone = make(chan struct{})

	// Simulate handler admission before ReverseProxy reaches DialContext.
	if !e.beginProxy() {
		t.Fatal("beginProxy rejected before shutdown")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	shutdownResult := make(chan error, 1)
	go func() { shutdownResult <- e.Shutdown(ctx) }()

	deadline := time.Now().Add(time.Second)
	for !e.isShuttingDown() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !e.isShuttingDown() {
		t.Fatal("shutdown did not enter draining state")
	}

	conn, err := e.openRouteStream(context.Background(), "app.test")
	if err != nil {
		e.proxyWG.Done()
		t.Fatalf("already-admitted request could not open during drain: %v", err)
	}
	if _, err := conn.Write([]byte("request")); err != nil {
		e.proxyWG.Done()
		t.Fatalf("admitted request write: %v", err)
	}
	stream.finish()
	waitAdmissionReleased(t, e, sess)
	e.proxyWG.Done()

	select {
	case err := <-shutdownResult:
		if err != nil {
			t.Fatalf("graceful Shutdown: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not finish after admitted proxy drained")
	}
}

func TestProxyErrorMapping(t *testing.T) {
	e := &Edge{cfg: &config.Server{}, proxies: make(map[string]*httputil.ReverseProxy)}
	proxy := e.proxyFor("app.test")
	request := httptest.NewRequest(http.MethodGet, "https://app.test/", nil)

	capacity := httptest.NewRecorder()
	proxy.ErrorHandler(capacity, request, errors.Join(errors.New("open"), tunnel.ErrCapacity))
	if capacity.Code != http.StatusServiceUnavailable {
		t.Fatalf("capacity status = %d, want 503", capacity.Code)
	}
	if got := capacity.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1", got)
	}
	if got := capacity.Body.String(); got != "{\"error\":\"tunnel capacity reached\"}\n" {
		t.Fatalf("capacity body = %q", got)
	}

	other := httptest.NewRecorder()
	proxy.ErrorHandler(other, request, errors.New("open failed"))
	if other.Code != http.StatusBadGateway {
		t.Fatalf("other status = %d, want 502", other.Code)
	}

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	canceledRequest := request.Clone(canceledCtx)
	canceled := httptest.NewRecorder()
	proxy.ErrorHandler(canceled, canceledRequest, tunnel.ErrCapacity)
	if canceled.Body.Len() != 0 {
		t.Fatalf("canceled request was rewritten: %q", canceled.Body.String())
	}
}
