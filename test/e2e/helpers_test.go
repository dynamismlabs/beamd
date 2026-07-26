package e2e

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/dynamismlabs/beamd/internal/auth"
	"github.com/dynamismlabs/beamd/internal/certs"
	"github.com/dynamismlabs/beamd/internal/client"
	"github.com/dynamismlabs/beamd/internal/config"
	"github.com/dynamismlabs/beamd/internal/daemon"
	"github.com/dynamismlabs/beamd/internal/edge"
)

const testBaseDomain = "test.example.com"

// testMetricsToken enables the operator /metrics endpoint on test edges.
const testMetricsToken = "test-metrics-token"

// e2eTransportEnv lets CI run the entire behavioral suite once with a forced
// TCP tunnel and once with a forced QUIC tunnel. Keeping this separate from
// BEAMD_TRANSPORT avoids changing process-wide configuration tests; spawned
// CLI clients receive the selected mode through writeCLIConfig.
const e2eTransportEnv = "BEAMD_E2E_TRANSPORT"

func e2eTransport(t *testing.T) string {
	t.Helper()
	transport := os.Getenv(e2eTransportEnv)
	if transport == "" {
		return "tcp"
	}
	if transport != "tcp" && transport != "quic" {
		t.Fatalf("%s=%q: want tcp or quic", e2eTransportEnv, transport)
	}
	return transport
}

// applyE2ETransport makes directly constructed edge configs explicit. In
// particular, non-zero Part B capacities are required to opt a direct config
// into QUIC, and QUIC key material must stay inside the test's temp directory.
func applyE2ETransport(t *testing.T, cfg *config.Server) {
	t.Helper()
	if cfg.MaxStreamsPerSession == 0 {
		cfg.MaxStreamsPerSession = config.DefaultMaxStreamsPerSession
	}
	if cfg.MaxStreamsTotal == 0 {
		cfg.MaxStreamsTotal = config.DefaultMaxStreamsTotal
	}
	if cfg.MaxPreAuthSessions == 0 {
		cfg.MaxPreAuthSessions = config.DefaultMaxPreAuthSessions
	}
	if cfg.MaxSessionsTotal == 0 {
		cfg.MaxSessionsTotal = config.DefaultMaxSessionsTotal
	}

	cfg.DisableQUIC = e2eTransport(t) != "quic"
	if cfg.DisableQUIC {
		return
	}
	cfg.ListenQUIC = cfg.ListenHTTPS
	if cfg.DataDir == "" {
		cfg.DataDir = t.TempDir()
	}
}

func forceE2EClientTransport(t *testing.T, opts client.Options) client.Options {
	t.Helper()
	opts.Transport = e2eTransport(t)
	return opts
}

// getMetrics scrapes /metrics on the base domain with the operator bearer
// token. Fails the test on transport error.
func getMetrics(t *testing.T, hc *http.Client) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "https://"+testBaseDomain+"/metrics", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testMetricsToken)
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	return resp
}

// ---------------------------------------------------------------------
// Edge setup
// ---------------------------------------------------------------------

func startEdge(t *testing.T, tokens map[string]string) (*edge.Edge, string) {
	t.Helper()
	edgeAddr := freeListenAddr(t)
	cfg := &config.Server{
		BaseDomain:         testBaseDomain,
		URLShape:           "subdomain",
		ListenHTTPS:        edgeAddr,
		ACMEEmail:          "test@example.com",
		DNSProvider:        "stub",
		TokenStore:         "memory:",
		MaxTunnelsPerToken: 25,
		MetricsToken:       testMetricsToken,
	}
	applyE2ETransport(t, cfg)
	mgr, err := certs.NewSelfSignedManager(cfg.BaseDomain)
	if err != nil {
		t.Fatalf("cert manager: %v", err)
	}
	e := edge.New(cfg, "test", auth.NewMemoryStore(tokens), mgr)
	go func() { _ = e.Serve() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		_ = e.Shutdown(ctx)
	})
	waitForTCP(t, edgeAddr, 2*time.Second)
	return e, edgeAddr
}

