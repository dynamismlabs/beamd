package tunnel

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newYamuxPair(t *testing.T) (*YamuxSession, *YamuxSession) {
	t.Helper()
	left, right := net.Pipe()
	client, err := NewYamuxClient(left, 0)
	if err != nil {
		t.Fatalf("NewYamuxClient: %v", err)
	}
	server, err := NewYamuxServer(right, 0)
	if err != nil {
		t.Fatalf("NewYamuxServer: %v", err)
	}
	t.Cleanup(func() {
		_ = client.CloseWithError(CloseNormal, "test cleanup")
		_ = server.CloseWithError(CloseNormal, "test cleanup")
	})
	return client, server
}

func openYamuxStreamPair(t *testing.T, opener, accepter *YamuxSession) (Stream, Stream) {
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
	accepted := <-acceptCh
	if accepted.err != nil {
		t.Fatalf("AcceptStream: %v", accepted.err)
	}
	return opened, accepted.stream
}

func TestYamuxConfigPartBHardening(t *testing.T) {
	cfg := YamuxConfig(0)
	if cfg.MaxStreamWindowSize != DefaultStreamWindow {
		t.Errorf("MaxStreamWindowSize = %d, want %d", cfg.MaxStreamWindowSize, DefaultStreamWindow)
	}
	if cfg.AcceptBacklog != 64 {
		t.Errorf("AcceptBacklog = %d, want 64", cfg.AcceptBacklog)
	}
	if cfg.EnableKeepAlive {
		t.Error("EnableKeepAlive = true, want false")
	}
	if cfg.KeepAliveInterval != 20*time.Second {
		t.Errorf("KeepAliveInterval = %v, want 20s", cfg.KeepAliveInterval)
	}
	if cfg.StreamOpenTimeout != 75*time.Second {
		t.Errorf("StreamOpenTimeout = %v, want 75s", cfg.StreamOpenTimeout)
	}
	if cfg.StreamCloseTimeout != 5*time.Minute {
		t.Errorf("StreamCloseTimeout = %v, want 5m", cfg.StreamCloseTimeout)
	}
	if streamCompletionTimeout <= cfg.StreamCloseTimeout {
		t.Errorf(
			"streamCompletionTimeout = %v, must outlive raw close timeout %v",
			streamCompletionTimeout,
			cfg.StreamCloseTimeout,
		)
	}
}

func TestYamuxDiagnosticWriterCapturesStreamEstablishmentTimeout(t *testing.T) {
	state := newSessionState()
	writer := yamuxDiagnosticWriter{state: state}
	message := []byte(
		"2026/07/30 12:00:00 [ERR] yamux: aborted stream open (destination=pipe): i/o deadline reached\n",
	)
	n, err := writer.Write(message)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len(message) {
		t.Fatalf("Write count = %d, want %d", n, len(message))
	}
	info := state.closeInfo()
	if info.Reason != "other" || !errors.Is(info.Cause, ErrOpenTimeout) {
		t.Fatalf("CloseInfo = %+v, want retained stream-establishment timeout", info)
	}
}

type gatedWriteYamuxConn struct {
	net.Conn
	writeStarted chan struct{}
	writeRelease chan struct{}
	startOnce    sync.Once
	releaseOnce  sync.Once
}

func newGatedWriteYamuxConn(conn net.Conn) *gatedWriteYamuxConn {
	return &gatedWriteYamuxConn{
		Conn:         conn,
		writeStarted: make(chan struct{}),
		writeRelease: make(chan struct{}),
	}
}

func (c *gatedWriteYamuxConn) Write(p []byte) (int, error) {
	c.startOnce.Do(func() { close(c.writeStarted) })
	select {
	case <-c.writeRelease:
		return c.Conn.Write(p)
	case <-time.After(10 * time.Second):
		return 0, context.DeadlineExceeded
	}
}

func (c *gatedWriteYamuxConn) releaseWrites() {
	c.releaseOnce.Do(func() { close(c.writeRelease) })
}

