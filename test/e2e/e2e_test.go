// Package e2e holds the end-to-end test suite for beam. Tests are
// grouped by subject in the order they exercise the system:
//
//   - Tunnel routing — basic single/multi-tunnel + unknown-host
//   - Control protocol — register / hello / heartbeat / collisions / caps
//   - Certificates — per-slug wildcard reuse + DNS provisioning
//   - Daemon + MCP + reconnect — the client-side surface
//   - Proxy correctness — headers, body limits, WebSocket
//   - Observability — metrics, structured shutdown
//   - Auth discovery — hosted-mode bootstrap (`/.well-known/beam-auth`)
//
// Shared test infrastructure lives in helpers_test.go.
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
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/dynamismlabs/beamd/internal/auth"
	"github.com/dynamismlabs/beamd/internal/certs"
	"github.com/dynamismlabs/beamd/internal/client"
	"github.com/dynamismlabs/beamd/internal/config"
	"github.com/dynamismlabs/beamd/internal/dns"
	"github.com/dynamismlabs/beamd/internal/edge"
	"github.com/dynamismlabs/beamd/internal/mcp"
)

// ====================================================================
// Tunnel routing
// ====================================================================

func TestTunnel_FlatTokenServesAtBaseDomain(t *testing.T) {
	dummyPort := startDummyApp(t, "dummy")
	// Empty slug → flat routing: tunnels live directly at <name>.<base>.
	_, edgeAddr := startEdge(t, map[string]string{"T1": ""})

	c := connectClient(t, edgeAddr, "T1")
	if c.Slug() != "" {
		t.Errorf("flat slug = %q, want empty", c.Slug())
	}
	url, err := c.Register("hello", dummyPort)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if want := "https://hello." + testBaseDomain; url != want {
		t.Errorf("flat url = %q, want %q (no slug level)", url, want)
	}

	// Public request routes through AND the *.<base> cert is served on the
	// flat SNI (no namespace label).
	host := "hello." + testBaseDomain
	checkResponse(t, publicHTTPSClient(edgeAddr, host), "https://"+host+"/foo", "dummy: GET /foo\n")
}

func TestTunnel_SingleRegisteredAppServesPublicURL(t *testing.T) {
	dummyPort := startDummyApp(t, "dummy")
	_, edgeAddr := startEdge(t, map[string]string{"T1": "turing"})

	c := connectClient(t, edgeAddr, "T1")
	url, err := c.Register("hardcoded", dummyPort)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	wantURL := "https://hardcoded.turing." + testBaseDomain
	if url != wantURL {
		t.Errorf("url = %q, want %q", url, wantURL)
	}

	host := "hardcoded.turing." + testBaseDomain
	hc := publicHTTPSClient(edgeAddr, host)

	resp, err := hc.Get("https://" + host + "/foo")
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

	resp2, err := hc.Get("https://" + host + "/bar")
	if err != nil {
		t.Fatalf("GET /bar: %v", err)
	}
	defer resp2.Body.Close()
	body2, _ := io.ReadAll(resp2.Body)
	if want := "dummy: GET /bar\n"; string(body2) != want {
		t.Errorf("second body = %q, want %q", string(body2), want)
	}

	// /healthz bypasses the route table.
	resp3, err := hc.Get("https://" + host + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Errorf("healthz status = %d, want 200", resp3.StatusCode)
	}
}

// A Host header carrying a port — which happens when the edge serves on a
// non-:443 port or sits behind a proxy — must still route. The edge strips the
// port before the route lookup (browsers omit :443, so this is invisible in
// production but breaks local/proxied setups without the strip).
func TestTunnel_HostHeaderWithPortRoutes(t *testing.T) {
	dummyPort := startDummyApp(t, "ported")
	_, edgeAddr := startEdge(t, map[string]string{"T1": "turing"})

	c := connectClient(t, edgeAddr, "T1")
	if _, err := c.Register("svc", dummyPort); err != nil {
		t.Fatalf("register: %v", err)
	}

	host := "svc.turing." + testBaseDomain
	hc := publicHTTPSClient(edgeAddr, host)
	resp, err := hc.Get("https://" + host + ":8443/foo") // port in the Host header
	if err != nil {
		t.Fatalf("GET with port in Host: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 (a port in Host must still route)", resp.StatusCode)
	}
	if want := "ported: GET /foo\n"; string(body) != want {
		t.Errorf("body = %q, want %q", string(body), want)
	}
}

