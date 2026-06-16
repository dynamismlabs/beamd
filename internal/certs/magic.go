package certs

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/libdns/libdns"
)

// MagicConfig is what New(MagicManager) needs to issue real certs.
type MagicConfig struct {
	BaseDomain string

	// ACMEEmail is the contact address registered with the ACME CA.
	ACMEEmail string

	// ACMECA is the ACME directory URL. Empty falls back to Let's
	// Encrypt production. Use `certmagic.LetsEncryptStagingCA` for
	// testing against LE staging.
	ACMECA string

	// DNSProvider supplies DNS-01 challenge solving (TXT writes +
	// cleanup). Any libdns.RecordAppender + libdns.RecordDeleter
	// implementation works; our internal/dns providers all satisfy it.
	DNSProvider DNSProvider

	// StorageDir is where certs + ACME account state live on disk.
	// Required so we don't re-issue on every restart.
	StorageDir string

	// PropagationTimeout caps how long we wait for the DNS-01 TXT
	// record to propagate. Default 2 minutes; tests can lower.
	PropagationTimeout time.Duration

	// EagerNames are obtained (via DNS-01) at construction time so the edge has a
	// real cert before serving traffic. Typically the apex (`base_domain`, for
	// `/healthz` + `/.well-known/beam-auth`) and — for the hyphen/flat shapes —
	// the base wildcard `*.<base>` (one cert covering every tunnel). Best-effort:
	// a transient DNS hiccup logs a warning and is retried on first handshake
	// rather than blocking startup.
	EagerNames []string

	// OnDemandDecision, when set, enables certmagic **On-Demand TLS** for hosts
	// that are NOT under the base domain — i.e. customer **custom domains**
	// (url-model §8, path B). A per-host cert is issued on the first handshake
	// only if this returns nil; otherwise issuance is refused. Wire it to
	// NewResolveHostAuthorizer so only control-plane-verified hosts get a cert —
	// that allowlist is what makes On-Demand safe. nil → custom domains are not
	// served (the host falls back to the self-signed cert). On-Demand certs use
	// TLS-ALPN-01 (only need the host to resolve to the edge), since we don't
	// control the customer's DNS.
	OnDemandDecision func(ctx context.Context, name string) error

	// --- Advanced / integration-testing knobs — leave zero/nil in production. ---

	// ChallengeTLSPort overrides the port certmagic expects ACME TLS-ALPN-01
	// validation to arrive on. The ACME spec mandates :443 in production, so this
	// must stay 0 (→ 443) there; a local ACME test server (Pebble) validates on a
	// custom port, which is the only reason to set it.
	ChallengeTLSPort int
	// ACMETrustedRoots is the root pool the ACME client uses to reach the ACME
	// directory. nil → system roots (correct for Let's Encrypt). The cert
	// integration test sets this to Pebble's self-signed CA.
	ACMETrustedRoots *x509.CertPool
}

// DNSProvider is what certmagic needs to solve DNS-01 challenges. It
// is intentionally a subset of `internal/dns.Provider` — any
// libdns-conforming provider works.
type DNSProvider interface {
	libdns.RecordAppender
	libdns.RecordDeleter
}

// MagicManager is the production Manager. It runs TWO certmagic configs because
// the two issuance modes are mutually exclusive on a single config:
//
//   - cmWild: DNS-01 issuer, NO On-Demand. ManageSync OBTAINS eagerly here, which
//     is how the base apex + the `*.<base>` wildcard (covering every tunnel) get
//     real certs. Wildcards are DNS-01-only, and we control the base zone.
//   - cmOnDemand: TLS-ALPN-01 issuer + On-Demand DecisionFunc, for customer
//     **custom domains** whose DNS we do NOT control (issued per-host on the first
//     handshake, gated by the control plane's resolve-host allowlist).
//
// They MUST be separate: certmagic's ManageSync does not obtain certs ahead of
// time when On-Demand is configured (config.go: it defers to the DecisionFunc),
// so putting both on one config means the base/wildcard certs never issue and
// every handshake hits the custom-domain authorizer (which correctly refuses
// them). GetCertificate routes by whether the SNI is under the base domain.
type MagicManager struct {
	cmWild       *certmagic.Config // DNS-01, eager: base apex + *.<base>
	cmOnDemand   *certmagic.Config // TLS-ALPN-01 + On-Demand: custom domains (nil if disabled)
	baseDomain   string
	fallbackCert *tls.Certificate

	mu        sync.Mutex
	inflight  map[string]bool // background DNS-01 issuance in progress, by name-set
	issuances int
}

