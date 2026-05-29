package certs

import (
	"bytes"
	"context"
	"crypto/tls"
	"net"
	"path/filepath"
	"testing"
	"time"

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
	if _, err := m.cm.CacheUnmanagedTLSCertificate(context.Background(), apexCert, nil); err != nil {
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
