// Package client implements the beam client side.
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

	"github.com/dynamismlabs/beamd/internal/mux"
	"github.com/dynamismlabs/beamd/internal/naming"
	"github.com/dynamismlabs/beamd/internal/proto"
)

const ALPNBeam = "beam/1"

const (
	DefaultHeartbeatInterval = 20 * time.Second
	DefaultRegisterTimeout   = 5 * time.Second
	DefaultReconnectInitial  = 500 * time.Millisecond
	DefaultReconnectMax      = 30 * time.Second
)

// Options tune client behavior. Zero values fall back to sensible
// defaults; tests typically override heartbeat / reconnect cadence so
// they finish in well under a second.
type Options struct {
	HeartbeatInterval time.Duration
	RegisterTimeout   time.Duration
	ReconnectInitial  time.Duration
	ReconnectMax      time.Duration

	// InsecureSkipVerify disables verification of the edge's TLS cert.
	// Default false: the edge cert is verified, so the bearer token only
	// rides a trusted connection. Set true only for a self-signed dev edge.
	InsecureSkipVerify bool

	// Scope is the requested org/scope, sent in the hello. Empty means "the
	// credential's default" (the edge picks personal for a session, the fixed
	// slug for an OSS token / API key). The resolved scope comes back as the
	// hello_ok slug and must stay stable across reconnects.
	Scope string
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
	pending   *pendingRegister // single-slot, set under pendingMu
}

