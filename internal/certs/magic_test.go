package certs

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/libdns/libdns"
)

// stubProvider satisfies certmagic's DNSProvider (libdns Appender+Deleter)
// without making real DNS calls. ACME issuance fails because the
// challenge can't be solved, but construction succeeds.
type stubProvider struct{}

func (stubProvider) AppendRecords(ctx context.Context, zone string, recs []libdns.Record) ([]libdns.Record, error) {
	return recs, nil
}
func (stubProvider) DeleteRecords(ctx context.Context, zone string, recs []libdns.Record) ([]libdns.Record, error) {
	return recs, nil
}

func TestNewMagicManager_Construction(t *testing.T) {
	dir := t.TempDir()
	m, err := NewMagicManager(MagicConfig{
		BaseDomain:  "beam.example.com",
		ACMEEmail:   "ops@example.com",
		ACMECA:      "https://acme-staging-v02.api.letsencrypt.org/directory",
		DNSProvider: stubProvider{},
		StorageDir:  filepath.Join(dir, "certs"),
	})
	if err != nil {
		t.Fatalf("NewMagicManager: %v", err)
	}
	if m == nil {
		t.Fatal("got nil manager")
	}
	if m.IssuanceCount() != 0 {
		t.Errorf("fresh manager IssuanceCount = %d, want 0", m.IssuanceCount())
	}

	// Off-domain SNI returns the fallback cert (no ACME involved).
	cert, err := m.GetCertificate(&tls.ClientHelloInfo{ServerName: "localhost"})
	if err != nil {
		t.Errorf("fallback GetCertificate: %v", err)
	}
	if cert == nil {
		t.Error("fallback cert nil")
	}
}

// servedCert drives a real in-memory TLS handshake against getCert with the
// given SNI and returns the certificate the server presented. A real
// handshake (unlike a synthetic ClientHelloInfo) carries the context
// certmagic needs.
func servedCert(t *testing.T, getCert func(*tls.ClientHelloInfo) (*tls.Certificate, error), sni string) *tls.Certificate {
	t.Helper()
	var captured *tls.Certificate
	srvCfg := &tls.Config{
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			c, err := getCert(hello)
			captured = c
			return c, err
		},
	}
	c1, c2 := net.Pipe()
	deadline := time.Now().Add(2 * time.Second)
	_ = c1.SetDeadline(deadline)
	_ = c2.SetDeadline(deadline)
	defer c1.Close()
	defer c2.Close()

	srv := tls.Server(c1, srvCfg)
	cli := tls.Client(c2, &tls.Config{ServerName: sni, InsecureSkipVerify: true})
	done := make(chan struct{}, 2)
	go func() { _ = srv.Handshake(); done <- struct{}{} }()
	go func() { _ = cli.Handshake(); done <- struct{}{} }()
	<-done
	<-done

	if captured == nil {
		t.Fatalf("GetCertificate was never called for SNI %q", sni)
	}
	return captured
}

