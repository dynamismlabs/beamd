package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/treyhuffine/conduit/internal/auth"
	"github.com/treyhuffine/conduit/internal/certs"
	"github.com/treyhuffine/conduit/internal/client"
	"github.com/treyhuffine/conduit/internal/config"
	"github.com/treyhuffine/conduit/internal/dns"
	"github.com/treyhuffine/conduit/internal/edge"
)

// startEdgeWithCertMgr is like startEdge but lets the test inspect the
// cert manager (to assert issuance counts).
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

// TestM4_CertReuseAcrossAppsUnderSlug is the headline M4 acceptance:
// fresh slug, first request triggers issuance; second app under same
// slug serves over TLS with no new issuance.
func TestM4_CertReuseAcrossAppsUnderSlug(t *testing.T) {
	port1 := startDummyApp(t, "api")
	port2 := startDummyApp(t, "web")

	_, mgr, edgeAddr := startEdgeWithCertMgr(t, map[string]string{"T1": "trey"})
	c := connectClient(t, edgeAddr, "T1")

	if _, err := c.Register("api", port1); err != nil {
		t.Fatalf("register api: %v", err)
	}
	if _, err := c.Register("web", port2); err != nil {
		t.Fatalf("register web: %v", err)
	}

	host1 := "api.trey." + testBaseDomain
	host2 := "web.trey." + testBaseDomain

	checkResponse(t, publicHTTPSClient(edgeAddr, host1), "https://"+host1+"/x", "api: GET /x\n")
	if got := mgr.IssuanceCount(); got != 1 {
		t.Errorf("after first slug request, issuance = %d, want 1", got)
	}

	checkResponse(t, publicHTTPSClient(edgeAddr, host2), "https://"+host2+"/y", "web: GET /y\n")
	if got := mgr.IssuanceCount(); got != 1 {
		t.Errorf("after second app under same slug, issuance = %d, want 1 (cert should be reused)", got)
	}
}

// TestM4_DistinctSlugsGetDistinctCerts confirms the cache key is the
// slug, not the apex domain.
func TestM4_DistinctSlugsGetDistinctCerts(t *testing.T) {
	port := startDummyApp(t, "p")

	_, mgr, edgeAddr := startEdgeWithCertMgr(t, map[string]string{
		"T1": "trey",
		"T2": "alex",
	})

	c1 := connectClient(t, edgeAddr, "T1")
	if _, err := c1.Register("api", port); err != nil {
		t.Fatalf("c1 register: %v", err)
	}
	host1 := "api.trey." + testBaseDomain
	checkResponse(t, publicHTTPSClient(edgeAddr, host1), "https://"+host1+"/x", "p: GET /x\n")
	if got := mgr.IssuanceCount(); got != 1 {
		t.Errorf("after slug trey, issuance = %d, want 1", got)
	}

	c2 := connectClient(t, edgeAddr, "T2")
	if _, err := c2.Register("api", port); err != nil {
		t.Fatalf("c2 register: %v", err)
	}
	host2 := "api.alex." + testBaseDomain
	checkResponse(t, publicHTTPSClient(edgeAddr, host2), "https://"+host2+"/y", "p: GET /y\n")
	if got := mgr.IssuanceCount(); got != 2 {
		t.Errorf("after distinct slug alex, issuance = %d, want 2", got)
	}
}

// TestM4_ProvisionSlugWritesDNSRecords exercises the DNS-provider side
// of `conduitd provision-dev` against a stub libdns provider.
func TestM4_ProvisionSlugWritesDNSRecords(t *testing.T) {
	p := dns.NewStubProvider()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := dns.ProvisionSlug(ctx, p, "conduit.example.com", "trey", "1.2.3.4", "2001:db8::1"); err != nil {
		t.Fatalf("ProvisionSlug: %v", err)
	}

	recs := p.Records("conduit.example.com")
	if len(recs) != 4 {
		t.Fatalf("got %d records, want 4", len(recs))
	}

	type key struct{ name, typ string }
	want := map[key]bool{
		{"trey", "A"}:      false,
		{"*.trey", "A"}:    false,
		{"trey", "AAAA"}:   false,
		{"*.trey", "AAAA"}: false,
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

	// Re-running is a no-op (record set is identical).
	if err := dns.ProvisionSlug(ctx, p, "conduit.example.com", "trey", "1.2.3.4", "2001:db8::1"); err != nil {
		t.Fatalf("ProvisionSlug rerun: %v", err)
	}
	if got := len(p.Records("conduit.example.com")); got != 4 {
		t.Errorf("after idempotent rerun, got %d records, want 4", got)
	}
}

// TestM4_BothLoginPathsClaim_ Same exercises that two clients with
// different tokens but the same slug share the same cert (issuance ==
// 1). This is the M5-foreshadowing case (multi-laptop reconnect under
// the same slug).
func TestM4_TwoTokensSameSlugShareCert(t *testing.T) {
	port := startDummyApp(t, "p")

	_, mgr, edgeAddr := startEdgeWithCertMgr(t, map[string]string{
		"T1": "trey",
		"T2": "trey", // both tokens belong to the same developer
	})

	c1 := connectClient(t, edgeAddr, "T1")
	if _, err := c1.Register("api", port); err != nil {
		t.Fatalf("c1 register: %v", err)
	}
	host := "api.trey." + testBaseDomain
	checkResponse(t, publicHTTPSClient(edgeAddr, host), "https://"+host+"/x", "p: GET /x\n")

	c2 := connectClient(t, edgeAddr, "T2")
	// "web" under the same slug — would hit the same wildcard cert.
	if _, err := c2.Register("web", port); err != nil {
		t.Fatalf("c2 register: %v", err)
	}
	host2 := "web.trey." + testBaseDomain
	checkResponse(t, publicHTTPSClient(edgeAddr, host2), "https://"+host2+"/y", "p: GET /y\n")

	if got := mgr.IssuanceCount(); got != 1 {
		t.Errorf("issuance = %d, want 1 (same slug → cert shared)", got)
	}

	_ = client.DefaultHeartbeatInterval // keep the client package used in the M4 test file
}
