package tunnel

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
)

const (
	DefaultStreamWindow = 4 << 20
	yamuxStreamLimit    = 64
	// A local yamux stream SYN shares the TCP ordering domain with all bulk
	// data. Under the frozen high-RTT/loss mixed workload it can legitimately
	// remain blocked beyond the QUIC open bound. Keep this below yamux's own
	// establishment timer so the adapter still owns and joins a stuck open.
	yamuxStreamOpenTimeout = 60 * time.Second
	// yamux's StreamOpenTimeout is not the caller-visible OpenStream bound.
	// It waits for the peer's stream ACK and closes the entire session when it
	// expires. The ACK shares the TCP connection with bulk stream data, so it
	// must tolerate legitimate head-of-line delay under concurrent loss.
	streamEstablishmentTimeout = 75 * time.Second
	// yamux applies this fallback to graceful FINs as well as abandoned
	// streams. A short value can turn a healthy close into an RST and make the
	// peer discard response bytes already accepted into its receive buffer.
	streamCloseTimeout      = 5 * time.Minute
	streamCompletionTimeout = streamCloseTimeout + time.Second
)

// YamuxConfig is the single Part B yamux configuration used by both roles.
func YamuxConfig(windowBytes uint32) *yamux.Config {
	if windowBytes == 0 {
		windowBytes = DefaultStreamWindow
	}
	cfg := yamux.DefaultConfig()
	cfg.AcceptBacklog = yamuxStreamLimit
	cfg.EnableKeepAlive = false
	cfg.KeepAliveInterval = 20 * time.Second
	cfg.ConnectionWriteTimeout = 30 * time.Second
	cfg.StreamOpenTimeout = streamEstablishmentTimeout
	cfg.StreamCloseTimeout = streamCloseTimeout
	cfg.MaxStreamWindowSize = windowBytes
	cfg.LogOutput = io.Discard
	return cfg
}

type observedConn struct {
	net.Conn
	onError func(error)
}

type yamuxDiagnosticWriter struct {
	state *sessionState
}

func (w yamuxDiagnosticWriter) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("yamux: aborted stream open (")) {
		w.state.claim(CloseInfo{
			Reason: "other",
			Cause:  ErrOpenTimeout,
		})
		slog.Warn("yamux stream establishment timed out",
			"event", "yamux_stream_open_timeout",
			"transport", KindYamux,
			"timeout", streamEstablishmentTimeout,
		)
	}
	return len(p), nil
}

func (c *observedConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if err != nil {
		c.onError(err)
	}
	return n, err
}

func (c *observedConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if err != nil {
		c.onError(err)
	}
	return n, err
}

type YamuxSession struct {
	raw         *yamux.Session
	state       *sessionState
	gate        chan struct{}
	openTimeout time.Duration
}

func NewYamuxClient(conn net.Conn, windowBytes uint32) (*YamuxSession, error) {
	return newYamuxSession(conn, windowBytes, false)
}

func NewYamuxServer(conn net.Conn, windowBytes uint32) (*YamuxSession, error) {
	return newYamuxSession(conn, windowBytes, true)
}

func newYamuxSession(conn net.Conn, windowBytes uint32, server bool) (*YamuxSession, error) {
	s := &YamuxSession{
		state:       newSessionState(),
		gate:        make(chan struct{}, yamuxStreamLimit),
		openTimeout: yamuxStreamOpenTimeout,
	}
	observed := &observedConn{Conn: conn, onError: s.observeConnError}
	cfg := YamuxConfig(windowBytes)
	cfg.LogOutput = yamuxDiagnosticWriter{state: s.state}
	var (
		raw *yamux.Session
		err error
	)
	if server {
		raw, err = yamux.Server(observed, cfg)
	} else {
		raw, err = yamux.Client(observed, cfg)
	}
	if err != nil {
		return nil, err
	}
	s.raw = raw
	go s.watch()
	return s, nil
}

func (s *YamuxSession) Kind() Kind { return KindYamux }

