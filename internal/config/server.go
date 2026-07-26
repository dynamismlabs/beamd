package config

import (
	"fmt"
	"log/slog"
	"net"
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
	ListenQUIC       string `yaml:"listen_quic"`
	DisableQUIC      bool   `yaml:"disable_quic"`
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

	// Transport admission limits bound the receive-flow-control exposure of
	// authenticated tunnel sessions. These are concurrency limits, not
	// bandwidth limits.
	MaxStreamsPerSession int `yaml:"max_streams_per_session"`
	MaxStreamsTotal      int `yaml:"max_streams_total"`
	MaxPreAuthSessions   int `yaml:"max_pre_auth_sessions"`
	MaxSessionsTotal     int `yaml:"max_sessions_total"`

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

const (
	DefaultMaxStreamsPerSession = 64
	DefaultMaxStreamsTotal      = 128
	DefaultMaxPreAuthSessions   = 32
	DefaultMaxSessionsTotal     = 8

	MaxStreamsPerSession = 64
	MaxStreamsTotal      = 128
	MaxPreAuthSessions   = 128
	MaxSessionsTotal     = 8

	MaxYamuxWindowExposureBytes  int64 = 512 << 20
	MaxQUICConnectionWindowBytes int64 = 64 << 20
	MaxQUICWindowExposureBytes   int64 = 512 << 20
)

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

func defaultServer() *Server {
	return &Server{
		DisableQUIC:          true,
		MaxStreamsPerSession: DefaultMaxStreamsPerSession,
		MaxStreamsTotal:      DefaultMaxStreamsTotal,
		MaxPreAuthSessions:   DefaultMaxPreAuthSessions,
		MaxSessionsTotal:     DefaultMaxSessionsTotal,
	}
}

func LoadServer(path string) (*Server, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	// Defaults must exist before unmarshalling. In particular, this lets an
	// explicit `disable_quic: false` override the shipped true default.
	cfg := defaultServer()
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
	if err := applyServerEnvOverrides(cfg); err != nil {
		return nil, fmt.Errorf("environment overrides for %q: %w", path, err)
	}
	// Because this value started with non-zero compiled defaults, a zero here
	// can only have been explicitly supplied by YAML or an environment
	// override. Reject it before FinalizeRuntime applies defaults for direct
	// test literals.
	if err := validateConfiguredPartBCapacities(cfg); err != nil {
		return nil, fmt.Errorf("validate %q: %w", path, err)
	}
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
	return s.FinalizeRuntime()
}

// FinalizeRuntime resolves derived runtime defaults and validates transport
// capacity. It is intentionally idempotent: LoadServer calls it with the
// default yamux window, and the serve entry point calls it again after
// injecting the process-wide BEAMD_YAMUX_STREAM_WINDOW_BYTES value.
//
// Directly constructed Server values used by tests may call this method too;
// omitted capacity fields receive their shipped defaults. A wholly
// zero-capacity literal also receives the shipped default-off QUIC posture.
func (s *Server) FinalizeRuntime() error {
	if s.MaxRequestBodyBytes == 0 {
		s.MaxRequestBodyBytes = 32 << 20
	}
	allCapacitiesZero := s.MaxStreamsPerSession == 0 &&
		s.MaxStreamsTotal == 0 &&
		s.MaxPreAuthSessions == 0 &&
		s.MaxSessionsTotal == 0
	if allCapacitiesZero {
		// A wholly zero-valued direct literal predates Part B and therefore
		// inherits the shipped default-off posture. Loaded configs already
		// contain compiled defaults before YAML is decoded, so an explicit
		// YAML false is not changed here.
		s.DisableQUIC = true
	}
	if s.MaxStreamsPerSession == 0 {
		s.MaxStreamsPerSession = DefaultMaxStreamsPerSession
	}
	if s.MaxStreamsTotal == 0 {
		s.MaxStreamsTotal = DefaultMaxStreamsTotal
	}
	if s.MaxPreAuthSessions == 0 {
		s.MaxPreAuthSessions = DefaultMaxPreAuthSessions
	}
	if s.MaxSessionsTotal == 0 {
		s.MaxSessionsTotal = DefaultMaxSessionsTotal
	}
	if s.ListenQUIC == "" {
		s.ListenQUIC = s.ListenHTTPS
	}
	if s.YamuxStreamWindowBytes == 0 {
		s.YamuxStreamWindowBytes = DefaultYamuxStreamWindowBytes
	}
	if err := validateYamuxWindow(s.YamuxStreamWindowBytes); err != nil {
		return err
	}

	if s.MaxStreamsPerSession < 1 || s.MaxStreamsPerSession > MaxStreamsPerSession {
		return fmt.Errorf("max_streams_per_session must be between 1 and %d (got %d)",
			MaxStreamsPerSession, s.MaxStreamsPerSession)
	}
	if s.MaxStreamsTotal < s.MaxStreamsPerSession || s.MaxStreamsTotal > MaxStreamsTotal {
		return fmt.Errorf("max_streams_total must be between max_streams_per_session (%d) and %d (got %d)",
			s.MaxStreamsPerSession, MaxStreamsTotal, s.MaxStreamsTotal)
	}
	if s.MaxPreAuthSessions < 1 || s.MaxPreAuthSessions > MaxPreAuthSessions {
		return fmt.Errorf("max_pre_auth_sessions must be between 1 and %d (got %d)",
			MaxPreAuthSessions, s.MaxPreAuthSessions)
	}
	if s.MaxSessionsTotal < 1 || s.MaxSessionsTotal > MaxSessionsTotal {
		return fmt.Errorf("max_sessions_total must be between 1 and %d (got %d)",
			MaxSessionsTotal, s.MaxSessionsTotal)
	}

	if !s.DisableQUIC {
		if err := validateUDPListenAddress(s.ListenQUIC); err != nil {
			return fmt.Errorf("listen_quic %q: %w", s.ListenQUIC, err)
		}
	}

	yamuxExposure := s.YamuxStreamWindowBytes * int64(s.MaxStreamsTotal)
	if yamuxExposure > MaxYamuxWindowExposureBytes {
		return fmt.Errorf(
			"yamux window exposure exceeds maximum: %s=%d * max_streams_total=%d = %d bytes, maximum %d bytes",
			YamuxWindowEnvVar, s.YamuxStreamWindowBytes, s.MaxStreamsTotal,
			yamuxExposure, MaxYamuxWindowExposureBytes,
		)
	}
	quicExposure := MaxQUICConnectionWindowBytes * int64(s.MaxSessionsTotal)
	if quicExposure > MaxQUICWindowExposureBytes {
		return fmt.Errorf(
			"QUIC window exposure exceeds maximum: connection_window=%d * max_sessions_total=%d = %d bytes, maximum %d bytes",
			MaxQUICConnectionWindowBytes, s.MaxSessionsTotal, quicExposure,
			MaxQUICWindowExposureBytes,
		)
	}
	return nil
}

func validateConfiguredPartBCapacities(s *Server) error {
	switch {
	case s.MaxStreamsPerSession == 0:
		return fmt.Errorf("max_streams_per_session must not be zero")
	case s.MaxStreamsTotal == 0:
		return fmt.Errorf("max_streams_total must not be zero")
	case s.MaxPreAuthSessions == 0:
		return fmt.Errorf("max_pre_auth_sessions must not be zero")
	case s.MaxSessionsTotal == 0:
		return fmt.Errorf("max_sessions_total must not be zero")
	default:
		return nil
	}
}

func validateUDPListenAddress(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid UDP listen address: %w", err)
	}
	if port == "" {
		return fmt.Errorf("UDP listen port is required")
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return fmt.Errorf("UDP listen port must be numeric: %w", err)
	}
	if _, err := net.ResolveUDPAddr("udp", addr); err != nil {
		return fmt.Errorf("invalid UDP listen address: %w", err)
	}
	// Port zero is useful for tests and is a valid kernel-assigned listen port,
	// so it is deliberately accepted.
	return nil
}

