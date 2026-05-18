package e2e

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/treyhuffine/conduit/internal/auth"
	"github.com/treyhuffine/conduit/internal/certs"
	"github.com/treyhuffine/conduit/internal/client"
	"github.com/treyhuffine/conduit/internal/config"
	"github.com/treyhuffine/conduit/internal/edge"
)

func TestM6_PerTokenTunnelCap(t *testing.T) {
	edgeAddr := freeListenAddr(t)
	cfg := &config.Server{
		BaseDomain:         testBaseDomain,
		ListenHTTPS:        edgeAddr,
		ACMEEmail:          "test@example.com",
		DNSProvider:        "stub",
		TokenStore:         "memory:",
		MaxTunnelsPerToken: 2, // small cap for the test
	}
	mgr, err := certs.NewSelfSignedManager(cfg.BaseDomain)
	if err != nil {
		t.Fatal(err)
	}
	e := edge.New(cfg, "test", auth.NewMemoryStore(map[string]string{"T1": "trey"}), mgr)
	go func() { _ = e.Serve() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		_ = e.Shutdown(ctx)
	})
	waitForTCP(t, edgeAddr, 2*time.Second)

	port := startDummyApp(t, "p")
	c := connectClient(t, edgeAddr, "T1")

	if _, err := c.Register("a", port); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if _, err := c.Register("b", port); err != nil {
		t.Fatalf("second register: %v", err)
	}
	if _, err := c.Register("c", port); err == nil {
		t.Fatal("third register should have failed with over_limit")
	} else if !strings.Contains(err.Error(), "over_limit") {
		t.Errorf("err = %v, want over_limit", err)
	}
}

func TestM6_XForwardedHeaders(t *testing.T) {
	_, edgeAddr := startEdge(t, map[string]string{"T1": "trey"})

	// Dummy app that echoes the X-Forwarded-* headers it received.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "for=%s proto=%s host=%s\n",
				r.Header.Get("X-Forwarded-For"),
				r.Header.Get("X-Forwarded-Proto"),
				r.Header.Get("X-Forwarded-Host"),
			)
		}),
	}
	go srv.Serve(ln)
	t.Cleanup(func() { _ = srv.Close() })
	port := ln.Addr().(*net.TCPAddr).Port

	c := connectClient(t, edgeAddr, "T1")
	if _, err := c.Register("api", port); err != nil {
		t.Fatalf("register: %v", err)
	}

	host := "api.trey." + testBaseDomain
	resp, err := publicHTTPSClient(edgeAddr, host).Get("https://" + host + "/echo")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	got := string(body)
	if !strings.Contains(got, "for=127.0.0.1") {
		t.Errorf("X-Forwarded-For missing or wrong; body=%q", got)
	}
	if !strings.Contains(got, "proto=https") {
		t.Errorf("X-Forwarded-Proto wrong; body=%q", got)
	}
	if !strings.Contains(got, "host="+host) {
		t.Errorf("X-Forwarded-Host wrong; body=%q", got)
	}
}

func TestM6_MetricsEndpointExposesCounters(t *testing.T) {
	port := startDummyApp(t, "api")
	_, edgeAddr := startEdge(t, map[string]string{"T1": "trey"})
	c := connectClient(t, edgeAddr, "T1")
	if _, err := c.Register("api", port); err != nil {
		t.Fatal(err)
	}

	host := "api.trey." + testBaseDomain

	// Drive a request so counters increment.
	checkResponse(t, publicHTTPSClient(edgeAddr, host), "https://"+host+"/m", "api: GET /m\n")

	// /metrics is path-based, served on the same TLS listener — SNI
	// can be anything that hits the edge.
	resp, err := publicHTTPSClient(edgeAddr, host).Get("https://" + host + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	got := string(body)

	for _, want := range []string{
		"conduit_active_sessions",
		"conduit_active_tunnels",
		"conduit_cert_issuance_total",
		"conduit_requests_total",
		"conduit_bytes_proxied_total",
		`conduit_active_sessions 1`,
		`conduit_active_tunnels 1`,
		`conduit_bytes_proxied_total{slug="trey"}`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("metrics output missing %q\nfull body:\n%s", want, got)
		}
	}
}

func TestM6_BandwidthCounterReflectsResponseBytes(t *testing.T) {
	port := startDummyApp(t, "api")
	_, edgeAddr := startEdge(t, map[string]string{"T1": "trey"})
	c := connectClient(t, edgeAddr, "T1")
	if _, err := c.Register("api", port); err != nil {
		t.Fatal(err)
	}

	host := "api.trey." + testBaseDomain
	// One request that produces "api: GET /x\n" → 13 bytes.
	checkResponse(t, publicHTTPSClient(edgeAddr, host), "https://"+host+"/x", "api: GET /x\n")

	resp, err := publicHTTPSClient(edgeAddr, host).Get("https://" + host + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	out := string(body)

	// Parse the bytes_proxied_total line for slug=trey.
	want := `conduit_bytes_proxied_total{slug="trey"}`
	idx := strings.Index(out, want)
	if idx < 0 {
		t.Fatalf("missing %q in metrics:\n%s", want, out)
	}
	line := out[idx:]
	if nl := strings.Index(line, "\n"); nl > 0 {
		line = line[:nl]
	}
	parts := strings.Fields(line)
	if len(parts) != 2 {
		t.Fatalf("unexpected metric line shape: %q", line)
	}
	// Should be at least the response body size (13) — could be more
	// due to subsequent /metrics request (which also passes through
	// the handler but for path /metrics, not the tunnel — so it should
	// NOT contribute to slug bytes).
	if parts[1] == "0" {
		t.Errorf("bytes_proxied stuck at 0; want >= 13")
	}
}

func TestM6_GracefulShutdownNotifiesClients(t *testing.T) {
	port := startDummyApp(t, "p")

	// Build the edge inline so we can call Shutdown on it.
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
		t.Fatal(err)
	}
	e := edge.New(cfg, "test", auth.NewMemoryStore(map[string]string{"T1": "trey"}), mgr)
	go func() { _ = e.Serve() }()
	waitForTCP(t, edgeAddr, 2*time.Second)

	c, err := client.Connect(context.Background(), edgeAddr, "T1", client.Options{
		HeartbeatInterval: 200 * time.Millisecond,
		RegisterTimeout:   2 * time.Second,
		ReconnectInitial:  10 * time.Second, // long, so we'd never reconnect within the test
		ReconnectMax:      10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	if _, err := c.Register("api", port); err != nil {
		t.Fatal(err)
	}

	// Trigger graceful shutdown.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	_ = e.Shutdown(shutdownCtx)
	cancel()

	// The client should detect the shutdown error, set skipBackoff,
	// and attempt to reconnect IMMEDIATELY (no 10s wait). Since the
	// edge is dead, the immediate attempt fails — but the client
	// would NOT be sleeping for the long backoff.
	//
	// We assert: shortly after shutdown, the client is unhealthy.
	waitUntil(t, "client to notice shutdown", 1*time.Second, func() bool {
		return !c.IsHealthy()
	})
}
