// Package edge implements the public ingress for beamd.
//
// M3: TCP/TLS and QUIC listeners carry the same transport-neutral session
// protocol. Client control connections speak NDJSON on the first stream the
// client opens. Routes are populated dynamically
// via `register`; each public request opens a fresh data stream and
// is proxied through it.
package edge

import (
	"bufio"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dynamismlabs/beamd/internal/auth"
	"github.com/dynamismlabs/beamd/internal/certs"
	"github.com/dynamismlabs/beamd/internal/config"
	"github.com/dynamismlabs/beamd/internal/naming"
	"github.com/dynamismlabs/beamd/internal/proto"
	"github.com/dynamismlabs/beamd/internal/reqlog"
	"github.com/dynamismlabs/beamd/internal/tunnel"
)

const (
	ALPNBeam               = "beam/1"
	preauthHandshakeBudget = 5 * time.Second
)

var errStaleRoute = errors.New("tunnel route changed during stream admission")

type visitorRequestContextKey struct{}

type quicListenFunc func(
	string,
	*tls.Config,
	string,
	func(error),
) (tunnel.Listener, io.Closer, error)

// acmeTLS1 is the ACME TLS-ALPN-01 challenge protocol — advertised so certmagic
// can issue On-Demand custom-domain certs over this listener (url-model §8.2).
const acmeTLS1 = "acme-tls/1"

type Edge struct {
	cfg        *config.Server
	version    string
	tokens     auth.Store
	certs      certs.Manager
	configErr  error
	listenQUIC quicListenFunc

	heartbeatTimeout time.Duration

	mu       sync.RWMutex
	ln       net.Listener
	quicLn   tunnel.Listener
	quicIO   io.Closer
	sessions map[*Session]struct{}
	routes   map[string]*Route // hostname → route
	proxies  map[string]*httputil.ReverseProxy
	pubSrvs  map[*http.Server]struct{} // per-public-conn HTTP servers, tracked so Shutdown can drain them
	metrics  *metrics

	// lifecycleMu serializes listener/session admission and WaitGroup.Add with
	// the transition into shutdown. No Add may occur after shuttingDown becomes
	// true.
	lifecycleMu  sync.Mutex
	shuttingDown bool
	rawConns     map[net.Conn]struct{}
	preauth      map[tunnel.Session]struct{}
	hijacked     map[net.Conn]struct{}
	acceptWG     sync.WaitGroup
	handlerWG    sync.WaitGroup
	sessionWG    sync.WaitGroup
	proxyWG      sync.WaitGroup
	backgroundWG sync.WaitGroup
	streamWG     sync.WaitGroup

	tlsHandshakeSlots chan struct{}
	preAuthSlots      chan struct{}
	authSlots         chan struct{}
	globalStreamSlots chan struct{}

	// traffic is the self-hosted bandwidth recorder (in-memory + persisted),
	// powering /metrics and the usage webhook. trafficSinks holds any extra
	// recorders (e.g. a hosted account-aware billing sink) registered before
	// Serve; the proxy fans byte deltas out to all of them.
	traffic      *trafficStore
	trafficSinks []TrafficRecorder

	// reqSink receives per-request events (the file sink + optional shipper);
	// NopSink by default. reqHeartbeat is the long-connection accounting window.
	// The cap* flags + ipTruncate gate/minimize the analytics fields at the edge.
	reqSink      reqlog.Sink
	reqHeartbeat time.Duration
	capPath      bool
	capClientIP  bool
	capUserAgent bool
	capReferer   bool
	ipTruncate   bool

	// hostnames resolves a scope's full hostname set (aliases + custom domains)
	// so a tunnel registers under all of them. nil in self-host mode.
	hostnames *hostnamesClient

	// firstSession is closed when the first session completes hello/hello_ok.
	firstSessionOnce sync.Once
	firstSession     chan struct{}

	shutdownOnce sync.Once
	shutdown     chan struct{}
	shutdownDone chan struct{}
	shutdownErr  error
}

type Session struct {
	transport tunnel.Session
	kind      tunnel.Kind
	slug      string
	control   tunnel.Stream
	id        string
	remote    string
	handshake time.Duration

	streamSlots   chan struct{}
	activeStreams atomic.Int64
	authRelease   sync.Once

	streamLifecycleMu sync.Mutex
	streamsClosing    bool
	streamWG          sync.WaitGroup
	helperWG          sync.WaitGroup

	writeGate chan struct{} // one token serializes control-stream writes
	writeOnce sync.Once

	mu    sync.Mutex
	names map[string]struct{}
	// hosts maps a tunnel name → every hostname it's registered under (the
	// default-shape host(s) for the scope's slug(s) plus any custom-domain hosts).
	// Used to remove them all on unregister.
	hosts         map[string][]string
	lastHeartbeat time.Time
}

type Route struct {
	session *Session
	name    string
}

type streamLease struct {
	edge    *Edge
	session *Session
	once    sync.Once
}

func (e *Edge) acquireStreamLease(sess *Session) (*streamLease, error) {
	select {
	case e.globalStreamSlots <- struct{}{}:
	default:
		e.metrics.recordCapacityRejection("global_stream")
		return nil, tunnel.ErrCapacity
	}
	select {
	case sess.streamSlots <- struct{}{}:
	default:
		<-e.globalStreamSlots
		e.metrics.recordCapacityRejection("session_stream")
		return nil, tunnel.ErrCapacity
	}

	sess.activeStreams.Add(1)
	e.metrics.addStream(sess.kind, 1)
	return &streamLease{edge: e, session: sess}, nil
}

func (l *streamLease) release() {
	l.once.Do(func() {
		<-l.session.streamSlots
		<-l.edge.globalStreamSlots
		l.session.activeStreams.Add(-1)
		l.edge.metrics.addStream(l.session.kind, -1)
	})
}

// reserveStreamWatcher registers the goroutine that owns traffic finalization
// and lease release. Both the edge-wide shutdown and the individual session
// join these watchers. Production callers already own a proxyWG slot before
// reaching DialContext; graceful shutdown waits that group before it can wait
// streamWG, so an admitted request may still register here after the shutdown
// flag flips without racing a zero-counter Wait.
func (e *Edge) reserveStreamWatcher(sess *Session) bool {
	sess.streamLifecycleMu.Lock()
	defer sess.streamLifecycleMu.Unlock()
	if sess.streamsClosing {
		return false
	}
	e.streamWG.Add(1)
	sess.streamWG.Add(1)
	return true
}

func (e *Edge) releaseStreamWatcher(sess *Session) {
	sess.streamWG.Done()
	e.streamWG.Done()
}

func (sess *Session) stopStreamAdmission() {
	sess.streamLifecycleMu.Lock()
	sess.streamsClosing = true
	sess.streamLifecycleMu.Unlock()
}

func (sess *Session) waitStreamWatchers() {
	sess.streamWG.Wait()
}

// startHelper gives the session handler ownership of auxiliary lifecycle
// goroutines. All helpers are registered before the control loop can return,
// so dropSession can safely join the group without racing a late Add.
func (sess *Session) startHelper(run func()) {
	sess.helperWG.Add(1)
	go func() {
		defer sess.helperWG.Done()
		run()
	}()
}

func (sess *Session) waitHelpers() {
	sess.helperWG.Wait()
}

