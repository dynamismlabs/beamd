package config

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validServerYAML = `
base_domain: beam.example.com
edge_ipv4: 1.2.3.4
listen_https: ":8443"
acme_email: ops@example.com
dns_provider: cloudflare
token_store: file:./tokens.json
max_tunnels_per_token: 25
`

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

func TestLoadServer_Valid(t *testing.T) {
	p := writeFile(t, t.TempDir(), "beamd.yaml", validServerYAML)
	cfg, err := LoadServer(p)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if cfg.BaseDomain != "beam.example.com" {
		t.Errorf("BaseDomain = %q, want beam.example.com", cfg.BaseDomain)
	}
	if cfg.MaxTunnelsPerToken != 25 {
		t.Errorf("MaxTunnelsPerToken = %d, want 25", cfg.MaxTunnelsPerToken)
	}
	if cfg.ListenQUIC != ":8443" {
		t.Errorf("ListenQUIC = %q, want derived :8443", cfg.ListenQUIC)
	}
	if !cfg.DisableQUIC {
		t.Error("DisableQUIC = false, want shipped default true")
	}
	if cfg.MaxStreamsPerSession != 64 || cfg.MaxStreamsTotal != 128 ||
		cfg.MaxPreAuthSessions != 32 || cfg.MaxSessionsTotal != 8 {
		t.Errorf("Part B defaults not applied: %+v", cfg)
	}
	if cfg.YamuxStreamWindowBytes != DefaultYamuxStreamWindowBytes {
		t.Errorf("YamuxStreamWindowBytes = %d, want %d",
			cfg.YamuxStreamWindowBytes, DefaultYamuxStreamWindowBytes)
	}
}

func TestLoadServer_MissingRequired(t *testing.T) {
	p := writeFile(t, t.TempDir(), "beamd.yaml", `listen_https: ":8443"`)
	if _, err := LoadServer(p); err == nil {
		t.Fatal("expected error for missing required fields")
	}
}

func TestLoadServer_IPMode(t *testing.T) {
	// Supported modes load (incl. unquoted `off` — yaml.v3 keeps it a string).
	for _, mode := range []string{"", "truncate", "off"} {
		body := validServerYAML
		if mode != "" {
			body += "request_log:\n  ip_mode: " + mode + "\n"
		}
		p := writeFile(t, t.TempDir(), "beamd.yaml", body)
		cfg, err := LoadServer(p)
		if err != nil {
			t.Fatalf("ip_mode %q: unexpected error: %v", mode, err)
		}
		if cfg.RequestLog.IPMode != mode {
			t.Errorf("ip_mode = %q, want %q", cfg.RequestLog.IPMode, mode)
		}
	}
	// An unimplemented mode must fail loudly rather than silently truncate — an
	// operator picking "hash" expects hashing, not /24 truncation.
	p := writeFile(t, t.TempDir(), "beamd.yaml", validServerYAML+"request_log:\n  ip_mode: hash\n")
	if _, err := LoadServer(p); err == nil {
		t.Fatal("expected an error for unsupported ip_mode: hash")
	}
}

func TestLoadServer_URLShape(t *testing.T) {
	// Valid shapes load; empty defaults to hyphen (matches the control plane).
	for in, want := range map[string]string{"": "hyphen", "hyphen": "hyphen", "subdomain": "subdomain", "flat": "flat"} {
		body := validServerYAML
		if in != "" {
			body += "url_shape: " + in + "\n"
		}
		p := writeFile(t, t.TempDir(), "beamd.yaml", body)
		cfg, err := LoadServer(p)
		if err != nil {
			t.Fatalf("url_shape %q: unexpected error: %v", in, err)
		}
		if cfg.URLShape != want {
			t.Errorf("url_shape %q → %q, want %q", in, cfg.URLShape, want)
		}
	}
	// A typo must fail loudly rather than silently become hyphen — otherwise the
	// edge would route + issue certs for a different shape than the control plane.
	p := writeFile(t, t.TempDir(), "beamd.yaml", validServerYAML+"url_shape: subdomian\n")
	if _, err := LoadServer(p); err == nil {
		t.Fatal("expected an error for unsupported url_shape: subdomian")
	}
}

func TestLoadServer_EnvOverride(t *testing.T) {
	p := writeFile(t, t.TempDir(), "beamd.yaml", validServerYAML)
	t.Setenv("BEAMD_BASE_DOMAIN", "override.example.com")
	cfg, err := LoadServer(p)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if cfg.BaseDomain != "override.example.com" {
		t.Errorf("BaseDomain = %q, want override.example.com (env override)", cfg.BaseDomain)
	}
}

