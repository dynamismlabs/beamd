// Package edge implements the public ingress for beamd.
//
// M3: TLS listener with ALPN demux. Client control connections speak
// the NDJSON protocol from PRD §8 on a dedicated yamux stream (the
// first stream the client opens). Routes are populated dynamically
// via `register`; each public request opens a fresh data stream and
// is proxied through it.
package edge

import (
	"bufio"
	"context"
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
	"time"

	"github.com/hashicorp/yamux"

	"github.com/dynamismlabs/beamd/internal/auth"
	"github.com/dynamismlabs/beamd/internal/certs"
	"github.com/dynamismlabs/beamd/internal/config"
	"github.com/dynamismlabs/beamd/internal/mux"
	"github.com/dynamismlabs/beamd/internal/naming"
	"github.com/dynamismlabs/beamd/internal/proto"
	"github.com/dynamismlabs/beamd/internal/reqlog"
)

const ALPNBeam = "beam/1"

// acmeTLS1 is the ACME TLS-ALPN-01 challenge protocol — advertised so certmagic
// can issue On-Demand custom-domain certs over this listener (url-model §8.2).
const acmeTLS1 = "acme-tls/1"

type Edge struct {
	cfg     *config.Server
	version string
	tokens  auth.Store
	certs   certs.Manager

	heartbeatTimeout time.Duration

	mu       sync.RWMutex
	ln       net.Listener
	sessions map[*Session]struct{}
	routes   map[string]*Route // hostname → route
	proxies  map[string]*httputil.ReverseProxy
	pubSrvs  map[*http.Server]struct{} // per-public-conn HTTP servers, tracked so Shutdown can drain them
	metrics  *metrics

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
}

type Session struct {
	yamux   *yamux.Session
	slug    string
	control *yamux.Stream

	writeMu sync.Mutex // serializes control-stream writes

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

func New(cfg *config.Server, version string, tokens auth.Store, certMgr certs.Manager) *Edge {
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
	return &Edge{
		cfg:              cfg,
		version:          version,
		tokens:           tokens,
		certs:            certMgr,
		heartbeatTimeout: 60 * time.Second,
		sessions:         make(map[*Session]struct{}),
		routes:           make(map[string]*Route),
		proxies:          make(map[string]*httputil.ReverseProxy),
		pubSrvs:          make(map[*http.Server]struct{}),
		metrics:          newMetrics(),
		traffic:          newTrafficStore(trafficPath(cfg)),
		reqSink:          reqlog.NopSink{},
		reqHeartbeat:     time.Duration(hbSec) * time.Second,
		capPath:          capPath,
		capClientIP:      capIP,
		capUserAgent:     capUA,
		capReferer:       capRef,
		ipTruncate:       cfg.RequestLog.IPMode != "off",
		hostnames:        newHostnamesClient(cfg.TokenStore),
		firstSession:     make(chan struct{}),
		shutdown:         make(chan struct{}),
	}
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
	tlsCfg := &tls.Config{
		GetCertificate: e.certs.GetCertificate,
		// `acme-tls/1` lets certmagic solve ACME TLS-ALPN-01 challenges over this
		// listener — how On-Demand custom-domain certs (url-model §8.2, path B)
		// get issued without a :80 listener. GetCertificate returns the challenge
		// cert for these handshakes; they carry no app data, so the conn handler
		// just sees them close.
		NextProtos: []string{ALPNBeam, acmeTLS1, "h2", "http/1.1"},
	}

	ln, err := tls.Listen("tcp", e.cfg.ListenHTTPS, tlsCfg)
	if err != nil {
		return fmt.Errorf("listen %s: %w", e.cfg.ListenHTTPS, err)
	}
	e.mu.Lock()
	e.ln = ln
	e.mu.Unlock()
	slog.Info("edge listening", "addr", e.cfg.ListenHTTPS)

	go e.flushTrafficPeriodically()

	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept: %w", err)
		}
		go e.handle(c)
	}
}

// Shutdown stops accepting new connections, notifies every client
// with `error{code:"shutdown"}` so they reconnect immediately, then
// drains in-flight public requests up to ctx's deadline before
// force-closing remaining sessions. Idempotent.
func (e *Edge) Shutdown(ctx context.Context) error {
	e.shutdownOnce.Do(func() { close(e.shutdown) })

	// Persist final bandwidth totals before we go.
	if err := e.traffic.Flush(); err != nil {
		slog.Warn("traffic store final flush failed", "err", err.Error())
	}

	e.mu.Lock()
	ln := e.ln
	e.ln = nil
	srvs := make([]*http.Server, 0, len(e.pubSrvs))
	for s := range e.pubSrvs {
		srvs = append(srvs, s)
	}
	sessions := make([]*Session, 0, len(e.sessions))
	for s := range e.sessions {
		sessions = append(sessions, s)
	}
	e.mu.Unlock()

	if ln != nil {
		_ = ln.Close()
	}

	for _, s := range sessions {
		s.send(&proto.Error{
			Type:    proto.TypeError,
			Code:    proto.CodeShutdown,
			Message: "edge shutting down",
		})
	}

	// Drain in-flight public requests via each public conn's
	// http.Server.Shutdown — runs concurrently.
	done := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		for _, srv := range srvs {
			wg.Add(1)
			go func(s *http.Server) {
				defer wg.Done()
				_ = s.Shutdown(ctx)
			}(srv)
		}
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}

	// Force-close any lingering yamux sessions.
	for _, s := range sessions {
		_ = s.yamux.Close()
	}
	return nil
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