func New(cfg *config.Server, version string, tokens auth.Store, certMgr certs.Manager) *Edge {
	// Directly constructed test/server configs bypass LoadServer. FinalizeRuntime
	// is idempotent and supplies the same shipped defaults in that path.
	runtimeErr := cfg.FinalizeRuntime()
	if runtimeErr != nil {
		slog.Error("invalid edge runtime configuration", "err", runtimeErr.Error())
	}
	hbSec := cfg.RequestLog.HeartbeatSeconds
	if hbSec <= 0 {
		hbSec = 60
	}
	capPath, capIP, capUA, capRef := cfg.RequestLog.Capture.Captures()
	// ip_mode "off" means do NOT collect the client IP at all. A raw visitor IP
	// must never leave the edge, so we drop it rather than ship it un-truncated
	// (capturing-but-not-truncating would do exactly that — request-events §4.4).
	if cfg.RequestLog.IPMode == "off" {
		capIP = false
	}
	e := &Edge{
		cfg:               cfg,
		version:           version,
		tokens:            tokens,
		certs:             certMgr,
		configErr:         runtimeErr,
		listenQUIC:        tunnel.ListenQUIC,
		heartbeatTimeout:  60 * time.Second,
		sessions:          make(map[*Session]struct{}),
		routes:            make(map[string]*Route),
		proxies:           make(map[string]*httputil.ReverseProxy),
		pubSrvs:           make(map[*http.Server]struct{}),
		rawConns:          make(map[net.Conn]struct{}),
		preauth:           make(map[tunnel.Session]struct{}),
		hijacked:          make(map[net.Conn]struct{}),
		tlsHandshakeSlots: make(chan struct{}, 128),
		preAuthSlots:      make(chan struct{}, safeChannelCapacity(cfg.MaxPreAuthSessions)),
		authSlots:         make(chan struct{}, safeChannelCapacity(cfg.MaxSessionsTotal)),
		globalStreamSlots: make(chan struct{}, safeChannelCapacity(cfg.MaxStreamsTotal)),
		metrics:           newMetrics(),
		traffic:           newTrafficStore(trafficPath(cfg)),
		reqSink:           reqlog.NopSink{},
		reqHeartbeat:      time.Duration(hbSec) * time.Second,
		capPath:           capPath,
		capClientIP:       capIP,
		capUserAgent:      capUA,
		capReferer:        capRef,
		ipTruncate:        cfg.RequestLog.IPMode != "off",
		hostnames:         newHostnamesClient(cfg.TokenStore),
		firstSession:      make(chan struct{}),
		shutdown:          make(chan struct{}),
		shutdownDone:      make(chan struct{}),
	}
	e.metrics.configure(cfg.MaxStreamsPerSession, cfg.MaxStreamsTotal, cfg.YamuxStreamWindowBytes)
	// Gate on-demand DNS-01 wildcard issuance on hosts the edge actually serves,
	// so an unauthenticated peer sending arbitrary <app>.<slug>.<base> SNIs
	// can't drive real ACME orders for attacker-chosen wildcards (which would
	// burn the operator's CA rate limits). The production MagicManager honors
	// this; the dev/test SelfSignedManager doesn't implement it and issues
	// cheap self-signed certs, so the type-assert simply no-ops there.
	if hm, ok := certMgr.(interface{ SetHostAllowed(func(string) bool) }); ok {
		hm.SetHostAllowed(e.hostKnownForCert)
	}
	return e
}

func safeChannelCapacity(value int) int {
	if value < 1 {
		return 1
	}
	return value
}

// hostKnownForCert reports whether the edge should obtain a real cert for this
// SNI: the apex (always the operator's own) or a hostname with a live route.
// An unregistered host has nothing to serve, so refusing issuance for it costs
// nothing and closes the ACME-amplification vector.
func (e *Edge) hostKnownForCert(name string) bool {
	if strings.EqualFold(name, e.cfg.BaseDomain) {
		return true
	}
	e.mu.RLock()
	_, ok := e.routes[name]
	e.mu.RUnlock()
	return ok
}

// SetHostnamesEndpoint overrides the scope-hostnames endpoint + secret (tests
// point this at a stub control plane to exercise multi-host registration).
func (e *Edge) SetHostnamesEndpoint(url, secret string) {
	e.hostnames = &hostnamesClient{
		url:    url,
		secret: secret,
		ttl:    5 * time.Minute,
		client: &http.Client{Timeout: 5 * time.Second},
		cache:  make(map[string]hostnamesEntry),
	}
}

// trafficPath returns where the bandwidth store persists, or "" to keep
// it in-memory only (no data_dir configured — e.g. tests).
func trafficPath(cfg *config.Server) string {
	if cfg.DataDir == "" {
		return ""
	}
	return filepath.Join(cfg.DataDir, "bandwidth.json")
}

// AddTrafficSink registers an additional TrafficRecorder that receives
// every per-tunnel byte delta alongside the built-in store. Intended for
// hosted deployments to plug in durable, account-aware billing. Must be
// called before Serve (sinks are read without locking on the hot path).
func (e *Edge) AddTrafficSink(r TrafficRecorder) {
	e.trafficSinks = append(e.trafficSinks, r)
}

// recordTraffic fans a closed connection's byte totals out to the
// built-in store and any registered sinks.
func (e *Edge) recordTraffic(slug, name string, bytesIn, bytesOut int64) {
	e.traffic.RecordTraffic(slug, name, bytesIn, bytesOut)
	for _, s := range e.trafficSinks {
		s.RecordTraffic(slug, name, bytesIn, bytesOut)
	}
}

// SetHeartbeatTimeout overrides the default 60s server-side heartbeat
// deadline. Useful for tests; production tunes via config later.
func (e *Edge) SetHeartbeatTimeout(d time.Duration) {
	e.heartbeatTimeout = d
}

// flushTrafficPeriodically persists the bandwidth store on an interval so
// a crash loses at most one interval of counts. The authoritative final
// flush happens in Shutdown. No-op when persistence is disabled.
func (e *Edge) flushTrafficPeriodically() {
	tick := time.NewTicker(60 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-e.shutdown:
			return
		case <-tick.C:
			if err := e.traffic.Flush(); err != nil {
				slog.Warn("traffic store flush failed", "err", err.Error())
			}
		}
	}
}

func (e *Edge) Serve() error {
	if e.configErr != nil {
		return fmt.Errorf("edge runtime configuration: %w", e.configErr)
	}
	if err := e.cfg.FinalizeRuntime(); err != nil {
		return fmt.Errorf("edge runtime configuration: %w", err)
	}
	tlsCfg := &tls.Config{
		GetCertificate: e.certs.GetCertificate,
		// Pin the floor explicitly rather than rely on the toolchain default:
		// a GODEBUG flip or older build must not silently re-enable TLS 1.0/1.1.
		MinVersion: tls.VersionTLS12,
		// `acme-tls/1` lets certmagic solve ACME TLS-ALPN-01 challenges over this
		// listener — how On-Demand custom-domain certs (url-model §8.2, path B)
		// get issued without a :80 listener. GetCertificate returns the challenge
		// cert for these handshakes; they carry no app data, so the conn handler
		// just sees them close.
		NextProtos: []string{ALPNBeam, acmeTLS1, "h2", "http/1.1"},
	}

	ln, err := net.Listen("tcp", e.cfg.ListenHTTPS)
	if err != nil {
		return fmt.Errorf("listen %s: %w", e.cfg.ListenHTTPS, err)
	}
	var (
		quicLn tunnel.Listener
		quicIO io.Closer
	)
	if !e.cfg.DisableQUIC {
		quicTLS := tlsCfg.Clone()
		quicTLS.MinVersion = tls.VersionTLS13
		quicTLS.NextProtos = []string{tunnel.ALPNQUIC}
		listenQUIC := e.listenQUIC
		if listenQUIC == nil {
			listenQUIC = tunnel.ListenQUIC
		}
		quicLn, quicIO, err = listenQUIC(e.cfg.ListenQUIC, quicTLS, e.cfg.DataDir, e.observeQUICAttempt)
		if err != nil {
			_ = ln.Close()
			return fmt.Errorf("listen QUIC %s: %w", e.cfg.ListenQUIC, err)
		}
	}

	e.lifecycleMu.Lock()
	if e.shuttingDown {
		e.lifecycleMu.Unlock()
		_ = ln.Close()
		if quicLn != nil {
			_ = quicLn.Close()
		}
		if quicIO != nil {
			_ = quicIO.Close()
		}
		return nil
	}
	e.ln = ln
	e.quicLn = quicLn
	e.quicIO = quicIO
	acceptCount := 1
	if quicLn != nil {
		acceptCount++
	}
	e.acceptWG.Add(acceptCount)
	e.backgroundWG.Add(1)
	e.lifecycleMu.Unlock()
	e.metrics.setListener(tunnel.KindYamux, true)
	e.metrics.setListener(tunnel.KindQUIC, quicLn != nil)

	if e.cfg.MetricsToken == "" {
		slog.Warn("edge: /metrics is disabled (no metrics_token set) — set metrics_token or BEAMD_METRICS_TOKEN to enable operator scraping")
	}

	go func() {
		defer e.backgroundWG.Done()
		e.flushTrafficPeriodically()
	}()

	results := make(chan error, 2)
	go e.acceptTCP(ln, tlsCfg, results)
	if quicLn != nil {
		go e.acceptQUIC(quicLn, results)
	}

	transports := []string{"tcp"}
	if quicLn != nil {
		transports = append(transports, "quic")
	}
	slog.Info("ready",
		"version", e.version,
		"base_domain", e.cfg.BaseDomain,
		"listen_https", e.cfg.ListenHTTPS,
		"listen_quic", e.cfg.ListenQUIC,
		"transports", transports,
		"yamux_stream_window_bytes", e.cfg.YamuxStreamWindowBytes,
	)

	var firstErr error
	for range acceptCount {
		acceptErr := <-results
		if acceptErr != nil && firstErr == nil {
			firstErr = acceptErr
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = e.Shutdown(ctx)
			cancel()
		}
	}
	return firstErr
}

