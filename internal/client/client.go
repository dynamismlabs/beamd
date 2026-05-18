// Package client implements the conduit client side.
//
// M5: a long-lived client that maintains a TLS+yamux session to the
// edge across network blips. `Connect` opens the first session
// synchronously; a background goroutine then watches for session loss
// and reconnects with exponential backoff, replaying every registration
// so URLs stay stable.
//
// Each accepted data stream begins with `<name>\n`; the rest is
// shuttled raw to the local backend port the caller registered for
// that name.
package client

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/treyhuffine/conduit/internal/mux"
	"github.com/treyhuffine/conduit/internal/proto"
)

const ALPNConduit = "conduit/1"

const (
	DefaultHeartbeatInterval = 20 * time.Second
	DefaultRegisterTimeout   = 5 * time.Second
	DefaultReconnectInitial  = 500 * time.Millisecond
	DefaultReconnectMax      = 30 * time.Second
)

// Options tune client behavior. Zero values fall back to sensible
// defaults; tests typically override heartbeat / reconnect cadence so
// they finish in well under a second.
//
// Server TLS verification is unconditionally skipped in M5 — we're on
// self-signed certs until the deferred `certs.MagicManager` lands.
type Options struct {
	HeartbeatInterval time.Duration
	RegisterTimeout   time.Duration
	ReconnectInitial  time.Duration
	ReconnectMax      time.Duration
}

func (o *Options) applyDefaults() {
	if o.HeartbeatInterval <= 0 {
		o.HeartbeatInterval = DefaultHeartbeatInterval
	}
	if o.RegisterTimeout <= 0 {
		o.RegisterTimeout = DefaultRegisterTimeout
	}
	if o.ReconnectInitial <= 0 {
		o.ReconnectInitial = DefaultReconnectInitial
	}
	if o.ReconnectMax <= 0 {
		o.ReconnectMax = DefaultReconnectMax
	}
}

// session bundles everything tied to a single yamux session lifetime.
// On reconnect, a fresh session replaces this one atomically.
type session struct {
	conn    *tls.Conn
	yamux   *yamux.Session
	control *yamux.Stream
	br      *bufio.Reader
	writeMu sync.Mutex

	pendingMu sync.Mutex
	pending   chan controlReply // single-slot, set under pendingMu
}

func (s *session) write(msg any) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return proto.Write(s.control, msg)
}

type Client struct {
	serverAddr string
	token      string
	opts       Options

	mu         sync.Mutex
	sess       *session
	slug       string
	baseDomain string
	intended   map[string]int // name → port; the desired registration state
	registerMu sync.Mutex     // serializes the *user-initiated* register flow

	closeOnce sync.Once
	closed    chan struct{}

	// skipBackoff is set when we receive a shutdown error from the
	// server; manage() reads & clears it to bypass the first
	// reconnect backoff slot, matching PRD §M6.
	skipBackoff atomic.Bool
}

type controlReply struct {
	registered *proto.Registered
	err        *proto.Error
}

// Connect opens the first TLS+yamux session and completes the protocol
// handshake. Returns once the session is live; thereafter, a background
// goroutine maintains reconnect-with-replay across session losses.
func Connect(ctx context.Context, serverAddr, token string, opts ...Options) (*Client, error) {
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	o.applyDefaults()

	c := &Client{
		serverAddr: serverAddr,
		token:      token,
		opts:       o,
		intended:   make(map[string]int),
		closed:     make(chan struct{}),
	}

	if err := c.connectOnce(ctx, true); err != nil {
		return nil, err
	}

	go c.manage()
	return c, nil
}

// IsHealthy reports whether the client currently has an active session.
func (c *Client) IsHealthy() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sess != nil
}

func (c *Client) Slug() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.slug
}

func (c *Client) BaseDomain() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.baseDomain
}

// Closed is closed when the user calls Close — NOT when the underlying
// session is merely lost (reconnect is automatic).
func (c *Client) Closed() <-chan struct{} { return c.closed }

func (c *Client) Close() error {
	var err error
	c.closeOnce.Do(func() {
		close(c.closed)
		c.mu.Lock()
		s := c.sess
		c.sess = nil
		c.mu.Unlock()
		if s != nil {
			err = s.yamux.Close()
		}
	})
	return err
}

// Register asks the edge to expose `port` at the URL labeled `name`.
// If the client is currently disconnected, Register blocks (up to
// RegisterTimeout) waiting for a session.
func (c *Client) Register(name string, port int) (string, error) {
	c.mu.Lock()
	c.intended[name] = port
	c.mu.Unlock()

	c.registerMu.Lock()
	defer c.registerMu.Unlock()

	return c.registerNow(name, port)
}