// CloseAllSessions ends every active client session by closing its
// yamux session. Test helper for exercising the client's reconnect
// path — production code does NOT call this.
func (e *Edge) CloseAllSessions() {
	e.mu.RLock()
	sessions := make([]*Session, 0, len(e.sessions))
	for s := range e.sessions {
		sessions = append(sessions, s)
	}
	e.mu.RUnlock()
	for _, s := range sessions {
		_ = s.yamux.Close()
	}
}

func (e *Edge) handle(c net.Conn) {
	tlsConn, ok := c.(*tls.Conn)
	if !ok {
		_ = c.Close()
		return
	}
	if err := tlsConn.HandshakeContext(context.Background()); err != nil {
		slog.Debug("tls handshake failed", "err", err.Error())
		_ = c.Close()
		return
	}

	switch tlsConn.ConnectionState().NegotiatedProtocol {
	case ALPNBeam:
		e.handleClient(c)
	case acmeTLS1:
		// An ACME TLS-ALPN-01 challenge: validation happened during the
		// handshake (the challenge cert was served via GetCertificate). There's
		// no application data — just close.
		_ = c.Close()
	default:
		e.handlePublic(c)
	}
}

func (e *Edge) handleClient(c net.Conn) {
	yamuxSess, err := mux.Server(c)
	if err != nil {
		slog.Error("yamux server setup failed", "err", err.Error())
		_ = c.Close()
		return
	}
	defer yamuxSess.Close()

	control, err := yamuxSess.AcceptStream()
	if err != nil {
		slog.Debug("accept control stream", "err", err.Error())
		return
	}

	br := bufio.NewReader(control)
	typ, line, err := proto.Read(br)
	if err != nil || typ != proto.TypeHello {
		_ = proto.Write(control, &proto.Error{
			Type: proto.TypeError, Code: proto.CodeBadHello,
			Message: "first message must be hello",
		})
		return
	}

	var hello proto.Hello
	if err := json.Unmarshal(line, &hello); err != nil {
		_ = proto.Write(control, &proto.Error{
			Type: proto.TypeError, Code: proto.CodeBadHello, Message: err.Error(),
		})
		return
	}

	slug, ok := e.tokens.Resolve(hello.Token, hello.Scope)
	if !ok {
		msg := "invalid token"
		if hello.Scope != "" {
			msg = fmt.Sprintf("invalid token, or scope %q not available to this login", hello.Scope)
		}
		_ = proto.Write(control, &proto.Error{
			Type: proto.TypeError, Code: proto.CodeBadToken, Message: msg,
		})
		return
	}

	if err := proto.Write(control, &proto.HelloOK{
		Type: proto.TypeHelloOK, Slug: slug,
		BaseDomain: e.cfg.BaseDomain, Shape: string(e.cfg.Shape()), ProtoVersion: proto.ProtoVersion,
	}); err != nil {
		slog.Error("control: write hello_ok", "err", err.Error())
		return
	}

	sess := &Session{
		yamux:         yamuxSess,
		slug:          slug,
		control:       control,
		names:         make(map[string]struct{}),
		hosts:         make(map[string][]string),
		lastHeartbeat: time.Now(),
	}
	e.mu.Lock()
	e.sessions[sess] = struct{}{}
	e.mu.Unlock()
	e.metrics.activeSessions.Add(1)
	e.metrics.sessionsCreatedTotal.Add(1)
	e.firstSessionOnce.Do(func() { close(e.firstSession) })
	slog.Info("session opened", "slug", slug, "remote", c.RemoteAddr())

	defer e.dropSession(sess)

	hbCtx, hbCancel := context.WithCancel(context.Background())
	defer hbCancel()
	go e.heartbeatWatch(hbCtx, sess)

	for {
		typ, line, err := proto.Read(br)
		if err != nil {
			if err != io.EOF && !errors.Is(err, net.ErrClosed) {
				slog.Debug("control: read", "err", err.Error())
			}
			return
		}
		e.handleControlMsg(sess, typ, line)
	}
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
			sess.send(&proto.Error{Type: proto.TypeError, Code: proto.CodeInvalidName, Message: err.Error()})
			return
		}
		url, perr := e.register(sess, name)
		if perr != nil {
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

func (sess *Session) send(msg any) {
	sess.writeMu.Lock()
	defer sess.writeMu.Unlock()
	if err := proto.Write(sess.control, msg); err != nil {
		slog.Debug("control: write", "err", err.Error())
	}
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
		if existing.session.yamux.IsClosed() {
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
			removedNames[r.name] = struct{}{}
		}
	}
	if len(removedNames) > 0 {
		e.metrics.activeTunnels.Add(-int64(len(removedNames)))
	}
	e.mu.Unlock()
	_ = sess.yamux.Close()
	slog.Info("session dropped", "slug", sess.slug)
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
		case <-sess.yamux.CloseChan():
			return
		case <-tick.C:
			sess.mu.Lock()
			since := time.Since(sess.lastHeartbeat)
			sess.mu.Unlock()
			if since > e.heartbeatTimeout {
				slog.Warn("session: heartbeat timeout", "slug", sess.slug, "since", since)
				_ = sess.yamux.Close()
				return
			}
		}
	}
}

