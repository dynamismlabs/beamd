package tunnel

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
)

const (
	ALPNQUIC              = "beamd-quic/1"
	QUICALPN              = ALPNQUIC // compatibility alias for early transport callers
	quicStreamOpenTimeout = 5 * time.Second
)

var errNoResolvedAddresses = errors.New("resolver returned no addresses")

func commonQUICConfig() *quic.Config {
	return &quic.Config{
		HandshakeIdleTimeout:           10 * time.Second,
		MaxIdleTimeout:                 75 * time.Second,
		KeepAlivePeriod:                0,
		InitialStreamReceiveWindow:     4 << 20,
		MaxStreamReceiveWindow:         16 << 20,
		InitialConnectionReceiveWindow: 16 << 20,
		MaxConnectionReceiveWindow:     64 << 20,
		MaxIncomingUniStreams:          -1,
		EnableDatagrams:                false,
		DisablePathMTUDiscovery:        false,
	}
}

func ClientQUICConfig() *quic.Config {
	cfg := commonQUICConfig()
	cfg.MaxIncomingStreams = 64
	return cfg
}

func ServerQUICConfig() *quic.Config {
	cfg := commonQUICConfig()
	cfg.MaxIncomingStreams = 1
	cfg.Allow0RTT = false
	return cfg
}

func ClientQUICTLSConfig(base *tls.Config, serverName string) *tls.Config {
	if base == nil {
		base = &tls.Config{}
	}
	cfg := base.Clone()
	cfg.MinVersion = tls.VersionTLS13
	cfg.ServerName = serverName
	cfg.NextProtos = []string{ALPNQUIC}
	return cfg
}

func ServerQUICTLSConfig(base *tls.Config) *tls.Config {
	if base == nil {
		base = &tls.Config{}
	}
	cfg := base.Clone()
	cfg.MinVersion = tls.VersionTLS13
	cfg.NextProtos = []string{ALPNQUIC}
	return cfg
}

type QUICSession struct {
	raw         quicSessionConn
	state       *sessionState
	openTimeout time.Duration
}

func NewQUICSession(conn *quic.Conn) *QUICSession {
	return newQUICSession(quicConnAdapter{Conn: conn})
}

type quicSessionConn interface {
	OpenStreamSync(context.Context) (quicBidiStream, error)
	AcceptStream(context.Context) (quicBidiStream, error)
	Context() context.Context
	CloseWithError(quic.ApplicationErrorCode, string) error
	LocalAddr() net.Addr
	RemoteAddr() net.Addr
}

// quicConnAdapter narrows quic-go's concrete stream return values to the
// transport contract. Keeping this boundary explicit also lets the contract
// tests deterministically exercise blocked opens without a live QUIC peer.
type quicConnAdapter struct {
	*quic.Conn
}

func (c quicConnAdapter) OpenStreamSync(ctx context.Context) (quicBidiStream, error) {
	return c.Conn.OpenStreamSync(ctx)
}

func (c quicConnAdapter) AcceptStream(ctx context.Context) (quicBidiStream, error) {
	return c.Conn.AcceptStream(ctx)
}

func newQUICSession(conn quicSessionConn) *QUICSession {
	s := &QUICSession{
		raw:         conn,
		state:       newSessionState(),
		openTimeout: quicStreamOpenTimeout,
	}
	go s.watch()
	return s
}

func (s *QUICSession) Kind() Kind { return KindQUIC }

func (s *QUICSession) OpenStream(ctx context.Context) (Stream, error) {
	timeout := s.openTimeout
	if timeout <= 0 {
		timeout = quicStreamOpenTimeout
	}
	openCtx, cancel := context.WithTimeoutCause(ctx, timeout, ErrOpenTimeout)
	defer cancel()

	raw, err := s.raw.OpenStreamSync(openCtx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(context.Cause(openCtx), ErrOpenTimeout) {
			return nil, ErrOpenTimeout
		}
		return nil, s.normalizeSessionError(err)
	}
	if openCtx.Err() != nil {
		raw.CancelWrite(quic.StreamErrorCode(StreamCanceled))
		raw.CancelRead(quic.StreamErrorCode(StreamCanceled))
		return nil, derivedOpenError(ctx, openCtx)
	}
	return s.registerQUICStream(raw)
}

func (s *QUICSession) AcceptStream(ctx context.Context) (Stream, error) {
	raw, err := s.raw.AcceptStream(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, s.normalizeSessionError(err)
	}
	return s.registerQUICStream(raw)
}

