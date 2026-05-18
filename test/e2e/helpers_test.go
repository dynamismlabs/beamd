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

	"github.com/treyhuffine/conduit/internal/auth"
	"github.com/treyhuffine/conduit/internal/certs"
	"github.com/treyhuffine/conduit/internal/client"
	"github.com/treyhuffine/conduit/internal/config"
	"github.com/treyhuffine/conduit/internal/daemon"
	"github.com/treyhuffine/conduit/internal/edge"
)

const testBaseDomain = "test.example.com"

// ---------------------------------------------------------------------
// Edge setup
// ---------------------------------------------------------------------

func startEdge(t *testing.T, tokens map[string]string) (*edge.Edge, string) {
	t.Helper()
	edgeAddr := freeListenAddr(t)
	cfg := &config.Server{
		BaseDomain:         testBaseDomain,
		ListenHTTPS:        edgeAddr,
		ACMEEmail:          "test@example.com",
		DNSProvider:        "stub",
		TokenStore:         "memory:",
		MaxTunnelsPerToken: 25,
	}
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
		ListenHTTPS:        edgeAddr,
		ACMEEmail:          "test@example.com",
		DNSProvider:        "stub",
		TokenStore:         "memory:",
		MaxTunnelsPerToken: 25,
	}
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

// ---------------------------------------------------------------------
// Client setup
// ---------------------------------------------------------------------

func connectClient(t *testing.T, edgeAddr, token string) *client.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, err := client.Connect(ctx, edgeAddr, token, client.Options{
		HeartbeatInterval: 200 * time.Millisecond,
		RegisterTimeout:   2 * time.Second,
	})
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
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
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