func NewMagicManager(cfg MagicConfig) (*MagicManager, error) {
	if cfg.BaseDomain == "" {
		return nil, fmt.Errorf("BaseDomain is required")
	}
	if cfg.ACMEEmail == "" {
		return nil, fmt.Errorf("ACMEEmail is required")
	}
	if cfg.DNSProvider == nil {
		return nil, fmt.Errorf("DNSProvider is required (DNS-01 challenge)")
	}
	if cfg.StorageDir == "" {
		return nil, fmt.Errorf("StorageDir is required for cert persistence")
	}

	storage := &certmagic.FileStorage{Path: cfg.StorageDir}

	propagationTimeout := cfg.PropagationTimeout
	if propagationTimeout == 0 {
		propagationTimeout = 2 * time.Minute
	}
	acmeCA := cfg.ACMECA
	if acmeCA == "" {
		acmeCA = certmagic.LetsEncryptProductionCA
	}

	// A fresh cache per config (cache is per-config in certmagic); shared storage
	// is fine (certmagic namespaces by issuer/account).
	newCache := func() *certmagic.Cache {
		return certmagic.NewCache(certmagic.CacheOptions{
			GetConfigForCert: func(certmagic.Certificate) (*certmagic.Config, error) { return nil, nil },
		})
	}

	// --- cmWild: DNS-01, NO On-Demand → ManageSync obtains eagerly. ---
	cmWild := certmagic.New(newCache(), certmagic.Config{Storage: storage})
	cmWild.Issuers = []certmagic.Issuer{certmagic.NewACMEIssuer(cmWild, certmagic.ACMEIssuer{
		CA:           acmeCA,
		Email:        cfg.ACMEEmail,
		Agreed:       true,
		TrustedRoots: cfg.ACMETrustedRoots,
		DNS01Solver: &certmagic.DNS01Solver{
			DNSManager: certmagic.DNSManager{
				DNSProvider:        cfg.DNSProvider,
				PropagationTimeout: propagationTimeout,
			},
		},
	})}

	// --- cmOnDemand: TLS-ALPN-01 + On-Demand → custom domains only. ---
	var cmOnDemand *certmagic.Config
	if cfg.OnDemandDecision != nil {
		cmOnDemand = certmagic.New(newCache(), certmagic.Config{Storage: storage})
		cmOnDemand.Issuers = []certmagic.Issuer{certmagic.NewACMEIssuer(cmOnDemand, certmagic.ACMEIssuer{
			CA:                   acmeCA,
			Email:                cfg.ACMEEmail,
			Agreed:               true,
			TrustedRoots:         cfg.ACMETrustedRoots,
			DisableHTTPChallenge: true, // no :80 listener — TLS-ALPN-01 only
			// 0 → certmagic default (:443, per the ACME spec). The integration
			// test points this at Pebble's TLS-ALPN validation port.
			AltTLSALPNPort: cfg.ChallengeTLSPort,
		})}
		decide := cfg.OnDemandDecision
		cmOnDemand.OnDemand = &certmagic.OnDemandConfig{
			DecisionFunc: func(ctx context.Context, name string) error {
				if err := decide(ctx, name); err != nil {
					slog.Warn("certs: on-demand issuance refused", "host", name, "err", err.Error())
					return err
				}
				slog.Info("certs: on-demand issuance authorized", "host", name)
				return nil
			},
		}
	}

	fallback, err := generateSelfSignedCert(
		"localhost", cfg.BaseDomain, "*."+cfg.BaseDomain,
	)
	if err != nil {
		return nil, fmt.Errorf("fallback cert: %w", err)
	}

	m := &MagicManager{
		cmWild:       cmWild,
		cmOnDemand:   cmOnDemand,
		baseDomain:   cfg.BaseDomain,
		fallbackCert: &fallback,
		inflight:     make(map[string]bool),
	}

	// Eagerly obtain the operator names (apex + base wildcard) via DNS-01 so the
	// edge is cert-ready before traffic. Best-effort: on failure, log and let the
	// handshake path retry (GetCertificate → kickWild), rather than block boot.
	if len(cfg.EagerNames) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		if err := cmWild.ManageSync(ctx, cfg.EagerNames); err != nil {
			slog.Warn("certs: eager issuance failed (will retry on handshake)",
				"names", cfg.EagerNames, "err", err.Error())
		} else {
			m.mu.Lock()
			m.issuances += len(cfg.EagerNames)
			m.mu.Unlock()
			slog.Info("certs: eager names issued via DNS-01", "names", cfg.EagerNames)
		}
		cancel()
	}

	return m, nil
}

