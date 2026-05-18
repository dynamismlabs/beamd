package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/treyhuffine/conduit/internal/client"
)

func TestM3_RegisterTwoTunnels(t *testing.T) {
	port1 := startDummyApp(t, "api")
	port2 := startDummyApp(t, "web")

	_, edgeAddr := startEdge(t, map[string]string{"T1": "trey"})
	c := connectClient(t, edgeAddr, "T1")

	url1, err := c.Register("api", port1)
	if err != nil {
		t.Fatalf("register api: %v", err)
	}
	url2, err := c.Register("web", port2)
	if err != nil {
		t.Fatalf("register web: %v", err)
	}

	wantAPI := "https://api.trey." + testBaseDomain
	wantWeb := "https://web.trey." + testBaseDomain
	if url1 != wantAPI {
		t.Errorf("url1 = %q, want %q", url1, wantAPI)
	}
	if url2 != wantWeb {
		t.Errorf("url2 = %q, want %q", url2, wantWeb)
	}

	host1 := "api.trey." + testBaseDomain
	host2 := "web.trey." + testBaseDomain
	checkResponse(t, publicHTTPSClient(edgeAddr, host1), "https://"+host1+"/x", "api: GET /x\n")
	checkResponse(t, publicHTTPSClient(edgeAddr, host2), "https://"+host2+"/y", "web: GET /y\n")
}

func TestM3_DefaultNameFromPort(t *testing.T) {
	port := startDummyApp(t, "p")
	_, edgeAddr := startEdge(t, map[string]string{"T1": "trey"})
	c := connectClient(t, edgeAddr, "T1")

	url, err := c.Register("", port)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if !strings.Contains(url, ".trey."+testBaseDomain) {
		t.Errorf("url %q missing slug+base", url)
	}
	// Port number becomes the label.
	if !strings.HasPrefix(url, "https://") {
		t.Errorf("missing https scheme: %q", url)
	}
}

func TestM3_InvalidName(t *testing.T) {
	port := startDummyApp(t, "p")
	_, edgeAddr := startEdge(t, map[string]string{"T1": "trey"})
	c := connectClient(t, edgeAddr, "T1")

	cases := []string{"Bad_Name", "API", "has.dot", "-leading", "trailing-", strings.Repeat("a", 64)}
	for _, name := range cases {
		_, err := c.Register(name, port)
		if err == nil {
			t.Errorf("register(%q) = nil error, want invalid_name", name)
			continue
		}
		if !strings.Contains(err.Error(), "invalid_name") {
			t.Errorf("register(%q) error = %v, want invalid_name", name, err)
		}
	}
}

func TestM3_BadToken(t *testing.T) {
	_, edgeAddr := startEdge(t, map[string]string{"T1": "trey"})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := client.Connect(ctx, edgeAddr, "wrong-token")
	if err == nil {
		t.Fatal("connect with wrong token should fail")
	}
	if !strings.Contains(err.Error(), "bad_token") {
		t.Errorf("err = %v, want bad_token", err)
	}
}

func TestM3_NameCollision(t *testing.T) {
	port := startDummyApp(t, "p")
	_, edgeAddr := startEdge(t, map[string]string{"T1": "trey"})

	// First client owns "api".
	c1 := connectClient(t, edgeAddr, "T1")
	if _, err := c1.Register("api", port); err != nil {
		t.Fatalf("c1 register: %v", err)
	}

	// Re-register from the same session is idempotent.
	if _, err := c1.Register("api", port); err != nil {
		t.Errorf("c1 re-register should be idempotent, got: %v", err)
	}

	// Second client (same slug, different session) collides.
	c2 := connectClient(t, edgeAddr, "T1")
	_, err := c2.Register("api", port)
	if err == nil {
		t.Fatal("c2 register should have failed with name_taken")
	}
	if !strings.Contains(err.Error(), "name_taken") {
		t.Errorf("c2 err = %v, want name_taken", err)
	}

	// c2 can register a different name fine.
	if _, err := c2.Register("web", port); err != nil {
		t.Errorf("c2 register web: %v", err)
	}
}

func TestM3_HeartbeatTimeoutDropsSession(t *testing.T) {
	port := startDummyApp(t, "p")
	e, edgeAddr := startEdge(t, map[string]string{"T1": "trey"})

	// Aggressive timeouts so the test finishes in well under a second.
	e.SetHeartbeatTimeout(200 * time.Millisecond)

	// Client heartbeat is LONGER than server timeout, so the watchdog
	// will drop the session. Reconnect-initial is long so the client
	// doesn't auto-reconnect inside the test window (we want to
	// observe the dropped state).
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, err := client.Connect(ctx, edgeAddr, "T1", client.Options{
		HeartbeatInterval: 10 * time.Second,
		RegisterTimeout:   2 * time.Second,
		ReconnectInitial:  10 * time.Second,
		ReconnectMax:      10 * time.Second,
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Close()

	if _, err := c.Register("api", port); err != nil {
		t.Fatalf("register: %v", err)
	}
	if e.RouteCount() != 1 {
		t.Errorf("route count = %d, want 1", e.RouteCount())
	}

	// Wait for the server-side watchdog to fire.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if e.SessionCount() == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := e.SessionCount(); got != 0 {
		t.Errorf("session count = %d, want 0 after heartbeat timeout", got)
	}
	if got := e.RouteCount(); got != 0 {
		t.Errorf("route count = %d, want 0 after session drop", got)
	}
}

func TestM3_HardCloseDropsSession(t *testing.T) {
	port := startDummyApp(t, "p")
	e, edgeAddr := startEdge(t, map[string]string{"T1": "trey"})

	c := connectClient(t, edgeAddr, "T1")
	if _, err := c.Register("api", port); err != nil {
		t.Fatalf("register: %v", err)
	}
	if e.SessionCount() != 1 {
		t.Fatalf("session count before close = %d", e.SessionCount())
	}

	_ = c.Close() // graceful close; yamux session ends → server drops

	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if e.SessionCount() == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := e.SessionCount(); got != 0 {
		t.Errorf("session count = %d, want 0 after client close", got)
	}
}

func TestM3_PreviousNameUsableAfterDrop(t *testing.T) {
	// After a client disconnects, its names should be free for the next
	// session to claim. This is the foundation for M5 reconnect-with-replay.
	port := startDummyApp(t, "p")
	e, edgeAddr := startEdge(t, map[string]string{"T1": "trey"})

	c1 := connectClient(t, edgeAddr, "T1")
	if _, err := c1.Register("api", port); err != nil {
		t.Fatalf("c1 register: %v", err)
	}
	_ = c1.Close()

	// Wait for drop.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if e.SessionCount() == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	c2 := connectClient(t, edgeAddr, "T1")
	if _, err := c2.Register("api", port); err != nil {
		t.Errorf("c2 register should succeed after c1 dropped: %v", err)
	}
}