func (e *Edge) acceptTCP(ln net.Listener, tlsCfg *tls.Config, results chan<- error) {
	defer e.acceptWG.Done()
	for {
		raw, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				results <- nil
			} else {
				results <- fmt.Errorf("TCP accept: %w", err)
			}
			return
		}
		select {
		case e.tlsHandshakeSlots <- struct{}{}:
		default:
			e.metrics.recordCapacityRejection("tls_handshake")
			_ = raw.Close()
			continue
		}

		e.lifecycleMu.Lock()
		if e.shuttingDown {
			e.lifecycleMu.Unlock()
			<-e.tlsHandshakeSlots
			_ = raw.Close()
			continue
		}
		e.rawConns[raw] = struct{}{}
		e.handlerWG.Add(1)
		e.lifecycleMu.Unlock()
		go e.handleTCP(raw, tlsCfg)
	}
}

func (e *Edge) acceptQUIC(ln tunnel.Listener, results chan<- error) {
	defer e.acceptWG.Done()
	for {
		transport, err := ln.Accept(context.Background())
		if err != nil {
			if errors.Is(err, net.ErrClosed) || e.isShuttingDown() {
				results <- nil
			} else {
				results <- fmt.Errorf("QUIC accept: %w", err)
			}
			return
		}
		if !e.acquirePreauth(transport) {
			_ = transport.CloseWithError(tunnel.CloseCapacity, "pre-authentication capacity reached")
			continue
		}
		e.lifecycleMu.Lock()
		if e.shuttingDown {
			e.lifecycleMu.Unlock()
			e.releasePreauth(transport)
			_ = transport.CloseWithError(tunnel.CloseShutdown, "edge shutting down")
			continue
		}
		e.sessionWG.Add(1)
		e.lifecycleMu.Unlock()
		go func() {
			defer e.sessionWG.Done()
			e.handleTunnelSession(transport)
		}()
	}
}

func (e *Edge) isShuttingDown() bool {
	e.lifecycleMu.Lock()
	defer e.lifecycleMu.Unlock()
	return e.shuttingDown
}

func (e *Edge) observeQUICAttempt(err error) {
	if err != nil {
		e.metrics.recordHandshakeError(tunnel.KindQUIC, classifyHandshakeError(err))
	}
}

func classifyHandshakeError(err error) string {
	if err == nil {
		return "other"
	}
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) ||
		(errors.As(err, &netErr) && netErr.Timeout()) {
		return "timeout"
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "tls"),
		strings.Contains(message, "certificate"),
		strings.Contains(message, "x509"):
		return "tls"
	case strings.Contains(message, "protocol"),
		strings.Contains(message, "version"),
		strings.Contains(message, "alpn"),
		strings.Contains(message, "application protocol"):
		return "protocol"
	default:
		return "other"
	}
}

func classifySessionClose(info tunnel.CloseInfo) string {
	return tunnel.CloseReason(info)
}

func (e *Edge) handleTCP(raw net.Conn, tlsCfg *tls.Config) {
	defer func() {
		e.lifecycleMu.Lock()
		delete(e.rawConns, raw)
		e.lifecycleMu.Unlock()
		e.handlerWG.Done()
	}()

	handshakeSlotHeld := true
	releaseHandshake := func() {
		if handshakeSlotHeld {
			handshakeSlotHeld = false
			<-e.tlsHandshakeSlots
		}
	}
	defer releaseHandshake()

	tlsConn := tls.Server(raw, tlsCfg)
	_ = raw.SetDeadline(time.Now().Add(preAuthTimeout))
	started := time.Now()
	if err := tlsConn.HandshakeContext(context.Background()); err != nil {
		releaseHandshake()
		e.metrics.recordHandshakeError(tunnel.KindYamux, classifyHandshakeError(err))
		slog.Debug("tls handshake failed", "err", err.Error())
		_ = raw.Close()
		return
	}
	releaseHandshake()

	if e.isShuttingDown() {
		_ = raw.Close()
		return
	}
	switch tlsConn.ConnectionState().NegotiatedProtocol {
	case ALPNBeam:
		_ = raw.SetDeadline(time.Time{})
		transport, err := tunnel.NewYamuxServer(tlsConn, uint32(e.cfg.YamuxStreamWindowBytes))
		if err != nil {
			slog.Error("yamux server setup failed", "err", err.Error())
			_ = raw.Close()
			return
		}
		if !e.acquirePreauth(transport) {
			_ = transport.CloseWithError(tunnel.CloseCapacity, "pre-authentication capacity reached")
			return
		}
		slog.Debug("tunnel transport handshake complete",
			"transport", tunnel.KindYamux,
			"handshake_ms", time.Since(started).Milliseconds(),
		)
		e.handleTunnelSession(transport)
	case acmeTLS1:
		_ = raw.Close()
	default:
		_ = raw.SetDeadline(time.Time{})
		e.handlePublic(tlsConn)
	}
}

// Shutdown stops accepting new connections, notifies every client
// with `error{code:"shutdown"}` so they reconnect immediately, then
// drains in-flight public requests up to ctx's deadline before
// force-closing remaining sessions. Idempotent.
func (e *Edge) Shutdown(ctx context.Context) error {
	e.shutdownOnce.Do(func() {
		defer close(e.shutdownDone)

		e.lifecycleMu.Lock()
		e.shuttingDown = true
		close(e.shutdown)
		ln := e.ln
		quicLn := e.quicLn
		quicIO := e.quicIO
		e.ln = nil
		e.quicLn = nil
		e.quicIO = nil
		rawConns := make([]net.Conn, 0, len(e.rawConns))
		for conn := range e.rawConns {
			rawConns = append(rawConns, conn)
		}
		preauth := make([]tunnel.Session, 0, len(e.preauth))
		for transport := range e.preauth {
			preauth = append(preauth, transport)
		}
		hijacked := make([]net.Conn, 0, len(e.hijacked))
		for conn := range e.hijacked {
			hijacked = append(hijacked, conn)
		}
		e.mu.RLock()
		srvs := make([]*http.Server, 0, len(e.pubSrvs))
		for srv := range e.pubSrvs {
			srvs = append(srvs, srv)
		}
		sessions := make([]*Session, 0, len(e.sessions))
		for sess := range e.sessions {
			sessions = append(sessions, sess)
		}
		e.mu.RUnlock()
		e.lifecycleMu.Unlock()

		if ln != nil {
			_ = ln.Close()
		}
		if quicLn != nil {
			_ = quicLn.Close()
		}
		e.metrics.setListener(tunnel.KindYamux, false)
		e.metrics.setListener(tunnel.KindQUIC, false)

		var forceOnce sync.Once
		forceClose := func() {
			forceOnce.Do(func() {
				for _, srv := range srvs {
					_ = srv.Close()
				}
				for _, conn := range hijacked {
					_ = conn.Close()
				}
				for _, conn := range rawConns {
					_ = conn.Close()
				}
			})
		}
		stopDeadlineWatch := make(chan struct{})
		deadlineWatchDone := make(chan struct{})
		go func() {
			defer close(deadlineWatchDone)
			select {
			case <-ctx.Done():
				forceClose()
			case <-stopDeadlineWatch:
			}
		}()
		defer func() {
			close(stopDeadlineWatch)
			<-deadlineWatchDone
		}()

		var notifyWG sync.WaitGroup
		for _, sess := range sessions {
			notifyWG.Add(1)
			go func(s *Session) {
				defer notifyWG.Done()
				writeCtx, cancel := context.WithTimeout(ctx, time.Second)
				defer cancel()
				_ = s.sendShutdownContext(writeCtx, &proto.Error{
					Type: proto.TypeError, Code: proto.CodeShutdown, Message: "edge shutting down",
				})
			}(sess)
		}
		notifyWG.Wait()

		var drainWG sync.WaitGroup
		for _, srv := range srvs {
			drainWG.Add(1)
			go func(srv *http.Server) {
				defer drainWG.Done()
				_ = srv.Shutdown(ctx)
			}(srv)
		}
		drained := make(chan struct{})
		go func() {
			drainWG.Wait()
			e.proxyWG.Wait()
			close(drained)
		}()
		select {
		case <-drained:
		case <-ctx.Done():
			forceClose()
		}

		for _, transport := range preauth {
			_ = transport.CloseWithError(tunnel.CloseShutdown, "edge shutting down")
		}
		for _, sess := range sessions {
			_ = sess.transport.CloseWithError(tunnel.CloseShutdown, "edge shutting down")
		}
		if quicIO != nil {
			_ = quicIO.Close()
		}

		workersJoined := false
		if err := waitGroups(
			ctx,
			&e.acceptWG,
			&e.handlerWG,
			&e.sessionWG,
			&e.proxyWG,
			&e.backgroundWG,
			&e.streamWG,
		); err != nil {
			forceClose()
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			cleanupErr := waitGroups(
				cleanupCtx,
				&e.acceptWG,
				&e.handlerWG,
				&e.sessionWG,
				&e.proxyWG,
				&e.backgroundWG,
				&e.streamWG,
			)
			cancel()
			if cleanupErr != nil {
				e.shutdownErr = errors.Join(err, fmt.Errorf("forced shutdown cleanup: %w", cleanupErr))
			} else {
				e.shutdownErr = err
				workersJoined = true
			}
		} else {
			workersJoined = true
		}
		// Traffic finalization runs in leased-stream watchers. Never race the
		// persistent store's final flush against a worker that failed to drain.
		if workersJoined {
			if err := e.traffic.Flush(); err != nil {
				slog.Warn("traffic store final flush failed", "err", err.Error())
				if e.shutdownErr == nil {
					e.shutdownErr = err
				}
			}
		} else {
			slog.Warn("traffic store final flush skipped because proxy workers did not drain")
		}
	})
	return e.shutdownErr
}