func (s *QUICSession) registerQUICStream(raw quicBidiStream) (Stream, error) {
	stream := newQUICStream(raw, s)
	if s.state.register(stream) {
		return stream, nil
	}
	stream.Abort(StreamCanceled)
	return nil, ErrSessionClosed
}

func (s *QUICSession) Done() <-chan struct{} { return s.state.doneChan() }

func (s *QUICSession) IsClosed() bool {
	return s.state.isClosed() || (s.raw != nil && s.raw.Context().Err() != nil)
}

func (s *QUICSession) CloseInfo() CloseInfo { return s.state.closeInfo() }

func (s *QUICSession) CloseWithError(code ErrorCode, reason string) error {
	info := CloseInfo{
		Code:      code,
		CodeValid: true,
		Reason:    reason,
	}
	// The raw connection context can become terminal before watch publishes
	// CloseInfo. Preserve that already-observable remote/network event rather
	// than replacing it with a later local cleanup close.
	if cause := context.Cause(s.raw.Context()); cause != nil {
		s.state.finish(closeInfoFromQUIC(cause))
		return nil
	}
	// Only the first local terminal claimant may send CONNECTION_CLOSE.
	// Otherwise concurrent callers could expose one code locally while the
	// peer receives a different loser's code.
	if !s.state.claim(info) {
		return nil
	}
	err := s.raw.CloseWithError(quic.ApplicationErrorCode(code), sanitizeReason(reason))
	s.state.finish(info)
	return err
}

func (s *QUICSession) LocalAddr() net.Addr  { return s.raw.LocalAddr() }
func (s *QUICSession) RemoteAddr() net.Addr { return s.raw.RemoteAddr() }

func (s *QUICSession) watch() {
	<-s.raw.Context().Done()
	s.state.finish(closeInfoFromQUIC(context.Cause(s.raw.Context())))
}

func (s *QUICSession) normalizeSessionError(err error) error {
	if err == nil {
		return nil
	}
	if s.raw.Context().Err() != nil ||
		errors.Is(err, net.ErrClosed) {
		return fmt.Errorf("%w: %w", ErrSessionClosed, err)
	}
	return err
}

func closeInfoFromQUIC(err error) CloseInfo {
	info := CloseInfo{Cause: err, Reason: "other"}
	if err == nil {
		return info
	}

	var applicationErr *quic.ApplicationError
	if errors.As(err, &applicationErr) {
		info.Code = ErrorCode(applicationErr.ErrorCode)
		info.CodeValid = true
		info.Remote = applicationErr.Remote
		info.Reason = applicationErr.ErrorMessage
		return info
	}

	var idleErr *quic.IdleTimeoutError
	if errors.As(err, &idleErr) {
		info.Reason = "idle"
		return info
	}

	var statelessResetErr *quic.StatelessResetError
	if errors.As(err, &statelessResetErr) {
		info.Reason = "network"
		return info
	}

	var transportErr *quic.TransportError
	if errors.As(err, &transportErr) {
		info.Remote = transportErr.Remote
		if transportErr.ErrorCode == quic.NoViablePathError ||
			transportErr.ErrorCode == quic.ConnectionRefused {
			info.Reason = "network"
		} else {
			info.Reason = "protocol"
		}
		return info
	}

	var versionErr *quic.VersionNegotiationError
	if errors.As(err, &versionErr) {
		info.Reason = "protocol"
		return info
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		info.Reason = "network"
		return info
	}
	return info
}

type QUICListener struct {
	raw       *quic.Listener
	transport *quic.Transport
	conn      net.PacketConn
	observe   func(error)

	closeOnce          sync.Once
	closeErr           error
	transportCloseOnce sync.Once
	transportCloseErr  error
}

func NewQUICListener(listener *quic.Listener) *QUICListener {
	return &QUICListener{raw: listener}
}

// QUICServerKeys are the persisted 32-byte server secrets consumed by
// quic.Transport. File creation and validation belong to process startup.
type QUICServerKeys struct {
	StatelessReset [32]byte
	TokenGenerator [32]byte
}

