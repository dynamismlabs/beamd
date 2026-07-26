package config

// Accounts: one credential per beamd server. A machine can be logged into
// several edges at once; each is an "account" keyed by its server host (the
// gh-auth / kubectl-context model). Layout under ~/.beamd:
//
//	config                  # Global: default account (current server) + naming defaults
//	accounts/<host>.yaml    # one Account per server
//	agents/<host>.sock      # that account's detached-agent socket
//
// Hosted accounts carry a cached scope set + a default scope; OSS accounts are
// just {server, token} (+ an optional operator-assigned slug). See
// docs/identity-and-accounts.md.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"gopkg.in/yaml.v3"
)

// NamingDefaults are the global fallback for tunnel naming. Mirrors the
// `beamd.yaml` keys so the precedence ladder is uniform.
type NamingDefaults struct {
	From string `yaml:"from,omitempty"`
	Name string `yaml:"name,omitempty"`
}

// Global is ~/.beamd/config: which account is current (by server) + global
// naming defaults. Extensible (room for future keys like trusted_servers).
type Global struct {
	Current  string         `yaml:"current,omitempty"` // the default account's server
	Defaults NamingDefaults `yaml:"defaults,omitempty"`
}

// ScopeRef is one org a session may act in (hosted). Cached for display/pick.
type ScopeRef struct {
	Slug string `yaml:"slug"`
	Role string `yaml:"role,omitempty"`
}

// Account is one credential bound to one server (~/.beamd/accounts/<host>.yaml).
// It is a superset of Client: a connection only needs {server, token,
// insecure}, but the file also caches the scope set + default scope for a
// hosted session, and an operator-assigned slug for an OSS edge.
type Account struct {
	Server             string     `yaml:"server"`
	Token              string     `yaml:"token"`
	Transport          string     `yaml:"transport,omitempty"`
	Kind               string     `yaml:"kind,omitempty"` // "token" (OSS / API key) | "session" (hosted login)
	InsecureSkipVerify bool       `yaml:"insecure_skip_verify,omitempty"`
	Slug               string     `yaml:"slug,omitempty"`          // OSS: operator-assigned fixed scope
	Scopes             []ScopeRef `yaml:"scopes,omitempty"`        // hosted: cached scope set
	DefaultScope       string     `yaml:"default_scope,omitempty"` // `beamd default`
}

// Client returns the connection credential for this account. The yamux stream
// window is NOT an account property (§8.1): it is resolved from the process
// environment at each entry point, never persisted here.
func (a *Account) Client() *Client {
	return &Client{
		Server:             a.Server,
		Token:              a.Token,
		Transport:          a.Transport,
		InsecureSkipVerify: a.InsecureSkipVerify,
	}
}

func beamdDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".beamd"), nil
}

// GlobalPath is ~/.beamd/config.
func GlobalPath() (string, error) { return DefaultClientPath() }

func accountsDir() (string, error) {
	d, err := beamdDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "accounts"), nil
}

func agentsDir() (string, error) {
	d, err := beamdDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "agents"), nil
}

var nonFilenameChars = regexp.MustCompile(`[^a-zA-Z0-9._-]`)

// accountKey turns a server (host:port) into a stable filesystem-safe key.
// Two distinct servers never collide (`:` → `_`, etc.). We never reverse it —
// the server is also stored inside the file.
func accountKey(server string) string {
	return nonFilenameChars.ReplaceAllString(server, "_")
}

// AccountPath is ~/.beamd/accounts/<key>.yaml for a server.
func AccountPath(server string) (string, error) {
	d, err := accountsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, accountKey(server)+".yaml"), nil
}

// AgentSocketFor returns the detached-agent socket for a server's account, so
// each account's detached tunnels are held by their own agent.
func AgentSocketFor(server string) (string, error) {
	d, err := agentsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, accountKey(server)+".sock"), nil
}

func AccountExists(server string) bool {
	p, err := AccountPath(server)
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}

func LoadAccount(server string) (*Account, error) {
	p, err := AccountPath(server)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("read account %q: %w", server, err)
	}
	a := &Account{}
	if err := yaml.Unmarshal(b, a); err != nil {
		return nil, fmt.Errorf("parse account %q: %w", server, err)
	}
	return a, nil
}

func SaveAccount(a *Account) error {
	p, err := AccountPath(a.Server)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("mkdir %q: %w", filepath.Dir(p), err)
	}
	b, err := yaml.Marshal(a)
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}

// DeleteAccount removes accounts/<key>.yaml. Missing file is not an error.
func DeleteAccount(server string) error {
	p, err := AccountPath(server)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// ListAccounts returns every saved account, sorted by server. Files that don't
// parse or carry no server are skipped (not fatal — one bad file shouldn't
// hide the rest).
func ListAccounts() ([]*Account, error) {
	d, err := accountsDir()
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
	var accts []*Account
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(d, e.Name()))
		if err != nil {
			continue
		}
		a := &Account{}
		if err := yaml.Unmarshal(b, a); err != nil || a.Server == "" {
			continue
		}
		accts = append(accts, a)
	}
	sort.Slice(accts, func(i, j int) bool { return accts[i].Server < accts[j].Server })
	return accts, nil
}

// LoadGlobal reads ~/.beamd/config as a Global. A missing file yields a
// zero-value Global (no current account yet), not an error.
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
