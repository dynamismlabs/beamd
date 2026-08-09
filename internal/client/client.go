// Package client implements the beam client side.
//
// M5: a long-lived client that maintains a multiplexed transport session to
// the edge across network blips. `Connect` opens the first session
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

	"github.com/dynamismlabs/beamd/internal/naming"
	"github.com/dynamismlabs/beamd/internal/proto"
	"github.com/dynamismlabs/beamd/internal/tunnel"
)

const ALPNBeam = "beam/1"

var errSessionUnavailable = errors.New("transport session unavailable")

const (
	DefaultHeartbeatInterval = 20 * time.Second
	DefaultRegisterTimeout   = 5 * time.Second
	DefaultReconnectInitial  = 500 * time.Millisecond
	DefaultReconnectMax      = 30 * time.Second
	DefaultTransport         = "tcp"

	autoQUICCandidateTimeout = 3 * time.Second
	candidateTimeout         = 5 * time.Second
	candidateCleanupTimeout  = time.Second
	quicReprobeInterval      = 10 * time.Minute
	backendDialTimeout       = 5 * time.Second
	maxStreamHandlers        = 64
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

	// YamuxStreamWindowBytes sets the agent's yamux per-stream receive window,
	// passed to the TCP adapter. 0 → the adapter's 4 MiB default. The CLI populates it from
	// config.ResolveYamuxWindow (the process-wide BEAMD_YAMUX_STREAM_WINDOW_BYTES,
	// already defaulted and range-validated). See transport-performance-spec §8.1.
	YamuxStreamWindowBytes int64

	// Transport selects tcp, quic, or auto. Direct library callers retain the
	// conservative tcp default; the CLI resolves hosted session credentials to
	// auto before constructing Options.
	Transport string
}

func (o *Options) applyDefaults() error {
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
	if o.Transport == "" {
		o.Transport = DefaultTransport
	}
	switch o.Transport {
	case "tcp", "quic", "auto":
	default:
		return fmt.Errorf("transport %q is not supported (use tcp, quic, or auto)", o.Transport)
	}
	return nil
}

// session bundles everything tied to a single transport session lifetime.
// On reconnect, a fresh session replaces this one atomically.
type session struct {
	transport tunnel.Session
	control   tunnel.Stream
	br        *bufio.Reader
	writeMu   sync.Mutex

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
	if err := proto.Write(s.control, msg); err != nil {
		_ = s.transport.CloseWithError(tunnel.CloseProtocol, "control write failed")
		return err
	}
	return nil
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

	handlerSlots   chan struct{}
	handlerRejects atomic.Uint64

	fallbackCount  atomic.Uint64
	reconnectCount atomic.Uint64
	lastFallback   atomic.Value // string
	lastClose      atomic.Value // string

	// Selection history is protected by mu. lastSuccessful is the transport
	// that most recently completed hello_ok. tcpFallbackAt suppresses repeated
	// QUIC probes for ten minutes after an automatic fallback.
	lastSuccessful tunnel.Kind
	tcpFallbackAt  time.Time

	quicDialer quicCandidateDialer
}

type controlReply struct {
	registered *proto.Registered
	err        *proto.Error
}

type quicCandidateDialer interface {
	Resolve(context.Context, string) (string, []string, error)
	DialResolved(context.Context, string, string, *tls.Config) (tunnel.Session, error)
}