func (m *MagicManager) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	name := strings.ToLower(strings.TrimSuffix(hello.ServerName, "."))

	// Under the base domain (the apex or a tunnel host) → the DNS-01 wildcard
	// config. The cert is eager-managed (or issued by a prior handshake); if it's
	// not cached yet, kick a background DNS-01 issuance and serve the fallback for
	// this one handshake (so we never block the handshake on a DNS round-trip).
	if name == m.baseDomain || strings.HasSuffix(name, "."+m.baseDomain) {
		if cert, err := m.cmWild.GetCertificate(hello); err == nil {
			return cert, nil
		}
		m.kickWild(name)
		return m.fallbackCert, nil
	}

	// Not under the base → a customer custom domain → on-demand TLS-ALPN-01
	// (gated by the resolve-host DecisionFunc). Needs the handshake context.
	if m.cmOnDemand != nil && hello.Context() != nil {
		if cert, err := m.cmOnDemand.GetCertificate(hello); err == nil {
			return cert, nil
		}
	}
	return m.fallbackCert, nil
}

// kickWild issues the cert(s) covering an under-base host via DNS-01 in the
// background (deduped per name-set), so the next handshake serves a real cert.
func (m *MagicManager) kickWild(name string) {
	names := m.wildNamesFor(name)
	if len(names) == 0 {
		return
	}
	key := strings.Join(names, ",")

	m.mu.Lock()
	if m.inflight[key] {
		m.mu.Unlock()
		return
	}
	m.inflight[key] = true
	m.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		slog.Info("certs: issuing via DNS-01", "names", names)
		err := m.cmWild.ManageSync(ctx, names)
		m.mu.Lock()
		m.inflight[key] = false
		if err == nil {
			m.issuances += len(names)
		}
		m.mu.Unlock()
		if err != nil {
			slog.Warn("certs: DNS-01 issuance failed", "names", names, "err", err.Error())
			return
		}
		slog.Info("certs: issued via DNS-01", "names", names)
	}()
}

// wildNamesFor returns the cert SAN set for an under-base host: the per-slug
// wildcard (subdomain shape) or the base wildcard (hyphen/flat), or the apex.
func (m *MagicManager) wildNamesFor(name string) []string {
	if slug, ok := extractSlug(name, m.baseDomain); ok {
		return certNamesFor(slug, m.baseDomain)
	}
	if name == m.baseDomain {
		return []string{m.baseDomain}
	}
	return nil // deeper nesting under the base — unsupported
}

// PreWarm obtains a slug's wildcard ahead of its first handshake (DNS-01).
func (m *MagicManager) PreWarm(slug string) error {
	return m.cmWild.ManageSync(context.Background(), certNamesFor(slug, m.baseDomain))
}

func (m *MagicManager) IssuanceCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.issuances
}