// startEdgeWithCertMgr is like startEdge but exposes the cert manager
// so tests can assert issuance counts.
func startEdgeWithCertMgr(t *testing.T, tokens map[string]string) (*edge.Edge, *certs.SelfSignedManager, string) {
	t.Helper()
	edgeAddr := freeListenAddr(t)
	cfg := &config.Server{
		BaseDomain:         testBaseDomain,
		URLShape:           "subdomain",
		ListenHTTPS:        edgeAddr,
		ACMEEmail:          "test@example.com",
		DNSProvider:        "stub",
		TokenStore:         "memory:",
		MaxTunnelsPerToken: 25,
		MetricsToken:       testMetricsToken,
	}
	applyE2ETransport(t, cfg)
	mgr, err := certs.NewSelfSignedManager(cfg.BaseDomain)
	if err != nil {
		t.Fatalf("cert manager: %v", err)
	}
	e := edge.New(cfg, "test", auth.NewMemoryStore(tokens), mgr)
	go func() { _ = e.Serve() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		_ = e.Shutdown(ctx)
	})
	waitForTCP(t, edgeAddr, 2*time.Second)
	return e, mgr, edgeAddr
}

// startEdgeCfg is like startEdge but lets a test tweak the server config
// before the edge starts (e.g. to flip preview_embed on).
func startEdgeCfg(t *testing.T, tokens map[string]string, mutate func(*config.Server)) (*edge.Edge, string) {
	t.Helper()
	edgeAddr := freeListenAddr(t)
	cfg := &config.Server{
		BaseDomain:         testBaseDomain,
		URLShape:           "subdomain",
		ListenHTTPS:        edgeAddr,
		ACMEEmail:          "test@example.com",
		DNSProvider:        "stub",
		TokenStore:         "memory:",
		MaxTunnelsPerToken: 25,
		MetricsToken:       testMetricsToken,
	}
	if mutate != nil {
		mutate(cfg)
	}
	applyE2ETransport(t, cfg)
	mgr, err := certs.NewSelfSignedManager(cfg.BaseDomain)
	if err != nil {
		t.Fatalf("cert manager: %v", err)
	}
	e := edge.New(cfg, "test", auth.NewMemoryStore(tokens), mgr)
	go func() { _ = e.Serve() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		_ = e.Shutdown(ctx)
	})
	waitForTCP(t, edgeAddr, 2*time.Second)
	return e, edgeAddr
}

// startFramingBackend brings up a backend that sets iframe-blocking
// headers (X-Frame-Options + a CSP with frame-ancestors alongside an
// unrelated directive). Returns its port.
func startFramingBackend(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("framing backend listen: %v", err)
	}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Frame-Options", "DENY")
			w.Header().Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'")
			fmt.Fprint(w, "ok")
		}),
	}
	go srv.Serve(ln)
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().(*net.TCPAddr).Port
}

// ---------------------------------------------------------------------
// Client setup
// ---------------------------------------------------------------------

func connectClient(t *testing.T, edgeAddr, token string) *client.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, err := client.Connect(ctx, edgeAddr, token, forceE2EClientTransport(t, client.Options{
		HeartbeatInterval:  200 * time.Millisecond,
		RegisterTimeout:    2 * time.Second,
		InsecureSkipVerify: true, // test edges are self-signed
	}))
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// connectClientWithOpts lets tests pick their own reconnect/heartbeat
// cadence (e.g. faster timeouts for reconnect tests, slower for
// "observe a disconnected state" tests).
func connectClientWithOpts(t *testing.T, edgeAddr, token string, opts client.Options) *client.Client {
	t.Helper()
	opts.InsecureSkipVerify = true // test edges are self-signed
	opts = forceE2EClientTransport(t, opts)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, err := client.Connect(ctx, edgeAddr, token, opts)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// ---------------------------------------------------------------------
// Daemon setup
// ---------------------------------------------------------------------

func startDaemon(t *testing.T, c *client.Client) *daemon.LocalClient {
	t.Helper()
	// Don't use t.TempDir() — it embeds the full test name in the path,
	// and macOS caps unix socket paths at 104 chars. /tmp/cd-XXXX.sock
	// stays well under that for any test name.
	f, err := os.CreateTemp("/tmp", "cd-*.sock")
	if err != nil {
		t.Fatalf("create temp socket: %v", err)
	}
	socket := f.Name()
	_ = f.Close()
	_ = os.Remove(socket) // daemon creates the socket itself
	t.Cleanup(func() { _ = os.Remove(socket) })

	d := daemon.New(c, socket)
	go func() { _ = d.Serve() }()
	t.Cleanup(func() { _ = d.Shutdown(context.Background()) })

	lc := daemon.NewLocalClient(socket)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		_, err := lc.Ping(ctx)
		cancel()
		if err == nil {
			return lc
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("daemon never came up at %s", socket)
	return nil
}

// ---------------------------------------------------------------------
// Dummy backend
// ---------------------------------------------------------------------

// startDummyApp brings up a tiny HTTP server that echoes "<name>: <method> <path>"
// on every request, listening on a random port. Returns the port.
func startDummyApp(t *testing.T, name string) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("dummy listen: %v", err)
	}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// GET /__hdrs reflects the forwarding / client-IP headers the
			// backend actually received, so tests can assert the edge scrubbed
			// client-supplied values. One "Header: value" per line.
			if r.URL.Path == "/__hdrs" {
				for _, h := range []string{
					"X-Forwarded-For", "X-Forwarded-Proto", "X-Forwarded-Host",
					"X-Real-Ip", "Forwarded", "True-Client-Ip", "Cf-Connecting-Ip",
					"Fastly-Client-Ip", "X-Client-Ip", "X-Cluster-Client-Ip",
					"Client-Ip", "Proxy-Client-Ip", "X-Original-Forwarded-For",
				} {
					fmt.Fprintf(w, "%s: %s\n", h, r.Header.Get(h))
				}
				return
			}
			fmt.Fprintf(w, "%s: %s %s\n", name, r.Method, r.URL.Path)
		}),
	}
	go srv.Serve(ln)
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().(*net.TCPAddr).Port
}