func TestYamuxStreamACKMayArriveAfterAdapterOpenBound(t *testing.T) {
	left, right := net.Pipe()
	gatedServerConn := newGatedWriteYamuxConn(right)
	client, err := NewYamuxClient(left, 0)
	if err != nil {
		t.Fatalf("NewYamuxClient: %v", err)
	}
	server, err := NewYamuxServer(gatedServerConn, 0)
	if err != nil {
		t.Fatalf("NewYamuxServer: %v", err)
	}
	t.Cleanup(func() {
		gatedServerConn.releaseWrites()
		_ = client.CloseWithError(CloseNormal, "test cleanup")
		_ = server.CloseWithError(CloseNormal, "test cleanup")
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	clientStream, err := client.OpenStream(ctx)
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	accepted := make(chan struct {
		stream Stream
		err    error
	}, 1)
	go func() {
		stream, err := server.AcceptStream(ctx)
		accepted <- struct {
			stream Stream
			err    error
		}{stream, err}
	}()

	awaitClosed(t, gatedServerConn.writeStarted, "server stream ACK")
	select {
	case <-client.Done():
		t.Fatal("yamux session closed before the former stream-ACK timeout")
	case <-time.After(streamOpenTimeout + 250*time.Millisecond):
	}
	if client.IsClosed() || server.IsClosed() {
		t.Fatal("yamux session did not survive a delayed stream ACK")
	}

	gatedServerConn.releaseWrites()
	result := <-accepted
	if result.err != nil {
		t.Fatalf("AcceptStream after releasing ACK: %v", result.err)
	}
	clientStream.Abort(StreamCanceled)
	result.stream.Abort(StreamCanceled)
}

func newTestYamuxCompletionStream() *yamuxStream {
	parent := &YamuxSession{
		state: newSessionState(),
		gate:  make(chan struct{}, yamuxStreamLimit),
	}
	stream := &yamuxStream{
		parent: parent,
		done:   make(chan struct{}),
	}
	if !parent.state.register(stream) {
		panic("test parent unexpectedly closed")
	}
	return stream
}

func completeYamuxStreamOrdinarily(stream *yamuxStream) {
	stream.markLocalClosed()
	stream.markRemoteDone()
}

func TestYamuxOrdinaryCompletionPublishesDoneBeforeUnregister(t *testing.T) {
	stream := newTestYamuxCompletionStream()
	state := stream.parent.state

	// Holding the registry mutex stops unregister at the exact boundary under
	// test. Ordinary completion must still publish Stream.Done before blocking
	// on that mutex; the old unregister-then-close order cannot pass this seam.
	state.mu.Lock()
	childReturned := make(chan struct{})
	go func() {
		completeYamuxStreamOrdinarily(stream)
		close(childReturned)
	}()
	select {
	case <-stream.Done():
	case <-time.After(time.Second):
		state.mu.Unlock()
		t.Fatal("yamux Stream.Done was not published before unregister")
	}

	parentReturned := make(chan struct{})
	go func() {
		state.finish(CloseInfo{Reason: "network"})
		close(parentReturned)
	}()
	select {
	case <-state.doneChan():
		state.mu.Unlock()
		t.Fatal("yamux Session.Done closed while the registry mutex was held")
	default:
	}
	state.mu.Unlock()

	awaitClosed(t, childReturned, "ordinary yamux completion")
	awaitClosed(t, parentReturned, "yamux parent finish")
	awaitClosed(t, state.doneChan(), "yamux Session.Done")
	select {
	case <-stream.Done():
	default:
		t.Fatal("yamux Session.Done became observable before Stream.Done")
	}
}

func TestYamuxOrdinaryCompletionRaceParentFinish(t *testing.T) {
	const iterations = 1_000
	for i := range iterations {
		stream := newTestYamuxCompletionStream()
		state := stream.parent.state
		start := make(chan struct{})
		var workers sync.WaitGroup
		workers.Add(2)
		go func() {
			defer workers.Done()
			<-start
			completeYamuxStreamOrdinarily(stream)
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
			t.Fatalf("iteration %d: yamux Session.Done won before Stream.Done", i)
		}
		workers.Wait()
	}
}

func TestYamuxOpenPrefersAuthoritativeCallerCancellation(t *testing.T) {
	for range 1_000 {
		session := &YamuxSession{
			state: newSessionState(),
			gate:  make(chan struct{}, yamuxStreamLimit),
		}
		for range yamuxStreamLimit {
			session.gate <- struct{}{}
		}
		session.state.finish(CloseInfo{Reason: "network"})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_, err := session.OpenStream(ctx)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("OpenStream error = %v, want authoritative context.Canceled", err)
		}
	}
}

type blockedYamuxConn struct {
	closed       chan struct{}
	writeStarted chan struct{}
	closeOnce    sync.Once
	startOnce    sync.Once
	activeWrites atomic.Int64
}

func newBlockedYamuxConn() *blockedYamuxConn {
	return &blockedYamuxConn{
		closed:       make(chan struct{}),
		writeStarted: make(chan struct{}),
	}
}

func (c *blockedYamuxConn) Read([]byte) (int, error) {
	<-c.closed
	return 0, io.EOF
}

func (c *blockedYamuxConn) Write([]byte) (int, error) {
	c.activeWrites.Add(1)
	defer c.activeWrites.Add(-1)
	c.startOnce.Do(func() { close(c.writeStarted) })
	<-c.closed
	return 0, net.ErrClosed
}

func (c *blockedYamuxConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

func (c *blockedYamuxConn) LocalAddr() net.Addr              { return blockedYamuxAddr("local") }
func (c *blockedYamuxConn) RemoteAddr() net.Addr             { return blockedYamuxAddr("remote") }
func (c *blockedYamuxConn) SetDeadline(time.Time) error      { return nil }
func (c *blockedYamuxConn) SetReadDeadline(time.Time) error  { return nil }
func (c *blockedYamuxConn) SetWriteDeadline(time.Time) error { return nil }

type blockedYamuxAddr string

func (a blockedYamuxAddr) Network() string { return "blocked-yamux" }
func (a blockedYamuxAddr) String() string  { return string(a) }

type gatedCloseYamuxConn struct {
	closeStarted chan struct{}
	closeRelease chan struct{}
	closeOnce    sync.Once
}

func newGatedCloseYamuxConn() *gatedCloseYamuxConn {
	return &gatedCloseYamuxConn{
		closeStarted: make(chan struct{}),
		closeRelease: make(chan struct{}),
	}
}

func (c *gatedCloseYamuxConn) Read([]byte) (int, error) { return 0, io.EOF }
func (c *gatedCloseYamuxConn) Write(p []byte) (int, error) {
	return len(p), nil
}
func (c *gatedCloseYamuxConn) Close() error {
	c.closeOnce.Do(func() {
		close(c.closeStarted)
		<-c.closeRelease
	})
	return nil
}
func (c *gatedCloseYamuxConn) LocalAddr() net.Addr              { return blockedYamuxAddr("local") }
func (c *gatedCloseYamuxConn) RemoteAddr() net.Addr             { return blockedYamuxAddr("remote") }
func (c *gatedCloseYamuxConn) SetDeadline(time.Time) error      { return nil }
func (c *gatedCloseYamuxConn) SetReadDeadline(time.Time) error  { return nil }
func (c *gatedCloseYamuxConn) SetWriteDeadline(time.Time) error { return nil }

func TestYamuxRawClosePrecedesWrapperCompletionTimer(t *testing.T) {
	tests := []struct {
		name      string
		terminate func(*yamuxStream) error
	}{
		{
			name: "close write",
			terminate: func(stream *yamuxStream) error {
				return stream.CloseWrite()
			},
		},
		{
			name: "abort",
			terminate: func(stream *yamuxStream) error {
				stream.Abort(StreamCanceled)
				return nil
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stream := newTestYamuxCompletionStream()
			raw := newGatedCloseYamuxConn()
			stream.raw = raw
			var releaseOnce sync.Once
			releaseRawClose := func() {
				releaseOnce.Do(func() { close(raw.closeRelease) })
			}
			t.Cleanup(func() {
				releaseRawClose()
				stream.parent.state.finish(CloseInfo{Reason: "test cleanup"})
			})

			returned := make(chan error, 1)
			go func() {
				returned <- test.terminate(stream)
			}()
			awaitClosed(t, raw.closeStarted, "raw yamux close to start")

			stream.mu.Lock()
			localClosed := stream.localClosed
			timerSet := stream.closeTimer != nil
			stream.mu.Unlock()
			if localClosed || timerSet {
				t.Fatalf(
					"wrapper completion started while raw Close was blocked: localClosed=%t timerSet=%t",
					localClosed,
					timerSet,
				)
			}
			select {
			case err := <-returned:
				t.Fatalf("terminal call returned before raw Close: %v", err)
			default:
			}

			releaseRawClose()
			select {
			case err := <-returned:
				if err != nil {
					t.Fatalf("terminal call: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for terminal call")
			}

			stream.mu.Lock()
			localClosed = stream.localClosed
			stream.mu.Unlock()
			if !localClosed {
				t.Fatal("wrapper completion did not start after raw Close returned")
			}
		})
	}
}

func TestYamuxParentCompletionDuringRawCloseDoesNotRearmTimer(t *testing.T) {
	stream := newTestYamuxCompletionStream()
	raw := newGatedCloseYamuxConn()
	stream.raw = raw
	var releaseOnce sync.Once
	releaseRawClose := func() {
		releaseOnce.Do(func() { close(raw.closeRelease) })
	}
	t.Cleanup(releaseRawClose)

	returned := make(chan error, 1)
	go func() {
		returned <- stream.CloseWrite()
	}()
	awaitClosed(t, raw.closeStarted, "raw yamux close to start")

	stream.parent.state.finish(CloseInfo{Reason: "test parent shutdown"})
	awaitClosed(t, stream.Done(), "stream completion from parent shutdown")
	releaseRawClose()
	select {
	case err := <-returned:
		if err != nil {
			t.Fatalf("CloseWrite: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for CloseWrite")
	}

	stream.mu.Lock()
	localClosed := stream.localClosed
	completed := stream.completed
	timerSet := stream.closeTimer != nil
	stream.mu.Unlock()
	if !localClosed || !completed || timerSet {
		t.Fatalf(
			"post-parent-close state: localClosed=%t completed=%t timerSet=%t",
			localClosed,
			completed,
			timerSet,
		)
	}
}

func TestYamuxStuckOpenTimeoutClosesJoinsAndReleasesGate(t *testing.T) {
	conn := newBlockedYamuxConn()
	session, err := NewYamuxClient(conn, 0)
	if err != nil {
		t.Fatalf("NewYamuxClient: %v", err)
	}
	session.openTimeout = 20 * time.Millisecond

	_, err = session.OpenStream(context.Background())
	if err != ErrOpenTimeout {
		t.Fatalf("OpenStream error = %v, want exact ErrOpenTimeout", err)
	}
	awaitClosed(t, session.Done(), "yamux session after stuck open")
	select {
	case <-conn.closed:
	default:
		t.Fatal("stuck OpenStream timeout did not close the underlying connection")
	}
	if got := conn.activeWrites.Load(); got != 0 {
		t.Fatalf("active underlying writes after OpenStream returned = %d, want 0 (joined)", got)
	}
	if got := len(session.gate); got != 0 {
		t.Fatalf("yamux open-gate occupancy after timeout = %d, want 0", got)
	}
	if !session.raw.IsClosed() {
		t.Fatal("yamux session remained open after stuck OpenStream timeout")
	}
	info := session.CloseInfo()
	if !errors.Is(info.Cause, ErrOpenTimeout) {
		t.Fatalf("CloseInfo = %+v, want adapter timeout cause", info)
	}
}

func TestYamuxStuckOpenCallerCancellationPropagatesAndJoins(t *testing.T) {
	conn := newBlockedYamuxConn()
	session, err := NewYamuxClient(conn, 0)
	if err != nil {
		t.Fatalf("NewYamuxClient: %v", err)
	}
	session.openTimeout = time.Second
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := session.OpenStream(ctx)
		result <- err
	}()
	<-conn.writeStarted
	cancel()
	if err := <-result; err != context.Canceled {
		t.Fatalf("OpenStream error = %v, want exact context.Canceled", err)
	}
	awaitClosed(t, session.Done(), "yamux session after canceled stuck open")
	if got := conn.activeWrites.Load(); got != 0 {
		t.Fatalf("active underlying writes after cancellation returned = %d, want 0 (joined)", got)
	}
	if got := len(session.gate); got != 0 {
		t.Fatalf("yamux open-gate occupancy after cancellation = %d, want 0", got)
	}
}

func TestYamuxPreCanceledOpenDoesNotTouchHealthySession(t *testing.T) {
	client, server := newYamuxPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	for range 1_000 {
		_, err := client.OpenStream(ctx)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("OpenStream error = %v, want authoritative context.Canceled", err)
		}
		if client.IsClosed() || server.IsClosed() {
			t.Fatal("pre-canceled OpenStream touched an otherwise healthy session")
		}
	}

	clientStream, serverStream := openYamuxStreamPair(t, client, server)
	clientStream.Abort(StreamCanceled)
	serverStream.Abort(StreamCanceled)
}

func TestYamuxSessionContractAndDoneOrdering(t *testing.T) {
	client, server := newYamuxPair(t)
	if client.Kind() != KindYamux || server.Kind() != KindYamux {
		t.Fatalf("kinds = %q/%q, want tcp/tcp", client.Kind(), server.Kind())
	}
	clientStream, serverStream := openYamuxStreamPair(t, client, server)

	payload := []byte("hello")
	writeCh := make(chan error, 1)
	go func() {
		_, err := clientStream.Write(payload)
		if err == nil {
			err = clientStream.CloseWrite()
		}
		writeCh <- err
	}()
	got, err := io.ReadAll(serverStream)
	if err != nil {
		t.Fatalf("server ReadAll: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("payload = %q, want %q", got, payload)
	}
	if err := <-writeCh; err != nil {
		t.Fatalf("client write/close: %v", err)
	}
	if err := serverStream.CloseWrite(); err != nil {
		t.Fatalf("server CloseWrite: %v", err)
	}
	if _, err := io.ReadAll(clientStream); err != nil {
		t.Fatalf("client ReadAll: %v", err)
	}
	awaitClosed(t, clientStream.Done(), "client stream Done")
	awaitClosed(t, serverStream.Done(), "server stream Done")

	idleClient, _ := openYamuxStreamPair(t, client, server)
	if err := client.CloseWithError(CloseProtocol, "bad control"); err != nil {
		t.Fatalf("CloseWithError: %v", err)
	}
	awaitClosed(t, client.Done(), "client session Done")
	select {
	case <-idleClient.Done():
	default:
		t.Fatal("session Done closed before child stream Done")
	}
	info := client.CloseInfo()
	if !info.CodeValid || info.Code != CloseProtocol || info.Remote {
		t.Fatalf("CloseInfo = %+v, want local protocol close", info)
	}
}

func TestYamuxGracefulClosePreservesBufferedTailPastFiveSeconds(t *testing.T) {
	client, server := newYamuxPair(t)
	clientStream, serverStream := openYamuxStreamPair(t, client, server)
	deadline := time.Now().Add(15 * time.Second)
	if err := clientStream.SetDeadline(deadline); err != nil {
		t.Fatalf("set client stream deadline: %v", err)
	}
	if err := serverStream.SetDeadline(deadline); err != nil {
		t.Fatalf("set server stream deadline: %v", err)
	}

	payload := bytes.Repeat([]byte{0xa5}, 1<<20)
	writeDone := make(chan error, 1)
	go func() {
		_, err := serverStream.Write(payload)
		if err == nil {
			err = serverStream.CloseWrite()
		}
		writeDone <- err
	}()
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatalf("write buffered response: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out writing buffered response")
	}

	// A graceful FIN may sit behind already-accepted response bytes while the
	// receiving application is paused. The former five-second close timeout
	// turned that healthy close into an RST and made yamux discard recvBuf.
	time.Sleep(6 * time.Second)

	select {
	case <-serverStream.Done():
		t.Fatal("sender Stream.Done closed before the peer returned FIN")
	default:
	}
	got, err := io.ReadAll(clientStream)
	if err != nil {
		t.Fatalf("read buffered response after graceful close: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("buffered response length = %d, want %d exact bytes", len(got), len(payload))
	}

	if err := clientStream.CloseWrite(); err != nil {
		t.Fatalf("close receiving side: %v", err)
	}
	if _, err := io.ReadAll(serverStream); err != nil {
		t.Fatalf("observe receiving-side FIN: %v", err)
	}
	awaitClosed(t, clientStream.Done(), "client stream Done")
	awaitClosed(t, serverStream.Done(), "server stream Done")
}

func TestYamuxFullCloseObservesRemoteFINAfterContentLengthRead(t *testing.T) {
	client, server := newYamuxPair(t)
	clientStream, serverStream := openYamuxStreamPair(t, client, server)

	const response = "fixed-size"
	writeDone := make(chan error, 1)
	go func() {
		_, err := serverStream.Write([]byte(response))
		if err == nil {
			err = serverStream.CloseWrite()
		}
		writeDone <- err
	}()

	// Read exactly Content-Length bytes and stop before the EOF read, matching
	// net/http's common fixed-length response behavior.
	got := make([]byte, len(response))
	if _, err := io.ReadFull(clientStream, got); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if string(got) != response {
		t.Fatalf("response = %q, want %q", got, response)
	}
	if err := <-writeDone; err != nil {
		t.Fatalf("write response: %v", err)
	}

	if err := clientStream.Close(); err != nil {
		t.Fatalf("full close: %v", err)
	}
	awaitClosed(t, clientStream.Done(), "client stream Done after fixed-length read")

	if err := serverStream.Close(); err != nil {
		t.Fatalf("server full close: %v", err)
	}
	awaitClosed(t, serverStream.Done(), "server stream Done")
}

func TestYamuxInternalCloseIsNotReportedAsRemoteNormal(t *testing.T) {
	client, _ := newYamuxPair(t)
	if err := client.raw.Close(); err != nil {
		t.Fatalf("raw Close: %v", err)
	}
	awaitClosed(t, client.Done(), "client session Done")
	info := client.CloseInfo()
	if info.Remote && info.Reason == "normal" {
		t.Fatalf("CloseInfo = %+v, internal close must not claim a clean peer EOF", info)
	}
}

func TestYamuxObservedPeerEOFFIsRemoteNormal(t *testing.T) {
	client, server := newYamuxPair(t)
	if err := server.CloseWithError(CloseNormal, "peer close"); err != nil {
		t.Fatalf("server CloseWithError: %v", err)
	}
	awaitClosed(t, client.Done(), "client session Done")
	info := client.CloseInfo()
	if !info.Remote || info.Reason != "normal" || info.CodeValid {
		t.Fatalf("CloseInfo = %+v, want observed remote normal EOF", info)
	}
	if !errors.Is(info.Cause, io.EOF) {
		t.Fatalf("CloseInfo cause = %v, want peer EOF", info.Cause)
	}
}

func TestYamuxLocalAndRemoteCloseInfo(t *testing.T) {
	client, server := newYamuxPair(t)
	if err := client.CloseWithError(CloseAuth, "local auth rejection"); err != nil {
		t.Fatalf("client CloseWithError: %v", err)
	}
	awaitClosed(t, client.Done(), "locally closed yamux session")
	awaitClosed(t, server.Done(), "remotely closed yamux session")

	local := client.CloseInfo()
	if !local.CodeValid || local.Code != CloseAuth || local.Remote ||
		local.Reason != "local auth rejection" || CloseReason(local) != "protocol" {
		t.Fatalf("local CloseInfo = %+v, want retained local auth close", local)
	}
	remote := server.CloseInfo()
	if remote.CodeValid || !remote.Remote || remote.Reason != "normal" ||
		!errors.Is(remote.Cause, io.EOF) || CloseReason(remote) != "normal" {
		t.Fatalf("remote CloseInfo = %+v, want clean remote yamux EOF", remote)
	}
}

func TestYamuxRepeatedConcurrentTerminalCalls(t *testing.T) {
	t.Run("stream", func(t *testing.T) {
		client, server := newYamuxPair(t)
		clientStream, serverStream := openYamuxStreamPair(t, client, server)
		const calls = 96
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(calls)
		for i := range calls {
			go func() {
				defer wg.Done()
				<-start
				switch i % 3 {
				case 0:
					_ = clientStream.Close()
				case 1:
					_ = clientStream.CloseWrite()
				default:
					clientStream.Abort(StreamCanceled)
				}
			}()
		}
		close(start)
		wg.Wait()
		serverStream.Abort(StreamCanceled)
		awaitClosed(t, clientStream.Done(), "repeatedly terminalized yamux stream")
		awaitClosed(t, serverStream.Done(), "peer yamux stream")
		if got := len(client.gate); got != 0 {
			t.Fatalf("yamux open-gate occupancy = %d, want exactly-once release", got)
		}
	})

	t.Run("session", func(t *testing.T) {
		client, server := newYamuxPair(t)
		const calls = 96
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
				if err := client.CloseWithError(code, "concurrent close"); err != nil {
					t.Errorf("CloseWithError: %v", err)
				}
			}()
		}
		close(start)
		wg.Wait()
		awaitClosed(t, client.Done(), "repeatedly terminalized yamux session")
		awaitClosed(t, server.Done(), "remote yamux session")

		info := client.CloseInfo()
		if !info.CodeValid || info.Remote ||
			(info.Code != CloseProtocol && info.Code != CloseShutdown) {
			t.Fatalf("local CloseInfo = %+v, want one local winning application close", info)
		}
		remote := server.CloseInfo()
		if remote.CodeValid || !remote.Remote || remote.Reason != "normal" {
			t.Fatalf("remote CloseInfo = %+v, want clean remote yamux EOF", remote)
		}
	})
}

func TestYamuxAcceptCancellationKeepsSessionHealthy(t *testing.T) {
	client, server := newYamuxPair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := server.AcceptStream(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("AcceptStream error = %v, want context deadline", err)
	}
	if server.IsClosed() {
		t.Fatal("accept cancellation closed a healthy yamux session")
	}
	clientStream, serverStream := openYamuxStreamPair(t, client, server)
	clientStream.Abort(StreamCanceled)
	serverStream.Abort(StreamCanceled)
}

func TestYamuxSessionShutdownErrorsNormalize(t *testing.T) {
	client, server := newYamuxPair(t)
	if err := server.CloseWithError(CloseShutdown, "done"); err != nil {
		t.Fatalf("server close: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := client.AcceptStream(ctx)
	if !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("AcceptStream error = %v, want ErrSessionClosed", err)
	}
}

func awaitClosed(t *testing.T, ch <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}