func waitGroups(ctx context.Context, groups ...*sync.WaitGroup) error {
	done := make(chan struct{})
	go func() {
		for _, group := range groups {
			group.Wait()
		}
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// FirstSession returns a channel closed once the first client has
// completed hello/hello_ok. Test plumbing — production gating happens
// per-(slug,name) once the control protocol is in play (M5).
func (e *Edge) FirstSession() <-chan struct{} { return e.firstSession }

// RouteCount returns the number of currently registered routes.
// Exposed for tests; production observability uses the metrics in M6.
func (e *Edge) RouteCount() int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return len(e.routes)
}

// SessionCount returns the number of currently open client sessions.
func (e *Edge) SessionCount() int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return len(e.sessions)
}

// SessionsCreatedTotal returns the monotonic count of every session
// ever accepted (PRD §M5/M6 — used by tests to wait for "a fresh
// session" rather than a stable count after a forced disconnect).
func (e *Edge) SessionsCreatedTotal() int64 {
	return e.metrics.sessionsCreatedTotal.Load()
}

// CloseAllSessions ends every active client transport session. Test helper for
// exercising the client's reconnect
// path — production code does NOT call this.
func (e *Edge) CloseAllSessions() {
	e.mu.RLock()
	sessions := make([]*Session, 0, len(e.sessions))
	for s := range e.sessions {
		sessions = append(sessions, s)
	}
	e.mu.RUnlock()
	for _, s := range sessions {
		_ = s.transport.CloseWithError(tunnel.CloseSuperseded, "test forced reconnect")
	}
}

// preAuthTimeout bounds a raw TCP TLS handshake. The common transport hello
// path has its own five-second context/deadline.
const preAuthTimeout = 15 * time.Second

func (e *Edge) acquirePreauth(transport tunnel.Session) bool {
	select {
	case e.preAuthSlots <- struct{}{}:
	default:
		e.metrics.recordCapacityRejection("preauth_session")
		return false
	}
	e.lifecycleMu.Lock()
	if e.shuttingDown {
		e.lifecycleMu.Unlock()
		<-e.preAuthSlots
		return false
	}
	e.preauth[transport] = struct{}{}
	e.lifecycleMu.Unlock()
	e.metrics.addSessionState(transport.Kind(), "preauth", 1)
	return true
}

func (e *Edge) releasePreauth(transport tunnel.Session) {
	e.lifecycleMu.Lock()
	if _, ok := e.preauth[transport]; !ok {
		e.lifecycleMu.Unlock()
		return
	}
	delete(e.preauth, transport)
	e.lifecycleMu.Unlock()
	e.metrics.addSessionState(transport.Kind(), "preauth", -1)
	<-e.preAuthSlots
}

func (e *Edge) acquireAuthenticated() bool {
	e.lifecycleMu.Lock()
	defer e.lifecycleMu.Unlock()
	if e.shuttingDown {
		return false
	}
	select {
	case e.authSlots <- struct{}{}:
		return true
	default:
		e.metrics.recordCapacityRejection("authenticated_session")
		return false
	}
}

func (e *Edge) promoteAuthenticated(transport tunnel.Session, sess *Session) bool {
	e.lifecycleMu.Lock()
	if e.shuttingDown {
		e.lifecycleMu.Unlock()
		return false
	}
	if _, ok := e.preauth[transport]; !ok {
		e.lifecycleMu.Unlock()
		return false
	}
	delete(e.preauth, transport)
	e.mu.Lock()
	e.sessions[sess] = struct{}{}
	e.mu.Unlock()
	e.lifecycleMu.Unlock()

	e.metrics.addSessionState(transport.Kind(), "preauth", -1)
	<-e.preAuthSlots
	return true
}

