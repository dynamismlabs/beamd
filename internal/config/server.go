package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"

	"gopkg.in/yaml.v3"

	"github.com/dynamismlabs/beamd/internal/naming"
)

type Server struct {
	BaseDomain string `yaml:"base_domain"`
	// URLShape is how tunnel hostnames are rendered + routed: "hyphen"
	// (<name>-<slug>.<base>, the default), "subdomain" (<name>.<slug>.<base>),
	// or "flat" (<name>.<base>). MUST match the control plane's
	// NEXT_PUBLIC_URL_SHAPE — the edge routes by Host and issues the certs.
	URLShape         string `yaml:"url_shape"`
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

	// MetricsToken gates the operator /metrics endpoint (which exposes every
	// slug, tunnel name, and byte count). Empty = /metrics is DISABLED (404);
	// set it (or BEAMD_METRICS_TOKEN) and scrapers must send
	// `Authorization: Bearer <token>`. Never blank-and-public — the dump is
	// cross-tenant sensitive.
	MetricsToken string `yaml:"metrics_token"`

	// DataDir is where beamd persists state (cert cache, ACME
	// account, etc.). Created if missing.
	DataDir string `yaml:"data_dir"`

	// MaxRequestBodyBytes caps the size of any single public request
	// body. The edge wraps each request body with http.MaxBytesReader
	// at this limit; oversized requests get HTTP 413. Defaults to 32
	// MiB. Set to -1 to disable.
	MaxRequestBodyBytes int64 `yaml:"max_request_body_bytes"`

	// YamuxStreamWindowBytes is a RUNTIME-INJECTION field (never persisted): the
	// edge's yamux per-stream receive window in bytes, resolved once from
	// BEAMD_YAMUX_STREAM_WINDOW_BYTES by the serve entry point and set here before
	// edge.New (transport-performance-spec §8.1 / §11.1). It governs the
	// download/response-body direction (agent → edge). `yaml:"-"` keeps it out of
	// config files; 0 means "unset" and mux applies the 4 MiB default.
	YamuxStreamWindowBytes int64 `yaml:"-"`

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

	// RequestLog configures the per-request event file sink (always on —
	// OSS gets local request logs for free).
	RequestLog RequestLogConfig `yaml:"request_log"`

	// RequestReporter, if WebhookURL is set, tails the request log and ships
	// batches to the control plane. Hosted-only — the web app does billing +
	// analytics on top of the events it receives (request-events-spec §4.6).
	RequestReporter RequestReporterConfig `yaml:"request_reporter"`
}

// AuthDiscovery is the response body of /.well-known/beam-auth.
// All fields empty → device-code login is not offered.
type AuthDiscovery struct {
	DeviceCodeURL   string `yaml:"device_code_url"   json:"device_code_url,omitempty"`
	TokenURL        string `yaml:"token_url"         json:"token_url,omitempty"`
	VerificationURI string `yaml:"verification_uri"  json:"verification_uri,omitempty"`
}

// RequestLogConfig configures the always-on per-request file sink.
type RequestLogConfig struct {
	Enabled          *bool         `yaml:"enabled"`           // default true
	Path             string        `yaml:"path"`              // default <data_dir>/requests.log
	MaxSizeMB        int           `yaml:"max_size_mb"`       // default 128
	FsyncMs          int           `yaml:"fsync_ms"`          // default 250
	HeartbeatSeconds int           `yaml:"heartbeat_seconds"` // long-conn window; default 60
	IPMode           string        `yaml:"ip_mode"`           // truncate (default) | off
	Capture          CaptureConfig `yaml:"capture"`           // analytics-field toggles
}

// CaptureConfig toggles the analytics fields (billing fields always ship). A nil
// pointer means "default on"; set false to drop the field at the edge.
type CaptureConfig struct {
	Path      *bool `yaml:"path"`
	ClientIP  *bool `yaml:"client_ip"`
	UserAgent *bool `yaml:"user_agent"`
	Referer   *bool `yaml:"referer"`
}

// RequestReporterConfig configures the hosted-only request shipper. Leave
// WebhookURL empty to disable (OSS default — the local file is still written).
type RequestReporterConfig struct {
	WebhookURL string `yaml:"webhook_url"`
	SecretEnv  string `yaml:"secret_env"`
	BatchSize  int    `yaml:"batch_size"`  // default 500
	FlushMs    int    `yaml:"flush_ms"`    // default 1000
	CursorFile string `yaml:"cursor_file"` // default <data_dir>/requests.cursor
}

// boolOr returns *p when set, else def — for "default on" toggles.
func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

// RequestLogEnabled reports whether the file sink should run (default true).
func (s *Server) RequestLogEnabled() bool { return boolOr(s.RequestLog.Enabled, true) }

// Captures resolves the analytics-field toggles (all default on).
func (c CaptureConfig) Captures() (path, clientIP, userAgent, referer bool) {
	return boolOr(c.Path, true), boolOr(c.ClientIP, true), boolOr(c.UserAgent, true), boolOr(c.Referer, true)
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
	// An inline DNS-provider credential (a zone-controlling API token) in a
	// group/world-readable config is a local-disclosure risk. Warn loudly —
	// checked against the YAML value before env overrides, so the env-var form
	// (BEAMD_DNS_PROVIDER_CREDS, the recommended path) doesn't trip it.
	if cfg.DNSProviderCreds != "" {
		if fi, statErr := os.Stat(path); statErr == nil && fi.Mode().Perm()&0o077 != 0 {
			slog.Warn("config: dns_provider_creds is set inline in a group/world-readable file — chmod it 0600 or use the BEAMD_DNS_PROVIDER_CREDS env var",
				"path", path, "mode", fi.Mode().Perm().String())
		}
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
	switch naming.Shape(s.URLShape) {
	case "":
		s.URLShape = string(naming.ShapeHyphen) // the shipped default (matches the control plane)
	case naming.ShapeHyphen, naming.ShapeSubdomain, naming.ShapeFlat:
		// valid — leave as-is.
	default:
		// Fail loud rather than silently fall back to hyphen (ParseShape maps any
		// unknown string to hyphen): a typo like "subdomian" would otherwise make
		// the edge route + issue certs for a different shape than the control plane
		// renders.
		return fmt.Errorf("url_shape %q is not supported (use %q, %q, or %q)",
			s.URLShape, naming.ShapeHyphen, naming.ShapeSubdomain, naming.ShapeFlat)
	}
	switch s.RequestLog.IPMode {
	case "", "truncate", "off":
		// "" / "truncate" minimize the IP at the edge; "off" drops it entirely.
	default:
		// Fail loud rather than silently truncate a mode we don't implement (e.g.
		// "hash") — an operator picking it expects different behavior.
		return fmt.Errorf("request_log.ip_mode %q is not supported (use \"truncate\" or \"off\")", s.RequestLog.IPMode)
	}
	return nil
}

// Shape returns the parsed URL shape (hyphen by default).
func (s *Server) Shape() naming.Shape {
	return naming.ParseShape(s.URLShape)
}

func applyServerEnvOverrides(s *Server) {
	envs := map[string]*string{
		"BEAMD_BASE_DOMAIN":        &s.BaseDomain,
		"BEAMD_URL_SHAPE":          &s.URLShape,
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
		"BEAMD_METRICS_TOKEN":      &s.MetricsToken,
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
