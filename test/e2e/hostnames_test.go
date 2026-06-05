package e2e

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dynamismlabs/beamd/internal/config"
)

// A tunnel registers under EVERY host its scope answers on: the default-shape
// host for each bound slug (primary + retained rename aliases) plus each verified
// custom domain. This is what makes an old URL keep working after a rename and a
// custom domain serve the scope (url-model §4). The edge fetches the set from the
// control plane's scope-hostnames endpoint; here we point it at a stub.
func TestMultiHostRegistration(t *testing.T) {
	// Stub control plane: scope `turing` answers on slug `turing` + alias
	// `oldturing`, plus the verified custom domain `acme.example`.
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/scope-hostnames" || r.URL.Query().Get("slug") != "turing" {
			w.WriteHeader(404)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-secret" {
			w.WriteHeader(401)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"primary_slug": "turing",
			"slugs":        []string{"turing", "oldturing"},
			"shape":        "subdomain",
			"domains": []map[string]any{
				{"domain": "acme.example", "primary": true, "cert_mode": "on_demand"},
			},
			"primary_host": "api.acme.example",
		})
	}))
	defer stub.Close()

	dummyPort := startDummyApp(t, "multi")
	e, edgeAddr := startEdge(t, map[string]string{"T1": "turing"})
	e.SetHostnamesEndpoint(stub.URL+"/scope-hostnames", "test-secret")

	c := connectClient(t, edgeAddr, "T1")
	url, err := c.Register("api", dummyPort)
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	// The returned URL renders on the primary custom domain.
	if want := "https://api.acme.example"; url != want {
		t.Errorf("register URL = %q, want %q (the primary custom domain)", url, want)
	}

	// The tunnel is registered under all hosts: both slugs + the custom domain.
	got := e.RouteHosts()
	sort.Strings(got)
	want := []string{
		"api.acme.example",                // custom domain
		"api.oldturing." + testBaseDomain, // retained rename alias
		"api.turing." + testBaseDomain,    // primary slug
	}
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("route hosts = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("route hosts = %v, want %v", got, want)
		}
	}

	// The alias host actually routes (it's under the base domain, so the
	// self-signed cert serves it) — an old URL keeps working after a rename.
	aliasHost := "api.oldturing." + testBaseDomain
	hc := publicHTTPSClient(edgeAddr, aliasHost)
	resp, err := hc.Get("https://" + aliasHost + "/foo")
	if err != nil {
		t.Fatalf("GET via alias host: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("alias host status = %d, want 200 (the alias must route)", resp.StatusCode)
	}

	// Unregister removes ALL of the tunnel's hosts, not just the primary.
	// (Unregister is fire-and-forget over the control stream, so poll.)
	if err := c.Unregister("api"); err != nil {
		t.Fatalf("unregister: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(e.RouteHosts()) > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if hosts := e.RouteHosts(); len(hosts) != 0 {
		t.Errorf("after unregister, route hosts = %v, want none (all hosts removed)", hosts)
	}
}

// Under the hyphen shape, `<name>-<slug>` is one DNS label and must fit 63 chars.
// The edge (the cert authority) rejects an over-long combo at register, so cert
// issuance never fails opaquely (url-model §7).
func TestHyphenLabelLengthBackstop(t *testing.T) {
	dummyPort := startDummyApp(t, "len")
	e, edgeAddr := startEdgeCfg(t, map[string]string{"T1": "turing"}, func(c *config.Server) {
		c.URLShape = "hyphen"
	})
	_ = e

	c := connectClient(t, edgeAddr, "T1")
	// name(60) + "-" + "turing"(6) = 67 > 63 → rejected.
	longName := strings.Repeat("a", 60)
	if _, err := c.Register(longName, dummyPort); err == nil {
		t.Errorf("register of a 60-char name under a 6-char slug should be rejected (label > 63)")
	}

	// A short name under the same slug is fine.
	if _, err := c.Register("api", dummyPort); err != nil {
		t.Errorf("short name should register: %v", err)
	}
}

// Without a hostnames endpoint (self-host), a tunnel registers under just the
// single default host — the multi-host machinery is hosted-only.
func TestSingleHostWhenNoHostnamesEndpoint(t *testing.T) {
	dummyPort := startDummyApp(t, "solo")
	e, edgeAddr := startEdge(t, map[string]string{"T1": "turing"})
	// no SetHostnamesEndpoint → e.hostnames is nil

	c := connectClient(t, edgeAddr, "T1")
	if _, err := c.Register("api", dummyPort); err != nil {
		t.Fatalf("register: %v", err)
	}
	if hosts := e.RouteHosts(); len(hosts) != 1 {
		t.Errorf("route hosts = %v, want exactly 1 (single-host self-host mode)", hosts)
	}
}