func (e *Edge) handleTunnelSession(transport tunnel.Session) {
	handshakeStarted := time.Now()
	authenticated := false
	defer func() {
		if !authenticated {
			e.releasePreauth(transport)
		}
		if !transport.IsClosed() {
			_ = transport.CloseWithError(tunnel.CloseNormal, "session handler ended")
		}
	}()

	control, cancel, err := acceptPreauthControl(transport, preauthHandshakeBudget)
	if err != nil {
		e.metrics.recordHandshakeError(transport.Kind(), classifyHandshakeError(err))
		slog.Debug("accept control stream", "transport", transport.Kind(), "err", err.Error())
		return
	}
	defer cancel()

	br := bufio.NewReader(control)
	typ, line, err := proto.Read(br)
	if err != nil || typ != proto.TypeHello {
		e.metrics.recordHandshakeError(transport.Kind(), "protocol")
		_ = proto.Write(control, &proto.Error{
			Type: proto.TypeError, Code: proto.CodeBadHello,
			Message: "first message must be hello",
		})
		_ = transport.CloseWithError(tunnel.CloseProtocol, "first message must be hello")
		return
	}

	var hello proto.Hello
	if err := json.Unmarshal(line, &hello); err != nil {
		e.metrics.recordHandshakeError(transport.Kind(), "protocol")
		_ = proto.Write(control, &proto.Error{
			Type: proto.TypeError, Code: proto.CodeBadHello, Message: err.Error(),
		})
		_ = transport.CloseWithError(tunnel.CloseProtocol, "malformed hello")
		return
	}
	if hello.ProtoVersion != proto.ProtoVersion {
		e.metrics.recordHandshakeError(transport.Kind(), "protocol")
		_ = proto.Write(control, &proto.Error{
			Type:    proto.TypeError,
			Code:    proto.CodeBadVersion,
			Message: fmt.Sprintf("protocol version %d is not supported; expected %d", hello.ProtoVersion, proto.ProtoVersion),
		})
		_ = transport.CloseWithError(tunnel.CloseProtocol, "protocol version mismatch")
		return
	}

	slug, ok := e.tokens.Resolve(hello.Token, hello.Scope)
	if !ok {
		e.metrics.recordHandshakeError(transport.Kind(), "protocol")
		msg := "invalid token"
		if hello.Scope != "" {
			msg = fmt.Sprintf("invalid token, or scope %q not available to this login", hello.Scope)
		}
		_ = proto.Write(control, &proto.Error{
			Type: proto.TypeError, Code: proto.CodeBadToken, Message: msg,
		})
		_ = transport.CloseWithError(tunnel.CloseAuth, "authentication rejected")
		return
	}
	if !e.acquireAuthenticated() {
		if e.isShuttingDown() {
			_ = transport.CloseWithError(tunnel.CloseShutdown, "edge shutting down")
			return
		}
		_ = proto.Write(control, &proto.Error{
			Type: proto.TypeError, Code: proto.CodeOverLimit, Message: "authenticated session capacity reached",
		})
		_ = transport.CloseWithError(tunnel.CloseCapacity, "authenticated session capacity reached")
		return
	}

	if err := proto.Write(control, &proto.HelloOK{
		Type: proto.TypeHelloOK, Slug: slug,
		BaseDomain: e.cfg.BaseDomain, Shape: string(e.cfg.Shape()), ProtoVersion: proto.ProtoVersion,
	}); err != nil {
		<-e.authSlots
		e.metrics.recordHandshakeError(transport.Kind(), "other")
		_ = transport.CloseWithError(tunnel.CloseProtocol, "write hello_ok failed")
		return
	}
	sess := &Session{
		transport:     transport,
		kind:          transport.Kind(),
		slug:          slug,
		control:       control,
		id:            reqlog.NewID(),
		remote:        transport.RemoteAddr().String(),
		handshake:     time.Since(handshakeStarted),
		streamSlots:   make(chan struct{}, e.cfg.MaxStreamsPerSession),
		writeGate:     make(chan struct{}, 1),
		names:         make(map[string]struct{}),
		hosts:         make(map[string][]string),
		lastHeartbeat: time.Now(),
	}
	if !e.promoteAuthenticated(transport, sess) {
		<-e.authSlots
		_ = transport.CloseWithError(tunnel.CloseShutdown, "edge shutting down")
		return
	}
	_ = control.SetDeadline(time.Time{})
	cancel()
	authenticated = true

	e.metrics.activeSessions.Add(1)
	e.metrics.sessionsCreatedTotal.Add(1)
	e.metrics.addSessionState(transport.Kind(), "authenticated", 1)
	e.metrics.recordSessionCreated(transport.Kind())
	e.firstSessionOnce.Do(func() { close(e.firstSession) })
	slog.Info("session opened",
		"event", "session_opened",
		"session_id", sess.id,
		"transport", transport.Kind(),
		"slug", slug,
		"remote_addr", sess.remote,
		"handshake_ms", sess.handshake.Milliseconds(),
		"active_streams", sess.activeStreams.Load(),
		"close_reason", "",
		"error_category", "",
	)

	hbCtx, hbCancel := context.WithCancel(context.Background())
	// Cancel heartbeat before dropSession closes and joins the transport tree.
	// Keeping both operations in one defer makes the ordering explicit; a
	// separate later defer could otherwise strand dropSession in helperWG.Wait.
	defer func() {
		hbCancel()
		e.dropSession(sess)
	}()
	sess.startHelper(func() { e.heartbeatWatch(hbCtx, sess) })
	sess.startHelper(func() { e.rejectUnexpectedStreams(sess) })

	for {
		typ, line, err := proto.Read(br)
		if err != nil {
			_ = transport.CloseWithError(tunnel.CloseProtocol, "control stream ended")
			// Closing the session owns control-stream teardown. Sending a
			// stream FIN after CONNECTION_CLOSE has been claimed can reach a
			// QUIC peer first, where the clean EOF races the authoritative
			// application close and may be misclassified as a local protocol
			// failure.
			if err != io.EOF && !errors.Is(err, net.ErrClosed) && !errors.Is(err, tunnel.ErrSessionClosed) {
				slog.Debug("control: read", "transport", transport.Kind(), "err", err.Error())
			}
			return
		}
		e.handleControlMsg(sess, typ, line)
	}
}

// acceptPreauthControl applies one wall-clock budget to both accepting the
// control stream and reading/writing the hello exchange. The returned cancel
// must remain live until authentication finishes so the stream deadline is
// never restarted after AcceptStream.
func acceptPreauthControl(
	transport tunnel.Session,
	budget time.Duration,
) (tunnel.Stream, context.CancelFunc, error) {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	control, err := transport.AcceptStream(ctx)
	if err != nil {
		cancel()
		return nil, func() {}, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = control.SetDeadline(deadline)
	}
	return control, cancel, nil
}

func (e *Edge) rejectUnexpectedStreams(sess *Session) {
	stream, err := sess.transport.AcceptStream(context.Background())
	if err != nil {
		return
	}
	stream.Abort(tunnel.StreamCanceled)
	_ = sess.transport.CloseWithError(tunnel.CloseProtocol, "unexpected agent-opened stream")
}

func (e *Edge) handleControlMsg(sess *Session, typ string, line []byte) {
	switch typ {
	case proto.TypeRegister:
		var msg proto.Register
		if err := json.Unmarshal(line, &msg); err != nil {
			sess.send(&proto.Error{Type: proto.TypeError, Code: proto.CodeBadMessage, Message: err.Error()})
			return
		}
		name := msg.Name
		if name == "" {
			name = naming.LabelFromPort(msg.Port)
		}
		if err := naming.ValidateLabel(name); err != nil {
			sess.send(&proto.Error{Type: proto.TypeError, Code: proto.CodeInvalidName, Name: name, Message: err.Error()})
			return
		}
		url, perr := e.register(sess, name)
		if perr != nil {
			perr.Name = name // correlate this error to the register that caused it
			sess.send(perr)
			return
		}
		sess.send(&proto.Registered{Type: proto.TypeRegistered, Name: name, URL: url})

	case proto.TypeUnregister:
		var msg proto.Unregister
		if err := json.Unmarshal(line, &msg); err != nil {
			sess.send(&proto.Error{Type: proto.TypeError, Code: proto.CodeBadMessage, Message: err.Error()})
			return
		}
		e.unregister(sess, msg.Name)

	case proto.TypeHeartbeat:
		sess.mu.Lock()
		sess.lastHeartbeat = time.Now()
		sess.mu.Unlock()

	default:
		sess.send(&proto.Error{Type: proto.TypeError, Code: proto.CodeUnknownMsg, Message: "unknown type: " + typ})
	}
}

func (sess *Session) send(msg any) error {
	return sess.sendContext(context.Background(), msg)
}

func (sess *Session) sendContext(ctx context.Context, msg any) error {
	return sess.writeControlContext(ctx, msg, true)
}

func (sess *Session) sendShutdownContext(ctx context.Context, msg any) error {
	return sess.writeControlContext(ctx, msg, false)
}

func (sess *Session) writeControlContext(ctx context.Context, msg any, closeProtocolOnFailure bool) error {
	sess.writeOnce.Do(func() {
		if sess.writeGate == nil {
			sess.writeGate = make(chan struct{}, 1)
		}
	})
	select {
	case sess.writeGate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-sess.transport.Done():
		return tunnel.ErrSessionClosed
	}
	defer func() { <-sess.writeGate }()
	if deadline, ok := ctx.Deadline(); ok {
		_ = sess.control.SetWriteDeadline(deadline)
		defer sess.control.SetWriteDeadline(time.Time{})
	}
	if err := proto.Write(sess.control, msg); err != nil {
		slog.Debug("control: write", "err", err.Error())
		if closeProtocolOnFailure {
			_ = sess.transport.CloseWithError(tunnel.CloseProtocol, "control write failed")
		}
		return err
	}
	return nil
}

