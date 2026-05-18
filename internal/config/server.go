package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Server struct {
	BaseDomain         string `yaml:"base_domain"`
	EdgeIPv4           string `yaml:"edge_ipv4"`
	EdgeIPv6           string `yaml:"edge_ipv6"`
	ListenHTTPS        string `yaml:"listen_https"`
	ACMEEmail          string `yaml:"acme_email"`
	ACMECA             string `yaml:"acme_ca"`
	DNSProvider        string `yaml:"dns_provider"`
	DNSProviderCreds   string `yaml:"dns_provider_creds"`
	TokenStore         string `yaml:"token_store"`
	MaxTunnelsPerToken int    `yaml:"max_tunnels_per_token"`

	// DataDir is where conduitd persists state (cert cache, ACME
	// account, etc.). Created if missing.
	DataDir string `yaml:"data_dir"`

	// MaxRequestBodyBytes caps the size of any single public request
	// body. The edge wraps each request body with http.MaxBytesReader
	// at this limit; oversized requests get HTTP 413. Defaults to 32
	// MiB. Set to -1 to disable.
	MaxRequestBodyBytes int64 `yaml:"max_request_body_bytes"`

	// AuthDiscovery describes the device-code endpoints to advertise at
	// /.well-known/conduit-auth. Empty in OSS deployments (CLI then
	// requires --token); populated in hosted deployments so the client
	// can do browser-based login against the web app.
	AuthDiscovery AuthDiscovery `yaml:"auth_discovery"`
}

// AuthDiscovery is the response body of /.well-known/conduit-auth.
// All fields empty → device-code login is not offered.
type AuthDiscovery struct {
	DeviceCodeURL   string `yaml:"device_code_url"   json:"device_code_url,omitempty"`
	TokenURL        string `yaml:"token_url"         json:"token_url,omitempty"`
	VerificationURI string `yaml:"verification_uri"  json:"verification_uri,omitempty"`
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
		s.DataDir = "/var/lib/conduit"
	}
	if s.MaxRequestBodyBytes == 0 {
		s.MaxRequestBodyBytes = 32 << 20 // 32 MiB default
	}
	return nil
}

func applyServerEnvOverrides(s *Server) {
	envs := map[string]*string{
		"CONDUIT_BASE_DOMAIN":        &s.BaseDomain,
		"CONDUIT_EDGE_IPV4":          &s.EdgeIPv4,
		"CONDUIT_EDGE_IPV6":          &s.EdgeIPv6,
		"CONDUIT_LISTEN_HTTPS":       &s.ListenHTTPS,
		"CONDUIT_ACME_EMAIL":         &s.ACMEEmail,
		"CONDUIT_ACME_CA":            &s.ACMECA,
		"CONDUIT_DNS_PROVIDER":       &s.DNSProvider,
		"CONDUIT_DNS_PROVIDER_CREDS": &s.DNSProviderCreds,
		"CONDUIT_TOKEN_STORE":        &s.TokenStore,
		"CONDUIT_DATA_DIR":           &s.DataDir,
	}
	for k, dst := range envs {
		if v := os.Getenv(k); v != "" {
			*dst = v
		}
	}
}
