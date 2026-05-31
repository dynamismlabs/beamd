package config

// Project context (.beamd) supplies a per-project identity and/or naming
// default. It is never a secret — it references a profile or an edge by
// hostname, so it is safe to commit. Discovery walks up from cwd to the
// first .beamd, and a sibling .beamd.local (gitignored) overlays it, the
// way .env / .env.local work.

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Project is a parsed .beamd. yaml.v3 ignores unknown keys, so a P1 client
// meeting a future key falls back gracefully instead of erroring
// (forward-compatible by construction).
type Project struct {
	Profile string `yaml:"profile,omitempty"` // references a global profile by name (personal)
	Server  string `yaml:"server,omitempty"`  // references an edge by hostname (committable)
	Name    string `yaml:"name,omitempty"`    // literal label (§2)
	From    string `yaml:"from,omitempty"`    // derive source (§2)
}

const (
	projectFile      = ".beamd"
	projectLocalFile = ".beamd.local"
)

// DiscoverProject walks up from startDir to the first directory containing a
// .beamd (or .beamd.local), stopping at $HOME or the filesystem root. It
// returns the merged config (.beamd.local overlaying .beamd) and the
// directory it was found in. A nil Project with no error means none was
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
		// Match only a regular *file* named .beamd / .beamd.local — never a
		// directory. This matters at $HOME, where `~/.beamd` is the global
		// config directory, not a project file (same name, different thing).
		base := filepath.Join(dir, projectFile)
		local := filepath.Join(dir, projectLocalFile)
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
// p already holds (so .beamd.local wins over .beamd).
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
	if overlay.Name != "" {
		p.Name = overlay.Name
	}
	if overlay.From != "" {
		p.From = overlay.From
	}
	return nil
}