func (e *Edge) register(sess *Session, name string) (string, *proto.Error) {
	// 63-char backstop: under the hyphen shape `<name>-<slug>` is ONE DNS label,
	// which must fit 63 chars. The edge is the cert authority, so reject here —
	// otherwise an over-long label fails cert issuance opaquely (url-model §7).
	if sess.slug != "" && e.cfg.Shape() == naming.ShapeHyphen {
		if len(name)+1+len(sess.slug) > 63 {
			return "", &proto.Error{
				Type: proto.TypeError, Code: proto.CodeInvalidName,
				Message: fmt.Sprintf(
					"name too long for this workspace URL (%s-%s exceeds 63 chars) — shorten it or use a custom domain",
					name, sess.slug),
			}
		}
	}

	// The full hostname set (default-shape per bound slug + custom domains) and
	// the primary host to render. Fetched outside the lock (may do a network call).
	hosts, primary := e.hostsForTunnel(name, sess.slug)

	e.mu.Lock()
	defer e.mu.Unlock()

	// Idempotent re-registration from the same session.
	sess.mu.Lock()
	_, already := sess.names[name]
	sess.mu.Unlock()
	if already {
		return "https://" + primary, nil
	}

	// Conflict check across ALL hosts. A host held by an already-dead session
	// (its dropSession is racing this register) is reclaimable — we'd rather
	// show stable URLs than spurious `name_taken` during reconnect-with-replay.
	// Track the distinct dead tunnels we reclaim: we fully evict them below (so
	// their dropSession won't decrement activeTunnels for them) and net the gauge
	// by len(displaced) when we add this tunnel.
	type displacedKey struct {
		sess *Session
		name string
	}
	displaced := make(map[displacedKey]struct{})
	for _, h := range hosts {
		existing, ok := e.routes[h]
		if !ok || existing.session == sess {
			continue
		}
		if existing.session.transport.IsClosed() {
			displaced[displacedKey{existing.session, existing.name}] = struct{}{}
			continue
		}
		return "", &proto.Error{
			Type: proto.TypeError, Code: proto.CodeNameTaken,
			Message: h + " is taken",
		}
	}

	// Reclaim each distinct dead tunnel we're displacing BEFORE the cap check and
	// the overwrite: drop ALL of its remaining routes and clear its session
	// bookkeeping now. This does two things: (1) the per-token cap below counts
	// only genuinely-live tunnels, so a reconnect racing the dead session's
	// dropSession isn't spuriously rejected with `over_limit`; (2) the dead
	// session's pending dropSession finds nothing of its own to decrement, so it
	// can't double-decrement activeTunnels for a tunnel we've already taken over
	// (any of its hosts NOT in our new set — e.g. an alias dropped between two
	// scope-hostnames fetches — would otherwise survive as a stale route and drift
	// the gauge below the true live count).
	for dk := range displaced {
		for h, r := range e.routes {
			if r.session == dk.sess && r.name == dk.name {
				delete(e.routes, h)
				delete(e.proxies, h)
			}
		}
		dk.sess.mu.Lock()
		delete(dk.sess.names, dk.name)
		delete(dk.sess.hosts, dk.name)
		dk.sess.mu.Unlock()
	}

	// Per-token (per-slug) tunnel cap, counted as DISTINCT tunnels (not route
	// entries — a tunnel now has several with multi-host). PRD §12.
	if max := e.cfg.MaxTunnelsPerToken; max > 0 {
		live := 0
		for s := range e.sessions {
			if s.slug == sess.slug {
				s.mu.Lock()
				live += len(s.names)
				s.mu.Unlock()
			}
		}
		if live >= max {
			return "", &proto.Error{
				Type: proto.TypeError, Code: proto.CodeOverLimit,
				Message: fmt.Sprintf("max %d tunnels per token", max),
			}
		}
	}

	for _, h := range hosts {
		e.routes[h] = &Route{session: sess, name: name}
	}
	sess.mu.Lock()
	sess.names[name] = struct{}{}
	sess.hosts[name] = hosts
	sess.mu.Unlock()
	// Net: +1 for this tunnel, −1 per distinct dead tunnel we reclaimed (each fully
	// evicted above, so dropSession won't decrement it again).
	e.metrics.activeTunnels.Add(1 - int64(len(displaced)))
	slog.Info("registered", "slug", sess.slug, "name", name, "host", primary, "hosts", len(hosts))
	return "https://" + primary, nil
}

// RouteHosts returns every hostname currently in the route table (test helper —
// lets tests assert multi-host registration without TLS/cert concerns).
func (e *Edge) RouteHosts() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]string, 0, len(e.routes))
	for h := range e.routes {
		out = append(out, h)
	}
	return out
}

func (e *Edge) unregister(sess *Session, name string) {
	sess.mu.Lock()
	hosts := sess.hosts[name]
	sess.mu.Unlock()
	if len(hosts) == 0 {
		hosts = []string{naming.Hostname(name, sess.slug, e.cfg.BaseDomain, e.cfg.Shape())}
	}

	e.mu.Lock()
	removed := false
	for _, h := range hosts {
		if r, ok := e.routes[h]; ok && r.session == sess {
			delete(e.routes, h)
			delete(e.proxies, h) // keep the proxy cache from growing unboundedly
			removed = true
		}
	}
	if removed {
		e.metrics.activeTunnels.Add(-1) // one tunnel, regardless of host count
	}
	e.mu.Unlock()

	sess.mu.Lock()
	delete(sess.names, name)
	delete(sess.hosts, name)
	sess.mu.Unlock()
	slog.Info("unregistered", "slug", sess.slug, "name", name)
}

func (e *Edge) dropSession(sess *Session) {
	// Close stream admission before removing routes or waiting. This prevents a
	// zero-counter Wait from racing a late request that captured the old route.
	sess.stopStreamAdmission()
	e.mu.Lock()
	if _, was := e.sessions[sess]; was {
		delete(e.sessions, sess)
		e.metrics.activeSessions.Add(-1)
	}
	// Count DISTINCT tunnels (names), not route entries — a tunnel may answer on
	// several hosts under multi-host registration.
	removedNames := make(map[string]struct{})
	for host, r := range e.routes {
		if r.session == sess {
			delete(e.routes, host)
			delete(e.proxies, host) // keep the proxy cache from growing unboundedly
			removedNames[r.name] = struct{}{}
		}
	}
	if len(removedNames) > 0 {
		e.metrics.activeTunnels.Add(-int64(len(removedNames)))
	}
	e.mu.Unlock()
	if !sess.transport.IsClosed() {
		_ = sess.transport.CloseWithError(tunnel.CloseNormal, "session dropped")
	}
	// A successfully authenticated session owns its capacity slot until the
	// transport's entire child tree is terminal. Releasing on a wall-clock
	// timeout would allow a slow adapter to exceed the configured session cap.
	<-sess.transport.Done()
	sess.waitHelpers()
	sess.waitStreamWatchers()
	sess.authRelease.Do(func() {
		<-e.authSlots
		e.metrics.addSessionState(sess.kind, "authenticated", -1)
	})
	closeReason := classifySessionClose(sess.transport.CloseInfo())
	e.metrics.recordSessionClose(sess.kind, closeReason)
	slog.Info("session dropped",
		"event", "session_closed",
		"session_id", sess.id,
		"transport", sess.kind,
		"slug", sess.slug,
		"remote_addr", sess.remote,
		"handshake_ms", sess.handshake.Milliseconds(),
		"active_streams", sess.activeStreams.Load(),
		"close_reason", closeReason,
		"error_category", closeReason,
	)
}

func (e *Edge) heartbeatWatch(ctx context.Context, sess *Session) {
	interval := e.heartbeatTimeout / 6
	if interval < 50*time.Millisecond {
		interval = 50 * time.Millisecond
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-sess.transport.Done():
			return
		case <-tick.C:
			sess.mu.Lock()
			since := time.Since(sess.lastHeartbeat)
			sess.mu.Unlock()
			if since > e.heartbeatTimeout {
				slog.Warn("session: heartbeat timeout", "slug", sess.slug, "since", since)
				_ = sess.transport.CloseWithError(tunnel.CloseIdle, "heartbeat timeout")
				return
			}
		}
	}
}

func (e *Edge) handlePublic(c net.Conn) {
	listener := &singleConnListener{conn: c}
	srv := &http.Server{
		Handler:        http.HandlerFunc(e.handler),
		MaxHeaderBytes: 1 << 20, // 1 MiB
		// Slow-loris guard: a peer must finish request headers promptly and
		// can't hold an idle keep-alive conn forever. Deliberately NO
		// ReadTimeout/WriteTimeout — tunneled dev apps stream (SSE, HMR
		// websockets, long polls) and a whole-request/response clock would
		// sever them mid-flight.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// Serve blocks in the listener's second Accept so this server remains
		// tracked for the accepted connection's full lifetime. Wake that
		// Accept only after net/http is finished with the connection. Returning
		// the original *tls.Conn from the listener is required for net/http to
		// dispatch a negotiated h2 connection to its HTTP/2 implementation.
		ConnState: func(_ net.Conn, state http.ConnState) {
			if state == http.StateClosed || state == http.StateHijacked {
				_ = listener.Close()
			}
		},
	}
	e.lifecycleMu.Lock()
	if e.shuttingDown {
		e.lifecycleMu.Unlock()
		_ = c.Close()
		return
	}
	e.mu.Lock()
	e.pubSrvs[srv] = struct{}{}
	e.mu.Unlock()
	e.lifecycleMu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.pubSrvs, srv)
		e.mu.Unlock()
	}()
	if err := srv.Serve(listener); err != nil && !errors.Is(err, errListenerExhausted) && !errors.Is(err, http.ErrServerClosed) {
		slog.Debug("public conn ended", "err", err.Error())
	}
}