// Connect opens the first transport session and completes the protocol
// handshake. Returns once the session is live; thereafter, a background
// goroutine maintains reconnect-with-replay across session losses.
func Connect(ctx context.Context, serverAddr, token string, opts ...Options) (*Client, error) {
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	if err := o.applyDefaults(); err != nil {
		return nil, err
	}

	c := &Client{
		serverAddr:   serverAddr,
		token:        token,
		opts:         o,
		intended:     make(map[string]int),
		closed:       make(chan struct{}),
		handlerSlots: make(chan struct{}, maxStreamHandlers),
		quicDialer:   tunnel.NewQUICDialer(),
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
	return c.sess != nil && !c.sess.transport.IsClosed()
}

// Transport reports the currently connected transport. It is empty while the
// client is disconnected.
func (c *Client) Transport() tunnel.Kind {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sess == nil || c.sess.transport.IsClosed() {
		return ""
	}
	return c.sess.transport.Kind()
}

// Diagnostics is the process-local transport state surfaced through the
// detached agent's health endpoint.
type Diagnostics struct {
	ConfiguredTransport string
	FallbackCount       uint64
	LastFallbackReason  string
	ReconnectCount      uint64
	LastCloseReason     string
}

func (c *Client) Diagnostics() Diagnostics {
	return Diagnostics{
		ConfiguredTransport: c.opts.Transport,
		FallbackCount:       c.fallbackCount.Load(),
		LastFallbackReason:  atomicString(&c.lastFallback),
		ReconnectCount:      c.reconnectCount.Load(),
		LastCloseReason:     atomicString(&c.lastClose),
	}
}

func atomicString(v *atomic.Value) string {
	if value := v.Load(); value != nil {
		if s, ok := value.(string); ok {
			return s
		}
	}
	return ""
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
			err = s.transport.CloseWithError(tunnel.CloseNormal, "client closed")
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
		if s != nil && !sessionUnavailable(s) {
			url, err := c.doRegisterOnSessionUntil(s, name, port, deadline)
			if !errors.Is(err, errSessionUnavailable) {
				return url, err
			}
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return "", fmt.Errorf("register %q: no active session within %s", name, c.opts.RegisterTimeout)
		}
		wait := 50 * time.Millisecond
		if remaining < wait {
			wait = remaining
		}
		timer := time.NewTimer(wait)
		select {
		case <-c.closed:
			timer.Stop()
			return "", fmt.Errorf("client closed")
		case <-timer.C:
		}
	}
}

func (c *Client) doRegisterOnSession(s *session, name string, port int) (string, error) {
	return c.doRegisterOnSessionUntil(
		s,
		name,
		port,
		time.Now().Add(c.opts.RegisterTimeout),
	)
}

func (c *Client) doRegisterOnSessionUntil(
	s *session,
	name string,
	port int,
	deadline time.Time,
) (string, error) {
	if sessionUnavailable(s) {
		return "", errSessionUnavailable
	}
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
		if sessionUnavailable(s) {
			return "", errSessionUnavailable
		}
		return "", fmt.Errorf("send register: %w", err)
	}

	remaining := time.Until(deadline)
	if remaining <= 0 {
		return "", fmt.Errorf("register %q: timeout after %s", name, c.opts.RegisterTimeout)
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
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
	case <-s.transport.Done():
		// Prefer a reply that became ready at the same boundary. Explicit
		// register errors are terminal and must not be retried on a replacement
		// session.
		select {
		case r := <-pending.ch:
			if r.err != nil {
				return "", fmt.Errorf("%s: %s", r.err.Code, r.err.Message)
			}
			if r.registered != nil {
				return r.registered.URL, nil
			}
		default:
		}
		return "", errSessionUnavailable
	case <-timer.C:
		return "", fmt.Errorf("register %q: timeout after %s", name, c.opts.RegisterTimeout)
	}
}

