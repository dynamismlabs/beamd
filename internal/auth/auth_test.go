package auth

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMemoryStore(t *testing.T) {
	s := NewMemoryStore(map[string]string{"T1": "trey", "T2": "alex"})
	if slug, ok := s.Resolve("T1"); !ok || slug != "trey" {
		t.Errorf("Resolve(T1) = (%q, %v)", slug, ok)
	}
	if _, ok := s.Resolve("nope"); ok {
		t.Error("Resolve(nope) should fail")
	}
}

func TestFileStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.json")
	body := `{"T1":"trey","T2":"alex"}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewFileStore(path)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if slug, ok := s.Resolve("T2"); !ok || slug != "alex" {
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
	if slug, _ := s.Resolve("T"); slug != "s" {
		t.Errorf("got %q", slug)
	}

	if _, err := Open("memory:"); err != nil {
		t.Errorf("Open memory: %v", err)
	}

	if _, err := Open("postgres://..."); err == nil {
		t.Error("unsupported spec should error")
	}
}