// requestTarget is the path we record in a request event. It is the
// percent-ENCODED path (r.URL.EscapedPath), NOT the decoded r.URL.Path:
//
//   - Decoding is what caused the reqlog wedge: net/http turns a scanner's
//     "%00" into a raw 0x00 in r.URL.Path, which Postgres text can't store —
//     failing the control plane's bulk insert and, via the shipper's
//     cursor-advances-only-on-2xx retry, blocking ingest forever
//     (ops/incident-reqlog-nul-wedge.md, fix #2). EscapedPath keeps "%00"
//     (and every other control byte / non-ASCII byte) percent-encoded, so a
//     raw control byte never reaches the wire.
//   - It records what the client actually sent ("/x%00y"), better forensics
//     than a silently-mangled path.
//   - The query string is intentionally dropped (EscapedPath omits it): query
//     strings carry secrets and must not be logged (request-events-spec §5).
func requestTarget(r *http.Request) string {
	return r.URL.EscapedPath()
}

func (e *Edge) handler(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/healthz":
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status":  "ok",
			"version": e.version,
		})
		return
	case "/metrics":
		// Operator-only. The dump names every slug, tunnel, and byte count, so
		// it is (a) served only on the bare base domain — never on tunnel
		// hostnames, so a tunneled app keeps its own /metrics path — and (b)
		// authenticated by a bearer token inside handleMetrics. On any other
		// host, fall through to normal routing.
		if e.isBaseHost(r.Host) {
			e.handleMetrics(w, r)
			return
		}
	case "/.well-known/beam-auth":
		e.handleAuthDiscovery(w, r)
		return
	}

	rr := &responseRecorder{ResponseWriter: w}
	start := time.Now()
	var finishOrdinaryProxy func()

	// Count request bytes (bytes_in), then cap body size. Oversized bodies
	// produce HTTP 413 via http.MaxBytesReader's error response.
	var bodyCount *countingReader
	var proxyBody io.ReadCloser
	if r.Body != nil {
		bodyCount = &countingReader{rc: r.Body}
		proxyBody = bodyCount
		if cap := e.cfg.MaxRequestBodyBytes; cap > 0 {
			proxyBody = http.MaxBytesReader(rr, bodyCount, cap)
		}
	}

	// Routes are keyed by the bare hostname. Strip any port from the Host
	// header — present when the edge serves on a non-standard port or sits
	// behind a proxy — so lookups don't miss. Browsers omit :443, so this is a
	// no-op in the common production case.
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}

	e.mu.RLock()
	route := e.routes[host]
	e.mu.RUnlock()

	slug := ""
	if route != nil {
		slug = route.session.slug
	}
	meta := e.metaFor(host, slug, r.Method, requestTarget(r), r.RemoteAddr, r.UserAgent(), r.Referer(), start)
	declaredBodyTooLarge := e.cfg.MaxRequestBodyBytes > 0 &&
		r.ContentLength > e.cfg.MaxRequestBodyBytes

	if route == nil {
		http.Error(rr, "no route for host "+host, http.StatusNotFound)
	} else if declaredBodyTooLarge {
		// Reject a known oversized request before opening a tunnel stream. The
		// MaxBytesReader below remains authoritative for chunked/unknown-length
		// bodies whose actual size can only be discovered while reading.
		http.Error(rr, "request body too large", http.StatusRequestEntityTooLarge)
	} else {
		if !e.beginProxy() {
			rr.Header().Set("Content-Type", "application/json")
			rr.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(rr, `{"error":"edge shutting down"}`+"\n")
		} else {
			var proxyDone sync.Once
			finishProxy := func() { proxyDone.Do(e.proxyWG.Done) }
			finishOrdinaryProxy = finishProxy

			// On a WebSocket/upgrade, transfer active-proxy ownership to the
			// hijacked connection. Its Close removes it from the shutdown set
			// and decrements the wait group exactly once.
			rr.wrapHijack = func(c net.Conn) (net.Conn, bool) {
				e.lifecycleMu.Lock()
				if e.shuttingDown {
					e.lifecycleMu.Unlock()
					_ = c.Close()
					return c, false
				}
				var tracked net.Conn
				tracked = e.startWSHeartbeat(meta, c, func() {
					e.lifecycleMu.Lock()
					delete(e.hijacked, tracked)
					e.lifecycleMu.Unlock()
					finishProxy()
				})
				e.hijacked[tracked] = struct{}{}
				e.lifecycleMu.Unlock()
				return tracked, true
			}
			// net/http.Transport deliberately detaches DialContext
			// cancellation from the request once dialing begins, but preserves
			// context values. Carry the original visitor context explicitly so
			// stream setup and the leased stream retain request cancellation
			// semantics instead of the dial-only context.
			proxyRequest := r.WithContext(context.WithValue(
				r.Context(),
				visitorRequestContextKey{},
				r.Context(),
			))
			// Keep r.Body itself untouched. net/http's HTTP/1.1 server uses its
			// concrete body type to detect a final response sent before request
			// EOF and invoke its RST-avoidance half-close path. Counting and
			// limiting only the cloned outbound request preserves that server
			// lifecycle behavior while ReverseProxy sees the same wrapped body.
			proxyRequest.Body = proxyBody
			e.proxyFor(host).ServeHTTP(rr, proxyRequest)
		}
	}

	if rr.status == 0 {
		rr.status = http.StatusOK
	}
	e.metrics.recordRequest(rr.status)

	outcome := reqlog.OutcomeOK
	switch {
	case route == nil:
		outcome = reqlog.OutcomeNoRoute
	case rr.status == http.StatusRequestEntityTooLarge:
		outcome = reqlog.OutcomeSizeLimit
	case rr.status >= 500:
		outcome = reqlog.OutcomeBackendError
	}

	// A hijacked (WS) connection's events — incl. the final one — are emitted by
	// its heartbeat goroutine; the handler only emits for ordinary requests.
	if rr.hijackedConn == nil {
		var bytesIn int64
		if bodyCount != nil {
			bytesIn = bodyCount.Count()
		}
		// TTFB reflects the *backend's* first byte; a no_route 404 is the edge's
		// own response, so omit it there (spec §3).
		firstByte := rr.firstByteAt
		if outcome == reqlog.OutcomeNoRoute {
			firstByte = time.Time{}
		}
		e.emitRequest(meta, rr.status, outcome, bytesIn, rr.bytes, firstByte)
		if finishOrdinaryProxy != nil {
			// Keep shutdown ownership until the terminal request event is
			// committed. This is essential for an upgrade that hijacked just
			// after shutdown began: no HTTP server owns that handler anymore,
			// so proxyWG is the only join preventing sink teardown from racing
			// the ordinary fallback event.
			finishOrdinaryProxy()
		}
	}

	slog.Debug("request",
		"host", r.Host,
		"method", r.Method,
		"status", rr.status,
		"bytes", rr.bytes,
		"duration_ms", time.Since(start).Milliseconds(),
		"slug", slug,
		"outcome", outcome,
	)
}

func (e *Edge) beginProxy() bool {
	e.lifecycleMu.Lock()
	defer e.lifecycleMu.Unlock()
	if e.shuttingDown {
		return false
	}
	e.proxyWG.Add(1)
	return true
}

// bearerTokenOK reports whether the request carries `Authorization: Bearer
// <want>` with a constant-time-equal token.
func bearerTokenOK(r *http.Request, want string) bool {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return false
	}
	got := strings.TrimPrefix(h, prefix)
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// isBaseHost reports whether the request Host (port stripped) is the bare
// base domain — the operator surface, as opposed to a tunnel hostname.
func (e *Edge) isBaseHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	return strings.EqualFold(host, e.cfg.BaseDomain)
}

func (e *Edge) handleMetrics(w http.ResponseWriter, r *http.Request) {
	// Auth. No token configured → the endpoint is disabled (404) so a
	// misconfigured deploy never exposes the dump. A configured token must be
	// presented as `Authorization: Bearer <token>`, compared in constant time.
	token := e.cfg.MetricsToken
	if token == "" {
		http.NotFound(w, r)
		return
	}
	if !bearerTokenOK(r, token) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="beamd-metrics"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	e.metrics.writeText(w, int64(e.certs.IssuanceCount()))
	e.traffic.writeMetrics(w)
	// Per-request event drops (sink backpressure) — loss is observable.
	if d, ok := e.reqSink.(interface{ Dropped() int64 }); ok {
		fmt.Fprintln(w, "# HELP beam_requests_dropped_total Per-request events dropped under sink backpressure.")
		fmt.Fprintln(w, "# TYPE beam_requests_dropped_total counter")
		fmt.Fprintf(w, "beam_requests_dropped_total %d\n", d.Dropped())
	}
}

