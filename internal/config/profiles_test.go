package config

import (
	"os"
	"path/filepath"
	"testing"
)

// withHome points os.UserHomeDir at a temp dir for the duration of a test.
func withHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func TestProfileRoundTrip(t *testing.T) {
	withHome(t)
	c := &Client{Server: "edge.example.com:443", Token: "tok"}
	if err := SaveProfile("work", c); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}
	if !ProfileExists("work") {
		t.Fatal("ProfileExists(work) = false after save")
	}
	got, err := LoadProfile("work")
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if got.Server != c.Server || got.Token != c.Token {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	names, err := ListProfiles()
	if err != nil || len(names) != 1 || names[0] != "work" {
		t.Errorf("ListProfiles = %v, %v", names, err)
	}
}

func TestAgentSocketFor(t *testing.T) {
	home := withHome(t)
	got, err := AgentSocketFor("work")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".beamd", "agents", "work.sock")
	if got != want {
		t.Errorf("AgentSocketFor(work) = %q, want %q", got, want)
	}
}

func TestDiscoverProject(t *testing.T) {
	withHome(t)
	root := t.TempDir()
	sub := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	// .beamd at root; .beamd.local overlays `from`.
	if err := os.WriteFile(filepath.Join(root, ".beamd"), []byte("profile: acme\nfrom: repo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".beamd.local"), []byte("from: branch\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	p, dir, err := DiscoverProject(sub)
	if err != nil {
		t.Fatalf("DiscoverProject: %v", err)
	}
	if p == nil {
		t.Fatal("expected a project, got nil")
	}
	if p.Profile != "acme" {
		t.Errorf("Profile = %q, want acme", p.Profile)
	}
	if p.From != "branch" {
		t.Errorf("From = %q, want branch (.beamd.local should override)", p.From)
	}
	if dir != root {
		t.Errorf("found dir = %q, want %q", dir, root)
	}

	// No .beamd anywhere → nil, no error.
	empty := t.TempDir()
	p2, _, err := DiscoverProject(empty)
	if err != nil {
		t.Fatalf("DiscoverProject(empty): %v", err)
	}
	if p2 != nil {
		t.Errorf("expected nil project in empty tree, got %+v", p2)
	}
}

// A `.beamd` *directory* (e.g. the global ~/.beamd config dir, which shares
// the name) must be ignored — only a regular .beamd file is a project file.
func TestDiscoverProject_IgnoresDirectory(t *testing.T) {
	withHome(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".beamd"), 0o755); err != nil {
		t.Fatal(err)
	}
	p, _, err := DiscoverProject(root)
	if err != nil {
		t.Fatalf("DiscoverProject must not error on a .beamd directory: %v", err)
	}
	if p != nil {
		t.Errorf("a .beamd directory should not be treated as a project file, got %+v", p)
	}
}
