package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/treyhuffine/conduit/internal/client"
	"github.com/treyhuffine/conduit/internal/daemon"
	"github.com/treyhuffine/conduit/internal/mcp"
)

// connectClientWithOpts is connectClient but lets the test pick its
// own reconnect/heartbeat cadence.
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

func startDaemon(t *testing.T, c *client.Client) *daemon.LocalClient {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "daemon.sock")
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

func TestM5_DaemonExposeAndList(t *testing.T) {
	port := startDummyApp(t, "api")
	_, edgeAddr := startEdge(t, map[string]string{"T1": "trey"})

	c := connectClient(t, edgeAddr, "T1")
	lc := startDaemon(t, c)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	url, err := lc.Expose(ctx, port, "api")
	if err != nil {
		t.Fatalf("Expose: %v", err)
	}
	wantURL := "https://api.trey." + testBaseDomain
	if url != wantURL {
		t.Errorf("url = %q, want %q", url, wantURL)
	}

	// Verify the URL actually serves.
	host := "api.trey." + testBaseDomain
	checkResponse(t, publicHTTPSClient(edgeAddr, host), "https://"+host+"/x", "api: GET /x\n")

	items, err := lc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("List returned %d items, want 1", len(items))
	}
	if items[0].Name != "api" || items[0].Port != port || !items[0].Healthy {
		t.Errorf("List item = %+v", items[0])
	}

	if err := lc.Unexpose(ctx, "api"); err != nil {
		t.Fatalf("Unexpose: %v", err)
	}
	items, err = lc.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Errorf("after unexpose, list returned %d items", len(items))
	}
}

func TestM5_HealthzReportsSlug(t *testing.T) {
	_, edgeAddr := startEdge(t, map[string]string{"T1": "trey"})
	c := connectClient(t, edgeAddr, "T1")
	lc := startDaemon(t, c)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	h, err := lc.Ping(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if h.Slug != "trey" {
		t.Errorf("slug = %q, want trey", h.Slug)
	}
	if !h.Healthy {
		t.Errorf("healthy = false, want true")
	}
}

func TestM5_ReconnectReplaysRegistration(t *testing.T) {
	port := startDummyApp(t, "api")
	e, edgeAddr := startEdge(t, map[string]string{"T1": "trey"})

	c := connectClientWithOpts(t, edgeAddr, "T1", client.Options{
		HeartbeatInterval: 100 * time.Millisecond,
		RegisterTimeout:   2 * time.Second,
		ReconnectInitial:  50 * time.Millisecond,
		ReconnectMax:      200 * time.Millisecond,
	})

	url, err := c.Register("api", port)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	host := "api.trey." + testBaseDomain
	checkResponse(t, publicHTTPSClient(edgeAddr, host), "https://"+host+"/before", "api: GET /before\n")

	// Yank every active session — simulates an edge restart from the
	// client's perspective. The client's manage() goroutine should
	// detect the close, reconnect with backoff, and replay the register.
	before := e.SessionsCreatedTotal()
	e.CloseAllSessions()

	// Wait until the edge has accepted a *new* session AND its route
	// has been replayed. Using SessionsCreatedTotal rather than
	// SessionCount avoids the race where the old session's cleanup
	// hasn't run yet and the old (1,1,true) state looks identical to
	// the post-reconnect state.
	waitUntil(t, "edge to see reconnect", 3*time.Second, func() bool {
		return e.SessionsCreatedTotal() > before && e.RouteCount() == 1 && c.IsHealthy()
	})

	// Replayed registration → same URL serves again.
	checkResponse(t, publicHTTPSClient(edgeAddr, host), "https://"+host+"/after", "api: GET /after\n")
	_ = url
}

func TestM5_MCPRoundTrip(t *testing.T) {
	port := startDummyApp(t, "api")
	_, edgeAddr := startEdge(t, map[string]string{"T1": "trey"})
	c := connectClient(t, edgeAddr, "T1")
	lc := startDaemon(t, c)

	in := &bytes.Buffer{}
	out := &bytes.Buffer{}

	// Pre-fill input with all the requests we want to make; the server
	// reads to EOF on bytes.Buffer.
	writeJSONRPC(in, "i1", "initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "test", "version": "0"},
	})
	writeJSONRPC(in, nil, "notifications/initialized", nil)
	writeJSONRPC(in, "l1", "tools/list", nil)
	writeJSONRPC(in, "c1", "tools/call", map[string]any{
		"name": "expose_port",
		"arguments": map[string]any{
			"port": port,
			"name": "api",
		},
	})

	srv := mcp.New(lc, in, out, "conduit", "test")
	if err := srv.Run(context.Background()); err != nil {
		t.Fatalf("mcp run: %v", err)
	}

	// Parse responses (3: init, tools/list, tools/call — no response
	// for the notification).
	dec := json.NewDecoder(out)
	var resps []map[string]any
	for {
		var r map[string]any
		if err := dec.Decode(&r); err != nil {
			break
		}
		resps = append(resps, r)
	}
	if len(resps) != 3 {
		t.Fatalf("got %d responses, want 3:\n%s", len(resps), out.String())
	}

	// initialize response: serverInfo.name = "conduit"
	init := resps[0]["result"].(map[string]any)
	server := init["serverInfo"].(map[string]any)
	if server["name"] != "conduit" {
		t.Errorf("serverInfo.name = %v, want conduit", server["name"])
	}

	// tools/list response: 3 tools
	toolsResp := resps[1]["result"].(map[string]any)
	tools := toolsResp["tools"].([]any)
	if len(tools) != 3 {
		t.Errorf("tools/list returned %d tools, want 3", len(tools))
	}

	// tools/call response: content text starts with https://
	callResp := resps[2]["result"].(map[string]any)
	content := callResp["content"].([]any)
	if len(content) == 0 {
		t.Fatal("tools/call returned empty content")
	}
	first := content[0].(map[string]any)
	text, _ := first["text"].(string)
	wantPrefix := "https://api.trey." + testBaseDomain
	if text != wantPrefix {
		t.Errorf("tools/call result = %q, want %q", text, wantPrefix)
	}

	// And the URL actually serves.
	host := "api.trey." + testBaseDomain
	checkResponse(t, publicHTTPSClient(edgeAddr, host), "https://"+host+"/m", "api: GET /m\n")
}

// -- Test helpers --

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
	w.Write(b)
	w.Write([]byte("\n"))
}

// Silence the unused-import warning on http; used by helpers.
var _ = http.StatusOK
var _ = fmt.Sprintf