// handleAuthDiscovery returns the device-code endpoints the hosted
// web app exposes, so `beamd login` (no-token mode) knows where to
// run the browser-based flow. In OSS deployments these fields are
// empty and the CLI falls back to requiring --token.
func (e *Edge) handleAuthDiscovery(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(e.cfg.AuthDiscovery)
}

func (e *Edge) proxyFor(host string) *httputil.ReverseProxy {
	e.mu.RLock()
	p := e.proxies[host]
	e.mu.RUnlock()
	if p != nil {
		return p
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if p := e.proxies[host]; p != nil {
		return p
	}

	p = &httputil.ReverseProxy{
		// Rewrite (not Director): the edge is the SOLE trusted hop — it
		// terminates public TLS on :443 itself — so every inbound forwarding /
		// client-IP header is attacker-controlled and must be discarded, not
		// trusted. Rewrite strips them before this runs (and, unlike Director,
		// isn't undone by hop-by-hop removal, so a client can't delete our
		// values via `Connection:`). SetXForwarded then sets X-Forwarded-For/
		// Host/Proto fresh from the real connection.
		//
		// WARNING: this is correct ONLY while the edge is the first hop. If it
		// is ever placed behind a trusted proxy/CDN, this stripping would throw
		// away the real client IP — revisit before fronting the edge.
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetXForwarded()
			// SetXForwarded covers only the standard X-Forwarded-* trio. Strip
			// the non-standard client-IP conventions a backend might trust too;
			// the list is curated (an allowlist is impossible), so add to it if
			// a tunneled app trusts another vendor header.
			for _, h := range []string{
				"Forwarded",                // RFC 7239
				"Forwarded-For",            // legacy
				"X-Forwarded",              // legacy
				"X-Original-Forwarded-For", // some LBs
				"X-Real-Ip",                // nginx convention
				"Client-Ip",                // PHP HTTP_CLIENT_IP — checked BEFORE XFF by the common idiom
				"True-Client-Ip",           // Akamai / Cloudflare Enterprise
				"Cf-Connecting-Ip",         // Cloudflare
				"Fastly-Client-Ip",         // Fastly
				"X-Client-Ip",              // Heroku / others
				"X-Cluster-Client-Ip",      // some LBs
				"Proxy-Client-Ip",          // WebLogic/JBoss family
				"Wl-Proxy-Client-Ip",       // WebLogic
				"X-Proxyuser-Ip",           // Google front ends
			} {
				pr.Out.Header.Del(h)
			}
			// The edge always terminates TLS, so the backend's scheme is https
			// regardless of what SetXForwarded inferred.
			pr.Out.Header.Set("X-Forwarded-Proto", "https")
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = host
			if _, ok := pr.Out.Header["User-Agent"]; !ok {
				pr.Out.Header.Set("User-Agent", "")
			}
		},
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				visitorCtx, ok := ctx.Value(visitorRequestContextKey{}).(context.Context)
				if !ok {
					return nil, errors.New("proxy dial missing visitor request context")
				}
				return e.openRouteStream(visitorCtx, host)
			},
			DisableKeepAlives: true,
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() != nil {
				return
			}
			var bodyTooLarge *http.MaxBytesError
			if errors.As(err, &bodyTooLarge) {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if errors.Is(err, tunnel.ErrCapacity) {
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, `{"error":"tunnel capacity reached"}`+"\n")
				return
			}
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, `{"error":"bad gateway"}`+"\n")
		},
	}
	// When preview embedding is enabled, strip headers that would block
	// the tunnel from being iframed cross-origin in a consumer app.
	// proxyFor only ever serves tunnel hosts, so no host check is needed.
	if e.cfg.PreviewEmbed {
		p.ModifyResponse = stripFramingHeaders
	}
	e.proxies[host] = p
	return p
}

func (e *Edge) openRouteStream(ctx context.Context, host string) (net.Conn, error) {
	e.mu.RLock()
	route := e.routes[host]
	e.mu.RUnlock()
	if route == nil {
		return nil, fmt.Errorf("no route for %s", host)
	}
	return e.openRouteStreamForRoute(ctx, host, route)
}

// openRouteStreamForRoute owns admission after a route snapshot has been
// captured. Keeping the snapshot explicit makes the identity recheck below
// testable and documents that leases belong to that exact route/session, not
// merely whichever route happens to occupy the hostname later.
func (e *Edge) openRouteStreamForRoute(
	ctx context.Context,
	host string,
	route *Route,
) (net.Conn, error) {
	watcherReserved := e.reserveStreamWatcher(route.session)
	if !watcherReserved {
		return nil, tunnel.ErrSessionClosed
	}
	defer func() {
		if watcherReserved {
			e.releaseStreamWatcher(route.session)
		}
	}()

	lease, err := e.acquireStreamLease(route.session)
	if err != nil {
		return nil, err
	}
	releaseBeforeStream := true
	defer func() {
		if releaseBeforeStream {
			lease.release()
		}
	}()

	e.mu.RLock()
	current := e.routes[host]
	stillCurrent := current == route &&
		current != nil &&
		current.session == route.session &&
		current.name == route.name
	e.mu.RUnlock()
	if !stillCurrent {
		return nil, errStaleRoute
	}
	if route.session.transport.IsClosed() {
		return nil, tunnel.ErrSessionClosed
	}

	stream, err := route.session.transport.OpenStream(ctx)
	if err != nil {
		if ctx.Err() == nil {
			e.metrics.recordStreamOpenError(route.session.kind, classifyStreamOpenError(err))
		}
		return nil, fmt.Errorf("open stream: %w", err)
	}
	slug, name := route.session.slug, route.name
	conn := newLeasedConn(
		ctx,
		stream,
		func(in, out int64) { e.recordTraffic(slug, name, in, out) },
		func() {
			lease.release()
			e.releaseStreamWatcher(route.session)
		},
	)
	releaseBeforeStream = false
	watcherReserved = false

	prefixDeadline := time.Now().Add(5 * time.Second)
	if callerDeadline, ok := ctx.Deadline(); ok && callerDeadline.Before(prefixDeadline) {
		prefixDeadline = callerDeadline
	}
	_ = stream.SetWriteDeadline(prefixDeadline)
	_, prefixErr := io.WriteString(stream, route.name+"\n")
	_ = stream.SetWriteDeadline(time.Time{})
	if prefixErr != nil {
		stream.Abort(tunnel.StreamCanceled)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("write name prefix: %w", prefixErr)
	}
	if err := ctx.Err(); err != nil {
		stream.Abort(tunnel.StreamCanceled)
		return nil, err
	}

	return conn, nil
}

func classifyStreamOpenError(err error) string {
	switch {
	case errors.Is(err, tunnel.ErrOpenTimeout), errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, tunnel.ErrSessionClosed), errors.Is(err, net.ErrClosed):
		return "closed"
	default:
		return "other"
	}
}

// stripFramingHeaders removes the response headers that prevent a page
// from being embedded in an iframe on another origin. Wired in as the
// proxy's ModifyResponse only when preview_embed is set.
func stripFramingHeaders(resp *http.Response) error {
	resp.Header.Del("X-Frame-Options")
	for _, h := range []string{"Content-Security-Policy", "Content-Security-Policy-Report-Only"} {
		if csp := resp.Header.Get(h); csp != "" {
			if relaxed := stripFrameAncestors(csp); relaxed == "" {
				resp.Header.Del(h)
			} else {
				resp.Header.Set(h, relaxed)
			}
		}
	}
	return nil
}

// stripFrameAncestors returns csp with any `frame-ancestors` directive
// removed, leaving the rest of the policy intact. Returns "" if nothing
// is left (so the caller can drop the header entirely).
func stripFrameAncestors(csp string) string {
	var kept []string
	for _, directive := range strings.Split(csp, ";") {
		d := strings.TrimSpace(directive)
		if d == "" {
			continue
		}
		name := d
		if i := strings.IndexAny(d, " \t"); i >= 0 {
			name = d[:i]
		}
		if strings.EqualFold(name, "frame-ancestors") {
			continue
		}
		kept = append(kept, d)
	}
	return strings.Join(kept, "; ")
}