func (e *Edge) handlePublic(c net.Conn) {
	srv := &http.Server{
		Handler:        http.HandlerFunc(e.handler),
		MaxHeaderBytes: 1 << 20, // 1 MiB
	}
	e.mu.Lock()
	e.pubSrvs[srv] = struct{}{}
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.pubSrvs, srv)
		e.mu.Unlock()
	}()
	if err := srv.Serve(&singleConnListener{conn: c}); err != nil && !errors.Is(err, errListenerExhausted) && !errors.Is(err, http.ErrServerClosed) {
		slog.Debug("public conn ended", "err", err.Error())
	}
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
		e.handleMetrics(w, r)
		return
	case "/.well-known/beam-auth":
		e.handleAuthDiscovery(w, r)
		return
	}

	rr := &responseRecorder{ResponseWriter: w}
	start := time.Now()

	// Count request bytes (bytes_in), then cap body size. Oversized bodies
	// produce HTTP 413 via http.MaxBytesReader's error response.
	var bodyCount *countingReader
	if r.Body != nil {
		bodyCount = &countingReader{rc: r.Body}
		r.Body = bodyCount
		if cap := e.cfg.MaxRequestBodyBytes; cap > 0 {
			r.Body = http.MaxBytesReader(rr, bodyCount, cap)
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
	meta := e.metaFor(host, slug, r.Method, r.URL.Path, r.RemoteAddr, r.UserAgent(), r.Referer(), start)

	if route == nil {
		http.Error(rr, "no route for host "+host, http.StatusNotFound)
	} else {
		// On a WebSocket/upgrade, the bytes flow through the hijacked conn (not
		// the recorder); wrap it so they're counted + heartbeated.
		rr.wrapHijack = func(c net.Conn) net.Conn { return e.startWSHeartbeat(meta, c) }
		e.proxyFor(host).ServeHTTP(rr, r)
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
			bytesIn = bodyCount.n
		}
		// TTFB reflects the *backend's* first byte; a no_route 404 is the edge's
		// own response, so omit it there (spec §3).
		firstByte := rr.firstByteAt
		if outcome == reqlog.OutcomeNoRoute {
			firstByte = time.Time{}
		}
		e.emitRequest(meta, rr.status, outcome, bytesIn, rr.bytes, firstByte)
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

func (e *Edge) handleMetrics(w http.ResponseWriter, r *http.Request) {
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
		Director: func(req *http.Request) {
			req.URL.Scheme = "http"
			req.URL.Host = host

			if _, ok := req.Header["User-Agent"]; !ok {
				req.Header.Set("User-Agent", "")
			}

			// X-Forwarded-* per PRD §M6.
			if ip, _, err := net.SplitHostPort(req.RemoteAddr); err == nil && ip != "" {
				if prior := req.Header.Get("X-Forwarded-For"); prior != "" {
					req.Header.Set("X-Forwarded-For", prior+", "+ip)
				} else {
					req.Header.Set("X-Forwarded-For", ip)
				}
			}
			req.Header.Set("X-Forwarded-Proto", "https")
			req.Header.Set("X-Forwarded-Host", req.Host)
		},
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				e.mu.RLock()
				route := e.routes[host]
				e.mu.RUnlock()
				if route == nil {
					return nil, fmt.Errorf("no route for %s", host)
				}
				stream, err := route.session.yamux.OpenStream()
				if err != nil {
					return nil, fmt.Errorf("open stream: %w", err)
				}
				if _, err := fmt.Fprintf(stream, "%s\n", route.name); err != nil {
					_ = stream.Close()
					return nil, fmt.Errorf("write name prefix: %w", err)
				}
				// Wrap after the name prefix so only real app traffic is
				// counted. Reported per-tunnel on close (incl. WebSocket).
				slug, name := route.session.slug, route.name
				return &countingConn{
					Conn:    stream,
					onClose: func(in, out int64) { e.recordTraffic(slug, name, in, out) },
				}, nil
			},
			DisableKeepAlives: true,
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