// ListenQUIC loads or creates the persisted server keys, binds a real UDP
// socket, and constructs the listener. The returned io.Closer owns the
// transport and socket; Listener.Close only stops admission so the edge can
// drain accepted sessions before closing UDP I/O.
func ListenQUIC(address string, tlsConfig *tls.Config, dataDir string, observe func(error)) (Listener, io.Closer, error) {
	keys, err := loadQUICServerKeys(dataDir)
	if err != nil {
		return nil, nil, err
	}
	udpAddress, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve QUIC listen address: %w", err)
	}
	conn, err := net.ListenUDP("udp", udpAddress)
	if err != nil {
		return nil, nil, err
	}
	listener, err := listenQUICOnUDP(conn, tlsConfig, keys, observe)
	if err != nil {
		return nil, nil, err
	}
	return listener, quicTransportCloser{listener}, nil
}

func listenQUICOnUDP(conn *net.UDPConn, tlsConfig *tls.Config, keys QUICServerKeys, observe func(error)) (*QUICListener, error) {
	resetKey := quic.StatelessResetKey(keys.StatelessReset)
	tokenKey := quic.TokenGeneratorKey(keys.TokenGenerator)
	transport := &quic.Transport{
		Conn:              conn,
		StatelessResetKey: &resetKey,
		TokenGeneratorKey: &tokenKey,
	}
	if observe != nil {
		transport.ConnContext = func(ctx context.Context, _ *quic.ClientInfo) (context.Context, error) {
			attempt := newQUICAttempt(observe)
			ctx = context.WithValue(ctx, quicAttemptContextKey{}, attempt)
			go attempt.watch(ctx)
			return ctx, nil
		}
	}
	listener, err := transport.Listen(ServerQUICTLSConfig(tlsConfig), ServerQUICConfig())
	if err != nil {
		_ = transport.Close()
		_ = conn.Close()
		return nil, err
	}
	return &QUICListener{
		raw:       listener,
		transport: transport,
		conn:      conn,
		observe:   observe,
	}, nil
}

func (l *QUICListener) Accept(ctx context.Context) (Session, error) {
	for {
		conn, err := l.raw.Accept(ctx)
		if err != nil {
			observeQUICError(l.observe, err)
			return nil, err
		}
		if attempt, ok := conn.Context().Value(quicAttemptContextKey{}).(*quicAttempt); ok {
			if !attempt.markAccepted() {
				// The context observer already classified this connection as
				// a failed handshake. Do not dispatch the same closed
				// connection as an accepted session.
				_ = conn.CloseWithError(0, "connection closed before accept")
				continue
			}
		}
		return NewQUICSession(conn), nil
	}
}

func (l *QUICListener) Close() error {
	l.closeOnce.Do(func() {
		l.closeErr = l.raw.Close()
	})
	return l.closeErr
}

func (l *QUICListener) CloseTransport() error {
	l.transportCloseOnce.Do(func() {
		var errs []error
		if err := l.Close(); err != nil {
			errs = append(errs, err)
		}
		if l.transport != nil {
			if err := l.transport.Close(); err != nil {
				errs = append(errs, err)
			}
		}
		if l.conn != nil {
			if err := l.conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				errs = append(errs, err)
			}
		}
		l.transportCloseErr = errors.Join(errs...)
	})
	return l.transportCloseErr
}

func (l *QUICListener) Addr() net.Addr { return l.raw.Addr() }

type quicTransportCloser struct {
	listener *QUICListener
}

func (c quicTransportCloser) Close() error {
	return c.listener.CloseTransport()
}

type quicAttemptContextKey struct{}

type quicAttempt struct {
	mu       sync.Mutex
	resolved bool
	accepted bool
	done     chan struct{}
	observe  func(error)
}

func newQUICAttempt(observe func(error)) *quicAttempt {
	return &quicAttempt{
		done:    make(chan struct{}),
		observe: observe,
	}
}

func (a *quicAttempt) markAccepted() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.resolved {
		return a.accepted
	}
	a.resolved = true
	a.accepted = true
	close(a.done)
	return true
}

func (a *quicAttempt) watch(ctx context.Context) {
	select {
	case <-a.done:
		return
	case <-ctx.Done():
	}
	a.mu.Lock()
	if a.resolved {
		a.mu.Unlock()
		return
	}
	a.resolved = true
	close(a.done)
	a.mu.Unlock()
	observeQUICError(a.observe, context.Cause(ctx))
}

func observeQUICError(observe func(error), err error) {
	if observe == nil || err == nil ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, quic.ErrServerClosed) {
		return
	}
	observe(err)
}

const (
	statelessResetKeyFile = "quic-stateless-reset.key"
	tokenGeneratorKeyFile = "quic-token-generator.key"
)