// pendingRegister is the in-flight register awaiting its reply. Carrying the
// name lets readControl drop a mismatched `registered` (e.g. the late reply of
// a register that already timed out) instead of resolving the wrong caller
// with the wrong URL.
type pendingRegister struct {
	name string
	ch   chan controlReply
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
	shape      string         // edge URL shape from hello_ok ("" from older edges → hyphen)
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

// Shape is the edge's URL shape from hello_ok (hyphen|subdomain|flat). Empty
// when talking to an older edge that predates the field; callers should treat ""
// as the hyphen default (naming.ParseShape does).
func (c *Client) Shape() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.shape
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
	if name == "" {
		// The edge derives the label from the port for an empty name and
		// sends that label on data streams; key our backend map under the
		// same label so stream lookups match (otherwise the edge 502s with
		// "no backend for name").
		name = naming.LabelFromPort(port)
	}

	c.mu.Lock()
	prev, had := c.intended[name]
	c.intended[name] = port
	c.mu.Unlock()

	c.registerMu.Lock()
	defer c.registerMu.Unlock()

	url, err := c.registerNow(name, port)
	if err != nil {
		// Roll back: a failed open must not linger as intended state, or the
		// next reconnect's replay silently resurrects it as a ghost tunnel the
		// user was told does not exist. (Guard on port so a concurrent
		// re-Register of the same name isn't clobbered.)
		c.mu.Lock()
		if cur, ok := c.intended[name]; ok && cur == port {
			if had {
				c.intended[name] = prev
			} else {
				delete(c.intended, name)
			}
		}
		c.mu.Unlock()
	}
	return url, err
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
	pending := &pendingRegister{name: name, ch: make(chan controlReply, 1)}
	s.pendingMu.Lock()
	s.pending = pending
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
	case r := <-pending.ch:
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
	// DialContext (not tls.Dial) so the caller's ctx deadline actually bounds
	// the dial + handshake — a black-hole network can't hang past it. The
	// dialer sets SNI from c.serverAddr's host when ServerName is empty, so
	// verification matches the edge hostname.
	dialer := tls.Dialer{Config: &tls.Config{
		InsecureSkipVerify: c.opts.InsecureSkipVerify, //nolint:gosec // opt-in for self-signed dev edges
		NextProtos:         []string{ALPNBeam},
	}}
	rawConn, err := dialer.DialContext(ctx, "tcp", c.serverAddr)
	if err != nil {
		return fmt.Errorf("dial %s: %w", c.serverAddr, err)
	}
	conn := rawConn.(*tls.Conn)
	if got := conn.ConnectionState().NegotiatedProtocol; got != ALPNBeam {
		_ = conn.Close()
		return fmt.Errorf("server did not negotiate %q (got %q)", ALPNBeam, got)
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

	// The dialer's ctx bounded dial+TLS only. Extend the same deadline over
	// the hello exchange — otherwise an edge that accepts TLS+yamux but never
	// services the control stream leaves us blocked in this "single attempt"
	// forever, and manage() never retries.
	if dl, ok := ctx.Deadline(); ok {
		_ = control.SetDeadline(dl)
	}

	if err := proto.Write(control, &proto.Hello{
		Type: proto.TypeHello, Token: c.token, Scope: c.opts.Scope, ProtoVersion: proto.ProtoVersion,
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
	_ = control.SetDeadline(time.Time{}) // handshake done; steady state is unbounded

	c.mu.Lock()
	// Close() may have run while we were dialing. It found c.sess nil and had
	// nothing to close — if we installed now, this fresh session would live on
	// as a phantom client the edge keeps alive via keepalives.
	select {
	case <-c.closed:
		c.mu.Unlock()
		_ = yamuxSess.Close()
		return fmt.Errorf("client closed")
	default:
	}
	if first {
		c.slug = hok.Slug
		c.baseDomain = hok.BaseDomain
		c.shape = hok.Shape
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
				// Replay in the background so manage() goes straight back to
				// watching CloseChan — a wedged replay must never stop us from
				// noticing the session died.
				go c.replayLoop()
				break
			}
			slog.Warn("client: reconnect attempt failed", "err", err.Error(), "backoff", backoff)
		}
	}
}

// replayLoop re-registers the intended state after a reconnect, retrying
// failed names with backoff for as long as this session lives. A name can
// fail transiently — most commonly `name_taken` when we reconnect before the
// edge's heartbeat watchdog reaps our previous session — so giving up after
// one pass would strand tunnels on a live session while `beamd list` still
// reports healthy. Exits when everything is replayed, the session dies (the
// next reconnect starts a fresh replay), or the client closes.
func (c *Client) replayLoop() {
	c.mu.Lock()
	s := c.sess
	c.mu.Unlock()
	if s == nil {
		return
	}

	var only map[string]bool // nil = replay everything intended
	backoff := c.opts.ReconnectInitial
	for {
		failed := c.replayIntended(s, only)
		if len(failed) == 0 {
			slog.Info("client: replay complete")
			return
		}
		slog.Warn("client: replay incomplete; retrying", "failed", failed, "backoff", backoff)
		select {
		case <-c.closed:
			return
		case <-s.yamux.CloseChan():
			return
		case <-time.After(jitter(backoff)):
		}
		backoff *= 2
		if backoff > c.opts.ReconnectMax {
			backoff = c.opts.ReconnectMax
		}
		only = make(map[string]bool, len(failed))
		for _, n := range failed {
			only[n] = true
		}
	}
}

// replayIntended registers each intended name on s (all of them when only is
// nil, else just the names in only), returning the ones that failed. Each
// register runs under registerMu — per name, not across the loop, so a
// user-initiated Register isn't blocked behind the whole replay — which keeps
// the session's single pending-reply slot serialized.
func (c *Client) replayIntended(s *session, only map[string]bool) []string {
	c.mu.Lock()
	intended := make(map[string]int, len(c.intended))
	for k, v := range c.intended {
		intended[k] = v
	}
	c.mu.Unlock()

	var failed []string
	for name, port := range intended {
		if only != nil && !only[name] {
			continue
		}
		c.registerMu.Lock()
		// Re-check under the lock: the user may have unregistered (or repointed)
		// the name while we were working through the list.
		c.mu.Lock()
		curPort, still := c.intended[name]
		c.mu.Unlock()
		if !still || curPort != port {
			c.registerMu.Unlock()
			continue
		}
		_, err := c.doRegisterOnSession(s, name, port)
		c.registerMu.Unlock()
		if err != nil {
			slog.Warn("client: replay register failed", "name", name, "err", err.Error())
			failed = append(failed, name)
		}
	}
	return failed
}

func (c *Client) readControl(s *session) {
	// A control-plane failure — the stream dying OR one torn/unparseable line —
	// must kill the whole session. yamux keepalives would otherwise keep the
	// data plane "alive" with nobody reading replies: every register times out
	// forever, and the only cure is a manual restart. Closing the session
	// makes CloseChan fire, so manage() reconnects and replays.
	defer func() { _ = s.yamux.Close() }()
	for {
		typ, line, err := proto.Read(s.br)
		if err != nil {
			if err != io.EOF && !errors.Is(err, net.ErrClosed) && !errors.Is(err, yamux.ErrSessionShutdown) {
				slog.Warn("client: control read failed; dropping session to reconnect", "err", err.Error())
			}
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
			if pending != nil && pending.name == msg.Name {
				pending.ch <- controlReply{registered: &msg}
			} else {
				// Nobody is waiting for this name — a late reply from a register
				// that already timed out. Delivering it would hand the wrong URL
				// to whichever register is in flight now.
				slog.Warn("client: unsolicited registered reply", "name", msg.Name)
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
			// A register-scoped error carries the register's Name; drop it if it
			// names a DIFFERENT register than the one in flight (a late error
			// from an already-timed-out register would otherwise fail the next
			// register and orphan a live tunnel). An empty Name is
			// connection-scoped (or from an older edge) → deliver as before.
			switch {
			case pending == nil:
				slog.Warn("client: unsolicited error", "code", msg.Code, "name", msg.Name, "msg", msg.Message)
			case msg.Name != "" && msg.Name != pending.name:
				slog.Warn("client: error for a different register, dropping", "err_name", msg.Name, "pending", pending.name, "code", msg.Code)
			default:
				pending.ch <- controlReply{err: &msg}
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
				// Can't write heartbeats → the edge will drop us at its timeout
				// anyway; close now so manage() reconnects immediately.
				_ = s.yamux.Close()
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