func sessionUnavailable(s *session) bool {
	if s == nil || s.transport == nil || s.transport.IsClosed() {
		return true
	}
	select {
	case <-s.transport.Done():
		return true
	default:
		return false
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

// candidateError carries the fixed fallback classification without flattening
// protocol or TLS errors into strings.
type candidateError struct {
	reason   string
	eligible bool
	err      error
}

func (e *candidateError) Error() string { return e.err.Error() }
func (e *candidateError) Unwrap() error { return e.err }

// connectOnce performs one ordered transport-selection cycle and installs the
// first candidate that completes hello_ok. Candidates never race.
func (c *Client) connectOnce(ctx context.Context, first bool) error {
	order := c.candidateOrder(first)
	var lastErr error
	for i, kind := range order {
		timeout := candidateTimeout
		if c.opts.Transport == "auto" && kind == tunnel.KindQUIC {
			timeout = autoQUICCandidateTimeout
		}
		candidateCtx, cancel := context.WithTimeout(ctx, timeout)
		s, hok, err := c.connectCandidate(candidateCtx, kind)
		if err == nil {
			err = rejectCanceledSuccessfulCandidate(ctx, candidateCtx, s)
		}
		cancel()
		if err == nil {
			if installErr := c.installSession(s, hok, first); installErr != nil {
				if cleanupErr := cleanupCandidate(s); cleanupErr != nil {
					return errors.Join(installErr, cleanupErr)
				}
				return installErr
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		lastErr = err

		var classified *candidateError
		canTryNext := errors.As(err, &classified) && classified.eligible && i+1 < len(order)
		if !canTryNext {
			return err
		}
		if kind == tunnel.KindQUIC && order[i+1] == tunnel.KindYamux {
			c.fallbackCount.Add(1)
			c.lastFallback.Store(classified.reason)
			c.mu.Lock()
			c.tcpFallbackAt = time.Now()
			c.mu.Unlock()
			slog.Warn("transport fallback",
				"event", "transport_fallback",
				"from", tunnel.KindQUIC,
				"to", tunnel.KindYamux,
				"reason", classified.reason,
			)
		}
	}
	return lastErr
}

// rejectCanceledSuccessfulCandidate closes a candidate that completed hello_ok
// concurrently with cancellation. The outer caller remains authoritative. An
// adapter-owned candidate deadline is a timeout availability failure and may
// fall back only after cleanup has fully joined Session.Done.
func rejectCanceledSuccessfulCandidate(ctx, candidateCtx context.Context, s *session) error {
	if err := ctx.Err(); err != nil {
		if cleanupErr := cleanupCandidate(s); cleanupErr != nil {
			return errors.Join(err, cleanupErr)
		}
		return err
	}
	if err := candidateCtx.Err(); err != nil {
		return failCandidate(s, classifyAvailabilityFailure(candidateCtx, err))
	}
	return nil
}

func (c *Client) candidateOrder(first bool) []tunnel.Kind {
	switch c.opts.Transport {
	case "quic":
		return []tunnel.Kind{tunnel.KindQUIC}
	case "tcp":
		return []tunnel.Kind{tunnel.KindYamux}
	}

	c.mu.Lock()
	last := c.lastSuccessful
	fallbackAt := c.tcpFallbackAt
	c.mu.Unlock()

	if first || last == "" || last == tunnel.KindQUIC {
		return []tunnel.Kind{tunnel.KindQUIC, tunnel.KindYamux}
	}
	if !fallbackAt.IsZero() && time.Since(fallbackAt) < quicReprobeInterval {
		// Keep the known-good TCP path first during the cooldown, but retain
		// QUIC as the second candidate so an unavailable TCP path doesn't
		// strand an auto-mode agent until the re-probe timer expires.
		return []tunnel.Kind{tunnel.KindYamux, tunnel.KindQUIC}
	}
	return []tunnel.Kind{tunnel.KindQUIC, tunnel.KindYamux}
}

func (c *Client) connectCandidate(ctx context.Context, kind tunnel.Kind) (*session, proto.HelloOK, error) {
	if kind == tunnel.KindQUIC {
		return c.connectQUICCandidate(ctx)
	}
	transport, err := c.dialTransport(ctx, kind)
	if err != nil {
		return nil, proto.HelloOK{}, classifyDialFailure(ctx, kind, err)
	}
	return c.negotiateCandidate(ctx, transport)
}

func (c *Client) connectQUICCandidate(ctx context.Context) (*session, proto.HelloOK, error) {
	serverName, addresses, err := c.quicDialer.Resolve(ctx, c.serverAddr)
	if err != nil {
		return nil, proto.HelloOK{}, classifyDialFailure(ctx, tunnel.KindQUIC, err)
	}
	tlsConfig := &tls.Config{
		InsecureSkipVerify: c.opts.InsecureSkipVerify, //nolint:gosec // opt-in local development
		MinVersion:         tls.VersionTLS13,
		NextProtos:         []string{tunnel.ALPNQUIC},
	}

	var lastErr error
	for _, address := range addresses {
		transport, dialErr := c.quicDialer.DialResolved(ctx, address, serverName, tlsConfig)
		if dialErr != nil {
			lastErr = classifyDialFailure(ctx, tunnel.KindQUIC, dialErr)
		} else {
			candidate, hello, candidateErr := c.negotiateCandidate(ctx, transport)
			if candidateErr == nil {
				return candidate, hello, nil
			}
			lastErr = candidateErr
		}
		if !retryResolvedQUICAddress(ctx, lastErr) {
			return nil, proto.HelloOK{}, lastErr
		}
	}
	if lastErr != nil {
		return nil, proto.HelloOK{}, lastErr
	}
	return nil, proto.HelloOK{}, &candidateError{
		reason:   "network",
		eligible: true,
		err:      errors.New("QUIC resolver returned no addresses"),
	}
}

func retryResolvedQUICAddress(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	var classified *candidateError
	return errors.As(err, &classified) &&
		classified.eligible &&
		classified.reason == "network"
}

func (c *Client) negotiateCandidate(
	ctx context.Context,
	transport tunnel.Session,
) (*session, proto.HelloOK, error) {
	candidate := &session{transport: transport}

	control, err := transport.OpenStream(ctx)
	if err != nil {
		failure := classifyAvailabilityFailure(ctx, fmt.Errorf("open control stream: %w", err))
		return nil, proto.HelloOK{}, failCandidate(candidate, failure)
	}
	candidate.control = control
	if deadline, ok := ctx.Deadline(); ok {
		_ = control.SetDeadline(deadline)
	}

	if err := proto.Write(control, &proto.Hello{
		Type: proto.TypeHello, Token: c.token, Scope: c.opts.Scope, ProtoVersion: proto.ProtoVersion,
	}); err != nil {
		failure := classifyAvailabilityFailure(ctx, fmt.Errorf("send hello: %w", err))
		return nil, proto.HelloOK{}, failCandidate(candidate, failure)
	}

	br := bufio.NewReader(control)
	typ, line, err := proto.Read(br)
	if err != nil {
		if rejection := closedHelloRejection(ctx, transport); rejection != nil {
			return nil, proto.HelloOK{}, failCandidate(candidate, rejection)
		}
		failure := classifyAvailabilityFailure(ctx, fmt.Errorf("read hello reply: %w", err))
		return nil, proto.HelloOK{}, failCandidate(candidate, failure)
	}
	if typ == proto.TypeError {
		var msg proto.Error
		if unmarshalErr := json.Unmarshal(line, &msg); unmarshalErr != nil {
			_ = transport.CloseWithError(tunnel.CloseProtocol, "malformed hello error")
			failure := &candidateError{err: fmt.Errorf("parse hello error: %w", unmarshalErr)}
			return nil, proto.HelloOK{}, failCandidate(candidate, failure)
		}
		code := tunnel.CloseProtocol
		if msg.Code == proto.CodeBadToken {
			code = tunnel.CloseAuth
		}
		_ = transport.CloseWithError(code, msg.Code)
		failure := &candidateError{
			err: fmt.Errorf("server rejected hello: %s — %s", msg.Code, msg.Message),
		}
		return nil, proto.HelloOK{}, failCandidate(candidate, failure)
	}
	if typ != proto.TypeHelloOK {
		_ = transport.CloseWithError(tunnel.CloseProtocol, "unexpected hello reply")
		failure := &candidateError{err: fmt.Errorf("expected hello_ok, got %s", typ)}
		return nil, proto.HelloOK{}, failCandidate(candidate, failure)
	}

	var hok proto.HelloOK
	if err := json.Unmarshal(line, &hok); err != nil {
		_ = transport.CloseWithError(tunnel.CloseProtocol, "malformed hello_ok")
		failure := &candidateError{err: fmt.Errorf("parse hello_ok: %w", err)}
		return nil, proto.HelloOK{}, failCandidate(candidate, failure)
	}
	if hok.ProtoVersion != proto.ProtoVersion {
		_ = transport.CloseWithError(tunnel.CloseProtocol, "protocol version mismatch")
		failure := &candidateError{
			err: fmt.Errorf("server protocol version %d does not match client version %d", hok.ProtoVersion, proto.ProtoVersion),
		}
		return nil, proto.HelloOK{}, failCandidate(candidate, failure)
	}
	_ = control.SetDeadline(time.Time{})
	candidate.br = br
	return candidate, hok, nil
}

// closedHelloRejection preserves the authoritative QUIC close classification
// when CONNECTION_CLOSE races the error stream. QUIC guarantees neither
// packet ordering nor retransmission after the peer closes the whole
// connection, so the close result must win when the NDJSON error doesn't
// arrive.
func closedHelloRejection(ctx context.Context, transport tunnel.Session) *candidateError {
	info := transport.CloseInfo()
	if !info.CodeValid && transport.IsClosed() {
		// The raw QUIC context becomes terminal just before the adapter's
		// lifecycle goroutine publishes CloseInfo and closes Done.
		select {
		case <-transport.Done():
		case <-ctx.Done():
			return nil
		}
		info = transport.CloseInfo()
	}
	if !transport.IsClosed() && !info.CodeValid {
		return nil
	}
	return helloFailureFromCloseInfo(info)
}

func helloRejectionFromCloseInfo(info tunnel.CloseInfo) *candidateError {
	if !info.CodeValid || !info.Remote {
		return nil
	}

	var code string
	switch info.Code {
	case tunnel.CloseAuth:
		code = proto.CodeBadToken
	case tunnel.CloseCapacity:
		code = proto.CodeOverLimit
	case tunnel.CloseProtocol:
		code = proto.CodeBadHello
		if strings.Contains(strings.ToLower(info.Reason), "version") {
			code = proto.CodeBadVersion
		}
	case tunnel.CloseShutdown:
		code = proto.CodeShutdown
	default:
		// Every authenticated QUIC application close is terminal, even when
		// a future peer uses a code this client doesn't recognize. Falling
		// back would risk authenticating a second session after an explicit
		// server rejection.
		code = fmt.Sprintf("application_close_0x%x", uint64(info.Code))
	}
	message := info.Reason
	if message == "" {
		message = "connection rejected"
	}
	return &candidateError{
		err: fmt.Errorf("server rejected hello: %s — %s", code, message),
	}
}

func helloFailureFromCloseInfo(info tunnel.CloseInfo) *candidateError {
	if info.CodeValid {
		return helloRejectionFromCloseInfo(info)
	}

	reason := tunnel.CloseReason(info)
	var err error
	if info.Cause != nil {
		err = fmt.Errorf("QUIC session closed during hello (%s): %w", reason, info.Cause)
	} else {
		err = fmt.Errorf("QUIC session closed during hello (%s)", reason)
	}
	switch reason {
	case "network":
		return &candidateError{reason: "network", eligible: true, err: err}
	case "idle":
		return &candidateError{reason: "timeout", eligible: true, err: err}
	default:
		// Protocol violations, version failures, and unknown transport
		// failures are terminal. Retrying them over TCP could bypass an
		// explicit peer rejection.
		return &candidateError{err: err}
	}
}

func (c *Client) dialTransport(ctx context.Context, kind tunnel.Kind) (tunnel.Session, error) {
	switch kind {
	case tunnel.KindYamux:
		dialer := tls.Dialer{Config: &tls.Config{
			InsecureSkipVerify: c.opts.InsecureSkipVerify, //nolint:gosec // opt-in local development
			NextProtos:         []string{ALPNBeam},
		}}
		rawConn, err := dialer.DialContext(ctx, "tcp", c.serverAddr)
		if err != nil {
			return nil, fmt.Errorf("dial TCP %s: %w", c.serverAddr, err)
		}
		conn := rawConn.(*tls.Conn)
		if got := conn.ConnectionState().NegotiatedProtocol; got != ALPNBeam {
			_ = conn.Close()
			return nil, fmt.Errorf("server did not negotiate %q (got %q)", ALPNBeam, got)
		}
		sess, err := tunnel.NewYamuxClient(conn, uint32(c.opts.YamuxStreamWindowBytes))
		if err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("yamux client: %w", err)
		}
		return sess, nil
	default:
		return nil, fmt.Errorf("unsupported transport kind %q", kind)
	}
}

func classifyDialFailure(ctx context.Context, kind tunnel.Kind, err error) error {
	if kind == tunnel.KindQUIC {
		if failure, ok := tunnel.AsDialFailure(err); ok {
			return &candidateError{reason: string(failure.Category), eligible: failure.FallbackEligible(), err: err}
		}
	}
	return classifyAvailabilityFailure(ctx, err)
}

func classifyAvailabilityFailure(ctx context.Context, err error) error {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded), errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, tunnel.ErrOpenTimeout):
		return &candidateError{reason: "timeout", eligible: true, err: err}
	case errors.Is(ctx.Err(), context.Canceled), errors.Is(err, context.Canceled):
		return &candidateError{err: err}
	case errors.Is(err, tunnel.ErrSessionClosed),
		errors.Is(err, net.ErrClosed),
		errors.Is(err, io.EOF),
		errors.Is(err, io.ErrUnexpectedEOF):
		return &candidateError{reason: "network", eligible: true, err: err}
	default:
		var netErr net.Error
		if errors.As(err, &netErr) {
			return &candidateError{reason: "network", eligible: true, err: err}
		}
		return &candidateError{err: err}
	}
}

