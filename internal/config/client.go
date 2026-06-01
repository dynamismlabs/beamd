package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Client is one identity: which edge + the bearer token. It's the shape of a
// profile file (~/.beamd/profiles/<name>) and of an explicit --config file
// (the automation path). AgentSocket is only set on the --config path; for
// profiles the socket is derived from the profile name (AgentSocketFor).
type Client struct {
	Server string `yaml:"server"`
	Token  string `yaml:"token"`
	// InsecureSkipVerify disables TLS verification of the edge's control
	// connection. Default (false) verifies the edge cert, so the bearer
	// token only ever rides a trusted connection. Set true ONLY for a
	// self-signed dev edge — it's the automation/agent equivalent of the
	// `--insecure` flag an interactive caller can pass instead.
	InsecureSkipVerify bool   `yaml:"insecure_skip_verify,omitempty"`
	AgentSocket        string `yaml:"agent_socket,omitempty"`
}

func DefaultClientPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".beamd", "config"), nil
}

func DefaultAgentSocket() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".beamd", "agent.sock"), nil
}

// SaveClient writes c to disk as YAML with 0600 perms, creating the
// parent directory if needed. Used by `beamd login`.
func SaveClient(c *Client, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir %q: %w", filepath.Dir(path), err)
	}
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

func LoadClient(path string) (*Client, error) {
	cfg := &Client{}
	b, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read config %q: %w", path, err)
		}
	} else if err := yaml.Unmarshal(b, cfg); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	if cfg.AgentSocket == "" {
		sock, err := DefaultAgentSocket()
		if err != nil {
			return nil, fmt.Errorf("resolve default agent socket: %w", err)
		}
		cfg.AgentSocket = sock
	}
	return cfg, nil
}