// registerNow sends a register message on the current session (waiting
// for one if disconnected). Assumes c.registerMu is held.
func (c *Client) registerNow(name string, port int) (string, error) {
	deadline := time.Now().Add(c.opts.RegisterTimeout)
	for {
		c.mu.Lock()
		s := c.sess
		c.mu.Unlock()
		if s != nil {
			return c.doRegisterOnSession(s, name, port)
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("register %q: no active session within %s", name, c.opts.RegisterTimeout)
		}
		select {
		case <-c.closed:
			return "", fmt.Errorf("client closed")
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (c *Client) doRegisterOnSession(s *session, name string, port int) (string, error) {
	reply := make(chan controlReply, 1)
	s.pendingMu.Lock()
	s.pending = reply
	s.pendingMu.Unlock()
	defer func() {
		s.pendingMu.Lock()
		s.pending = nil
		s.pendingMu.Unlock()
	}()

	if err := s.write(&proto.Register{
		Type: proto.TypeRegister, Name: name, Port: port,
	}); err != nil {
		return "", fmt.Errorf("send register: %w", err)
	}

	select {
	case r := <-reply:
		if r.err != nil {
			return "", fmt.Errorf("%s: %s", r.err.Code, r.err.Message)
		}
		if r.registered == nil {
			return "", fmt.Errorf("nil register reply")
		}
		return r.registered.URL, nil
	case <-c.closed:
		return "", fmt.Errorf("client closed")
	case <-time.After(c.opts.RegisterTimeout):
		return "", fmt.Errorf("register %q: timeout after %s", name, c.opts.RegisterTimeout)
	}
}

// Unregister drops the name from intended state. Best-effort send to
// the edge; a reconnect-then-replay will NOT re-add it.
func (c *Client) Unregister(name string) error {
	c.mu.Lock()
	delete(c.intended, name)
	s := c.sess
	c.mu.Unlock()
	if s == nil {
		return nil
	}
	return s.write(&proto.Unregister{Type: proto.TypeUnregister, Name: name})
}

// Intended returns a snapshot of (name → port) the caller has asked
// to expose. Used by the daemon's /list endpoint.
func (c *Client) Intended() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int, len(c.intended))
	for k, v := range c.intended {
		out[k] = v
	}
	return out
}

// connectOnce performs a single dial + handshake. If `first` is true,
// hello_ok results populate c.slug / c.baseDomain; subsequent reconnects
// must return the same slug (token didn't change).
func (c *Client) connectOnce(ctx context.Context, first bool) error {
	conn, err := tls.Dial("tcp", c.serverAddr, &tls.Config{
		InsecureSkipVerify: true, // M5: self-signed; MagicManager flips this off
		NextProtos:         []string{ALPNConduit},
	})
	if err != nil {
		return fmt.Errorf("dial %s: %w", c.serverAddr, err)
	}
	if got := conn.ConnectionState().NegotiatedProtocol; got != ALPNConduit {
		_ = conn.Close()
		return fmt.Errorf("server did not negotiate %q (got %q)", ALPNConduit, got)
	}

	yamuxSess, err := mux.Client(conn)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("yamux client: %w", err)
	}

	control, err := yamuxSess.OpenStream()
	if err != nil {
		_ = yamuxSess.Close()
		return fmt.Errorf("open control stream: %w", err)
	}

	if err := proto.Write(control, &proto.Hello{
		Type: proto.TypeHello, Token: c.token, ProtoVersion: proto.ProtoVersion,
	}); err != nil {
		_ = yamuxSess.Close()
		return fmt.Errorf("send hello: %w", err)
	}

	br := bufio.NewReader(control)
	typ, line, err := proto.Read(br)
	if err != nil {
		_ = yamuxSess.Close()
		return fmt.Errorf("read hello reply: %w", err)
	}
	switch typ {
	case proto.TypeHelloOK:
	case proto.TypeError:
		var e proto.Error
		_ = json.Unmarshal(line, &e)
		_ = yamuxSess.Close()
		return fmt.Errorf("server rejected hello: %s — %s", e.Code, e.Message)
	default:
		_ = yamuxSess.Close()
		return fmt.Errorf("expected hello_ok, got %s", typ)
	}
	var hok proto.HelloOK
	if err := json.Unmarshal(line, &hok); err != nil {
		_ = yamuxSess.Close()
		return fmt.Errorf("parse hello_ok: %w", err)
	}

	c.mu.Lock()
	if first {
		c.slug = hok.Slug
		c.baseDomain = hok.BaseDomain
	} else if hok.Slug != c.slug {
		c.mu.Unlock()
		_ = yamuxSess.Close()
		return fmt.Errorf("slug changed across reconnect: %s → %s", c.slug, hok.Slug)
	}
	s := &session{conn: conn, yamux: yamuxSess, control: control, br: br}
	c.sess = s
	c.mu.Unlock()

	go c.readControl(s)
	go c.heartbeatLoop(s)
	go c.acceptStreamsLoop(s)
	slog.Info("client connected", "slug", hok.Slug, "first", first)
	return nil
}