func TestLoadServer_PartBDefaultsPrecedeYAMLAndEnv(t *testing.T) {
	body := validServerYAML + `
listen_quic: ":9443"
disable_quic: false
max_streams_per_session: 48
max_streams_total: 96
max_pre_auth_sessions: 24
max_sessions_total: 6
`
	p := writeFile(t, t.TempDir(), "beamd.yaml", body)

	t.Setenv("BEAMD_LISTEN_QUIC", ":10443")
	t.Setenv("BEAMD_DISABLE_QUIC", "true")
	t.Setenv("BEAMD_MAX_STREAMS_PER_SESSION", "32")
	t.Setenv("BEAMD_MAX_STREAMS_TOTAL", "64")
	t.Setenv("BEAMD_MAX_PRE_AUTH_SESSIONS", "16")
	t.Setenv("BEAMD_MAX_SESSIONS_TOTAL", "4")

	cfg, err := LoadServer(p)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if cfg.ListenQUIC != ":10443" || !cfg.DisableQUIC {
		t.Errorf("environment did not win over YAML: listen=%q disabled=%v",
			cfg.ListenQUIC, cfg.DisableQUIC)
	}
	if cfg.MaxStreamsPerSession != 32 || cfg.MaxStreamsTotal != 64 ||
		cfg.MaxPreAuthSessions != 16 || cfg.MaxSessionsTotal != 4 {
		t.Errorf("capacity environment overrides not applied: %+v", cfg)
	}
}

func TestLoadServer_ExplicitFalseOverridesDefault(t *testing.T) {
	// Explicit YAML false must survive the compiled true default when no
	// environment override is present.
	p := writeFile(t, t.TempDir(), "beamd.yaml", validServerYAML+"disable_quic: false\n")
	cfg, err := LoadServer(p)
	if err != nil {
		t.Fatalf("LoadServer explicit false: %v", err)
	}
	if cfg.DisableQUIC {
		t.Error("explicit disable_quic: false was replaced by the true default")
	}
}

func TestLoadServer_PartBPresentEmptyEnvFails(t *testing.T) {
	for _, name := range []string{
		"BEAMD_LISTEN_QUIC",
		"BEAMD_DISABLE_QUIC",
		"BEAMD_MAX_STREAMS_PER_SESSION",
		"BEAMD_MAX_STREAMS_TOTAL",
		"BEAMD_MAX_PRE_AUTH_SESSIONS",
		"BEAMD_MAX_SESSIONS_TOTAL",
	} {
		t.Run(name, func(t *testing.T) {
			p := writeFile(t, t.TempDir(), "beamd.yaml", validServerYAML)
			t.Setenv(name, "")
			_, err := LoadServer(p)
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("LoadServer error = %v, want present-empty %s error", err, name)
			}
		})
	}
}

func TestLoadServer_PartBInvalidEnvFails(t *testing.T) {
	cases := map[string]string{
		"BEAMD_DISABLE_QUIC":            "sometimes",
		"BEAMD_MAX_STREAMS_PER_SESSION": "sixty-four",
		"BEAMD_MAX_STREAMS_TOTAL":       "12x",
		"BEAMD_MAX_PRE_AUTH_SESSIONS":   "9999999999999999999999999",
		"BEAMD_MAX_SESSIONS_TOTAL":      "1.5",
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			p := writeFile(t, t.TempDir(), "beamd.yaml", validServerYAML)
			t.Setenv(name, value)
			_, err := LoadServer(p)
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("LoadServer error = %v, want invalid %s error", err, name)
			}
		})
	}
}

func TestLoadServer_DisabledQUICSkipsAddressValidation(t *testing.T) {
	badAddress := validServerYAML + "listen_quic: definitely-not-a-listen-address\n"
	p := writeFile(t, t.TempDir(), "beamd.yaml", badAddress)
	if _, err := LoadServer(p); err != nil {
		t.Fatalf("disabled QUIC must ignore its malformed address: %v", err)
	}

	p = writeFile(t, t.TempDir(), "beamd.yaml", badAddress+"disable_quic: false\n")
	if _, err := LoadServer(p); err == nil || !strings.Contains(err.Error(), "listen_quic") {
		t.Fatalf("enabled QUIC malformed address error = %v, want listen_quic error", err)
	}

	p = writeFile(t, t.TempDir(), "beamd.yaml",
		validServerYAML+"listen_quic: \":https\"\ndisable_quic: false\n")
	if _, err := LoadServer(p); err == nil || !strings.Contains(err.Error(), "numeric") {
		t.Fatalf("enabled QUIC service-name port error = %v, want numeric-port error", err)
	}
}