func loadQUICServerKeys(dataDir string) (QUICServerKeys, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return QUICServerKeys{}, fmt.Errorf("create QUIC key directory: %w", err)
	}
	reset, err := loadOrCreateQUICKey(filepath.Join(dataDir, statelessResetKeyFile))
	if err != nil {
		return QUICServerKeys{}, err
	}
	token, err := loadOrCreateQUICKey(filepath.Join(dataDir, tokenGeneratorKeyFile))
	if err != nil {
		return QUICServerKeys{}, err
	}
	return QUICServerKeys{
		StatelessReset: reset,
		TokenGenerator: token,
	}, nil
}

func loadOrCreateQUICKey(path string) ([32]byte, error) {
	var key [32]byte
	info, err := os.Stat(path)
	if err == nil {
		if !info.Mode().IsRegular() {
			return key, fmt.Errorf("QUIC key %s is not a regular file", path)
		}
		if info.Mode().Perm() != 0o600 {
			return key, fmt.Errorf("QUIC key %s has mode %04o, want 0600", path, info.Mode().Perm())
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return key, fmt.Errorf("read QUIC key %s: %w", path, readErr)
		}
		if len(data) != len(key) {
			return key, fmt.Errorf("QUIC key %s has %d bytes, want %d", path, len(data), len(key))
		}
		copy(key[:], data)
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return key, fmt.Errorf("read QUIC key %s: %w", path, err)
	}
	if _, err := rand.Read(key[:]); err != nil {
		return key, fmt.Errorf("generate QUIC key %s: %w", path, err)
	}
	if err := writeQUICKeyAtomic(path, key[:]); err != nil {
		return [32]byte{}, err
	}
	return key, nil
}

func writeQUICKeyAtomic(path string, data []byte) (retErr error) {
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary QUIC key: %w", err)
	}
	tempPath := file.Name()
	closed := false
	defer func() {
		if !closed {
			if closeErr := file.Close(); retErr == nil && closeErr != nil {
				retErr = fmt.Errorf("close QUIC key: %w", closeErr)
			}
		}
		_ = os.Remove(tempPath)
	}()
	if err := file.Chmod(0o600); err != nil {
		return fmt.Errorf("chmod QUIC key: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("write QUIC key: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync QUIC key: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close QUIC key: %w", err)
	}
	closed = true
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("install QUIC key %s: %w", path, err)
	}
	return nil
}

type DialFailureCategory string

const (
	DialNetwork   DialFailureCategory = "network"
	DialTimeout   DialFailureCategory = "timeout"
	DialHandshake DialFailureCategory = "handshake"
	DialTerminal  DialFailureCategory = "terminal"
)

type DialFailure struct {
	Category DialFailureCategory
	Address  string
	Err      error
}

func (e *DialFailure) Error() string {
	if e.Address == "" {
		return fmt.Sprintf("QUIC dial %s: %v", e.Category, e.Err)
	}
	return fmt.Sprintf("QUIC dial %s (%s): %v", e.Address, e.Category, e.Err)
}

func (e *DialFailure) Unwrap() error { return e.Err }

func (e *DialFailure) FallbackEligible() bool {
	return e != nil && e.Category != DialTerminal
}

func AsDialFailure(err error) (*DialFailure, bool) {
	var failure *DialFailure
	ok := errors.As(err, &failure)
	return failure, ok
}

type IPResolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

type quicDialFunc func(context.Context, string, *tls.Config, *quic.Config) (*quic.Conn, error)

type QUICDialer struct {
	resolver   IPResolver
	tokenStore quic.TokenStore
	dialAddr   quicDialFunc
}

func NewQUICDialer() *QUICDialer {
	return NewQUICDialerWithResolver(net.DefaultResolver)
}

func NewQUICDialerWithResolver(resolver IPResolver) *QUICDialer {
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	return &QUICDialer{
		resolver:   resolver,
		tokenStore: quic.NewLRUTokenStore(8, 4),
		dialAddr:   quic.DialAddr,
	}
}

// DialQUIC is convenient for one-shot commands. Long-lived clients should
// retain a QUICDialer so address-validation tokens survive reconnects.
func DialQUIC(ctx context.Context, address string, tlsConfig *tls.Config) (Session, error) {
	return NewQUICDialer().Dial(ctx, address, tlsConfig)
}

// Resolve preserves the original hostname for SNI and returns numeric
// addresses in resolver order under the caller's candidate budget.
func (d *QUICDialer) Resolve(ctx context.Context, address string) (string, []string, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", nil, &DialFailure{Category: DialTerminal, Address: address, Err: err}
	}

	addresses, err := d.resolve(ctx, host, port)
	if err != nil {
		return "", nil, classifyDialFailure(address, err)
	}
	return host, addresses, nil
}

