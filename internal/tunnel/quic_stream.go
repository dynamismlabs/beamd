package tunnel

import (
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
)

type quicBidiStream interface {
	Read([]byte) (int, error)
	Write([]byte) (int, error)
	Close() error
	CancelRead(quic.StreamErrorCode)
	CancelWrite(quic.StreamErrorCode)
	SetDeadline(time.Time) error
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
}

type sendState uint8
type receiveState uint8

const (
	sendOpen sendState = iota
	sendFIN
	sendReset
)

const (
	receiveOpen receiveState = iota
	receiveTerminal
)

type quicStream struct {
	raw        quicBidiStream
	parent     *QUICSession
	localAddr  net.Addr
	remoteAddr net.Addr

	// writeMu prevents quic-go's forbidden Close/Write concurrency. Abort
	// deliberately does not acquire it: CancelWrite must wake a blocked Write.
	writeMu sync.Mutex

	// terminalMu protects both directional states and serializes graceful Close
	// against CancelWrite.
	terminalMu sync.Mutex
	send       sendState
	receive    receiveState

	done     chan struct{}
	doneOnce sync.Once
}

func newQUICStream(raw quicBidiStream, parent *QUICSession) *quicStream {
	return newQUICStreamWithAddresses(raw, parent, parent.LocalAddr(), parent.RemoteAddr())
}

func newQUICStreamWithAddresses(raw quicBidiStream, parent *QUICSession, localAddr, remoteAddr net.Addr) *quicStream {
	return &quicStream{
		raw:        raw,
		parent:     parent,
		localAddr:  localAddr,
		remoteAddr: remoteAddr,
		done:       make(chan struct{}),
	}
}

func (s *quicStream) Read(p []byte) (int, error) {
	n, err := s.raw.Read(p)
	if err != nil && receiveErrorIsTerminal(err) {
		s.markReceiveTerminal()
	}
	return n, s.normalizeIOError(err)
}

func receiveErrorIsTerminal(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, ErrSessionClosed) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return false
	}
	var streamErr *quic.StreamError
	if errors.As(err, &streamErr) {
		return true
	}
	// quic-go returns the connection terminal error when the parent dies.
	return true
}

func (s *quicStream) Write(p []byte) (int, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	s.terminalMu.Lock()
	open := s.send == sendOpen
	s.terminalMu.Unlock()
	if !open {
		return 0, net.ErrClosed
	}
	n, err := s.raw.Write(p)
	return n, s.normalizeIOError(err)
}

func (s *quicStream) CloseWrite() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.closeWriteLocked()
}

func (s *quicStream) closeWriteLocked() error {
	s.terminalMu.Lock()
	defer s.terminalMu.Unlock()

	switch s.send {
	case sendOpen:
		s.send = sendFIN
		err := s.raw.Close()
		s.maybeCompleteLocked()
		return err
	case sendFIN, sendReset:
		return nil
	default:
		panic("tunnel: invalid QUIC send state")
	}
}

func (s *quicStream) Abort(code ErrorCode) {
	s.terminalMu.Lock()
	if s.send != sendReset {
		s.send = sendReset
		s.raw.CancelWrite(quic.StreamErrorCode(code))
	}
	// Always call CancelRead. quic-go makes repeats and calls after EOF no-ops.
	s.raw.CancelRead(quic.StreamErrorCode(code))
	s.receive = receiveTerminal
	s.completeLocked()
	s.terminalMu.Unlock()
}

func (s *quicStream) Close() error {
	// Wake an early-response request-body writer before waiting for writeMu.
	_ = s.raw.SetWriteDeadline(time.Now())

	s.writeMu.Lock()
	err := s.closeWriteLocked()
	s.writeMu.Unlock()

	s.terminalMu.Lock()
	s.raw.CancelRead(quic.StreamErrorCode(StreamCanceled))
	s.receive = receiveTerminal
	s.maybeCompleteLocked()
	s.terminalMu.Unlock()
	return err
}

func (s *quicStream) Done() <-chan struct{} { return s.done }

func (s *quicStream) LocalAddr() net.Addr  { return s.localAddr }
func (s *quicStream) RemoteAddr() net.Addr { return s.remoteAddr }

func (s *quicStream) SetDeadline(t time.Time) error {
	return s.raw.SetDeadline(t)
}

func (s *quicStream) SetReadDeadline(t time.Time) error {
	return s.raw.SetReadDeadline(t)
}

func (s *quicStream) SetWriteDeadline(t time.Time) error {
	return s.raw.SetWriteDeadline(t)
}

func (s *quicStream) markReceiveTerminal() {
	s.terminalMu.Lock()
	s.receive = receiveTerminal
	s.maybeCompleteLocked()
	s.terminalMu.Unlock()
}

func (s *quicStream) maybeCompleteLocked() {
	if s.send != sendOpen && s.receive == receiveTerminal {
		s.completeLocked()
	}
}

func (s *quicStream) completeLocked() {
	s.doneOnce.Do(func() {
		s.parent.state.childDone(s, s.done)
	})
}

func (s *quicStream) parentTerminated() {
	s.terminalMu.Lock()
	s.send = sendReset
	s.receive = receiveTerminal
	s.completeLocked()
	s.terminalMu.Unlock()
}

func (s *quicStream) normalizeIOError(err error) error {
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, ErrSessionClosed) {
		return err
	}
	var streamErr *quic.StreamError
	if errors.As(err, &streamErr) {
		return err
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return err
	}
	if s.parent.IsClosed() {
		return errors.Join(ErrSessionClosed, err)
	}
	return err
}

var _ Stream = (*quicStream)(nil)