// manage runs in the background for the client's lifetime, restarting
// the session whenever it dies, with exponential backoff and replay
// of intended registrations.
func (c *Client) manage() {
	for {
		c.mu.Lock()
		s := c.sess
		c.mu.Unlock()
		if s == nil {
			// Should not happen — Connect populates it. Bail.
			return
		}
		select {
		case <-c.closed:
			return
		case <-s.yamux.CloseChan():
		}

		c.mu.Lock()
		c.sess = nil
		c.mu.Unlock()
		slog.Warn("client: session closed, reconnecting")

		backoff := c.opts.ReconnectInitial
		// `error{code:"shutdown"}` from the server pre-sets skipBackoff
		// so the first attempt fires immediately — the operator likely
		// brought up a replacement edge already. Otherwise we wait
		// ReconnectInitial before the first attempt (PRD §M5).
		skipFirstWait := c.skipBackoff.Swap(false)
		for {
			if !skipFirstWait {
				select {
				case <-c.closed:
					return
				case <-time.After(jitter(backoff)):
				}
				backoff *= 2
				if backoff > c.opts.ReconnectMax {
					backoff = c.opts.ReconnectMax
				}
			}
			skipFirstWait = false

			select {
			case <-c.closed:
				return
			default:
			}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err := c.connectOnce(ctx, false)
			cancel()
			if err == nil {
				if rerr := c.replayIntended(); rerr != nil {
					slog.Warn("client: replay after reconnect failed", "err", rerr.Error())
				} else {
					slog.Info("client: replay complete")
				}
				break
			}
			slog.Warn("client: reconnect attempt failed", "err", err.Error(), "backoff", backoff)
		}
	}
}

func (c *Client) replayIntended() error {
	c.mu.Lock()
	intended := make(map[string]int, len(c.intended))
	for k, v := range c.intended {
		intended[k] = v
	}
	s := c.sess
	c.mu.Unlock()

	if s == nil {
		return fmt.Errorf("no session")
	}
	for name, port := range intended {
		if _, err := c.doRegisterOnSession(s, name, port); err != nil {
			return fmt.Errorf("replay %s: %w", name, err)
		}
	}
	return nil
}

func (c *Client) readControl(s *session) {
	for {
		typ, line, err := proto.Read(s.br)
		if err != nil {
			return
		}
		s.pendingMu.Lock()
		pending := s.pending
		s.pendingMu.Unlock()

		switch typ {
		case proto.TypeRegistered:
			var msg proto.Registered
			if err := json.Unmarshal(line, &msg); err != nil {
				slog.Warn("client: parse registered", "err", err.Error())
				continue
			}
			if pending != nil {
				pending <- controlReply{registered: &msg}
			}
		case proto.TypeError:
			var msg proto.Error
			if err := json.Unmarshal(line, &msg); err != nil {
				continue
			}
			if msg.Code == proto.CodeShutdown {
				c.skipBackoff.Store(true)
				slog.Info("client: server signaled shutdown — reconnect will skip backoff")
			}
			if pending != nil {
				pending <- controlReply{err: &msg}
			} else {
				slog.Warn("client: unsolicited error", "code", msg.Code, "msg", msg.Message)
			}
		default:
			slog.Warn("client: unexpected control msg", "type", typ)
		}
	}
}

func (c *Client) heartbeatLoop(s *session) {
	tick := time.NewTicker(c.opts.HeartbeatInterval)
	defer tick.Stop()
	for {
		select {
		case <-c.closed:
			return
		case <-s.yamux.CloseChan():
			return
		case <-tick.C:
			if err := s.write(&proto.Heartbeat{Type: proto.TypeHeartbeat}); err != nil {
				return
			}
		}
	}
}

func (c *Client) acceptStreamsLoop(s *session) {
	for {
		stream, err := s.yamux.AcceptStream()
		if err != nil {
			if errors.Is(err, yamux.ErrSessionShutdown) || errors.Is(err, io.EOF) {
				return
			}
			slog.Debug("client: accept stream", "err", err.Error())
			return
		}
		go c.handleStream(stream)
	}
}

func (c *Client) handleStream(stream *yamux.Stream) {
	defer stream.Close()

	br := bufio.NewReader(stream)
	line, err := br.ReadString('\n')
	if err != nil {
		slog.Warn("stream: read name prefix", "err", err.Error())
		return
	}
	name := strings.TrimSpace(line)

	c.mu.Lock()
	port, ok := c.intended[name]
	c.mu.Unlock()
	if !ok {
		slog.Warn("stream: no backend for name", "name", name)
		return
	}

	backend, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		slog.Warn("stream: dial backend", "name", name, "err", err.Error())
		return
	}
	defer backend.Close()

	errc := make(chan error, 2)
	go func() { _, e := io.Copy(backend, br); errc <- e }()
	go func() { _, e := io.Copy(stream, backend); errc <- e }()
	<-errc
	_ = stream.Close()
	_ = backend.Close()
	<-errc
}

func jitter(d time.Duration) time.Duration {
	// ±25% jitter so reconnect storms don't synchronize.
	delta := time.Duration(rand.Int63n(int64(d / 2)))
	return d - d/4 + delta
}