// DialResolved opens one QUIC connection to a numeric address while retaining
// serverName for TLS verification and token-store identity.
func (d *QUICDialer) DialResolved(
	ctx context.Context,
	numericAddress string,
	serverName string,
	tlsConfig *tls.Config,
) (Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, classifyDialFailure(numericAddress, err)
	}
	cfg := ClientQUICConfig()
	cfg.TokenStore = d.tokenStore
	conn, err := d.dialAddr(
		ctx,
		numericAddress,
		ClientQUICTLSConfig(tlsConfig, serverName),
		cfg,
	)
	if err != nil {
		return nil, classifyDialFailure(numericAddress, err)
	}
	return NewQUICSession(conn), nil
}

// Dial resolves under ctx, preserves the original hostname for SNI and tries
// numeric addresses in resolver order within that same context budget.
func (d *QUICDialer) Dial(ctx context.Context, address string, tlsConfig *tls.Config) (Session, error) {
	host, addresses, err := d.Resolve(ctx, address)
	if err != nil {
		return nil, err
	}

	var lastFailure *DialFailure
	for _, numericAddress := range addresses {
		session, err := d.DialResolved(ctx, numericAddress, host, tlsConfig)
		if err == nil {
			return session, nil
		}

		failure, ok := AsDialFailure(err)
		if !ok {
			return nil, err
		}
		lastFailure = failure
		// Only an address-specific network/socket failure is eligible for
		// another resolved address. All addresses share the caller's budget.
		if failure.Category != DialNetwork {
			return nil, failure
		}
	}
	if lastFailure != nil {
		return nil, lastFailure
	}
	return nil, &DialFailure{
		Category: DialNetwork,
		Address:  address,
		Err:      errNoResolvedAddresses,
	}
}

func (d *QUICDialer) resolve(ctx context.Context, host, port string) ([]string, error) {
	if ip, err := netip.ParseAddr(host); err == nil {
		return []string{net.JoinHostPort(ip.String(), port)}, nil
	}
	ips, err := d.resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	addresses := make([]string, 0, len(ips))
	for _, ip := range ips {
		hostIP := ip.IP.String()
		if ip.Zone != "" {
			hostIP += "%" + ip.Zone
		}
		addresses = append(addresses, net.JoinHostPort(hostIP, port))
	}
	if len(addresses) == 0 {
		return nil, errNoResolvedAddresses
	}
	return addresses, nil
}

func classifyDialFailure(address string, err error) *DialFailure {
	category := DialTerminal
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		category = DialTimeout
	case errors.Is(err, context.Canceled):
		category = DialTerminal
	case errors.Is(err, errNoResolvedAddresses):
		category = DialNetwork
	default:
		var handshakeTimeout *quic.HandshakeTimeoutError
		var statelessReset *quic.StatelessResetError
		var versionNegotiation *quic.VersionNegotiationError
		var transportErr *quic.TransportError
		var certificateVerification *tls.CertificateVerificationError
		var certificateInvalid x509.CertificateInvalidError
		var hostnameError x509.HostnameError
		var unknownAuthority x509.UnknownAuthorityError
		var netErr net.Error

		switch {
		case errors.As(err, &certificateVerification),
			errors.As(err, &certificateInvalid),
			errors.As(err, &hostnameError),
			errors.As(err, &unknownAuthority):
			category = DialTerminal
		case errors.As(err, &handshakeTimeout):
			category = DialTimeout
		case errors.As(err, &statelessReset):
			category = DialNetwork
		case errors.As(err, &versionNegotiation):
			category = DialHandshake
		case errors.As(err, &transportErr):
			// QUIC crypto errors carry the TLS alert at 0x100 + alert.
			if transportErr.ErrorCode == quic.NoViablePathError ||
				transportErr.ErrorCode == quic.ConnectionRefused {
				category = DialNetwork
			} else if transportErr.ErrorCode >= 0x100 && transportErr.ErrorCode <= 0x1ff {
				category = DialTerminal
			} else {
				category = DialHandshake
			}
		case errors.As(err, &netErr):
			if netErr.Timeout() {
				category = DialTimeout
			} else {
				category = DialNetwork
			}
		}
	}
	return &DialFailure{Category: category, Address: address, Err: err}
}

var (
	_ Session  = (*QUICSession)(nil)
	_ Listener = (*QUICListener)(nil)
)