func cleanupCandidate(s *session) error {
	if s == nil || s.transport == nil {
		return nil
	}
	if s.control != nil {
		s.control.Abort(tunnel.StreamCanceled)
	}
	_ = s.transport.CloseWithError(tunnel.CloseNormal, "candidate cleanup")
	timer := time.NewTimer(candidateCleanupTimeout)
	defer timer.Stop()
	select {
	case <-s.transport.Done():
		return nil
	case <-timer.C:
		return fmt.Errorf("transport candidate cleanup exceeded %s", candidateCleanupTimeout)
	}
}

func failCandidate(s *session, failure error) error {
	if cleanupErr := cleanupCandidate(s); cleanupErr != nil {
		// Cleanup failure is terminal. Starting a fallback candidate while the
		// first candidate can still authenticate would violate single-session
		// ownership.
		return &candidateError{
			err: errors.Join(failure, cleanupErr),
		}
	}
	// Cleanup joins Session.Done, which is the point at which a close racing
	// the candidate deadline becomes authoritative. Never retain an eligible
	// timeout/network classification if the final QUIC close is terminal.
	var classified *candidateError
	if errors.As(failure, &classified) && classified.eligible &&
		s != nil && s.transport != nil {
		if authoritative := helloFailureFromCloseInfo(s.transport.CloseInfo()); authoritative != nil {
			return authoritative
		}
	}
	return failure
}

