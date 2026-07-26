// Package tunnel provides the transport-neutral stream/session contract used
// by the beamd edge and agent.
package tunnel

import (
	"context"
	"net"
	"sync"
	"unicode/utf8"
)

type Kind string
type ErrorCode uint64

const (
	KindQUIC  Kind = "quic"
	KindYamux Kind = "tcp"
)

const (
	CloseNormal     ErrorCode = 0x00
	CloseShutdown   ErrorCode = 0x01
	CloseProtocol   ErrorCode = 0x02
	CloseAuth       ErrorCode = 0x03
	CloseSuperseded ErrorCode = 0x04
	CloseCapacity   ErrorCode = 0x05

	StreamCanceled ErrorCode = 0x10
	StreamCapacity ErrorCode = 0x11
)

type Stream interface {
	net.Conn
	CloseWrite() error
	Abort(ErrorCode)
	Done() <-chan struct{}
}

type CloseInfo struct {
	Code      ErrorCode
	CodeValid bool
	Remote    bool
	Reason    string
	Cause     error
}

type Session interface {
	Kind() Kind
	OpenStream(context.Context) (Stream, error)
	AcceptStream(context.Context) (Stream, error)
	Done() <-chan struct{}
	IsClosed() bool
	CloseInfo() CloseInfo
	CloseWithError(ErrorCode, string) error
	LocalAddr() net.Addr
	RemoteAddr() net.Addr
}

type Listener interface {
	Accept(context.Context) (Session, error)
	Close() error
	Addr() net.Addr
}

// CloseReason maps a terminal session result onto the fixed-cardinality label
// used by metrics and lifecycle logs. CloseInfo.Reason remains the sanitized
// human-readable application description.
func CloseReason(info CloseInfo) string {
	if info.CodeValid {
		switch info.Code {
		case CloseNormal, CloseSuperseded:
			return "normal"
		case CloseShutdown:
			return "shutdown"
		case CloseProtocol, CloseAuth:
			return "protocol"
		default:
			return "other"
		}
	}
	switch info.Reason {
	case "normal", "shutdown", "protocol", "idle", "network", "other":
		return info.Reason
	default:
		return "other"
	}
}

func sanitizeReason(reason string) string {
	if !utf8.ValidString(reason) {
		reason = string([]rune(reason))
	}
	if len(reason) <= 256 {
		return reason
	}
	reason = reason[:256]
	for !utf8.ValidString(reason) {
		reason = reason[:len(reason)-1]
	}
	return reason
}

type childStream interface {
	parentTerminated()
}

// sessionState arbitrates the first terminal event and owns the child stream
// registry. finish terminalizes children before making the session's Done
// channel observable.
type sessionState struct {
	mu       sync.Mutex
	terminal bool
	finished bool
	info     CloseInfo
	children map[childStream]struct{}
	done     chan struct{}
}

func newSessionState() *sessionState {
	return &sessionState{
		children: make(map[childStream]struct{}),
		done:     make(chan struct{}),
	}
}

func (s *sessionState) claim(info CloseInfo) bool {
	info.Reason = sanitizeReason(info.Reason)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.terminal {
		return false
	}
	s.terminal = true
	s.info = info
	return true
}

func (s *sessionState) register(stream childStream) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.terminal {
		return false
	}
	s.children[stream] = struct{}{}
	return true
}

func (s *sessionState) unregister(stream childStream) {
	s.mu.Lock()
	delete(s.children, stream)
	s.mu.Unlock()
}

// childDone publishes the child's terminal state before removing it from the
// parent registry. That order is load-bearing: once unregister succeeds, a
// concurrent finish may observe no children and publish Session.Done. Closing
// the child channel first guarantees Stream.Done is already observable in
// every such interleaving. Callers must guard this with their per-stream once.
func (s *sessionState) childDone(stream childStream, done chan struct{}) {
	close(done)
	s.unregister(stream)
}

func (s *sessionState) finish(fallback CloseInfo) {
	fallback.Reason = sanitizeReason(fallback.Reason)

	s.mu.Lock()
	if s.finished {
		s.mu.Unlock()
		return
	}
	if !s.terminal {
		s.terminal = true
		s.info = fallback
	}
	s.finished = true
	children := make([]childStream, 0, len(s.children))
	for child := range s.children {
		children = append(children, child)
	}
	clear(s.children)
	s.mu.Unlock()

	for _, child := range children {
		child.parentTerminated()
	}
	close(s.done)
}

func (s *sessionState) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.terminal
}

func (s *sessionState) closeInfo() CloseInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.info
}

func (s *sessionState) doneChan() <-chan struct{} {
	return s.done
}