func TestTunnel_TwoBackendsConcurrentOverOneSession(t *testing.T) {
	port1 := startDummyApp(t, "app1")
	port2 := startDummyApp(t, "app2")

	_, edgeAddr := startEdge(t, map[string]string{"T1": "turing"})
	c := connectClient(t, edgeAddr, "T1")

	if _, err := c.Register("app1", port1); err != nil {
		t.Fatalf("register app1: %v", err)
	}
	if _, err := c.Register("app2", port2); err != nil {
		t.Fatalf("register app2: %v", err)
	}

	host1 := "app1.turing." + testBaseDomain
	host2 := "app2.turing." + testBaseDomain
	hc1 := publicHTTPSClient(edgeAddr, host1)
	hc2 := publicHTTPSClient(edgeAddr, host2)

	checkResponse(t, hc1, "https://"+host1+"/foo", "app1: GET /foo\n")
	checkResponse(t, hc2, "https://"+host2+"/bar", "app2: GET /bar\n")
	checkResponse(t, hc1, "https://"+host1+"/x", "app1: GET /x\n")
	checkResponse(t, hc2, "https://"+host2+"/x", "app2: GET /x\n")

	// 100 concurrent requests across both backends; assert no goroutine leak.
	gBefore := runtime.NumGoroutine()

	const perBackend = 50
	var wg sync.WaitGroup
	errs := make(chan error, perBackend*2)

	for i := 0; i < perBackend; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			path := fmt.Sprintf("/a%d", i)
			want := fmt.Sprintf("app1: GET %s\n", path)
			if err := getAndCheck(hc1, "https://"+host1+path, want); err != nil {
				errs <- fmt.Errorf("app1 %d: %w", i, err)
			}
		}(i)
		go func(i int) {
			defer wg.Done()
			path := fmt.Sprintf("/b%d", i)
			want := fmt.Sprintf("app2: GET %s\n", path)
			if err := getAndCheck(hc2, "https://"+host2+path, want); err != nil {
				errs <- fmt.Errorf("app2 %d: %w", i, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	hc1.CloseIdleConnections()
	hc2.CloseIdleConnections()
	time.Sleep(200 * time.Millisecond)

	gAfter := runtime.NumGoroutine()
	if gAfter > gBefore+30 {
		t.Errorf("possible goroutine leak: before=%d after=%d", gBefore, gAfter)
	}
}

func TestEdge_UnknownHostReturns404(t *testing.T) {
	dummyPort := startDummyApp(t, "dummy")

	_, edgeAddr := startEdge(t, map[string]string{"T1": "turing"})
	c := connectClient(t, edgeAddr, "T1")
	if _, err := c.Register("known", dummyPort); err != nil {
		t.Fatalf("register: %v", err)
	}

	resp, err := publicHTTPSClient(edgeAddr, "unknown.host").Get("https://unknown.host/foo")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// ====================================================================
// Control protocol — register / hello / heartbeat / collisions / caps
// ====================================================================

func TestRegister_TwoNamesProduceWorkingURLs(t *testing.T) {
	port1 := startDummyApp(t, "api")
	port2 := startDummyApp(t, "web")

	_, edgeAddr := startEdge(t, map[string]string{"T1": "turing"})
	c := connectClient(t, edgeAddr, "T1")

	url1, err := c.Register("api", port1)
	if err != nil {
		t.Fatalf("register api: %v", err)
	}
	url2, err := c.Register("web", port2)
	if err != nil {
		t.Fatalf("register web: %v", err)
	}

	if want := "https://api.turing." + testBaseDomain; url1 != want {
		t.Errorf("url1 = %q, want %q", url1, want)
	}
	if want := "https://web.turing." + testBaseDomain; url2 != want {
		t.Errorf("url2 = %q, want %q", url2, want)
	}

	host1 := "api.turing." + testBaseDomain
	host2 := "web.turing." + testBaseDomain
	checkResponse(t, publicHTTPSClient(edgeAddr, host1), "https://"+host1+"/x", "api: GET /x\n")
	checkResponse(t, publicHTTPSClient(edgeAddr, host2), "https://"+host2+"/y", "web: GET /y\n")
}

func TestRegister_DerivesNameFromPortWhenOmitted(t *testing.T) {
	port := startDummyApp(t, "p")
	_, edgeAddr := startEdge(t, map[string]string{"T1": "turing"})
	c := connectClient(t, edgeAddr, "T1")

	url, err := c.Register("", port)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	label := fmt.Sprintf("%d", port)
	if want := "https://" + label + ".turing." + testBaseDomain; url != want {
		t.Errorf("url = %q, want %q", url, want)
	}
	// Regression: the edge sends the *derived* name on each data stream, so
	// the client must key its backend under that label (not the empty name
	// it was called with) — otherwise public requests 502 "no backend".
	host := label + ".turing." + testBaseDomain
	checkResponse(t, publicHTTPSClient(edgeAddr, host), "https://"+host+"/x", "p: GET /x\n")
}

func TestRegister_RejectsInvalidNames(t *testing.T) {
	port := startDummyApp(t, "p")
	_, edgeAddr := startEdge(t, map[string]string{"T1": "turing"})
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

func TestRegister_CrossSessionCollisionReturnsNameTaken(t *testing.T) {
	port := startDummyApp(t, "p")
	_, edgeAddr := startEdge(t, map[string]string{"T1": "turing"})

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

func TestRegister_NameReusableAfterSessionDrops(t *testing.T) {
	port := startDummyApp(t, "p")
	e, edgeAddr := startEdge(t, map[string]string{"T1": "turing"})

	c1 := connectClient(t, edgeAddr, "T1")
	if _, err := c1.Register("api", port); err != nil {
		t.Fatalf("c1 register: %v", err)
	}
	_ = c1.Close()

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

func TestRegister_PerSlugTunnelCapReturnsOverLimit(t *testing.T) {
	edgeAddr := freeListenAddr(t)
	cfg := &config.Server{
		BaseDomain:         testBaseDomain,
		ListenHTTPS:        edgeAddr,
		ACMEEmail:          "test@example.com",
		DNSProvider:        "stub",
		TokenStore:         "memory:",
		MaxTunnelsPerToken: 2,
	}
	mgr, err := certs.NewSelfSignedManager(cfg.BaseDomain)
	if err != nil {
		t.Fatal(err)
	}
	e := edge.New(cfg, "test", auth.NewMemoryStore(map[string]string{"T1": "turing"}), mgr)
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

func TestHello_RejectsBadToken(t *testing.T) {
	_, edgeAddr := startEdge(t, map[string]string{"T1": "turing"})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := client.Connect(ctx, edgeAddr, "wrong-token", client.Options{InsecureSkipVerify: true})
	if err == nil {
		t.Fatal("connect with wrong token should fail")
	}
	if !strings.Contains(err.Error(), "bad_token") {
		t.Errorf("err = %v, want bad_token", err)
	}
}

func TestHeartbeat_TimeoutDropsSession(t *testing.T) {
	port := startDummyApp(t, "p")
	e, edgeAddr := startEdge(t, map[string]string{"T1": "turing"})

	e.SetHeartbeatTimeout(200 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, err := client.Connect(ctx, edgeAddr, "T1", client.Options{
		HeartbeatInterval:  10 * time.Second,
		RegisterTimeout:    2 * time.Second,
		InsecureSkipVerify: true,
		ReconnectInitial:   10 * time.Second,
		ReconnectMax:       10 * time.Second,
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

func TestSession_ClientCloseDropsRoutes(t *testing.T) {
	port := startDummyApp(t, "p")
	e, edgeAddr := startEdge(t, map[string]string{"T1": "turing"})

	c := connectClient(t, edgeAddr, "T1")
	if _, err := c.Register("api", port); err != nil {
		t.Fatalf("register: %v", err)
	}
	if e.SessionCount() != 1 {
		t.Fatalf("session count before close = %d", e.SessionCount())
	}

	_ = c.Close()

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

// ====================================================================
// Certificates
// ====================================================================

// Headline cert acceptance: first request under a slug issues a wildcard;
// a second app under the same slug serves over TLS with no new issuance.
func TestCerts_ReuseSameWildcardAcrossAppsUnderSlug(t *testing.T) {
	port1 := startDummyApp(t, "api")
	port2 := startDummyApp(t, "web")

	_, mgr, edgeAddr := startEdgeWithCertMgr(t, map[string]string{"T1": "turing"})
	c := connectClient(t, edgeAddr, "T1")

	if _, err := c.Register("api", port1); err != nil {
		t.Fatalf("register api: %v", err)
	}
	if _, err := c.Register("web", port2); err != nil {
		t.Fatalf("register web: %v", err)
	}

	host1 := "api.turing." + testBaseDomain
	host2 := "web.turing." + testBaseDomain

	checkResponse(t, publicHTTPSClient(edgeAddr, host1), "https://"+host1+"/x", "api: GET /x\n")
	if got := mgr.IssuanceCount(); got != 1 {
		t.Errorf("after first slug request, issuance = %d, want 1", got)
	}

	checkResponse(t, publicHTTPSClient(edgeAddr, host2), "https://"+host2+"/y", "web: GET /y\n")
	if got := mgr.IssuanceCount(); got != 1 {
		t.Errorf("after second app under same slug, issuance = %d, want 1 (cert should be reused)", got)
	}
}

// Cache key is the slug, not the apex.
func TestCerts_DistinctSlugsGetDistinctWildcards(t *testing.T) {
	port := startDummyApp(t, "p")

	_, mgr, edgeAddr := startEdgeWithCertMgr(t, map[string]string{
		"T1": "turing",
		"T2": "hopper",
	})

	c1 := connectClient(t, edgeAddr, "T1")
	if _, err := c1.Register("api", port); err != nil {
		t.Fatalf("c1 register: %v", err)
	}
	host1 := "api.turing." + testBaseDomain
	checkResponse(t, publicHTTPSClient(edgeAddr, host1), "https://"+host1+"/x", "p: GET /x\n")
	if got := mgr.IssuanceCount(); got != 1 {
		t.Errorf("after slug turing, issuance = %d, want 1", got)
	}

	c2 := connectClient(t, edgeAddr, "T2")
	if _, err := c2.Register("api", port); err != nil {
		t.Fatalf("c2 register: %v", err)
	}
	host2 := "api.hopper." + testBaseDomain
	checkResponse(t, publicHTTPSClient(edgeAddr, host2), "https://"+host2+"/y", "p: GET /y\n")
	if got := mgr.IssuanceCount(); got != 2 {
		t.Errorf("after distinct slug hopper, issuance = %d, want 2", got)
	}
}

// Two clients with different tokens but the same slug share one cert.
// Foreshadows the multi-laptop / reconnect-with-replay flow.
func TestCerts_DifferentTokensSameSlugShareWildcard(t *testing.T) {
	port := startDummyApp(t, "p")

	_, mgr, edgeAddr := startEdgeWithCertMgr(t, map[string]string{
		"T1": "turing",
		"T2": "turing",
	})

	c1 := connectClient(t, edgeAddr, "T1")
	if _, err := c1.Register("api", port); err != nil {
		t.Fatalf("c1 register: %v", err)
	}
	host := "api.turing." + testBaseDomain
	checkResponse(t, publicHTTPSClient(edgeAddr, host), "https://"+host+"/x", "p: GET /x\n")

	c2 := connectClient(t, edgeAddr, "T2")
	if _, err := c2.Register("web", port); err != nil {
		t.Fatalf("c2 register: %v", err)
	}
	host2 := "web.turing." + testBaseDomain
	checkResponse(t, publicHTTPSClient(edgeAddr, host2), "https://"+host2+"/y", "p: GET /y\n")

	if got := mgr.IssuanceCount(); got != 1 {
		t.Errorf("issuance = %d, want 1 (same slug → cert shared)", got)
	}
}

func TestProvisionDev_WritesApexAndWildcardARecords(t *testing.T) {
	p := dns.NewStubProvider()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := dns.ProvisionSlug(ctx, p, "beam.example.com", "beam.example.com", "turing", "1.2.3.4", "2001:db8::1"); err != nil {
		t.Fatalf("ProvisionSlug: %v", err)
	}

	recs := p.Records("beam.example.com")
	if len(recs) != 4 {
		t.Fatalf("got %d records, want 4", len(recs))
	}

	type key struct{ name, typ string }
	want := map[key]bool{
		{"turing", "A"}:      false,
		{"*.turing", "A"}:    false,
		{"turing", "AAAA"}:   false,
		{"*.turing", "AAAA"}: false,
	}
	for _, r := range recs {
		rr := r.RR()
		k := key{rr.Name, rr.Type}
		if _, ok := want[k]; ok {
			want[k] = true
		} else {
			t.Errorf("unexpected record: %+v", rr)
		}
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("missing record name=%s type=%s", k.name, k.typ)
		}
	}

	// Idempotent.
	if err := dns.ProvisionSlug(ctx, p, "beam.example.com", "beam.example.com", "turing", "1.2.3.4", "2001:db8::1"); err != nil {
		t.Fatalf("ProvisionSlug rerun: %v", err)
	}
	if got := len(p.Records("beam.example.com")); got != 4 {
		t.Errorf("after idempotent rerun, got %d records, want 4", got)
	}
}

// ====================================================================
// Daemon + MCP + reconnect
// ====================================================================

func TestDaemon_OpenListCloseRoundTrip(t *testing.T) {
	port := startDummyApp(t, "api")
	_, edgeAddr := startEdge(t, map[string]string{"T1": "turing"})

	c := connectClient(t, edgeAddr, "T1")
	lc := startDaemon(t, c)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	resp, err := lc.Open(ctx, port, "api")
	if err != nil {
		t.Fatalf("Expose: %v", err)
	}
	wantURL := "https://api.turing." + testBaseDomain
	if resp.URL != wantURL {
		t.Errorf("url = %q, want %q", resp.URL, wantURL)
	}
	// The enriched response carries the full resolved identity.
	if resp.Name != "api" || resp.Port != port || resp.Slug != "turing" || resp.BaseDomain != testBaseDomain {
		t.Errorf("expose response = %+v, want name=api port=%d slug=turing base=%s", resp, port, testBaseDomain)
	}

	host := "api.turing." + testBaseDomain
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

	removed, err := lc.Close(ctx, "api")
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !removed {
		t.Errorf("Close removed = false, want true (tunnel was present)")
	}
	// Idempotent: closing a name that's already gone is a no-op that
	// succeeds and reports removed=false.
	removed, err = lc.Close(ctx, "api")
	if err != nil {
		t.Fatalf("Close (idempotent rerun): %v", err)
	}
	if removed {
		t.Errorf("second Close removed = true, want false (already gone)")
	}

	items, err = lc.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Errorf("after close, list returned %d items", len(items))
	}
}

func TestDaemon_OpenEmptyNameDerivesPortLabel(t *testing.T) {
	port := startDummyApp(t, "x")
	_, edgeAddr := startEdge(t, map[string]string{"T1": "turing"})
	c := connectClient(t, edgeAddr, "T1")
	lc := startDaemon(t, c)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// No name → the edge derives it from the port (naming.LabelFromPort),
	// and the agent mirrors that derivation in the response.
	resp, err := lc.Open(ctx, port, "")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	wantName := fmt.Sprintf("%d", port)
	if resp.Name != wantName {
		t.Errorf("derived name = %q, want %q (the port label)", resp.Name, wantName)
	}
	if want := "https://" + wantName + ".turing." + testBaseDomain; resp.URL != want {
		t.Errorf("url = %q, want %q", resp.URL, want)
	}
}

func TestDaemon_HealthzReportsSlugAndHealth(t *testing.T) {
	_, edgeAddr := startEdge(t, map[string]string{"T1": "turing"})
	c := connectClient(t, edgeAddr, "T1")
	lc := startDaemon(t, c)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	h, err := lc.Ping(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if h.Slug != "turing" {
		t.Errorf("slug = %q, want turing", h.Slug)
	}
	if !h.Healthy {
		t.Errorf("healthy = false, want true")
	}
}

func TestClient_ReconnectReplaysRegistrationsKeepsURLs(t *testing.T) {
	port := startDummyApp(t, "api")
	e, edgeAddr := startEdge(t, map[string]string{"T1": "turing"})

	c := connectClientWithOpts(t, edgeAddr, "T1", client.Options{
		HeartbeatInterval:  100 * time.Millisecond,
		RegisterTimeout:    2 * time.Second,
		InsecureSkipVerify: true,
		ReconnectInitial:   50 * time.Millisecond,
		ReconnectMax:       200 * time.Millisecond,
	})

	if _, err := c.Register("api", port); err != nil {
		t.Fatalf("register: %v", err)
	}
	host := "api.turing." + testBaseDomain
	checkResponse(t, publicHTTPSClient(edgeAddr, host), "https://"+host+"/before", "api: GET /before\n")

	before := e.SessionsCreatedTotal()
	e.CloseAllSessions()

	// Using SessionsCreatedTotal (monotonic) avoids the race where the old
	// session's cleanup hasn't run yet — SessionCount + healthy could
	// transiently match the *pre-disconnect* state.
	waitUntil(t, "edge to see reconnect", 3*time.Second, func() bool {
		return e.SessionsCreatedTotal() > before && e.RouteCount() == 1 && c.IsHealthy()
	})

	checkResponse(t, publicHTTPSClient(edgeAddr, host), "https://"+host+"/after", "api: GET /after\n")
}

func TestMCP_InitializeListAndToolLifecycle(t *testing.T) {
	port := startDummyApp(t, "api")
	_, edgeAddr := startEdge(t, map[string]string{"T1": "turing"})
	c := connectClient(t, edgeAddr, "T1")
	lc := startDaemon(t, c)

	in := &bytes.Buffer{}
	out := &bytes.Buffer{}

	writeJSONRPC(in, "i1", "initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "test", "version": "0"},
	})
	writeJSONRPC(in, nil, "notifications/initialized", nil)
	writeJSONRPC(in, "l1", "tools/list", nil)
	// Full lifecycle through the MCP surface: open → list → remove → list.
	writeJSONRPC(in, "c1", "tools/call", map[string]any{
		"name":      "expose_port",
		"arguments": map[string]any{"port": port, "name": "api"},
	})
	writeJSONRPC(in, "c2", "tools/call", map[string]any{
		"name":      "list_tunnels",
		"arguments": map[string]any{},
	})
	writeJSONRPC(in, "c3", "tools/call", map[string]any{
		"name":      "remove_tunnel",
		"arguments": map[string]any{"name": "api"},
	})
	writeJSONRPC(in, "c4", "tools/call", map[string]any{
		"name":      "list_tunnels",
		"arguments": map[string]any{},
	})

	srv := mcp.New(lc, in, out, "beamd", "test")
	if err := srv.Run(context.Background()); err != nil {
		t.Fatalf("mcp run: %v", err)
	}

	dec := json.NewDecoder(out)
	var resps []map[string]any
	for {
		var r map[string]any
		if err := dec.Decode(&r); err != nil {
			break
		}
		resps = append(resps, r)
	}
	if len(resps) != 6 {
		t.Fatalf("got %d responses, want 6:\n%s", len(resps), out.String())
	}

	// initialize: serverInfo.name = "beamd"
	init := resps[0]["result"].(map[string]any)
	if server, _ := init["serverInfo"].(map[string]any); server["name"] != "beamd" {
		t.Errorf("serverInfo.name = %v, want beamd", server["name"])
	}

	// tools/list: exactly the four tools, asserted by name (guards the
	// open/close ↔ expose_port/remove_tunnel/list_tunnels mapping + whoami).
	toolList := resps[1]["result"].(map[string]any)["tools"].([]any)
	gotNames := map[string]bool{}
	for _, tl := range toolList {
		gotNames[tl.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"expose_port", "remove_tunnel", "list_tunnels", "whoami"} {
		if !gotNames[want] {
			t.Errorf("tools/list missing tool %q; got %v", want, gotNames)
		}
	}
	if len(toolList) != 4 {
		t.Errorf("tools/list returned %d tools, want 4", len(toolList))
	}

	// expose_port → the public URL
	if got, want := callContentText(t, resps[2]), "https://api.turing."+testBaseDomain; got != want {
		t.Errorf("expose_port result = %q, want %q", got, want)
	}

	// list_tunnels (after open) → contains the api tunnel
	if got := callContentText(t, resps[3]); !strings.Contains(got, `"api"`) {
		t.Errorf("list_tunnels after open = %q, want it to contain the api tunnel", got)
	}

	// remove_tunnel → ok
	if got := callContentText(t, resps[4]); got != "ok" {
		t.Errorf("remove_tunnel result = %q, want ok", got)
	}

	// list_tunnels (after remove) → empty array
	if got := callContentText(t, resps[5]); got != "[]" {
		t.Errorf("list_tunnels after remove = %q, want []", got)
	}
}

// callContentText pulls the text of the first content block out of a
// tools/call JSON-RPC response.
func callContentText(t *testing.T, resp map[string]any) string {
	t.Helper()
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("response has no result: %v", resp)
	}
	content := result["content"].([]any)
	if len(content) == 0 {
		t.Fatal("tools/call returned empty content")
	}
	text, _ := content[0].(map[string]any)["text"].(string)
	return text
}

// ====================================================================
// Proxy correctness — headers, body limits, WebSocket
// ====================================================================

func TestEmbed_StripsFramingHeadersWhenEnabled(t *testing.T) {
	port := startFramingBackend(t)
	_, edgeAddr := startEdgeCfg(t, map[string]string{"T1": "turing"}, func(c *config.Server) {
		c.PreviewEmbed = true
	})
	c := connectClient(t, edgeAddr, "T1")
	if _, err := c.Register("api", port); err != nil {
		t.Fatalf("register: %v", err)
	}

	host := "api.turing." + testBaseDomain
	resp, err := publicHTTPSClient(edgeAddr, host).Get("https://" + host + "/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("X-Frame-Options"); got != "" {
		t.Errorf("X-Frame-Options = %q, want it stripped", got)
	}
	csp := resp.Header.Get("Content-Security-Policy")
	if strings.Contains(strings.ToLower(csp), "frame-ancestors") {
		t.Errorf("CSP still contains frame-ancestors: %q", csp)
	}
	if !strings.Contains(csp, "default-src") {
		t.Errorf("CSP lost its unrelated directives: %q (want default-src kept)", csp)
	}
}

func TestEmbed_PreservesFramingHeadersByDefault(t *testing.T) {
	port := startFramingBackend(t)
	_, edgeAddr := startEdge(t, map[string]string{"T1": "turing"}) // preview_embed defaults to false
	c := connectClient(t, edgeAddr, "T1")
	if _, err := c.Register("api", port); err != nil {
		t.Fatalf("register: %v", err)
	}

	host := "api.turing." + testBaseDomain
	resp, err := publicHTTPSClient(edgeAddr, host).Get("https://" + host + "/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options = %q, want DENY (untouched by default)", got)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(strings.ToLower(csp), "frame-ancestors") {
		t.Errorf("CSP = %q, want frame-ancestors preserved by default", csp)
	}
}

func TestProxy_AddsXForwardedHeaders(t *testing.T) {
	_, edgeAddr := startEdge(t, map[string]string{"T1": "turing"})

	// Backend that echoes the received X-Forwarded-* headers.
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

	host := "api.turing." + testBaseDomain
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

// WebSocket upgrades flow through edge→yamux-stream→client→backend.
// Architecturally supported by httputil.ReverseProxy + responseRecorder's
// Hijacker impl; this proves it end-to-end with two duplex roundtrips.
func TestProxy_WebSocketUpgradeEndToEnd(t *testing.T) {
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

	_, edgeAddr := startEdge(t, map[string]string{"T1": "turing"})
	c := connectClient(t, edgeAddr, "T1")
	if _, err := c.Register("ws", port); err != nil {
		t.Fatalf("register: %v", err)
	}

	host := "ws.turing." + testBaseDomain
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

	// Second roundtrip confirms duplex copy isn't hanging up after the first frame.
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

func TestProxy_OversizedRequestBodyRejected(t *testing.T) {
	port := startDummyApp(t, "api")

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
	e := edge.New(cfg, "test", auth.NewMemoryStore(map[string]string{"T1": "turing"}), mgr)
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

	host := "api.turing." + testBaseDomain
	hc := publicHTTPSClient(edgeAddr, host)

	small := bytes.Repeat([]byte("x"), 100)
	resp, err := hc.Post("https://"+host+"/echo", "application/octet-stream", bytes.NewReader(small))
	if err != nil {
		t.Fatalf("POST small: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("small body got status %d, want 200", resp.StatusCode)
	}

	big := bytes.Repeat([]byte("x"), limit*4)
	resp2, err := hc.Post("https://"+host+"/echo", "application/octet-stream", bytes.NewReader(big))
	if err != nil {
		// Some TLS clients fail mid-write when the server closes after the
		// 413 — that's also evidence the edge rejected the body.
		return
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusRequestEntityTooLarge && resp2.StatusCode != http.StatusBadGateway {
		t.Errorf("oversized body got status %d, want 413 or 502", resp2.StatusCode)
	}
}

// ====================================================================
// Observability — metrics + shutdown
// ====================================================================

func TestMetrics_ExposesExpectedCounters(t *testing.T) {
	port := startDummyApp(t, "api")
	_, edgeAddr := startEdge(t, map[string]string{"T1": "turing"})
	c := connectClient(t, edgeAddr, "T1")
	if _, err := c.Register("api", port); err != nil {
		t.Fatal(err)
	}

	host := "api.turing." + testBaseDomain
	checkResponse(t, publicHTTPSClient(edgeAddr, host), "https://"+host+"/m", "api: GET /m\n")

	resp, err := publicHTTPSClient(edgeAddr, host).Get("https://" + host + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	got := string(body)

	for _, want := range []string{
		"beam_active_sessions",
		"beam_active_tunnels",
		"beam_cert_issuance_total",
		"beam_requests_total",
		"beam_bytes_in_total",
		"beam_bytes_out_total",
		`beam_active_sessions 1`,
		`beam_active_tunnels 1`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("metrics output missing %q\nfull body:\n%s", want, got)
		}
	}
}

func TestMetrics_BandwidthCounterReflectsTraffic(t *testing.T) {
	port := startDummyApp(t, "api")
	_, edgeAddr := startEdge(t, map[string]string{"T1": "turing"})
	c := connectClient(t, edgeAddr, "T1")
	if _, err := c.Register("api", port); err != nil {
		t.Fatal(err)
	}

	host := "api.turing." + testBaseDomain
	// "api: GET /x\n" → 13 bytes of egress; the request itself is ingress.
	checkResponse(t, publicHTTPSClient(edgeAddr, host), "https://"+host+"/x", "api: GET /x\n")

	hc := publicHTTPSClient(edgeAddr, host)
	outKey := `beam_bytes_out_total{slug="turing",name="api"}`
	inKey := `beam_bytes_in_total{slug="turing",name="api"}`

	// Bytes are recorded when the proxied connection closes, which can lag
	// the client's read slightly — poll until the egress counter lands.
	waitUntil(t, "egress bytes recorded per tunnel", 2*time.Second, func() bool {
		resp, err := hc.Get("https://" + host + "/metrics")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return metricValue(string(b), outKey) >= 13
	})

	// Confirm ingress (request bytes) is tracked too.
	resp, err := hc.Get("https://" + host + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if got := metricValue(string(b), inKey); got <= 0 {
		t.Errorf("%s = %d, want > 0 (request bytes)", inKey, got)
	}
}

// metricValue finds the metric line beginning with key and parses its
// trailing integer value. Returns -1 if absent or malformed.
func metricValue(body, key string) int64 {
	idx := strings.Index(body, key)
	if idx < 0 {
		return -1
	}
	line := body[idx:]
	if nl := strings.IndexByte(line, '\n'); nl >= 0 {
		line = line[:nl]
	}
	fields := strings.Fields(line)
	if len(fields) != 2 {
		return -1
	}
	n, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return -1
	}
	return n
}

func TestShutdown_SignalsClientsAndDrainsSessions(t *testing.T) {
	port := startDummyApp(t, "p")

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
	e := edge.New(cfg, "test", auth.NewMemoryStore(map[string]string{"T1": "turing"}), mgr)
	go func() { _ = e.Serve() }()
	waitForTCP(t, edgeAddr, 2*time.Second)

	c, err := client.Connect(context.Background(), edgeAddr, "T1", client.Options{
		HeartbeatInterval:  200 * time.Millisecond,
		RegisterTimeout:    2 * time.Second,
		InsecureSkipVerify: true,
		// Long backoff so we'd never reconnect inside the test window —
		// the `error{code:"shutdown"}` should still flip the client
		// unhealthy immediately.
		ReconnectInitial: 10 * time.Second,
		ReconnectMax:     10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	if _, err := c.Register("api", port); err != nil {
		t.Fatal(err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	_ = e.Shutdown(shutdownCtx)
	cancel()

	waitUntil(t, "client to notice shutdown", 1*time.Second, func() bool {
		return !c.IsHealthy()
	})
}

// ====================================================================
// Auth discovery — hosted-mode bootstrap
// ====================================================================

func TestAuthDiscovery_OSSAndHostedShapes(t *testing.T) {
	t.Run("OSS returns empty discovery", func(t *testing.T) {
		_, edgeAddr := startEdge(t, map[string]string{"T1": "turing"})

		resp, err := publicHTTPSClient(edgeAddr, testBaseDomain).Get(
			"https://" + testBaseDomain + "/.well-known/beam-auth")
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
		e := edge.New(cfg, "test", auth.NewMemoryStore(map[string]string{"T1": "turing"}), mgr)
		go func() { _ = e.Serve() }()
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
			defer cancel()
			_ = e.Shutdown(ctx)
		})
		waitForTCP(t, edgeAddr, 2*time.Second)

		resp, err := publicHTTPSClient(edgeAddr, testBaseDomain).Get(
			"https://" + testBaseDomain + "/.well-known/beam-auth")
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