func (s *YamuxSession) OpenStream(ctx context.Context) (Stream, error) {
	timeout := s.openTimeout
	if timeout <= 0 {
		timeout = yamuxStreamOpenTimeout
	}
	openCtx, cancel := context.WithTimeoutCause(ctx, timeout, ErrOpenTimeout)
	defer cancel()
	if openCtx.Err() != nil {
		return nil, derivedOpenError(ctx, openCtx)
	}
	if s.IsClosed() {
		return nil, ErrSessionClosed
	}

	select {
	case s.gate <- struct{}{}:
	case <-openCtx.Done():
		return nil, derivedOpenError(ctx, openCtx)
	case <-s.Done():
		if openCtx.Err() != nil {
			return nil, derivedOpenError(ctx, openCtx)
		}
		return nil, ErrSessionClosed
	}

	type result struct {
		stream *yamux.Stream
		err    error
	}
	resultCh := make(chan result, 1)
	go func() {
		stream, err := s.raw.OpenStream()
		resultCh <- result{stream: stream, err: err}
	}()

	select {
	case result := <-resultCh:
		if openCtx.Err() != nil {
			info := CloseInfo{Reason: "other", Cause: derivedOpenError(ctx, openCtx)}
			s.state.claim(info)
			_ = s.raw.Close()
			if result.stream != nil {
				_ = result.stream.Close()
			}
			s.releaseOpenSlot()
			s.state.finish(info)
			return nil, derivedOpenError(ctx, openCtx)
		}
		if result.err != nil || result.stream == nil {
			s.releaseOpenSlot()
			if result.err == nil {
				result.err = ErrSessionClosed
			}
			return nil, normalizeYamuxSessionError(result.err)
		}
		return s.registerYamuxStream(result.stream, true)

	case <-openCtx.Done():
		info := CloseInfo{Reason: "other", Cause: derivedOpenError(ctx, openCtx)}
		s.state.claim(info)
		_ = s.raw.Close()
		result := <-resultCh
		if result.stream != nil {
			_ = result.stream.Close()
		}
		s.releaseOpenSlot()
		s.state.finish(info)
		return nil, derivedOpenError(ctx, openCtx)

	case <-s.Done():
		result := <-resultCh
		if result.stream != nil {
			_ = result.stream.Close()
		}
		s.releaseOpenSlot()
		if openCtx.Err() != nil {
			return nil, derivedOpenError(ctx, openCtx)
		}
		return nil, ErrSessionClosed
	}
}

func (s *YamuxSession) AcceptStream(ctx context.Context) (Stream, error) {
	raw, err := s.raw.AcceptStreamWithContext(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, normalizeYamuxSessionError(err)
	}
	return s.registerYamuxStream(raw, false)
}

func (s *YamuxSession) registerYamuxStream(raw *yamux.Stream, holdsOpenSlot bool) (Stream, error) {
	stream := newYamuxStream(raw, s, holdsOpenSlot)
	if s.state.register(stream) {
		return stream, nil
	}
	stream.Abort(StreamCanceled)
	stream.parentTerminated()
	return nil, ErrSessionClosed
}

func (s *YamuxSession) Done() <-chan struct{} { return s.state.doneChan() }

func (s *YamuxSession) IsClosed() bool {
	return s.state.isClosed() || s.raw.IsClosed()
}

func (s *YamuxSession) CloseInfo() CloseInfo { return s.state.closeInfo() }

func (s *YamuxSession) CloseWithError(code ErrorCode, reason string) error {
	info := CloseInfo{
		Code:      code,
		CodeValid: true,
		Reason:    reason,
	}
	s.state.claim(info)
	err := s.raw.Close()
	s.state.finish(info)
	return err
}

func (s *YamuxSession) LocalAddr() net.Addr  { return s.raw.LocalAddr() }
func (s *YamuxSession) RemoteAddr() net.Addr { return s.raw.RemoteAddr() }

func (s *YamuxSession) watch() {
	<-s.raw.CloseChan()
	s.state.finish(CloseInfo{
		Reason: "other",
		Cause:  yamux.ErrSessionShutdown,
	})
}

func (s *YamuxSession) observeConnError(err error) {
	if err == nil || s.state.isClosed() {
		return
	}
	info := CloseInfo{Cause: err}
	if errors.Is(err, io.EOF) {
		info.Remote = true
		info.Reason = "normal"
	} else {
		info.Reason = "network"
	}
	s.state.claim(info)
}

func (s *YamuxSession) releaseOpenSlot() {
	select {
	case <-s.gate:
	default:
		panic("tunnel: yamux open gate released without ownership")
	}
}

func derivedOpenError(caller, derived context.Context) error {
	if err := caller.Err(); err != nil {
		return err
	}
	if errors.Is(context.Cause(derived), ErrOpenTimeout) {
		return ErrOpenTimeout
	}
	if err := context.Cause(derived); err != nil {
		return err
	}
	return derived.Err()
}

func normalizeYamuxSessionError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, yamux.ErrSessionShutdown) ||
		errors.Is(err, yamux.ErrConnectionWriteTimeout) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, net.ErrClosed) {
		return fmt.Errorf("%w: %w", ErrSessionClosed, err)
	}
	return err
}

