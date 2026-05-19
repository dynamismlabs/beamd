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
	if cfg.DaemonSocket == "" {
		t.Error("DaemonSocket should default to ~/.beam/daemon.sock")
	}
}

func TestLoadClient_ExplicitDaemonSocket(t *testing.T) {
	body := "daemon_socket: /tmp/custom.sock\n"
	p := writeFile(t, t.TempDir(), "config", body)
	cfg, err := LoadClient(p)
	if err != nil {
		t.Fatalf("LoadClient: %v", err)
	}
	if cfg.DaemonSocket != "/tmp/custom.sock" {
		t.Errorf("DaemonSocket = %q, want /tmp/custom.sock", cfg.DaemonSocket)
	}
}
