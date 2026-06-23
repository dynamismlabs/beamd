package config

import (
	"os"
	"path/filepath"
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
