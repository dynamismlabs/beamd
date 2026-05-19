// Package certs manages per-developer wildcard TLS certificates.
//
// PRD §5.1: one wildcard cert per slug (`*.<slug>.<base_domain>`),
// reused across all of that slug's apps. Issuance happens lazily on
// first TLS handshake for the slug, or eagerly via PreWarm (used by
// `beamd provision-dev`).
//
// M4 ships `SelfSignedManager`, which is sufficient for tests and
// dev-without-DNS. The production `MagicManager` (certmagic + ACME
// DNS-01 via libdns) sits behind the same `Manager` interface and is
// a separate concrete type — adding it does not touch any caller.
package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"strings"
	"sync"
	"time"
)

// Manager produces TLS certificates for incoming connections.
type Manager interface {
	// GetCertificate is plugged into tls.Config.GetCertificate.
	GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error)

	// IssuanceCount returns the total number of fresh certs issued.
	// Used by tests and metrics to verify the "one cert per slug, reused"
	// invariant (PRD §14 M4).
	IssuanceCount() int

	// PreWarm ensures a cert is issued for the given slug. Called by
	// `provision-dev` so the first user-facing request doesn't pay the
	// issuance latency.
	PreWarm(slug string) error
}

// SelfSignedManager is the dev/test cert manager. It generates a fresh
// self-signed ECDSA cert per slug on first request and caches it
// in-memory. Suitable for tests and `acme_ca: off`-style local dev.
//
// Cert SAN coverage: `*.<slug>.<base>` and `<slug>.<base>`.
type SelfSignedManager struct {
	baseDomain   string
	fallbackCert *tls.Certificate

	mu        sync.Mutex
	certs     map[string]*tls.Certificate
	issuances int
}

func NewSelfSignedManager(baseDomain string) (*SelfSignedManager, error) {
	fallback, err := generateSelfSignedCert(
		"localhost",
		baseDomain,
		"*."+baseDomain,
		"hardcoded.host",
		"app1.host",
		"app2.host",
		"unknown.host",
	)
	if err != nil {
		return nil, fmt.Errorf("generate fallback cert: %w", err)
	}
	return &SelfSignedManager{
		baseDomain:   baseDomain,
		fallbackCert: &fallback,
		certs:        make(map[string]*tls.Certificate),
	}, nil
}

func (m *SelfSignedManager) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	slug, ok := extractSlug(hello.ServerName, m.baseDomain)
	if !ok {
		return m.fallbackCert, nil
	}
	return m.issueOrGet(slug)
}

func (m *SelfSignedManager) PreWarm(slug string) error {
	_, err := m.issueOrGet(slug)
	return err
}

func (m *SelfSignedManager) IssuanceCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.issuances
}

func (m *SelfSignedManager) issueOrGet(slug string) (*tls.Certificate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.certs[slug]; ok {
		return c, nil
	}
	wildcard := fmt.Sprintf("*.%s.%s", slug, m.baseDomain)
	apex := fmt.Sprintf("%s.%s", slug, m.baseDomain)
	cert, err := generateSelfSignedCert(wildcard, apex)
	if err != nil {
		return nil, fmt.Errorf("issue self-signed for slug %q: %w", slug, err)
	}
	m.certs[slug] = &cert
	m.issuances++
	slog.Info("certs: self-signed wildcard issued", "slug", slug, "issuance_count", m.issuances)
	return &cert, nil
}

// extractSlug pulls the slug component out of an SNI that matches the
// `<app>.<slug>.<base>` shape. Returns ("", false) for anything else
// (apex hits, single-label SNIs, off-domain).
func extractSlug(sni, baseDomain string) (string, bool) {
	if sni == "" {
		return "", false
	}
	suffix := "." + baseDomain
	if !strings.HasSuffix(sni, suffix) {
		return "", false
	}
	prefix := strings.TrimSuffix(sni, suffix) // "<app>.<slug>" or "<slug>" or "<a>.<b>.<slug>"
	parts := strings.Split(prefix, ".")
	if len(parts) < 2 {
		// Only "<slug>.<base>" — no app label, can't satisfy our scheme.
		return "", false
	}
	return parts[len(parts)-1], true
}

// generateSelfSignedCert returns a fresh ECDSA P-256 cert valid for
// 30 days, listing the given hosts as DNS SANs (plus 127.0.0.1 / ::1).
func generateSelfSignedCert(hosts ...string) (tls.Certificate, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("serial: %w", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "beam-self-signed"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(30 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     hosts,
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("create cert: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("marshal key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return tls.X509KeyPair(certPEM, keyPEM)
}
