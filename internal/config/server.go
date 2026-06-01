package config

import (
	"fmt"
	"os"
	"strconv"

	"gopkg.in/yaml.v3"
)

type Server struct {
	BaseDomain       string `yaml:"base_domain"`
	EdgeIPv4         string `yaml:"edge_ipv4"`
	EdgeIPv6         string `yaml:"edge_ipv6"`
	ListenHTTPS      string `yaml:"listen_https"`
	ACMEEmail        string `yaml:"acme_email"`
	ACMECA           string `yaml:"acme_ca"`
	DNSProvider      string `yaml:"dns_provider"`
	DNSProviderCreds string `yaml:"dns_provider_creds"`

	// DNSZone is the registered DNS zone to manage records in. Leave
	// blank to auto-detect from base_domain (recommended). Set it
	// explicitly only to skip the provider's zone lookup, e.g.
	// base_domain=tunnel.dynami.sm with dns_zone=dynami.sm.
	DNSZone            string `yaml:"dns_zone"`
	TokenStore         string `yaml:"token_store"`
	MaxTunnelsPerToken int    `yaml:"max_tunnels_per_token"`

	// DataDir is where beamd persists state (cert cache, ACME
	// account, etc.). Created if missing.
	DataDir string `yaml:"data_dir"`

	// MaxRequestBodyBytes caps the size of any single public request
	// body. The edge wraps each request body with http.MaxBytesReader
	// at this limit; oversized requests get HTTP 413. Defaults to 32
	// MiB. Set to -1 to disable.
	MaxRequestBodyBytes int64 `yaml:"max_request_body_bytes"`

	// PreviewEmbed, when true, makes the edge strip iframe-blocking
	// response headers (X-Frame-Options and the CSP frame-ancestors
	// directive) from tunnel responses, so previews can be embedded
	// cross-origin in a consumer app. Off by default — an app's own
	// framing policy is respected unless the operator opts in.
	PreviewEmbed bool `yaml:"preview_embed"`

	// AuthDiscovery describes the device-code endpoints to advertise at
	// /.well-known/beam-auth. Empty in OSS deployments (CLI then
	// requires --token); populated in hosted deployments so the client
	// can do browser-based login against the web app.
	AuthDiscovery AuthDiscovery `yaml:"auth_discovery"`

	// UsageReporter, if WebhookURL is set, pushes per-slug usage
	// deltas to that webhook on an interval. Hosted-only — the web
	// app does billing on top of the events it receives.
	UsageReporter UsageReporterConfig `yaml:"usage_reporter"`
}

// AuthDiscovery is the response body of /.well-known/beam-auth.
// All fields empty → device-code login is not offered.
type AuthDiscovery struct {
	DeviceCodeURL   string `yaml:"device_code_url"   json:"device_code_url,omitempty"`
	TokenURL        string `yaml:"token_url"         json:"token_url,omitempty"`
	VerificationURI string `yaml:"verification_uri"  json:"verification_uri,omitempty"`
}

// UsageReporterConfig configures the per-slug usage reporter. Leave
// WebhookURL empty to disable (OSS default — the same data is still
// exposed at `/metrics`).
type UsageReporterConfig struct {
	WebhookURL      string `yaml:"webhook_url"`
	SecretEnv       string `yaml:"secret_env"`
	IntervalSeconds int    `yaml:"interval_seconds"`
	StateFile       string `yaml:"state_file"`
}

func LoadServer(path string) (*Server, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	cfg := &Server{}
	if err := yaml.Unmarshal(b, cfg); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	applyServerEnvOverrides(cfg)
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate %q: %w", path, err)
	}
	return cfg, nil
}

func (s *Server) Validate() error {
	if s.BaseDomain == "" {
		return fmt.Errorf("base_domain is required")
	}
	if s.ListenHTTPS == "" {
		return fmt.Errorf("listen_https is required")
	}
	if s.ACMEEmail == "" {
		return fmt.Errorf("acme_email is required")
	}
	if s.DNSProvider == "" {
		return fmt.Errorf("dns_provider is required")
	}
	if s.TokenStore == "" {
		return fmt.Errorf("token_store is required")
	}
	if s.MaxTunnelsPerToken <= 0 {
		s.MaxTunnelsPerToken = 25
	}
	if s.DataDir == "" {
		s.DataDir = "/var/lib/beamd"
	}
	if s.MaxRequestBodyBytes == 0 {
		s.MaxRequestBodyBytes = 32 << 20 // 32 MiB default
	}
	return nil
}

func applyServerEnvOverrides(s *Server) {
	envs := map[string]*string{
		"BEAMD_BASE_DOMAIN":        &s.BaseDomain,
		"BEAMD_EDGE_IPV4":          &s.EdgeIPv4,
		"BEAMD_EDGE_IPV6":          &s.EdgeIPv6,
		"BEAMD_LISTEN_HTTPS":       &s.ListenHTTPS,
		"BEAMD_ACME_EMAIL":         &s.ACMEEmail,
		"BEAMD_ACME_CA":            &s.ACMECA,
		"BEAMD_DNS_PROVIDER":       &s.DNSProvider,
		"BEAMD_DNS_PROVIDER_CREDS": &s.DNSProviderCreds,
		"BEAMD_DNS_ZONE":           &s.DNSZone,
		"BEAMD_TOKEN_STORE":        &s.TokenStore,
		"BEAMD_DATA_DIR":           &s.DataDir,
	}
	for k, dst := range envs {
		if v := os.Getenv(k); v != "" {
			*dst = v
		}
	}
	if v := os.Getenv("BEAMD_PREVIEW_EMBED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			s.PreviewEmbed = b
		}
	}
}