// Shape returns the parsed URL shape (hyphen by default).
func (s *Server) Shape() naming.Shape {
	return naming.ParseShape(s.URLShape)
}

func applyServerEnvOverrides(s *Server) error {
	// Preserve the existing override semantics for pre-Part-B settings.
	// Part B settings below use LookupEnv's stricter present-empty contract.
	legacyStringEnvs := []struct {
		name string
		dst  *string
	}{
		{"BEAMD_BASE_DOMAIN", &s.BaseDomain},
		{"BEAMD_URL_SHAPE", &s.URLShape},
		{"BEAMD_EDGE_IPV4", &s.EdgeIPv4},
		{"BEAMD_EDGE_IPV6", &s.EdgeIPv6},
		{"BEAMD_LISTEN_HTTPS", &s.ListenHTTPS},
		{"BEAMD_ACME_EMAIL", &s.ACMEEmail},
		{"BEAMD_ACME_CA", &s.ACMECA},
		{"BEAMD_DNS_PROVIDER", &s.DNSProvider},
		{"BEAMD_DNS_PROVIDER_CREDS", &s.DNSProviderCreds},
		{"BEAMD_DNS_ZONE", &s.DNSZone},
		{"BEAMD_TOKEN_STORE", &s.TokenStore},
		{"BEAMD_DATA_DIR", &s.DataDir},
		{"BEAMD_METRICS_TOKEN", &s.MetricsToken},
	}
	for _, item := range legacyStringEnvs {
		if v := os.Getenv(item.name); v != "" {
			*item.dst = v
		}
	}

	if v := os.Getenv("BEAMD_PREVIEW_EMBED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			s.PreviewEmbed = b
		}
	}

	partBStringEnvs := []struct {
		name string
		dst  *string
	}{
		{"BEAMD_LISTEN_QUIC", &s.ListenQUIC},
	}
	for _, item := range partBStringEnvs {
		v, ok := os.LookupEnv(item.name)
		if !ok {
			continue
		}
		if v == "" {
			return fmt.Errorf("%s is present but empty", item.name)
		}
		*item.dst = v
	}

	partBBoolEnvs := []struct {
		name string
		dst  *bool
	}{
		{"BEAMD_DISABLE_QUIC", &s.DisableQUIC},
	}
	for _, item := range partBBoolEnvs {
		v, ok := os.LookupEnv(item.name)
		if !ok {
			continue
		}
		if v == "" {
			return fmt.Errorf("%s is present but empty", item.name)
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("%s=%q: parse boolean: %w", item.name, v, err)
		}
		*item.dst = b
	}

	partBIntEnvs := []struct {
		name string
		dst  *int
	}{
		{"BEAMD_MAX_STREAMS_PER_SESSION", &s.MaxStreamsPerSession},
		{"BEAMD_MAX_STREAMS_TOTAL", &s.MaxStreamsTotal},
		{"BEAMD_MAX_PRE_AUTH_SESSIONS", &s.MaxPreAuthSessions},
		{"BEAMD_MAX_SESSIONS_TOTAL", &s.MaxSessionsTotal},
	}
	for _, item := range partBIntEnvs {
		v, ok := os.LookupEnv(item.name)
		if !ok {
			continue
		}
		if v == "" {
			return fmt.Errorf("%s is present but empty", item.name)
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("%s=%q: parse base-10 integer: %w", item.name, v, err)
		}
		*item.dst = n
	}
	return nil
}