// ---------------------------------------------------------------------
// Network / response assertions
// ---------------------------------------------------------------------

func freeListenAddr(t *testing.T) string {
	t.Helper()
	for attempt := 0; attempt < 100; attempt++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("free TCP port: %v", err)
		}
		addr := ln.Addr().String()
		if e2eTransport(t) != "quic" {
			_ = ln.Close()
			return addr
		}

		// A TCP port can be free while the same numeric UDP port is still
		// occupied by a recently closed QUIC listener. Hold the TCP
		// reservation while checking UDP so forced-QUIC tests never hand the
		// edge an address that can bind only half of its listeners.
		packetConn, packetErr := net.ListenPacket("udp", addr)
		if packetErr == nil {
			_ = packetConn.Close()
			_ = ln.Close()
			return addr
		}
		_ = ln.Close()
	}
	t.Fatal("could not find a port available to both TCP and UDP")
	return ""
}

func waitForTCP(t *testing.T, addr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s to accept TCP", addr)
}

// waitUntil polls cond every 20ms until it returns true or timeout fires.
func waitUntil(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// publicHTTPSClient is an http.Client wired to hit the edge at edgeAddr
// while presenting `sni` in its TLS handshake. Cert verification is
// skipped (we self-sign in tests).
func publicHTTPSClient(edgeAddr, sni string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return tls.Dial("tcp", edgeAddr, &tls.Config{
					InsecureSkipVerify: true,
					ServerName:         sni,
					NextProtos:         []string{"http/1.1"},
				})
			},
			MaxIdleConnsPerHost: 100,
		},
		Timeout: 10 * time.Second,
	}
}

// checkResponse fails the test if `url` doesn't return `want` (status 200
// + exact body match).
func checkResponse(t *testing.T, hc *http.Client, url, want string) {
	t.Helper()
	if err := getAndCheck(hc, url, want); err != nil {
		t.Error(err)
	}
}

// getAndCheck returns an error rather than failing the test — used by
// concurrent loops where t.Error from N goroutines would be noisy.
func getAndCheck(hc *http.Client, url, want string) error {
	resp, err := hc.Get(url)
	if err != nil {
		return fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status = %d, body = %q", resp.StatusCode, body)
	}
	if string(body) != want {
		return fmt.Errorf("body = %q, want %q", string(body), want)
	}
	return nil
}

// ---------------------------------------------------------------------
// MCP test helpers
// ---------------------------------------------------------------------

// writeJSONRPC encodes a JSON-RPC 2.0 request to w (newline-delimited).
// Pass nil for id to make it a notification.
func writeJSONRPC(w io.Writer, id any, method string, params any) {
	msg := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
	}
	if id != nil {
		msg["id"] = id
	}
	if params != nil {
		msg["params"] = params
	}
	b, _ := json.Marshal(msg)
	_, _ = w.Write(b)
	_, _ = w.Write([]byte("\n"))
}

// Force-import packages used only by certain tests, so a partial test
// run via -run still typechecks the helper file cleanly.
var (
	_ = bytes.Buffer{}
)