func (c *Client) installSession(s *session, hok proto.HelloOK, first bool) error {
	c.mu.Lock()
	select {
	case <-c.closed:
		c.mu.Unlock()
		return fmt.Errorf("client closed")
	default:
	}
	if first {
		c.slug = hok.Slug
		c.baseDomain = hok.BaseDomain
		c.shape = hok.Shape
	} else if hok.Slug != c.slug {
		c.mu.Unlock()
		return fmt.Errorf("slug changed across reconnect: %s → %s", c.slug, hok.Slug)
	}
	c.sess = s
	c.lastSuccessful = s.transport.Kind()
	if s.transport.Kind() == tunnel.KindQUIC {
		c.tcpFallbackAt = time.Time{}
	}
	c.mu.Unlock()

	go c.readControl(s)
	go c.heartbeatLoop(s)
	go c.acceptStreamsLoop(s)
	slog.Info("client connected",
		"event", "session_connected",
		"transport", s.transport.Kind(),
		"slug", hok.Slug,
		"first", first,
	)
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
		case <-s.transport.Done():
		}

		c.mu.Lock()
		if c.sess == s {
			c.sess = nil
		}
		c.mu.Unlock()
		closeReason := tunnel.CloseReason(s.transport.CloseInfo())
		c.lastClose.Store(closeReason)
		slog.Warn("client: session closed, reconnecting",
			"event", "session_closed",
			"transport", s.transport.Kind(),
			"close_reason", closeReason,
		)

		backoff := c.opts.ReconnectInitial
		// `error{code:"shutdown"}` from the server pre-sets skipBackoff.
		// QUIC may deliver the authoritative CONNECTION_CLOSE without the
		// preceding stream record, so CloseInfo independently triggers the
		// same immediate reconnect.
		skipFirstWait := c.skipBackoff.Swap(false) || closeReason == "shutdown"
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

			c.reconnectCount.Add(1)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err := c.connectOnce(ctx, false)
			cancel()
			if err == nil {
				// Replay in the background so manage() goes straight back to
				// watching Session.Done — a wedged replay must never stop us from
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
		case <-s.transport.Done():
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
	// makes Session.Done fire, so manage() reconnects and replays.
	terminalCode := tunnel.CloseProtocol
	terminalReason := "control stream ended"
	defer func() {
		_ = s.transport.CloseWithError(terminalCode, terminalReason)
		_ = s.control.CloseWrite()
	}()
	for {
		typ, line, err := proto.Read(s.br)
		if err != nil {
			if err != io.EOF && !errors.Is(err, net.ErrClosed) && !errors.Is(err, tunnel.ErrSessionClosed) {
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
				terminalCode = tunnel.CloseShutdown
				terminalReason = msg.Message
				if terminalReason == "" {
					terminalReason = "edge shutting down"
				}
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
		case <-s.transport.Done():
			return
		case <-tick.C:
			if err := s.write(&proto.Heartbeat{Type: proto.TypeHeartbeat}); err != nil {
				// Can't write heartbeats → the edge will drop us at its timeout
				// anyway; close now so manage() reconnects immediately.
				_ = s.transport.CloseWithError(tunnel.CloseProtocol, "heartbeat write failed")
				return
			}
		}
	}
}

func (c *Client) acceptStreamsLoop(s *session) {
	for {
		stream, err := s.transport.AcceptStream(context.Background())
		if err != nil {
			if errors.Is(err, tunnel.ErrSessionClosed) || errors.Is(err, io.EOF) {
				return
			}
			slog.Debug("client: accept stream", "err", err.Error())
			return
		}
		select {
		case c.handlerSlots <- struct{}{}:
			go func() {
				defer func() { <-c.handlerSlots }()
				c.handleStream(s, stream)
			}()
		default:
			rejectionCount := c.handlerRejects.Add(1)
			slog.Warn("client: rejecting stream over handler capacity",
				"event", "stream_rejected",
				"transport", s.transport.Kind(),
				"capacity", maxStreamHandlers,
				"reason", "capacity",
				"rejection_count", rejectionCount,
			)
			stream.Abort(tunnel.StreamCapacity)
		}
	}
}

func (c *Client) handleStream(s *session, stream tunnel.Stream) {
	_ = stream.SetReadDeadline(time.Now().Add(tunnel.PrefixSetupTimeout(s.transport.Kind())))
	br := bufio.NewReaderSize(stream, 64)
	line, err := br.ReadSlice('\n')
	if err != nil || len(line) == 0 || len(line) > 64 {
		slog.Warn("stream: invalid name prefix", "err", err)
		stream.Abort(tunnel.StreamCanceled)
		return
	}
	_ = stream.SetReadDeadline(time.Time{})
	name := strings.TrimSuffix(string(line), "\n")
	if err := naming.ValidateLabel(name); err != nil {
		slog.Warn("stream: invalid backend name", "name", name, "err", err.Error())
		stream.Abort(tunnel.StreamCanceled)
		return
	}

	c.mu.Lock()
	port, ok := c.intended[name]
	c.mu.Unlock()
	if !ok {
		slog.Warn("stream: no backend for name", "name", name)
		stream.Abort(tunnel.StreamCanceled)
		return
	}

	var dialer net.Dialer
	backend, err := dialLocalBackend(backendDialTimeout, port, dialer.DialContext)
	if err != nil {
		slog.Warn("stream: dial backend", "name", name, "err", err.Error())
		stream.Abort(tunnel.StreamCanceled)
		return
	}
	defer func() { _ = backend.Close() }()

	type copyResult struct {
		direction string
		err       error
	}
	results := make(chan copyResult, 2)
	go func() {
		_, copyErr := io.Copy(backend, br)
		if copyErr == nil {
			if closer, ok := backend.(interface{ CloseWrite() error }); ok {
				copyErr = closer.CloseWrite()
			}
		}
		results <- copyResult{direction: "request", err: copyErr}
	}()
	go func() {
		_, copyErr := io.Copy(stream, backend)
		if copyErr == nil {
			copyErr = stream.CloseWrite()
		}
		results <- copyResult{direction: "response", err: copyErr}
	}()

	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-finished:
		case <-c.closed:
			stream.Abort(tunnel.StreamCanceled)
			_ = backend.Close()
		case <-s.transport.Done():
			stream.Abort(tunnel.StreamCanceled)
			_ = backend.Close()
		}
	}()

	first := <-results
	if first.err != nil {
		if !errors.Is(first.err, net.ErrClosed) && !errors.Is(first.err, tunnel.ErrSessionClosed) {
			slog.Debug("stream copy failed", "direction", first.direction, "err", first.err.Error())
		}
		stream.Abort(tunnel.StreamCanceled)
		_ = backend.Close()
	}
	// A backend may intentionally answer before consuming the full request
	// body. Once the complete response has been half-closed onto the tunnel,
	// close the backend socket so its request writer wakes. Do not cancel the
	// tunnel receive side yet: the edge must be allowed to parse the reliable
	// response FIN before it stops forwarding the public request body.
	earlyResponse := first.direction == "response" && first.err == nil
	if earlyResponse {
		_ = backend.Close()
	}
	second := <-results
	if second.err != nil && !earlyResponse {
		if !errors.Is(second.err, net.ErrClosed) && !errors.Is(second.err, tunnel.ErrSessionClosed) {
			slog.Debug("stream copy failed", "direction", second.direction, "err", second.err.Error())
		}
		stream.Abort(tunnel.StreamCanceled)
		_ = backend.Close()
	}
	if earlyResponse {
		if second.err == nil {
			// The request also completed normally, so both halves are terminal.
			_ = stream.Close()
			return
		}
		// The backend close intentionally ended the request copy. Drain the
		// tunnel receive side until the edge observes the response and closes
		// its request half; this avoids resetting queued response bytes.
		_, _ = io.Copy(io.Discard, br)
		return
	}
	if first.err == nil && second.err == nil {
		_ = stream.Close()
	}
}

func dialLocalBackend(
	timeout time.Duration,
	port int,
	dialContext func(context.Context, string, string) (net.Conn, error),
) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return dialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", port))
}

func jitter(d time.Duration) time.Duration {
	// ±25% jitter so reconnect storms don't synchronize.
	delta := time.Duration(rand.Int63n(int64(d / 2)))
	return d - d/4 + delta
}