func TestMagicManager_ApexServesManagedCertNotFallback(t *testing.T) {
	dir := t.TempDir()
	m, err := NewMagicManager(MagicConfig{
		BaseDomain:  "beam.example.com",
		ACMEEmail:   "ops@example.com",
		ACMECA:      "https://acme-staging-v02.api.letsencrypt.org/directory",
		DNSProvider: stubProvider{},
		StorageDir:  filepath.Join(dir, "certs"),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Stand in for the eagerly-managed apex cert (issued by ACME in prod)
	// by caching one directly — no network/ACME needed.
	apexCert, err := generateSelfSignedCert("beam.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.cmWild.CacheUnmanagedTLSCertificate(context.Background(), apexCert, nil); err != nil {
		t.Fatalf("cache apex cert: %v", err)
	}

	// The apex must serve that managed cert, NOT the self-signed fallback.
	got := servedCert(t, m.GetCertificate, "beam.example.com")
	if len(got.Certificate) == 0 || !bytes.Equal(got.Certificate[0], apexCert.Certificate[0]) {
		t.Error("apex served the self-signed fallback instead of its managed cert")
	}

	// A genuinely unknown SNI still falls back to the self-signed cert.
	fb := servedCert(t, m.GetCertificate, "nope.example.org")
	if !bytes.Equal(fb.Certificate[0], m.fallbackCert.Certificate[0]) {
		t.Error("unknown SNI did not fall back to the self-signed cert")
	}
}

// TestMagicManager_UnderBaseDoesNotHitOnDemandDecision is the regression guard for
// the two-config split: a host UNDER the base domain (apex or tunnel) must be served
// by the eager DNS-01 wildcard config and must NEVER consult the custom-domain
// On-Demand DecisionFunc. (The old single-config setup had On-Demand defer eager
// issuance, so every base host hit the authorizer — which refuses them — and no
// base/wildcard cert ever issued.)
func TestMagicManager_UnderBaseDoesNotHitOnDemandDecision(t *testing.T) {
	dir := t.TempDir()
	var decisionCalled bool
	m, err := NewMagicManager(MagicConfig{
		BaseDomain:  "beam.example.com",
		ACMEEmail:   "ops@example.com",
		ACMECA:      "https://acme-staging-v02.api.letsencrypt.org/directory",
		DNSProvider: stubProvider{},
		StorageDir:  filepath.Join(dir, "certs"),
		OnDemandDecision: func(_ context.Context, _ string) error {
			decisionCalled = true
			return fmt.Errorf("refuse")
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Stand in for the eager DNS-01 wildcard by caching it directly.
	wildCert, err := generateSelfSignedCert("*.beam.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.cmWild.CacheUnmanagedTLSCertificate(context.Background(), wildCert, nil); err != nil {
		t.Fatalf("cache wildcard: %v", err)
	}

	// A tunnel host under the base serves the wildcard and does NOT consult the
	// custom-domain authorizer.
	got := servedCert(t, m.GetCertificate, "demo-acme.beam.example.com")
	if decisionCalled {
		t.Error("under-base host wrongly consulted the on-demand DecisionFunc (the bug)")
	}
	if len(got.Certificate) == 0 || !bytes.Equal(got.Certificate[0], wildCert.Certificate[0]) {
		t.Error("under-base tunnel host did not serve the cmWild wildcard cert")
	}

	// A genuine custom domain (not under the base) SHOULD consult the authorizer.
	decisionCalled = false
	_ = servedCert(t, m.GetCertificate, "api.acme.com")
	if !decisionCalled {
		t.Error("custom domain did not consult the on-demand DecisionFunc")
	}
}

func TestNewMagicManager_RequiredFields(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*MagicConfig)
		wantErr string
	}{
		{"no base domain", func(c *MagicConfig) { c.BaseDomain = "" }, "BaseDomain"},
		{"no email", func(c *MagicConfig) { c.ACMEEmail = "" }, "ACMEEmail"},
		{"no provider", func(c *MagicConfig) { c.DNSProvider = nil }, "DNSProvider"},
		{"no storage", func(c *MagicConfig) { c.StorageDir = "" }, "StorageDir"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := MagicConfig{
				BaseDomain:  "x.example.com",
				ACMEEmail:   "ops@example.com",
				DNSProvider: stubProvider{},
				StorageDir:  t.TempDir(),
			}
			c.mutate(&cfg)
			if _, err := NewMagicManager(cfg); err == nil {
				t.Errorf("expected error containing %q, got nil", c.wantErr)
			}
		})
	}
}

// The cache's GetConfigForCert callback is what certmagic's background
// maintenance loop uses to find the config that can renew each cert; if it
// returns nil the loop skips the cert and every eagerly-managed cert (apex +
// wildcards) silently expires at ~90 days. Pin the wiring: each cache's
// callback must return the config built on that cache.
func TestCacheConfigWiring_RenewalStaysEnabled(t *testing.T) {
	m, err := NewMagicManager(MagicConfig{
		BaseDomain:       "beam.example.com",
		ACMEEmail:        "ops@example.com",
		ACMECA:           "https://acme-staging-v02.api.letsencrypt.org/directory",
		DNSProvider:      stubProvider{},
		StorageDir:       filepath.Join(t.TempDir(), "certs"),
		OnDemandDecision: func(ctx context.Context, name string) error { return nil },
	})
	if err != nil {
		t.Fatalf("NewMagicManager: %v", err)
	}

	cfg, err := m.wildConfigGetter(certmagic.Certificate{})
	if err != nil {
		t.Fatalf("wild GetConfigForCert: %v", err)
	}
	if cfg != m.cmWild {
		t.Errorf("wild cache callback returned %p, want the wild config %p", cfg, m.cmWild)
	}

	od, err := m.onDemandConfigGetter(certmagic.Certificate{})
	if err != nil {
		t.Fatalf("on-demand GetConfigForCert: %v", err)
	}
	if od != m.cmOnDemand {
		t.Errorf("on-demand cache callback returned %p, want the on-demand config %p", od, m.cmOnDemand)
	}
}

// A failed background issuance must not be retried on the very next handshake
// — Let's Encrypt rate-limits failed validations per hostname (5/hour).
func TestKickWild_CooldownAfterFailure(t *testing.T) {
	m, err := NewMagicManager(MagicConfig{
		BaseDomain:  "beam.example.com",
		ACMEEmail:   "ops@example.com",
		ACMECA:      "https://acme-staging-v02.api.letsencrypt.org/directory",
		DNSProvider: stubProvider{},
		StorageDir:  filepath.Join(t.TempDir(), "certs"),
	})
	if err != nil {
		t.Fatalf("NewMagicManager: %v", err)
	}

	names := m.wildNamesFor("beam.example.com")
	key := strings.Join(names, ",")
	m.mu.Lock()
	m.lastFail[key] = time.Now()
	m.mu.Unlock()

	m.kickWild("beam.example.com")

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.inflight[key] {
		t.Error("kickWild started an ACME order during the failure cooldown")
	}
}

// Integration guard for the SEC-3/4 wiring AND the lastFail bound: an
// unauthenticated peer spraying distinct DENIED off-base SNIs through the real
// GetCertificate path must not grow the cooldown map. recordFailure fires on
// every on-demand error (including denials), so this proves the authorized-only
// guard keeps lastFail bounded end-to-end — the persistent-DoS vector the
// review flagged — while also exercising the DecisionFunc→gate.Decide seam.
func TestMagicManager_DeniedSNIsDoNotGrowCooldown(t *testing.T) {
	dir := t.TempDir()
	var calls int
	m, err := NewMagicManager(MagicConfig{
		BaseDomain:  "beam.example.com",
		ACMEEmail:   "ops@example.com",
		ACMECA:      "https://acme-staging-v02.api.letsencrypt.org/directory",
		DNSProvider: stubProvider{},
		StorageDir:  filepath.Join(dir, "certs"),
		OnDemandDecision: func(_ context.Context, _ string) error {
			calls++
			return fmt.Errorf("unverified") // every off-base name denied → certmagic refuses fast
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 200; i++ {
		_ = servedCert(t, m.GetCertificate, fmt.Sprintf("a%d.evil.example", i))
	}

	if calls == 0 {
		t.Fatal("off-base handshakes should consult the gated authorizer (DecisionFunc→gate.Decide not wired)")
	}
	m.gate.mu.Lock()
	n := len(m.gate.lastFail)
	m.gate.mu.Unlock()
	if n != 0 {
		t.Errorf("denied SNIs grew the cooldown map to %d entries — unbounded DoS vector", n)
	}
}

// kickWild must refuse DNS-01 issuance for hosts the edge doesn't serve, so an
// unauthenticated peer spraying <app>.<slug>.<base> SNIs cannot trigger real
// ACME orders or grow m.inflight/m.lastFail. Without the gate, each distinct
// SNI would spawn a background ManageSync (a real ACME order) — the HIGH the
// review found.
func TestMagicManager_KickWildGatedByHostAllowed(t *testing.T) {
	dir := t.TempDir()
	m, err := NewMagicManager(MagicConfig{
		BaseDomain:  "beam.example.com",
		ACMEEmail:   "ops@example.com",
		ACMECA:      "https://acme-staging-v02.api.letsencrypt.org/directory",
		DNSProvider: stubProvider{},
		StorageDir:  filepath.Join(dir, "certs"),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Only one host is "routed"; everything else is refused.
	m.SetHostAllowed(func(name string) bool { return name == "api.turing.beam.example.com" })

	for i := 0; i < 50; i++ {
		_ = servedCert(t, m.GetCertificate, fmt.Sprintf("a.evil%d.beam.example.com", i))
	}

	m.mu.Lock()
	inflight, lastFail := len(m.inflight), len(m.lastFail)
	m.mu.Unlock()
	if inflight != 0 || lastFail != 0 {
		t.Errorf("attacker SNIs reached issuance state: inflight=%d lastFail=%d (gate not enforced)", inflight, lastFail)
	}
	if m.IssuanceCount() != 0 {
		t.Errorf("attacker SNIs triggered issuance (count=%d)", m.IssuanceCount())
	}

	// The gate callback itself: known host allowed, unknown refused.
	m.mu.Lock()
	fn := m.issueAllowed
	m.mu.Unlock()
	if fn == nil || !fn("api.turing.beam.example.com") {
		t.Error("known routed host should be allowed to issue")
	}
	if fn("a.evil.beam.example.com") {
		t.Error("unrouted host must be refused")
	}
}