func TestServerFinalizeRuntime_CapacityBounds(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Server)
		field  string
	}{
		{"per-session-negative", func(s *Server) { s.MaxStreamsPerSession = -1 }, "max_streams_per_session"},
		{"per-session-high", func(s *Server) { s.MaxStreamsPerSession = 65 }, "max_streams_per_session"},
		{"total-below-session", func(s *Server) { s.MaxStreamsTotal = 63 }, "max_streams_total"},
		{"total-high", func(s *Server) { s.MaxStreamsTotal = 129 }, "max_streams_total"},
		{"preauth-negative", func(s *Server) { s.MaxPreAuthSessions = -1 }, "max_pre_auth_sessions"},
		{"preauth-high", func(s *Server) { s.MaxPreAuthSessions = 129 }, "max_pre_auth_sessions"},
		{"sessions-negative", func(s *Server) { s.MaxSessionsTotal = -1 }, "max_sessions_total"},
		{"sessions-high", func(s *Server) { s.MaxSessionsTotal = 9 }, "max_sessions_total"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := defaultServer()
			cfg.ListenHTTPS = ":8443"
			tc.mutate(cfg)
			err := cfg.FinalizeRuntime()
			if err == nil || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("FinalizeRuntime error = %v, want %s bound error", err, tc.field)
			}
		})
	}
}

func TestServerFinalizeRuntime_WindowProductsAndIdempotence(t *testing.T) {
	cfg := defaultServer()
	cfg.ListenHTTPS = ":8443"
	cfg.YamuxStreamWindowBytes = 8 << 20
	cfg.MaxStreamsTotal = 64
	if err := cfg.FinalizeRuntime(); err != nil {
		t.Fatalf("8 MiB * 64 should pass: %v", err)
	}
	if err := cfg.FinalizeRuntime(); err != nil {
		t.Fatalf("second FinalizeRuntime call should be idempotent: %v", err)
	}
	if cfg.ListenQUIC != ":8443" {
		t.Errorf("derived ListenQUIC = %q, want :8443", cfg.ListenQUIC)
	}

	cfg.MaxStreamsTotal = 65
	err := cfg.FinalizeRuntime()
	if err == nil ||
		!strings.Contains(err.Error(), YamuxWindowEnvVar) ||
		!strings.Contains(err.Error(), "max_streams_total=65") ||
		!strings.Contains(err.Error(), "536870912") {
		t.Fatalf("product error = %v, want both effective values and 512 MiB maximum", err)
	}

	cfg = defaultServer()
	cfg.ListenHTTPS = ":8443"
	cfg.YamuxStreamWindowBytes = 16 << 20
	cfg.MaxStreamsPerSession = 32
	cfg.MaxStreamsTotal = 32
	if err := cfg.FinalizeRuntime(); err != nil {
		t.Fatalf("16 MiB * 32 should pass: %v", err)
	}
}

func TestServerFinalizeRuntime_DirectLiteralGetsShippedDefaults(t *testing.T) {
	cfg := &Server{ListenHTTPS: ":8443"}
	if err := cfg.FinalizeRuntime(); err != nil {
		t.Fatalf("FinalizeRuntime direct literal: %v", err)
	}
	if !cfg.DisableQUIC {
		t.Error("zero-valued direct literal should inherit default-off QUIC")
	}
	if cfg.ListenQUIC != ":8443" ||
		cfg.MaxStreamsPerSession != DefaultMaxStreamsPerSession ||
		cfg.MaxStreamsTotal != DefaultMaxStreamsTotal ||
		cfg.MaxPreAuthSessions != DefaultMaxPreAuthSessions ||
		cfg.MaxSessionsTotal != DefaultMaxSessionsTotal {
		t.Errorf("direct literal defaults not applied: %+v", cfg)
	}
	if err := cfg.FinalizeRuntime(); err != nil {
		t.Fatalf("second FinalizeRuntime direct literal: %v", err)
	}

	// A direct QUIC test can make false explicit by supplying any capacity;
	// the remaining zero-valued capacities still inherit their defaults.
	enabled := &Server{
		ListenHTTPS:          ":8443",
		DisableQUIC:          false,
		MaxStreamsPerSession: DefaultMaxStreamsPerSession,
	}
	if err := enabled.FinalizeRuntime(); err != nil {
		t.Fatalf("FinalizeRuntime enabled direct literal: %v", err)
	}
	if enabled.DisableQUIC {
		t.Error("partially populated direct literal's explicit false was changed")
	}
}

