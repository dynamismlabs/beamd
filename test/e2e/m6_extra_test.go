package e2e

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/treyhuffine/conduit/internal/auth"
	"github.com/treyhuffine/conduit/internal/certs"
	"github.com/treyhuffine/conduit/internal/config"
	"github.com/treyhuffine/conduit/internal/edge"
)

// TestM6_WebSocketPassThrough verifies that WS upgrades flow through
// the full edge→yamux-stream→client→backend path. Architecturally
// supported by `httputil.ReverseProxy` + our `responseRecorder`'s
// Hijacker implementation; this proves it end-to-end.
func TestM6_WebSocketPassThrough(t *testing.T) {
	// Backend: a WS echo server.
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			for {
				mt, msg, err := conn.ReadMessage()
				if err != nil {
					return
				}
				if err := conn.WriteMessage(mt, append([]byte("echo: "), msg...)); err != nil {
					return
				}
			}
		}),
	}
	go srv.Serve(ln)
	t.Cleanup(func() { _ = srv.Close() })
	port := ln.Addr().(*net.TCPAddr).Port

	_, edgeAddr := startEdge(t, map[string]string{"T1": "trey"})
	c := connectClient(t, edgeAddr, "T1")
	if _, err := c.Register("ws", port); err != nil {
		t.Fatalf("register: %v", err)
	}

	host := "ws.trey." + testBaseDomain
	dialer := websocket.Dialer{
		HandshakeTimeout: 5 * time.Second,
		NetDialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return tls.Dial("tcp", edgeAddr, &tls.Config{
				InsecureSkipVerify: true,
				ServerName:         host,
				NextProtos:         []string{"http/1.1"},
			})
		},
	}

	wsConn, resp, err := dialer.DialContext(context.Background(), "wss://"+host+"/ws", nil)
	if err != nil {
		var bodyStr string
		if resp != nil {
			b, _ := io.ReadAll(resp.Body)
			bodyStr = string(b)
		}
		t.Fatalf("WS dial: %v (resp body: %s)", err, bodyStr)
	}
	defer wsConn.Close()

	if err := wsConn.WriteMessage(websocket.TextMessage, []byte("hello")); err != nil {
		t.Fatalf("WS write: %v", err)
	}
	_ = wsConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, msg, err := wsConn.ReadMessage()
	if err != nil {
		t.Fatalf("WS read: %v", err)
	}
	if string(msg) != "echo: hello" {
		t.Errorf("got %q, want %q", msg, "echo: hello")
	}

	// Round-trip a second message to confirm the duplex copy isn't
	// hanging up after the first frame.
	if err := wsConn.WriteMessage(websocket.TextMessage, []byte("again")); err != nil {
		t.Fatalf("WS write 2: %v", err)
	}
	_ = wsConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, msg, err = wsConn.ReadMessage()
	if err != nil {
		t.Fatalf("WS read 2: %v", err)
	}
	if string(msg) != "echo: again" {
		t.Errorf("second roundtrip: got %q, want %q", msg, "echo: again")
	}
}

// TestM6_RequestBodySizeLimit verifies oversized public request bodies
// are rejected by the edge before being forwarded over the tunnel.
func TestM6_RequestBodySizeLimit(t *testing.T) {
	port := startDummyApp(t, "api")

	// Tiny limit so we don't have to upload a real 32 MiB body.
	const limit = 1024
	edgeAddr := freeListenAddr(t)
	cfg := &config.Server{
		BaseDomain:          testBaseDomain,
		ListenHTTPS:         edgeAddr,
		ACMEEmail:           "test@example.com",
		DNSProvider:         "stub",
		TokenStore:          "memory:",
		MaxTunnelsPerToken:  25,
		MaxRequestBodyBytes: limit,
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

	c := connectClient(t, edgeAddr, "T1")
	if _, err := c.Register("api", port); err != nil {
		t.Fatal(err)
	}

	host := "api.trey." + testBaseDomain
	hc := publicHTTPSClient(edgeAddr, host)

	// Under-limit body works.
	small := bytes.Repeat([]byte("x"), 100)
	resp, err := hc.Post("https://"+host+"/echo", "application/octet-stream", bytes.NewReader(small))
	if err != nil {
		t.Fatalf("POST small: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("small body got status %d, want 200", resp.StatusCode)
	}

	// Over-limit body should be rejected.
	big := bytes.Repeat([]byte("x"), limit*4)
	resp2, err := hc.Post("https://"+host+"/echo", "application/octet-stream", bytes.NewReader(big))
	if err != nil {
		// Some TLS clients fail the request mid-write when the server
		// closes the conn after the 413 — accept that as evidence the
		// edge rejected it.
		return
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusRequestEntityTooLarge && resp2.StatusCode != http.StatusBadGateway {
		t.Errorf("oversized body got status %d, want 413 or 502", resp2.StatusCode)
	}
}

// TestM6_AuthDiscoveryEndpoint exercises /.well-known/conduit-auth in
// both the OSS shape (empty body → CLI requires --token) and the
// hosted shape (device-code URLs populated).
func TestM6_AuthDiscoveryEndpoint(t *testing.T) {
	t.Run("OSS returns empty discovery", func(t *testing.T) {
		_, edgeAddr := startEdge(t, map[string]string{"T1": "trey"})

		resp, err := publicHTTPSClient(edgeAddr, testBaseDomain).Get(
			"https://" + testBaseDomain + "/.well-known/conduit-auth")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		got := strings.TrimSpace(string(body))
		if got != "{}" {
			t.Errorf("OSS discovery body = %q, want %q", got, "{}")
		}
	})

	t.Run("hosted returns device-code URLs", func(t *testing.T) {
		edgeAddr := freeListenAddr(t)
		cfg := &config.Server{
			BaseDomain:         testBaseDomain,
			ListenHTTPS:        edgeAddr,
			ACMEEmail:          "test@example.com",
			DNSProvider:        "stub",
			TokenStore:         "memory:",
			MaxTunnelsPerToken: 25,
			AuthDiscovery: config.AuthDiscovery{
				DeviceCodeURL:   "https://app.example.com/api/device/code",
				TokenURL:        "https://app.example.com/api/device/token",
				VerificationURI: "https://app.example.com/device",
			},
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

		resp, err := publicHTTPSClient(edgeAddr, testBaseDomain).Get(
			"https://" + testBaseDomain + "/.well-known/conduit-auth")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		for _, want := range []string{
			"device_code_url",
			"token_url",
			"verification_uri",
			"https://app.example.com",
		} {
			if !strings.Contains(string(body), want) {
				t.Errorf("hosted discovery body missing %q:\n%s", want, string(body))
			}
		}
	})
}

// silence unused-import warning if helpers above evolve
var _ = fmt.Sprintf
