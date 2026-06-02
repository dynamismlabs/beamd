package auth

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMemoryStore(t *testing.T) {
	s := NewMemoryStore(map[string]string{"T1": "turing", "T2": "hopper"})
	// No requested scope → the token's slug.
	if slug, ok := s.Resolve("T1", ""); !ok || slug != "turing" {
		t.Errorf("Resolve(T1, \"\") = (%q, %v)", slug, ok)
	}
	// A matching requested scope is allowed.
	if slug, ok := s.Resolve("T1", "turing"); !ok || slug != "turing" {
		t.Errorf("Resolve(T1, turing) = (%q, %v)", slug, ok)
	}
	// A different requested scope is rejected (single-scope credential).
	if _, ok := s.Resolve("T1", "hopper"); ok {
		t.Error("Resolve(T1, hopper) should reject — token can't act in another scope")
	}
	if _, ok := s.Resolve("nope", ""); ok {
		t.Error("Resolve(nope) should fail")
	}
}

func TestFileStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.json")
	body := `{"T1":"turing","T2":"hopper"}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewFileStore(path)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if slug, ok := s.Resolve("T2", ""); !ok || slug != "hopper" {
		t.Errorf("Resolve(T2) = (%q, %v)", slug, ok)
	}
}

func TestOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.json")
	if err := os.WriteFile(path, []byte(`{"T":"s"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := Open("file:" + path)
	if err != nil {
		t.Fatalf("Open file: %v", err)
	}
	if slug, _ := s.Resolve("T", ""); slug != "s" {
		t.Errorf("got %q", slug)
	}

	if _, err := Open("memory:"); err != nil {
		t.Errorf("Open memory: %v", err)
	}

	if _, err := Open("postgres://..."); err == nil {
		t.Error("unsupported spec should error")
	}
}