func TestLoadServer_ExplicitZeroDoesNotRestoreDefault(t *testing.T) {
	p := writeFile(t, t.TempDir(), "beamd.yaml",
		validServerYAML+"max_streams_total: 0\n")
	if _, err := LoadServer(p); err == nil || !strings.Contains(err.Error(), "max_streams_total") {
		t.Fatalf("explicit zero error = %v, want max_streams_total validation failure", err)
	}

	t.Setenv("BEAMD_MAX_STREAMS_TOTAL", "0")
	p = writeFile(t, t.TempDir(), "beamd.yaml", validServerYAML)
	if _, err := LoadServer(p); err == nil || !strings.Contains(err.Error(), "max_streams_total") {
		t.Fatalf("environment zero error = %v, want max_streams_total validation failure", err)
	}
}

func TestLoadClient_MissingFile_DefaultsApplied(t *testing.T) {
	cfg, err := LoadClient(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("LoadClient should tolerate missing file: %v", err)
	}
	if cfg.AgentSocket == "" {
		t.Error("AgentSocket should default to ~/.beamd/agent.sock")
	}
}

func TestLoadClient_ExplicitAgentSocket(t *testing.T) {
	body := "agent_socket: /tmp/custom.sock\n"
	p := writeFile(t, t.TempDir(), "config", body)
	cfg, err := LoadClient(p)
	if err != nil {
		t.Fatalf("LoadClient: %v", err)
	}
	if cfg.AgentSocket != "/tmp/custom.sock" {
		t.Errorf("AgentSocket = %q, want /tmp/custom.sock", cfg.AgentSocket)
	}
}

func TestClientTransportPersistenceAndRuntimeOverride(t *testing.T) {
	p := writeFile(t, t.TempDir(), "config",
		"server: edge.example.com:443\ntoken: token\ntransport: auto\n")
	cfg, err := LoadClient(p)
	if err != nil {
		t.Fatalf("LoadClient: %v", err)
	}
	if cfg.Transport != TransportAuto {
		t.Fatalf("persisted transport = %q, want auto", cfg.Transport)
	}

	t.Setenv(TransportEnvVar, TransportQUIC)
	effective, err := ResolveTransport(cfg.Transport)
	if err != nil {
		t.Fatalf("ResolveTransport: %v", err)
	}
	if effective != TransportQUIC {
		t.Errorf("effective transport = %q, want quic", effective)
	}
	if cfg.Transport != TransportAuto {
		t.Errorf("runtime override mutated persisted value to %q", cfg.Transport)
	}

	saved := filepath.Join(t.TempDir(), "saved", "config")
	if err := SaveClient(cfg, saved); err != nil {
		t.Fatalf("SaveClient: %v", err)
	}
	b, err := os.ReadFile(saved)
	if err != nil {
		t.Fatalf("read saved config: %v", err)
	}
	if !strings.Contains(string(b), "transport: auto") ||
		strings.Contains(string(b), "transport: quic") {
		t.Errorf("saved config unexpectedly includes runtime override:\n%s", b)
	}
}

// SEC-6: an inline dns_provider_creds in a group/world-readable config must
// warn; a 0600 file (or env-provided creds) must not.
func TestLoadServer_WarnsOnLooseInlineCreds(t *testing.T) {
	base := validServerYAML + "dns_provider_creds: cf-secret-token\n"

	capture := func(t *testing.T, mode os.FileMode, body string, envCreds string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "beamd.yaml")
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil { // WriteFile mode is umask-masked
			t.Fatal(err)
		}
		if envCreds != "" {
			t.Setenv("BEAMD_DNS_PROVIDER_CREDS", envCreds)
		}
		var buf bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
		defer slog.SetDefault(prev)
		if _, err := LoadServer(p); err != nil {
			t.Fatalf("LoadServer: %v", err)
		}
		return buf.String()
	}

	if out := capture(t, 0o644, base, ""); !strings.Contains(out, "dns_provider_creds") {
		t.Errorf("0644 inline creds should warn, log was: %q", out)
	}
	if out := capture(t, 0o600, base, ""); strings.Contains(out, "dns_provider_creds") {
		t.Errorf("0600 inline creds should NOT warn, log was: %q", out)
	}
	// Creds via env only (no inline field) — the loose file has no secret.
	if out := capture(t, 0o644, validServerYAML, "cf-secret-token"); strings.Contains(out, "dns_provider_creds") {
		t.Errorf("env-provided creds should NOT warn on a loose file, log was: %q", out)
	}
}
