package config

// Profiles let one machine stay logged into many edges at once (the
// kubectl/gh/aws-profile model). Layout under ~/.beamd:
//
//	config                 # Global: current profile + naming defaults
//	profiles/<name>        # one Client ({server, token}) per profile
//	agents/<name>.sock     # that profile's detached-agent socket

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

// NamingDefaults are the global fallback for tunnel naming (§2). Mirrors the
// `.beamd` keys so the precedence ladder is uniform.
type NamingDefaults struct {
	From string `yaml:"from,omitempty"`
	Name string `yaml:"name,omitempty"`
}

// Global is ~/.beamd/config: which profile is current + global defaults.
// It is intentionally an extensible struct (room for future `trusted_servers`
// etc.) rather than a bare value.
type Global struct {
	Current  string         `yaml:"current,omitempty"`
	Defaults NamingDefaults `yaml:"defaults,omitempty"`
}

func beamdDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".beamd"), nil
}

// GlobalPath is ~/.beamd/config (same path the legacy single-config used).
func GlobalPath() (string, error) { return DefaultClientPath() }

func ProfilesDir() (string, error) {
	d, err := beamdDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "profiles"), nil
}

func ProfilePath(name string) (string, error) {
	d, err := ProfilesDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, name), nil
}

func agentsDir() (string, error) {
	d, err := beamdDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "agents"), nil
}

// LoadGlobal reads ~/.beamd/config as a Global. A missing file yields a
// zero-value Global (no current profile yet), not an error.
func LoadGlobal() (*Global, error) {
	p, err := GlobalPath()
	if err != nil {
		return nil, err
	}
	g := &Global{}
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return g, nil
		}
		return nil, fmt.Errorf("read global config %q: %w", p, err)
	}
	if err := yaml.Unmarshal(b, g); err != nil {
		return nil, fmt.Errorf("parse global config %q: %w", p, err)
	}
	return g, nil
}

func SaveGlobal(g *Global) error {
	p, err := GlobalPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("mkdir %q: %w", filepath.Dir(p), err)
	}
	b, err := yaml.Marshal(g)
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}

// ListProfiles returns the names of all saved profiles, sorted.
func ListProfiles() ([]string, error) {
	d, err := ProfilesDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(d)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

func ProfileExists(name string) bool {
	p, err := ProfilePath(name)
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}

// LoadProfile reads profiles/<name> as a Client ({server, token}). The agent
// socket is derived per-profile by AgentSocketFor, not stored.
func LoadProfile(name string) (*Client, error) {
	p, err := ProfilePath(name)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("read profile %q: %w", name, err)
	}
	c := &Client{}
	if err := yaml.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("parse profile %q: %w", name, err)
	}
	return c, nil
}

func SaveProfile(name string, c *Client) error {
	p, err := ProfilePath(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("mkdir %q: %w", filepath.Dir(p), err)
	}
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}

// DeleteProfile removes profiles/<name>. Missing file is not an error.
func DeleteProfile(name string) error {
	p, err := ProfilePath(name)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// AgentSocketFor returns the detached-agent socket for a profile:
// ~/.beamd/agents/<name>.sock, so each profile's detached tunnels are held
// by their own agent.
func AgentSocketFor(name string) (string, error) {
	d, err := agentsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, name+".sock"), nil
}
