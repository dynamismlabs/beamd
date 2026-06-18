package config

// Project context (beamd.yaml) supplies a per-project identity and/or naming
// default. It is never a secret — it references an org by slug or an edge by
// hostname, so it is safe to commit. Discovery walks up from cwd to the first
// beamd.yaml, and a sibling beamd.local.yaml (gitignored) overlays it, the
// way .env / .env.local work. Write one interactively with `beamd link`.

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Project is a parsed beamd.yaml. yaml.v3 ignores unknown keys, so a P1 client
// meeting a future key falls back gracefully instead of erroring
// (forward-compatible by construction).
type Project struct {
	Server string `yaml:"server,omitempty"` // which edge/account, by hostname (committable)
	Scope  string `yaml:"scope,omitempty"`  // which org/scope on that account (committable)
	Name   string `yaml:"name,omitempty"`   // literal label (§2)
	From   string `yaml:"from,omitempty"`   // derive source (§2)

	// Services maps a name → local port for the repo's apps, so `beamd open
	// api` exposes the right port under the label `api` (and `beamd open web`
	// the other). The shared identity (server/scope) is reused; only the label
	// + port differ per service. Names must be valid subdomain labels.
	Services map[string]int `yaml:"services,omitempty"`

	// Profile is the superseded per-edge alias (pre-accounts). Still parsed so
	// an old project file doesn't error, but no longer used for resolution —
	// pin the edge with `server:` instead. See docs/identity-and-accounts.md.
	Profile string `yaml:"profile,omitempty"`
}

// ProjectFile is the committable per-project config; ProjectLocalFile is its
// gitignored overlay. Exported so the CLI (`beamd link`) writes the same names
// discovery reads.
const (
	ProjectFile      = "beamd.yaml"
	ProjectLocalFile = "beamd.local.yaml"
)

// DiscoverProject walks up from startDir to the first directory containing a
// beamd.yaml (or beamd.local.yaml), stopping at $HOME or the filesystem root.
// It returns the merged config (beamd.local.yaml overlaying beamd.yaml) and
// the directory it was found in. A nil Project with no error means none was
// found — callers fall back to the global config.
func DiscoverProject(startDir string) (*Project, string, error) {
	dir, err := filepath.Abs(startDir)
	if err != nil {
		return nil, "", err
	}
	home, _ := os.UserHomeDir()
	if home != "" {
		home = filepath.Clean(home)
	}

	for {
		// Match only a regular *file* — guard against a stray directory that
		// happens to share the name (e.g. someone's `beamd.yaml/` dir).
		base := filepath.Join(dir, ProjectFile)
		local := filepath.Join(dir, ProjectLocalFile)
		baseOK := isRegularFile(base)
		localOK := isRegularFile(local)
		if baseOK || localOK {
			p := &Project{}
			if baseOK {
				if err := loadProjectFile(base, p); err != nil {
					return nil, "", err
				}
			}
			if localOK {
				if err := loadProjectFile(local, p); err != nil { // overlay
					return nil, "", err
				}
			}
			return p, dir, nil
		}

		parent := filepath.Dir(dir)
		if parent == dir || dir == home {
			return nil, "", nil // reached root or $HOME without a match
		}
		dir = parent
	}
}

func isRegularFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}

// loadProjectFile unmarshals path into p. Non-empty fields overlay whatever
// p already holds (so beamd.local.yaml wins over beamd.yaml).
func loadProjectFile(path string, p *Project) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %q: %w", path, err)
	}
	overlay := &Project{}
	if err := yaml.Unmarshal(b, overlay); err != nil {
		return fmt.Errorf("parse %q: %w", path, err)
	}
	if overlay.Profile != "" {
		p.Profile = overlay.Profile
	}
	if overlay.Server != "" {
		p.Server = overlay.Server
	}
	if overlay.Scope != "" {
		p.Scope = overlay.Scope
	}
	if overlay.Name != "" {
		p.Name = overlay.Name
	}
	if overlay.From != "" {
		p.From = overlay.From
	}
	// Services merge per-key (not whole-map replace) so beamd.local.yaml can
	// add or repoint a single service without restating the rest.
	for name, port := range overlay.Services {
		if p.Services == nil {
			p.Services = map[string]int{}
		}
		p.Services[name] = port
	}
	return nil
}
