package certs

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log/slog"
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

	// EagerNames are managed (and issued if needed) at construction
	// time. Typically the operator's apex (`base_domain`) so the edge
	// can serve `/healthz`, `/metrics`, and `/.well-known/beam-auth`
	// with a real cert — the per-slug wildcard doesn't cover the apex.
	EagerNames []string

	// OnDemandDecision, when set, enables certmagic **On-Demand TLS** for hosts
	// that are NOT under the base domain — i.e. customer **custom domains**
	// (url-model §8, path B). A per-host cert is issued on the first handshake
	// only if this returns nil; otherwise issuance is refused. Wire it to
	// NewResolveHostAuthorizer so only control-plane-verified hosts get a cert —
	// that allowlist is what makes On-Demand safe. nil → custom domains are not
	// served (the host falls back to the self-signed cert). On-Demand certs use
	// HTTP-01 / TLS-ALPN-01 (only need the host to resolve to the edge), since we
	// don't control the customer's DNS.
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

// MagicManager is the production Manager. Backed by certmagic with
// ACME DNS-01 issuance via a libdns provider. On the first TLS
// handshake for a slug, it triggers issuance of `*.<slug>.<base>`
// and caches it; subsequent handshakes for that slug return the
// cached cert. Renewals are handled by certmagic in the background.
//
// **Not exercised in automated tests** — verifying real ACME requires
// either Pebble in CI or LE-staging with a real domain. Operators
// should validate locally against LE-staging before pointing
// production traffic. PRD §M4 lists this as v1's most important
// reclamation target.
type MagicManager struct {
	cm           *certmagic.Config
	baseDomain   string
	fallbackCert *tls.Certificate

	mu        sync.Mutex
	managed   map[string]struct{} // slug → already managed
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

	// We construct a fresh Config rather than mutating Default so
	// multiple Edges in one process don't share state. Cache is per
	// Config.
	cache := certmagic.NewCache(certmagic.CacheOptions{
		GetConfigForCert: func(cert certmagic.Certificate) (*certmagic.Config, error) {
			// All certs we manage share one config.
			return nil, nil
		},
	})

	cm := certmagic.New(cache, certmagic.Config{
		Storage: storage,
		// Silence certmagic's internal logger; we route via slog.
		Logger: nil,
	})

	propagationTimeout := cfg.PropagationTimeout
	if propagationTimeout == 0 {
		propagationTimeout = 2 * time.Minute
	}

	acmeCA := cfg.ACMECA
	if acmeCA == "" {
		acmeCA = certmagic.LetsEncryptProductionCA
	}

	// DNS-01 issuer: the only way to get the per-slug WILDCARD certs
	// (`*.<slug>.<base>` / `*.<base>`) the default URL shapes need — wildcards
	// are DNS-01-only, and the edge controls the base domain's DNS.
	dnsIssuer := certmagic.NewACMEIssuer(cm, certmagic.ACMEIssuer{
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
	})
	// TLS-ALPN-01 issuer: for on-demand custom-domain certs (url-model §8.2,
	// path B). We do NOT control the customer's DNS, so DNS-01 is unavailable for
	// `api.acme.com`; instead certmagic solves the challenge over the edge's own
	// :443 (the listener advertises `acme-tls/1`). Setting DNS01Solver disables
	// the HTTP-01/TLS-ALPN-01 solvers entirely (certmagic enables them only when
	// DNS01Solver is nil), which is exactly why a single DNS-01 issuer can't serve
	// custom domains — hence this second issuer. HTTP-01 is disabled (no :80
	// listener), leaving TLS-ALPN-01. Ordered AFTER the DNS issuer so the hot
	// wildcard path issues directly via DNS-01; a custom domain fails the DNS
	// issuer fast (its zone isn't in our provider) and falls through to here.
	alpnIssuer := certmagic.NewACMEIssuer(cm, certmagic.ACMEIssuer{
		CA:                   acmeCA,
		Email:                cfg.ACMEEmail,
		Agreed:               true,
		TrustedRoots:         cfg.ACMETrustedRoots,
		DisableHTTPChallenge: true,
		// 0 → certmagic default (:443, per the ACME spec). The integration test
		// points this at Pebble's TLS-ALPN validation port.
		AltTLSALPNPort: cfg.ChallengeTLSPort,
	})
	cm.Issuers = []certmagic.Issuer{dnsIssuer, alpnIssuer}

	// On-Demand TLS for custom domains (url-model §8.2, path B). The
	// DecisionFunc is the allowlist: certmagic asks it before issuing a per-host
	// cert, so only control-plane-verified hosts can mint one. Wildcard base
	// hosts continue to use the eager/managed DNS-01 path above — On-Demand only
	// kicks in for names certmagic isn't already managing (custom domains).
	if cfg.OnDemandDecision != nil {
		decide := cfg.OnDemandDecision
		cm.OnDemand = &certmagic.OnDemandConfig{
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
		cm:           cm,
		baseDomain:   cfg.BaseDomain,
		fallbackCert: &fallback,
		managed:      make(map[string]struct{}),
	}

	// Eagerly manage operator-specified names (typically the apex
	// domain) so the discovery + /healthz endpoints have a real cert.
	if len(cfg.EagerNames) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		if err := cm.ManageSync(ctx, cfg.EagerNames); err != nil {
			cancel()
			return nil, fmt.Errorf("manage eager names %v: %w", cfg.EagerNames, err)
		}
		cancel()
		slog.Info("certs: eager names managed", "names", cfg.EagerNames)
	}

	return m, nil
}

func (m *MagicManager) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	slug, ok := extractSlug(hello.ServerName, m.baseDomain)
	if !ok {
		// No per-developer slug in the SNI — this is the apex / an eagerly-
		// managed name, OR a customer **custom domain** (url-model §8). Consult
		// certmagic: it returns the managed cert for the apex, and — when
		// On-Demand is enabled — issues a per-host cert for a verified custom
		// domain (gated by the DecisionFunc) and solves the ACME challenge
		// (incl. TLS-ALPN-01 over this very handshake). certmagic needs the
		// handshake context, so only consult it when there is one (real
		// handshakes always set it); for genuinely unknown SNIs / beam control
		// connections — which skip verification anyway — use the self-signed
		// fallback.
		if hello.Context() != nil {
			if cert, err := m.cm.GetCertificate(hello); err == nil {
				return cert, nil
			}
		}
		return m.fallbackCert, nil
	}

	if err := m.ensureManaged(hello.Context(), slug); err != nil {
		return nil, fmt.Errorf("ensure wildcard for slug %q: %w", slug, err)
	}
	return m.cm.GetCertificate(hello)
}

func (m *MagicManager) PreWarm(slug string) error {
	return m.ensureManaged(context.Background(), slug)
}

func (m *MagicManager) IssuanceCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.issuances
}

func (m *MagicManager) ensureManaged(ctx context.Context, slug string) error {
	m.mu.Lock()
	if _, ok := m.managed[slug]; ok {
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()

	names := certNamesFor(slug, m.baseDomain)

	slog.Info("certs: managing wildcard via ACME", "slug", slug, "names", names)
	if err := m.cm.ManageSync(ctx, names); err != nil {
		return err
	}

	m.mu.Lock()
	m.managed[slug] = struct{}{}
	m.issuances++
	m.mu.Unlock()
	slog.Info("certs: ACME wildcard issued", "slug", slug, "issuance_count", m.issuances)
	return nil
}

// silence unused-import warnings in environments where certmagic's
// generic logger is the only reason io is referenced.
var _ = io.Discard
