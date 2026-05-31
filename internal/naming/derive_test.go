package naming

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSanitize(t *testing.T) {
	cases := map[string]string{
		"myapp":          "myapp",
		"MyApp":          "myapp",
		"my_app":         "my-app",
		"feat/x":         "feat-x",
		"  spaced  ":     "spaced",
		"a--b___c":       "a-b-c",
		"--leading":      "leading",
		"trailing--":     "trailing",
		"@org/app":       "org-app",
		"":               "",
		"...":            "",
		"already-valid1": "already-valid1",
	}
	for in, want := range cases {
		if got := Sanitize(in); got != want {
			t.Errorf("Sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeTruncatesWithHash(t *testing.T) {
	long := strings.Repeat("a", 100)
	got := Sanitize(long)
	if len(got) != 63 {
		t.Fatalf("len = %d, want 63", len(got))
	}
	// Two distinct long inputs must not collide on truncation.
	other := strings.Repeat("a", 99) + "b"
	if Sanitize(long) == Sanitize(other) {
		t.Errorf("distinct long names collided: %q", got)
	}
	if err := ValidateLabel(got); err != nil {
		t.Errorf("truncated label is invalid: %v", err)
	}
}

func TestDeriveLabel_PortAndDir(t *testing.T) {
	if got, err := DeriveLabel("port", 3000, "/whatever"); err != nil || got != "3000" {
		t.Errorf("port: got %q, %v", got, err)
	}
	if got, err := DeriveLabel("", 8080, "/whatever"); err != nil || got != "8080" {
		t.Errorf("empty→port: got %q, %v", got, err)
	}
	dir := filepath.Join(t.TempDir(), "My_Project")
	if got, err := DeriveLabel("dir", 0, dir); err != nil || got != "my-project" {
		t.Errorf("dir: got %q, %v", got, err)
	}
}

func TestDeriveLabel_RepoAndBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := filepath.Join(t.TempDir(), "Cool-Repo")
	mustGit(t, "", "init", root)
	mustGit(t, root, "checkout", "-b", "feat/login")

	if got, err := DeriveLabel("repo", 0, root); err != nil || got != "cool-repo" {
		t.Errorf("repo: got %q, %v", got, err)
	}
	if got, err := DeriveLabel("branch", 0, root); err != nil || got != "feat-login" {
		t.Errorf("branch: got %q, %v", got, err)
	}
}

func TestDeriveLabel_Errors(t *testing.T) {
	notRepo := t.TempDir()
	if _, err := DeriveLabel("repo", 0, notRepo); err == nil {
		t.Error("repo outside a git repo should error")
	}
	if _, err := DeriveLabel("branch", 0, notRepo); err == nil {
		t.Error("branch outside a git repo should error")
	}
	if _, err := DeriveLabel("bogus", 0, notRepo); err == nil {
		t.Error("unknown source should error")
	}
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	var full []string
	if dir != "" {
		full = append(full, "-C", dir)
	}
	full = append(full, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = append(cmd.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
