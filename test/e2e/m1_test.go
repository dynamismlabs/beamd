package e2e

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/treyhuffine/conduit/internal/auth"
	"github.com/treyhuffine/conduit/internal/certs"
	"github.com/treyhuffine/conduit/internal/client"
	"github.com/treyhuffine/conduit/internal/config"
	"github.com/treyhuffine/conduit/internal/edge"
)

const testBaseDomain = "test.example.com"

func TestM1_SingleTunnel(t *testing.T) {
	dummyPort := startDummyApp(t, "dummy")

	e, edgeAddr := startEdge(t, map[string]string{"T1": "trey"})

	c := connectClient(t, edgeAddr, "T1")
	url, err := c.Register("hardcoded", dummyPort)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	wantURL := "https://hardcoded.trey." + testBaseDomain
	if url != wantURL {
		t.Errorf("url = %q, want %q", url, wantURL)
	}

	host := "hardcoded.trey." + testBaseDomain
	httpClient := publicHTTPSClient(edgeAddr, host)

	resp, err := httpClient.Get("https://" + host + "/foo")
	if err != nil {
		t.Fatalf("GET /foo: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if want := "dummy: GET /foo\n"; string(body) != want {
		t.Errorf("body = %q, want %q", string(body), want)
	}

	resp2, err := httpClient.Get("https://" + host + "/bar")
	if err != nil {
		t.Fatalf("GET /bar: %v", err)
	}
	defer resp2.Body.Close()
	body2, _ := io.ReadAll(resp2.Body)
	if want := "dummy: GET /bar\n"; string(body2) != want {
		t.Errorf("second body = %q, want %q", string(body2), want)
	}

	// /healthz bypasses the route table.
	resp3, err := httpClient.Get("https://" + host + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Errorf("healthz status = %d, want 200", resp3.StatusCode)
	}

	_ = e
}

// --- shared helpers ---

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