type yamuxStream struct {
	raw    net.Conn
	parent *YamuxSession

	readMu sync.Mutex

	mu          sync.Mutex
	localClosed bool
	remoteDone  bool
	completed   bool
	closeTimer  *time.Timer

	done      chan struct{}
	doneOnce  sync.Once
	drainOnce sync.Once

	holdsOpenSlot bool
	slotOnce      sync.Once
}

func newYamuxStream(raw *yamux.Stream, parent *YamuxSession, holdsOpenSlot bool) *yamuxStream {
	return &yamuxStream{
		raw:           raw,
		parent:        parent,
		done:          make(chan struct{}),
		holdsOpenSlot: holdsOpenSlot,
	}
}

func (s *yamuxStream) Read(p []byte) (int, error) {
	s.readMu.Lock()
	defer s.readMu.Unlock()

	n, err := s.raw.Read(p)
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, yamux.ErrConnectionReset) {
			s.markRemoteDone()
		}
		if errors.Is(err, yamux.ErrSessionShutdown) {
			return n, fmt.Errorf("%w: %w", ErrSessionClosed, err)
		}
	}
	return n, err
}

func (s *yamuxStream) Write(p []byte) (int, error) {
	n, err := s.raw.Write(p)
	if errors.Is(err, yamux.ErrSessionShutdown) ||
		errors.Is(err, yamux.ErrConnectionWriteTimeout) {
		return n, fmt.Errorf("%w: %w", ErrSessionClosed, err)
	}
	return n, err
}

// closeSend lets yamux arm (or finish) its raw-stream cleanup before the
// adapter starts its longer completion fallback. In yamux v0.1.2, Close
// installs StreamCloseTimeout before it can block while sending the FIN.
func (s *yamuxStream) closeSend() error {
	err := s.raw.Close()
	s.markLocalClosed()
	return err
}

func (s *yamuxStream) CloseWrite() error {
	return s.closeSend()
}

func (s *yamuxStream) Close() error {
	err := s.CloseWrite()
	s.drainReceive()
	return err
}

func (s *yamuxStream) Abort(ErrorCode) {
	_ = s.closeSend()
	s.drainReceive()
}

func (s *yamuxStream) Done() <-chan struct{} { return s.done }

func (s *yamuxStream) LocalAddr() net.Addr  { return s.raw.LocalAddr() }
func (s *yamuxStream) RemoteAddr() net.Addr { return s.raw.RemoteAddr() }

func (s *yamuxStream) SetDeadline(t time.Time) error {
	return s.raw.SetDeadline(t)
}

func (s *yamuxStream) SetReadDeadline(t time.Time) error {
	return s.raw.SetReadDeadline(t)
}

func (s *yamuxStream) SetWriteDeadline(t time.Time) error {
	return s.raw.SetWriteDeadline(t)
}

func (s *yamuxStream) markLocalClosed() {
	s.mu.Lock()
	if !s.localClosed {
		s.localClosed = true
		if !s.completed && !s.remoteDone && s.closeTimer == nil {
			// Keep Stream.Done (and its admission lease) alive beyond yamux's
			// raw cleanup timer so hidden receive-window exposure cannot outlive
			// the accounting guard.
			s.closeTimer = time.AfterFunc(streamCompletionTimeout, s.complete)
		}
	}
	complete := s.localClosed && s.remoteDone
	s.mu.Unlock()
	if complete {
		s.complete()
	}
}

func (s *yamuxStream) markRemoteDone() {
	s.mu.Lock()
	s.remoteDone = true
	complete := s.localClosed
	s.mu.Unlock()
	if complete {
		s.complete()
	}
}

// drainReceive observes yamux's remote FIN after a full net.Conn close even
// when the caller stopped at an HTTP Content-Length boundary and therefore
// never performed the final EOF read. CloseWrite deliberately does not drain:
// the opposite direction may still be copying a request or response.
func (s *yamuxStream) drainReceive() {
	s.drainOnce.Do(func() {
		go func() {
			var buf [32 << 10]byte
			for {
				if _, err := s.Read(buf[:]); err != nil {
					return
				}
			}
		}()
	})
}

func (s *yamuxStream) complete() {
	s.doneOnce.Do(func() {
		s.mu.Lock()
		s.completed = true
		if s.closeTimer != nil {
			s.closeTimer.Stop()
			s.closeTimer = nil
		}
		s.mu.Unlock()
		if s.holdsOpenSlot {
			s.slotOnce.Do(s.parent.releaseOpenSlot)
		}
		s.parent.state.childDone(s, s.done)
	})
}

func (s *yamuxStream) parentTerminated() {
	s.complete()
}

var (
	_ Session = (*YamuxSession)(nil)
	_ Stream  = (*yamuxStream)(nil)
)
