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

func TestAccountRoundTrip(t *testing.T) {
	withHome(t)
	a := &Account{Server: "edge.example.com:443", Token: "tok", Kind: "session", DefaultScope: "acme"}
	if err := SaveAccount(a); err != nil {
		t.Fatalf("SaveAccount: %v", err)
	}
	if !AccountExists("edge.example.com:443") {
		t.Fatal("AccountExists = false after save")
	}
	got, err := LoadAccount("edge.example.com:443")
	if err != nil {
		t.Fatalf("LoadAccount: %v", err)
	}
	if got.Server != a.Server || got.Token != a.Token || got.Kind != "session" || got.DefaultScope != "acme" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	// Client() projects just the connection credential.
	c := got.Client()
	if c.Server != a.Server || c.Token != a.Token {
		t.Errorf("Client() = %+v", c)
	}
	accts, err := ListAccounts()
	if err != nil || len(accts) != 1 || accts[0].Server != "edge.example.com:443" {
		t.Errorf("ListAccounts = %v, %v", accts, err)
	}
}

func TestAccountKeyedByServer(t *testing.T) {
	withHome(t)
	// Distinct servers → distinct files; the same server overwrites.
	_ = SaveAccount(&Account{Server: "a.test:443", Token: "1"})
	_ = SaveAccount(&Account{Server: "b.test:443", Token: "2"})
	_ = SaveAccount(&Account{Server: "a.test:443", Token: "1b"})
	accts, _ := ListAccounts()
	if len(accts) != 2 {
		t.Fatalf("ListAccounts len = %d, want 2 (%+v)", len(accts), accts)
	}
	a, _ := LoadAccount("a.test:443")
	if a.Token != "1b" {
		t.Errorf("a.test token = %q, want 1b (same server overwrites)", a.Token)
	}
	if err := DeleteAccount("a.test:443"); err != nil {
		t.Fatal(err)
	}
	if AccountExists("a.test:443") {
		t.Error("a.test still exists after delete")
	}
}

func TestAgentSocketFor(t *testing.T) {
	home := withHome(t)
	got, err := AgentSocketFor("edge.example.com:443")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".beamd", "agents", "edge.example.com_443.sock")
	if got != want {
		t.Errorf("AgentSocketFor = %q, want %q", got, want)
	}
}

func TestDiscoverProject(t *testing.T) {
	withHome(t)
	root := t.TempDir()
	sub := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	// beamd.yaml at root pins server + scope + services; beamd.local.yaml
	// overlays `from` and repoints a single service (per-key merge).
	if err := os.WriteFile(filepath.Join(root, ProjectFile), []byte("server: edge.acme.com\nscope: acme\nfrom: repo\nservices:\n  api: 3000\n  web: 8080\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ProjectLocalFile), []byte("from: branch\nservices:\n  api: 3999\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	p, dir, err := DiscoverProject(sub)
	if err != nil {
		t.Fatalf("DiscoverProject: %v", err)
	}
	if p == nil {
		t.Fatal("expected a project, got nil")
	}
	if p.Server != "edge.acme.com" {
		t.Errorf("Server = %q, want edge.acme.com", p.Server)
	}
	if p.Scope != "acme" {
		t.Errorf("Scope = %q, want acme", p.Scope)
	}
	if p.From != "branch" {
		t.Errorf("From = %q, want branch (beamd.local.yaml should override)", p.From)
	}
	// Services merge per-key: overlay repoints api, web is preserved.
	if p.Services["api"] != 3999 {
		t.Errorf("Services[api] = %d, want 3999 (beamd.local.yaml override)", p.Services["api"])
	}
	if p.Services["web"] != 8080 {
		t.Errorf("Services[web] = %d, want 8080 (preserved from beamd.yaml)", p.Services["web"])
	}
	if dir != root {
		t.Errorf("found dir = %q, want %q", dir, root)
	}

	// No beamd.yaml anywhere → nil, no error.
	empty := t.TempDir()
	p2, _, err := DiscoverProject(empty)
	if err != nil {
		t.Fatalf("DiscoverProject(empty): %v", err)
	}
	if p2 != nil {
		t.Errorf("expected nil project in empty tree, got %+v", p2)
	}
}

// A directory that happens to share the project-file name (e.g. someone's
// `beamd.yaml/` dir) must be ignored — only a regular file is a project file.
func TestDiscoverProject_IgnoresDirectory(t *testing.T) {
	withHome(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ProjectFile), 0o755); err != nil {
		t.Fatal(err)
	}
	p, _, err := DiscoverProject(root)
	if err != nil {
		t.Fatalf("DiscoverProject must not error on a %s directory: %v", ProjectFile, err)
	}
	if p != nil {
		t.Errorf("a %s directory should not be treated as a project file, got %+v", ProjectFile, p)
	}
}
