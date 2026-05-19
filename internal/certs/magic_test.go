package certs

import (
	"context"
	"crypto/tls"
	"path/filepath"
	"testing"

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
